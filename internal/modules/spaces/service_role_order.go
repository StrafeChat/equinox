package spaces

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
)

// Role order. A custom role's position is its rank: a higher position outranks a lower one,
// @everyone sits at 0 below them all, and every hierarchy check is "strictly below the actor's
// highest role". On a space's origin the custom roles' positions are unique and >= 1 after
// every write - two roles at one position compare as "at or above" each other, which locked
// both for anyone holding either. Every write that moves a role decides where the roles go
// with one of three pure functions and writes that with applyRoleOrder, under the space's role
// lock (lockRoles): placeRoles (insert and push up: a create, a bot's role, the batch reorder,
// the backfill), swapRoles (one role's PATCH {position}: it trades slots with the role there)
// and closeRoleGaps (the roles above a removed bot role move down into its slot).

// MaxRolePosition is the highest position a request may ask for. Positions are a 32-bit
// column, and placing a role pushes at most MaxRolesPerSpace others up by one each, so the
// headroom keeps every write in range.
const MaxRolePosition = math.MaxInt32 - MaxRolesPerSpace

var (
	ErrCannotMoveEveryone   = errors.New("cannot move @everyone")
	ErrInvalidRolePosition  = fmt.Errorf("role position must be from 1 to %d", MaxRolePosition)
	ErrInvalidRolePositions = fmt.Errorf("role positions must list from 1 to %d roles, each once", MaxRolesPerSpace)
)

// roleLocks serializes the writes to one space's roles within this process. It is package
// state, not a Service field, because a process holds several Services (the REST one, the
// OAuth2 installer's, the federation engine's) and they must all queue on the same lock. Two
// role-order writes that interleaved would read the same positions and leave roles tied; a
// position written back onto a role deleted meanwhile would recreate a stub of its row (a CQL
// UPDATE writes a row that is not there). Not shared between API replicas.
var roleLocks roleLockTable

type roleLockTable struct {
	mu    sync.Mutex
	locks map[int64]*roleLock
}

type roleLock struct {
	sync.Mutex
	users int // holder and waiters; the entry is dropped when the last one leaves
}

// lockRoles takes the space's role lock and returns its release, which may be called more
// than once. Taken after the space's federation check (a mirror's roles are the origin's to
// write) and never twice by one request: it does not nest.
func lockRoles(spaceID int64) func() {
	return roleLocks.lock(spaceID)
}

func (t *roleLockTable) lock(spaceID int64) func() {
	t.mu.Lock()
	if t.locks == nil {
		t.locks = make(map[int64]*roleLock)
	}
	l := t.locks[spaceID]
	if l == nil {
		l = &roleLock{}
		t.locks[spaceID] = l
	}
	l.users++
	t.mu.Unlock()
	l.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.Unlock()
			t.mu.Lock()
			if l.users--; l.users == 0 {
				delete(t.locks, spaceID)
			}
			t.mu.Unlock()
		})
	}
}

// placeRoles returns the new positions of a space's custom roles (every role but @everyone)
// after moves (role id -> requested position, each >= 1): only the roles whose position
// changes. The custom roles are sorted by key - the requested position for a moved role, its
// current position otherwise - and at equal keys a moved role comes first (it takes the slot;
// the occupant goes above it), then the role with the higher id (Discord: of two roles at one
// position, the newer ranks lower). Positions are then handed out in that order:
// p0 = max(key0, 1), pi = max(keyi, p(i-1)+1). A role that already fits keeps its position and
// only a colliding run is pushed up, so a write touches as few rows as it can; the relative
// order of the roles not in moves never changes; and with no moves it only resolves ties.
// Moves naming @everyone or a role not in roles are ignored (callers refuse them first).
func placeRoles(roles []SpaceRole, everyoneID int64, moves map[int64]int) map[int64]int {
	type entry struct {
		id       int64
		key, cur int
		moved    bool
	}
	order := make([]entry, 0, len(roles))
	for i := range roles {
		r := &roles[i]
		if r.ID == everyoneID {
			continue
		}
		e := entry{id: r.ID, key: r.Position, cur: r.Position}
		if p, ok := moves[r.ID]; ok {
			e.key, e.moved = p, true
		}
		order = append(order, e)
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := &order[i], &order[j]
		if a.key != b.key {
			return a.key < b.key
		}
		if a.moved != b.moved {
			return a.moved
		}
		return a.id > b.id
	})
	out := make(map[int64]int)
	prev := 0
	for _, e := range order {
		p := e.key
		if p <= prev {
			p = prev + 1
		}
		prev = p
		if p != e.cur {
			out[e.id] = p
		}
	}
	return out
}

// swapRoles is where one role's move (PATCH /spaces/:id/roles/:roleId {position}) puts the
// roles: the role takes the position it asked for, and the custom role holding it - if any -
// takes the role's old one; a free position is written as asked. Only the changed roles are
// returned, as from placeRoles.
//
// Swapping rather than inserting is what keeps the clients that reorder with one such request
// per role landing where they meant. Every client that does - the mobile app's releases, the
// desktop app, older web builds - hands the roles it moves the positions they already hold,
// reassigned in the new order (or 1..N for all of them), so the requests together are a
// permutation of slots: each one finalizes its slot and only ever displaces a role not yet
// placed, so they end in the intended order whatever order they arrive in. placeRoles' insert
// and push-up would move every role above the slot each time, and requests after the first
// would name slots that hold something else by then.
//
// The swap keeps positions unique only when they already are, so a space with ties or a role
// below 1 (legacy data the backfill had not reached) goes through placeRoles instead, which
// also settles them. For a non-owner, the caller's checks - the role is below their highest,
// and so is the position asked for - put the role displaced below them too.
func swapRoles(roles []SpaceRole, everyoneID, roleID int64, want int) map[int64]int {
	if !uniquePositions(roles, everyoneID) {
		return placeRoles(roles, everyoneID, map[int64]int{roleID: want})
	}
	from, occupant, found := 0, int64(0), false
	for i := range roles {
		r := &roles[i]
		switch {
		case r.ID == everyoneID:
		case r.ID == roleID:
			from, found = r.Position, true
		case r.Position == want:
			occupant = r.ID
		}
	}
	out := make(map[int64]int)
	if !found || from == want {
		return out
	}
	out[roleID] = want
	if occupant != 0 {
		out[occupant] = from
	}
	return out
}

// uniquePositions reports whether every custom role holds a position of its own, all >= 1.
func uniquePositions(roles []SpaceRole, everyoneID int64) bool {
	seen := make(map[int]struct{}, len(roles))
	for i := range roles {
		r := &roles[i]
		if r.ID == everyoneID {
			continue
		}
		if r.Position < 1 {
			return false
		}
		if _, dup := seen[r.Position]; dup {
			return false
		}
		seen[r.Position] = struct{}{}
	}
	return true
}

// closeRoleGaps is where the custom roles left after some were removed (a bot's managed roles,
// when it leaves) go: each moves down by the number of removed positions strictly below it,
// to no lower than 1 and always above the role ranked just under it, so the order never
// changes - a role that was tied with a removed one keeps its place against the others. kept
// is the roles left (with @everyone or without), gone the removed roles' positions; only the
// changed roles are returned.
//
// Not placeRoles with moves: that puts a moved role first among equal keys, so a role moving
// down onto a role that stays (they were tied with a removed one between them) would be put
// below it, swapping their ranks.
func closeRoleGaps(kept []SpaceRole, everyoneID int64, gone []int) map[int64]int {
	order := make([]SpaceRole, 0, len(kept))
	for _, r := range kept {
		if r.ID != everyoneID {
			order = append(order, r)
		}
	}
	// By rank: position, and at a tie the newer role (higher id) lower, as everywhere else.
	sort.Slice(order, func(i, j int) bool {
		if order[i].Position != order[j].Position {
			return order[i].Position < order[j].Position
		}
		return order[i].ID > order[j].ID
	})
	out := make(map[int64]int)
	prev := 0
	for _, r := range order {
		shift := 0
		for _, g := range gone {
			if g < r.Position {
				shift++
			}
		}
		p := max(r.Position-shift, 1)
		if p <= prev {
			p = prev + 1
		}
		prev = p
		if p != r.Position {
			out[r.ID] = p
		}
	}
	return out
}

// applyRoleOrder is the one writer of role positions. It runs under the space's role lock,
// with roles as read under it - plus, for a create, the new role, not stored yet - and the
// subject's other edits already made on its entry; placed is the placement function's
// answer for them (placeRoles, swapRoles or closeRoleGaps).
// subject is the role the request creates or edits (0 for a reorder or the backfill): it is
// written even when its position stays, since its other fields may have changed, and when
// created is set it is inserted, last, once the run above it has made room.
//
// The rows go out in an order that never shows two roles the other way round when neither
// the old order nor the new one has them so: a permission check reading meanwhile sees each
// role at its old position or its new one, so at worst two roles tie for a moment, and a tie
// refuses (strictly below) rather than allows. First the rows whose position stays, then the
// roles moving down, lowest destination first, then the roles moving up, highest destination
// first, then the created role. After the writes the snapshot is dropped, every written role
// is relayed to the instances mirroring the space and - when announce is set and a role other
// than the subject moved - one SPACE_UPDATE carries the whole role list. A failed write still
// drops the snapshot and relays what was written, and when announce is set and something was
// written, the roles are read back and that SPACE_UPDATE carries them as stored, so local
// clients see what the database and the mirrors now hold rather than the order before.
// Returns the roles as stored, by position.
func (s *Service) applyRoleOrder(ctx context.Context, sp *Space, everyoneID int64, roles []SpaceRole, placed map[int64]int, subject int64, created, announce bool) ([]SpaceRole, error) {
	type write struct {
		role *SpaceRole
		from int
	}
	now := time.Now().UTC()
	var writes []write
	var fresh *SpaceRole
	moved := false
	for i := range roles {
		r := &roles[i]
		from := r.Position
		if p, ok := placed[r.ID]; ok && r.ID != everyoneID {
			r.Position = p
		}
		switch {
		case r.ID == subject && created:
			fresh = r
			continue
		case r.ID == subject:
		case r.Position != from:
			moved = true
		default:
			continue
		}
		r.UpdatedAt = now
		writes = append(writes, write{role: r, from: from})
	}
	direction := func(w write) int {
		switch {
		case w.role.Position < w.from:
			return 1
		case w.role.Position > w.from:
			return 2
		}
		return 0
	}
	sort.SliceStable(writes, func(i, j int) bool {
		a, b := writes[i], writes[j]
		da, db := direction(a), direction(b)
		switch {
		case da != db:
			return da < db
		case da == 1:
			return a.role.Position < b.role.Position
		case da == 2:
			return a.role.Position > b.role.Position
		}
		return false
	})
	var written []*SpaceRole
	settle := func() {
		if len(written) == 0 {
			return
		}
		s.invalidateSnapshot(ctx, sp.ID)
		for _, r := range written {
			s.fedRoleChanged(ctx, sp.ID, r, false)
		}
	}
	fail := func(err error) ([]SpaceRole, error) {
		settle()
		if announce && len(written) > 0 {
			// Part of the new order is stored and relayed; the request's own events will not go
			// out. Tell local clients what is there, read back rather than assumed.
			if stored, lerr := s.repo.ListSpaceRoles(ctx, sp.ID); lerr == nil {
				sortRoles(stored)
				s.publishRoleOrder(ctx, sp, everyoneID, stored)
			} else {
				logger.Err("spaces", lerr, map[string]any{"space_id": sp.ID, "step": "re-read roles after a failed role-order write"})
			}
		}
		return nil, err
	}
	for _, w := range writes {
		if err := s.repo.UpdateSpaceRole(ctx, w.role); err != nil {
			return fail(err)
		}
		written = append(written, w.role)
	}
	if fresh != nil {
		if err := s.repo.InsertSpaceRole(ctx, fresh); err != nil {
			return fail(err)
		}
		written = append(written, fresh)
	}
	settle()
	all := append([]SpaceRole(nil), roles...)
	sortRoles(all)
	if announce && moved {
		s.publishRoleOrder(ctx, sp, everyoneID, all)
	}
	return all, nil
}

// publishRoleOrder tells the space its whole role list in one SPACE_UPDATE: what a write that
// moved roles besides the one it names sends, rather than an event per role.
//
// The event is a whole space - clients replace theirs with it, defaulting what is missing - so
// the space is read again here instead of sending the caller's copy, which was read before the
// request's writes and may predate a settings change made meanwhile (the role lock does not
// cover those): sending it would put the old name or settings back on every client. A space
// gone by now is not announced; one that cannot be read is sent as the caller read it, the
// best there is, rather than leaving clients without the new order.
func (s *Service) publishRoleOrder(ctx context.Context, sp *Space, everyoneID int64, roles []SpaceRole) {
	cur, err := s.repo.GetByID(ctx, sp.ID)
	switch {
	case err != nil:
		logger.Warn("spaces", "re-read space %d for its role order: %v", sp.ID, err)
		cur = sp
	case cur == nil:
		return
	}
	payload := spaceToEventPayload(cur)
	payload["everyone_role_id"] = id.Format(everyoneID)
	payload["roles"] = RoleMaps(roles)
	s.publishSpaceEvent(ctx, sp.ID, "SPACE_UPDATE", payload)
}

// ReorderSpaceRoles moves roles (PATCH /spaces/:id/roles, Discord's Modify Guild Role
// Positions) and returns every role of the space, by position. Manage Roles; except for the
// owner, every listed role must be below the actor's own highest and every requested position
// too, or nothing is written - the same rule as moving one role, so a reorder can never lift a
// role to or past its mover. @everyone never moves; a bot's managed role can. Each listed
// role whose position changed is audited; the roles pushed up to make room are not.
func (s *Service) ReorderSpaceRoles(ctx context.Context, actorID, spaceID int64, moves []RolePosition) ([]SpaceRole, error) {
	if err := checkRolePositions(moves); err != nil {
		return nil, err
	}
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return nil, err
	} else if origin != "" {
		return s.fed.RemoteReorderRoles(ctx, origin, spaceID, actor, moves)
	}
	if err := s.canManageRoles(ctx, actorID, spaceID); err != nil {
		return nil, err
	}
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil || sp == nil {
		return nil, ErrSpaceNotFound
	}
	everyoneID, err := s.ensureEveryoneRoleID(ctx, sp)
	if err != nil {
		return nil, err
	}
	unlock := lockRoles(spaceID)
	defer unlock()
	roles, err := s.repo.ListSpaceRoles(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	byID := indexRoles(roles)
	want := make(map[int64]int, len(moves))
	before := make(map[int64]int, len(moves))
	for _, m := range moves {
		if m.RoleID == everyoneID {
			return nil, ErrCannotMoveEveryone
		}
		r := byID[m.RoleID]
		if r == nil {
			return nil, ErrRoleNotFound
		}
		want[m.RoleID] = m.Position
		before[m.RoleID] = r.Position
	}
	if sp.OwnerID != actorID {
		highest, err := s.actorHighestPosition(ctx, spaceID, actorID, everyoneID, byID)
		if err != nil {
			return nil, err
		}
		for _, m := range moves {
			if before[m.RoleID] >= highest || m.Position >= highest {
				return nil, ErrRoleHierarchy
			}
		}
	}
	placed := placeRoles(roles, everyoneID, want)
	all, err := s.applyRoleOrder(ctx, sp, everyoneID, roles, placed, 0, false, true)
	if err != nil {
		return nil, err
	}
	for _, m := range moves {
		if p, ok := placed[m.RoleID]; ok {
			s.audit(ctx, spaceID, actorID, AuditRoleUpdate, id.Format(m.RoleID), map[string]change{
				"position": {Old: before[m.RoleID], New: p},
			}, "")
		}
	}
	return all, nil
}

// checkRolePositions is the shape a reorder must have before anything is read: 1 to
// MaxRolesPerSpace entries, no role twice, every position in range.
func checkRolePositions(moves []RolePosition) error {
	if len(moves) == 0 || len(moves) > MaxRolesPerSpace {
		return ErrInvalidRolePositions
	}
	seen := make(map[int64]struct{}, len(moves))
	for _, m := range moves {
		if _, dup := seen[m.RoleID]; dup {
			return ErrInvalidRolePositions
		}
		seen[m.RoleID] = struct{}{}
		if !validRolePosition(m.Position) {
			return ErrInvalidRolePosition
		}
	}
	return nil
}

func validRolePosition(p int) bool { return p >= 1 && p <= MaxRolePosition }

// indexRoles maps a role list by id (pointers into roles).
func indexRoles(roles []SpaceRole) map[int64]*SpaceRole {
	byID := make(map[int64]*SpaceRole, len(roles))
	for i := range roles {
		byID[roles[i].ID] = &roles[i]
	}
	return byID
}

// findRole returns a copy of the role with roleID in roles (nil when absent).
func findRole(roles []SpaceRole, roleID int64) *SpaceRole {
	for i := range roles {
		if roles[i].ID == roleID {
			r := roles[i]
			return &r
		}
	}
	return nil
}
