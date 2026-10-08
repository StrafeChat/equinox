// Package threads is Discord's thread model adapted to Strafe: a thread is a room of type
// rooms.TypeThread inside a space text channel, started from a message (then it shares the
// message's id) or on its own, public or private, with an explicit member list, an archive
// state that times out on its own, and a lock.
//
// Permissions come from the parent channel - the space snapshot resolves a thread's
// overrides to its channel's - plus four bits of their own: Create Public Threads, Create
// Private Threads, Send Messages In Threads (Send Messages itself does not grant sending in a
// thread) and Manage Threads (edit, archive, lock, delete any thread; see private ones). A
// private thread is visible only to its members and Manage Threads holders.
package threads

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/messages"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
	"github.com/StrafeChat/equinox/internal/stargate"
)

const (
	MaxThreadNameRunes       = 100
	MaxActiveThreadsPerSpace = 1000
	DefaultAutoArchiveMin    = 1440
	// Discord's auto-archive choices: an hour, a day, three days, a week.
	MaxAutoJoinMentions = 10
)

// SystemThreadCreated is posted in the parent channel when a public thread starts
// ({"actor_id", "thread_id", "name"}); SystemThreadRenamed in the thread itself
// ({"actor_id", "name"}).
const (
	SystemThreadCreated = "thread_created"
	SystemThreadRenamed = "thread_renamed"
)

var autoArchiveChoices = map[int]bool{60: true, 1440: true, 4320: true, 10080: true}

var (
	ErrNotFound            = errors.New("thread not found")
	ErrNotThread           = errors.New("that room is not a thread")
	ErrNotTextChannel      = errors.New("threads can only be created in a text channel")
	ErrMissingPerm         = errors.New("missing permission")
	ErrInvalidName         = errors.New("thread name must be 1-100 characters")
	ErrInvalidAutoArchive  = errors.New("auto_archive_minutes must be 60, 1440, 4320 or 10080")
	ErrAlreadyThread       = errors.New("that message already has a thread")
	ErrMessageNotFound     = errors.New("message not found")
	ErrTooManyThreads      = errors.New("this space already has the maximum number of active threads")
	ErrArchived            = errors.New("this thread is archived")
	ErrLocked              = errors.New("this thread is locked")
	ErrNotInvitable        = errors.New("only moderators can add people to this thread")
	ErrNotMember           = errors.New("you are not in this thread")
	ErrRemoteSpace         = errors.New("threads in a space hosted elsewhere are managed on its home instance")
	ErrCannotViewParent    = errors.New("that member cannot view the channel this thread is in")
	ErrInvalidSlowmode     = errors.New("slowmode must be between 0 and 21600 seconds")
	ErrSystemMessageThread = errors.New("a thread cannot be started from a system message")
)

// Federation tells whether a space is hosted on another instance.
type Federation interface {
	RemoteSpaceOrigin(ctx context.Context, spaceID int64) string
}

type Service struct {
	rooms  rooms.Repository
	msgs   messages.Repository
	spaces *spaces.Service
	users  auth.UserRepository
	redis  *redis.Client
	cfg    *config.Config
	notify rooms.OnRoomSystemEvent
	fed    Federation
}

func NewService(roomRepo rooms.Repository, msgRepo messages.Repository, spaceSvc *spaces.Service, users auth.UserRepository, rdb *redis.Client, cfg *config.Config, notify rooms.OnRoomSystemEvent) *Service {
	return &Service{rooms: roomRepo, msgs: msgRepo, spaces: spaceSvc, users: users, redis: rdb, cfg: cfg, notify: notify}
}

func (s *Service) SetFederation(f Federation) { s.fed = f }

// CreateInput is POST /rooms/:id/threads and POST /rooms/:id/messages/:msg_id/threads.
type CreateInput struct {
	Name               string `json:"name"`
	AutoArchiveMinutes int    `json:"auto_archive_minutes"`
	// Standalone threads only: a private thread (needs Create Private Threads) and whether
	// its members may add others (Discord's invitable; default true).
	Private   bool  `json:"private"`
	Invitable *bool `json:"invitable"`
}

// UpdateInput is PATCH /rooms/:id/thread; nil fields are left alone.
type UpdateInput struct {
	Name               *string `json:"name"`
	Archived           *bool   `json:"archived"`
	Locked             *bool   `json:"locked"`
	Invitable          *bool   `json:"invitable"`
	AutoArchiveMinutes *int    `json:"auto_archive_minutes"`
	SlowmodeSeconds    *int    `json:"slowmode_seconds"`
}

// Thread is a thread room with the per-viewer state the wire form carries.
type Thread struct {
	Room  *rooms.Room
	State spaces.ThreadState
}

// Member is one thread member with their profile.
type Member struct {
	UserID   int64
	JoinedAt time.Time
	User     *auth.User
}

// ---- lookups ----------------------------------------------------------------------------

func (s *Service) loadThread(ctx context.Context, threadID int64) (*rooms.Room, error) {
	r, err := s.rooms.GetByID(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if r == nil || r.SpaceID == nil {
		return nil, ErrNotFound
	}
	if r.Type != rooms.TypeThread {
		return nil, ErrNotThread
	}
	return r, nil
}

func (s *Service) loadTextChannel(ctx context.Context, roomID int64) (*rooms.Room, error) {
	r, err := s.rooms.GetByID(ctx, roomID)
	if err != nil {
		return nil, err
	}
	if r == nil || r.SpaceID == nil {
		return nil, ErrNotFound
	}
	if r.Type != rooms.TypeSpaceText {
		return nil, ErrNotTextChannel
	}
	return r, nil
}

func (s *Service) assertLocal(ctx context.Context, spaceID int64) error {
	if s.fed != nil && s.fed.RemoteSpaceOrigin(ctx, spaceID) != "" {
		return ErrRemoteSpace
	}
	return nil
}

// perms is the member's effective permissions in a room (a thread resolves through its
// channel; a private thread is 0 for non-members). ErrNotFound for non-members of the space.
func (s *Service) perms(ctx context.Context, userID int64, room *rooms.Room) (int64, error) {
	p, err := s.spaces.EffectiveChannelPermissions(ctx, userID, *room.SpaceID, room.ID)
	if err != nil {
		if errors.Is(err, spaces.ErrNotMember) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	return p, nil
}

// viewerPerms is perms plus the rule that a room you cannot view does not exist for you.
func (s *Service) viewerPerms(ctx context.Context, userID int64, room *rooms.Room) (int64, error) {
	p, err := s.perms(ctx, userID, room)
	if err != nil {
		return 0, err
	}
	if !permissions.Has(p, permissions.PermViewRoom) {
		return 0, ErrNotFound
	}
	return p, nil
}

func validName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n == 0 || n > MaxThreadNameRunes {
		return "", ErrInvalidName
	}
	return name, nil
}

func validAutoArchive(minutes int) (int, error) {
	if minutes == 0 {
		return DefaultAutoArchiveMin, nil
	}
	if !autoArchiveChoices[minutes] {
		return 0, ErrInvalidAutoArchive
	}
	return minutes, nil
}

func (s *Service) activeThreadCount(ctx context.Context, spaceID int64) (int, error) {
	list, _, err := s.spaces.SpaceRoomsWithOverrides(ctx, spaceID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range list {
		if r.Type == rooms.TypeThread && !r.ThreadIsArchived() {
			n++
		}
	}
	return n, nil
}

// ---- creation ---------------------------------------------------------------------------

// CreateFromMessage starts a public thread from a message in a text channel; the thread's id
// is the message's id (Discord's rule), and the message points back at it.
func (s *Service) CreateFromMessage(ctx context.Context, actorID, channelID, msgID int64, in CreateInput) (*Thread, error) {
	parent, err := s.loadTextChannel(ctx, channelID)
	if err != nil {
		return nil, err
	}
	if err := s.assertLocal(ctx, *parent.SpaceID); err != nil {
		return nil, err
	}
	p, err := s.viewerPerms(ctx, actorID, parent)
	if err != nil {
		return nil, err
	}
	if !permissions.Has(p, permissions.PermCreatePublicThreads) {
		return nil, ErrMissingPerm
	}
	msg, err := s.msgs.GetByID(ctx, channelID, msgID)
	if err != nil {
		return nil, err
	}
	if msg == nil || (msg.DeletedAt != nil && !msg.DeletedAt.IsZero()) {
		return nil, ErrMessageNotFound
	}
	if msg.SystemType != "" {
		return nil, ErrSystemMessageThread
	}
	if msg.ThreadID != nil {
		return nil, ErrAlreadyThread
	}
	if existing, _ := s.rooms.GetByID(ctx, msgID); existing != nil {
		return nil, ErrAlreadyThread
	}
	thread, err := s.create(ctx, actorID, parent, msgID, in, false, &msgID)
	if err != nil {
		return nil, err
	}
	_ = s.msgs.SetThreadID(ctx, channelID, msgID, &msgID)
	return thread, nil
}

// Create starts a thread that is not attached to a message: public, or private when asked
// (private threads need Create Private Threads and start with only their creator in them).
func (s *Service) Create(ctx context.Context, actorID, channelID int64, in CreateInput) (*Thread, error) {
	parent, err := s.loadTextChannel(ctx, channelID)
	if err != nil {
		return nil, err
	}
	if err := s.assertLocal(ctx, *parent.SpaceID); err != nil {
		return nil, err
	}
	p, err := s.viewerPerms(ctx, actorID, parent)
	if err != nil {
		return nil, err
	}
	need := permissions.PermCreatePublicThreads
	if in.Private {
		need = permissions.PermCreatePrivateThreads
	}
	if !permissions.Has(p, need) {
		return nil, ErrMissingPerm
	}
	return s.create(ctx, actorID, parent, id.Next(), in, in.Private, nil)
}

func (s *Service) create(ctx context.Context, actorID int64, parent *rooms.Room, threadID int64, in CreateInput, private bool, starter *int64) (*Thread, error) {
	name, err := validName(in.Name)
	if err != nil {
		return nil, err
	}
	autoArchive, err := validAutoArchive(in.AutoArchiveMinutes)
	if err != nil {
		return nil, err
	}
	if n, err := s.activeThreadCount(ctx, *parent.SpaceID); err != nil {
		return nil, err
	} else if n >= MaxActiveThreadsPerSpace {
		return nil, ErrTooManyThreads
	}
	now := time.Now().UTC()
	f, t := false, true
	invitable := true
	if in.Invitable != nil {
		invitable = *in.Invitable
	}
	spaceID := *parent.SpaceID
	parentID := parent.ID
	room := &rooms.Room{
		ID:                     threadID,
		Type:                   rooms.TypeThread,
		SpaceID:                &spaceID,
		ParentID:               &parentID,
		Name:                   name,
		CreatorID:              actorID,
		SlowmodeSeconds:        parent.SlowmodeSeconds,
		E2EEEnabled:            parent.E2EEEnabled,
		ThreadArchived:         &f,
		ThreadAutoArchiveMin:   autoArchive,
		ThreadLocked:           &f,
		ThreadPrivate:          boolPtr(private),
		ThreadInvitable:        boolPtr(invitable),
		ThreadLastActiveAt:     &now,
		ThreadStarterMessageID: starter,
	}
	_ = t
	if err := s.rooms.CreateThread(ctx, room); err != nil {
		return nil, err
	}
	if err := s.rooms.AddThreadMember(ctx, threadID, actorID, now); err != nil {
		return nil, err
	}
	_ = s.rooms.EnqueueThreadArchive(ctx, threadID, now.Add(time.Duration(autoArchive)*time.Minute))
	s.spaces.InvalidateSnapshot(ctx, spaceID)
	thread := &Thread{Room: room, State: spaces.ThreadState{MessageCount: 0, MemberCount: 1, Joined: true}}
	payload := s.threadJSON(thread)
	// Discord's newly_created: the broadcast carries the creator's membership, and nobody
	// else is in the thread yet - clients key their own "joined" off this flag.
	payload["newly_created"] = true
	s.publish(ctx, room, "THREAD_CREATE", payload)
	if !private {
		// Discord's THREAD_CREATED notice in the channel: "X started a thread: name".
		s.postNotice(ctx, parent, SystemThreadCreated, map[string]interface{}{
			"actor_id": id.Format(actorID), "thread_id": id.Format(threadID), "name": name,
		})
	}
	s.spaces.FedRoomChanged(ctx, spaceID, threadID, false)
	return thread, nil
}

// ---- settings ---------------------------------------------------------------------------

// Update edits a thread. Name, archive state and auto-archive: Manage Threads or the creator;
// unarchiving an unlocked thread: any member as well; lock and slowmode: Manage Threads only.
func (s *Service) Update(ctx context.Context, actorID, threadID int64, in UpdateInput) (*Thread, error) {
	room, err := s.loadThread(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if err := s.assertLocal(ctx, *room.SpaceID); err != nil {
		return nil, err
	}
	p, err := s.viewerPerms(ctx, actorID, room)
	if err != nil {
		return nil, err
	}
	mgr := permissions.Has(p, permissions.PermManageThreads)
	creator := room.CreatorID == actorID
	var patch rooms.ThreadPatch
	changes := map[string]interface{}{}
	if in.Name != nil {
		if !mgr && !creator {
			return nil, ErrMissingPerm
		}
		name, err := validName(*in.Name)
		if err != nil {
			return nil, err
		}
		if name != room.Name {
			patch.Name = &name
			changes["name"] = name
		}
	}
	if in.AutoArchiveMinutes != nil {
		if !mgr && !creator {
			return nil, ErrMissingPerm
		}
		m, err := validAutoArchive(*in.AutoArchiveMinutes)
		if err != nil {
			return nil, err
		}
		patch.AutoArchiveMinutes = &m
	}
	if in.Invitable != nil {
		if !mgr && !creator {
			return nil, ErrMissingPerm
		}
		patch.Invitable = in.Invitable
	}
	if in.Locked != nil {
		if !mgr {
			return nil, ErrMissingPerm
		}
		patch.Locked = in.Locked
	}
	if in.SlowmodeSeconds != nil {
		if !mgr {
			return nil, ErrMissingPerm
		}
		if *in.SlowmodeSeconds < 0 || *in.SlowmodeSeconds > 21600 {
			return nil, ErrInvalidSlowmode
		}
		patch.SlowmodeSeconds = in.SlowmodeSeconds
	}
	if in.Archived != nil && *in.Archived != room.ThreadIsArchived() {
		if *in.Archived {
			if !mgr && !creator {
				return nil, ErrMissingPerm
			}
		} else {
			locked := room.ThreadIsLocked() || (in.Locked != nil && *in.Locked)
			if locked {
				if !mgr && !creator {
					return nil, ErrLocked
				}
			} else if !mgr && !creator {
				if member, _ := s.rooms.IsThreadMember(ctx, threadID, actorID); !member {
					return nil, ErrNotMember
				}
			}
		}
		patch.Archived = in.Archived
	}
	// An archived thread only takes an unarchive (Discord's rule); a lock may ride along so
	// a moderator can seal one without reopening it first. Everything else waits.
	if room.ThreadIsArchived() && (patch.Archived == nil || *patch.Archived) &&
		(patch.Name != nil || patch.AutoArchiveMinutes != nil || patch.Invitable != nil || patch.SlowmodeSeconds != nil) {
		return nil, ErrArchived
	}
	if patch == (rooms.ThreadPatch{}) {
		return s.thread(ctx, actorID, room)
	}
	// Any of these is "activity": the auto-archive timer starts over from now.
	now := time.Now().UTC()
	if patch.Archived != nil && !*patch.Archived || patch.AutoArchiveMinutes != nil {
		patch.LastActiveAt = &now
	}
	if err := s.rooms.UpdateThread(ctx, threadID, patch); err != nil {
		return nil, err
	}
	updated, err := s.loadThread(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if !updated.ThreadIsArchived() {
		_ = s.rooms.EnqueueThreadArchive(ctx, threadID, lastActive(updated).Add(time.Duration(updated.ThreadAutoArchiveMin)*time.Minute))
	}
	if patch.Name != nil {
		s.postNotice(ctx, updated, SystemThreadRenamed, map[string]interface{}{"actor_id": id.Format(actorID), "name": *patch.Name})
	}
	thread, err := s.thread(ctx, actorID, updated)
	if err != nil {
		return nil, err
	}
	s.publish(ctx, updated, "THREAD_UPDATE", s.threadJSON(thread))
	s.spaces.FedRoomChanged(ctx, *updated.SpaceID, threadID, false)
	return thread, nil
}

// Delete removes a thread and its membership. Manage Threads only, as on Discord.
func (s *Service) Delete(ctx context.Context, actorID, threadID int64) error {
	room, err := s.loadThread(ctx, threadID)
	if err != nil {
		return err
	}
	if err := s.assertLocal(ctx, *room.SpaceID); err != nil {
		return err
	}
	p, err := s.viewerPerms(ctx, actorID, room)
	if err != nil {
		return err
	}
	if !permissions.Has(p, permissions.PermManageThreads) {
		return ErrMissingPerm
	}
	// Who gets the event is decided before the member rows go.
	recipients := s.recipients(ctx, room)
	if err := s.rooms.DeleteThread(ctx, *room.SpaceID, threadID); err != nil {
		return err
	}
	if room.ThreadStarterMessageID != nil && room.ParentID != nil {
		_ = s.msgs.SetThreadID(ctx, *room.ParentID, *room.ThreadStarterMessageID, nil)
	}
	s.spaces.InvalidateSnapshot(ctx, *room.SpaceID)
	s.publishTo(ctx, room, recipients, "THREAD_DELETE", map[string]interface{}{
		"room_id": id.Format(threadID), "space_id": id.Format(*room.SpaceID), "parent_id": id.Format(*room.ParentID),
	})
	s.spaces.FedRoomChanged(ctx, *room.SpaceID, threadID, true)
	return nil
}

// ---- membership -------------------------------------------------------------------------

// Join adds the caller to a thread they can see. Archived threads cannot be joined.
func (s *Service) Join(ctx context.Context, userID, threadID int64) error {
	room, err := s.loadThread(ctx, threadID)
	if err != nil {
		return err
	}
	if _, err := s.viewerPerms(ctx, userID, room); err != nil {
		return err
	}
	if room.ThreadIsArchived() {
		return ErrArchived
	}
	return s.addMembers(ctx, room, []int64{userID})
}

func (s *Service) Leave(ctx context.Context, userID, threadID int64) error {
	room, err := s.loadThread(ctx, threadID)
	if err != nil {
		return err
	}
	return s.removeMember(ctx, room, userID)
}

// AddMember adds someone else: by a member of the thread (any member of a public or an
// invitable private thread), or by a Manage Threads holder. The target must be able to view
// the channel the thread is in.
func (s *Service) AddMember(ctx context.Context, actorID, threadID, targetID int64) error {
	room, err := s.loadThread(ctx, threadID)
	if err != nil {
		return err
	}
	p, err := s.viewerPerms(ctx, actorID, room)
	if err != nil {
		return err
	}
	mgr := permissions.Has(p, permissions.PermManageThreads)
	if !mgr {
		if member, _ := s.rooms.IsThreadMember(ctx, threadID, actorID); !member {
			return ErrNotMember
		}
		if room.ThreadIsPrivate() && !room.ThreadIsInvitable() {
			return ErrNotInvitable
		}
	}
	if room.ThreadIsArchived() {
		return ErrArchived
	}
	parent, err := s.rooms.GetByID(ctx, *room.ParentID)
	if err != nil || parent == nil {
		return ErrNotFound
	}
	tp, err := s.spaces.EffectiveChannelPermissions(ctx, targetID, *room.SpaceID, parent.ID)
	if err != nil || !permissions.Has(tp, permissions.PermViewRoom) {
		return ErrCannotViewParent
	}
	return s.addMembers(ctx, room, []int64{targetID})
}

// RemoveMember removes someone else: Manage Threads, or the thread's creator.
func (s *Service) RemoveMember(ctx context.Context, actorID, threadID, targetID int64) error {
	room, err := s.loadThread(ctx, threadID)
	if err != nil {
		return err
	}
	p, err := s.viewerPerms(ctx, actorID, room)
	if err != nil {
		return err
	}
	if !permissions.Has(p, permissions.PermManageThreads) && room.CreatorID != actorID {
		return ErrMissingPerm
	}
	return s.removeMember(ctx, room, targetID)
}

func (s *Service) addMembers(ctx context.Context, room *rooms.Room, userIDs []int64) error {
	now := time.Now().UTC()
	added := make([]map[string]interface{}, 0, len(userIDs))
	for _, uid := range userIDs {
		if member, _ := s.rooms.IsThreadMember(ctx, room.ID, uid); member {
			continue
		}
		if err := s.rooms.AddThreadMember(ctx, room.ID, uid, now); err != nil {
			return err
		}
		added = append(added, map[string]interface{}{"user_id": id.Format(uid), "joined_at": now})
		if room.ThreadIsPrivate() {
			// Being added is how someone first learns of a private thread - Discord sends
			// THREAD_CREATE to the person added, so the client has the room to show.
			if t, err := s.thread(ctx, uid, room); err == nil {
				s.publishToUser(ctx, uid, room, "THREAD_CREATE", s.threadJSON(t))
			}
		}
		s.publishToUser(ctx, uid, room, "THREAD_MEMBER_UPDATE", map[string]interface{}{"joined": true})
	}
	if len(added) == 0 {
		return nil
	}
	n, _ := s.rooms.CountThreadMembers(ctx, room.ID)
	s.publish(ctx, room, "THREAD_MEMBERS_UPDATE", map[string]interface{}{
		"added_members": added, "removed_member_ids": []string{}, "member_count": n,
	})
	return nil
}

func (s *Service) removeMember(ctx context.Context, room *rooms.Room, userID int64) error {
	member, err := s.rooms.IsThreadMember(ctx, room.ID, userID)
	if err != nil {
		return err
	}
	if !member {
		return nil
	}
	// A private thread's events go to its members: tell the leaver while they still count.
	recipients := s.recipients(ctx, room)
	if err := s.rooms.RemoveThreadMember(ctx, room.ID, userID); err != nil {
		return err
	}
	n, _ := s.rooms.CountThreadMembers(ctx, room.ID)
	s.publishTo(ctx, room, recipients, "THREAD_MEMBERS_UPDATE", map[string]interface{}{
		"added_members": []interface{}{}, "removed_member_ids": []string{id.Format(userID)}, "member_count": n,
	})
	s.publishToUser(ctx, userID, room, "THREAD_MEMBER_UPDATE", map[string]interface{}{"joined": false})
	return nil
}

// ListMembers is a thread's members with their profiles, for anyone who can see the thread.
func (s *Service) ListMembers(ctx context.Context, userID, threadID int64) ([]Member, error) {
	room, err := s.loadThread(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if _, err := s.viewerPerms(ctx, userID, room); err != nil {
		return nil, err
	}
	rows, err := s.rooms.ListThreadMembers(ctx, threadID)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.UserID)
	}
	users, _ := s.users.GetByIDs(ctx, ids)
	out := make([]Member, 0, len(rows))
	for i, r := range rows {
		m := Member{UserID: r.UserID, JoinedAt: r.JoinedAt}
		if i < len(users) {
			m.User = users[i]
		}
		out = append(out, m)
	}
	return out, nil
}

// ---- listing ----------------------------------------------------------------------------

// ListActive is every active thread in the space the viewer can see (Discord's
// /threads/active), newest activity first.
func (s *Service) ListActive(ctx context.Context, userID, spaceID int64) ([]Thread, error) {
	if ok, err := s.spaces.IsMember(ctx, spaceID, userID); err != nil || !ok {
		return nil, ErrNotFound
	}
	visible, _, err := s.spaces.VisibleSpaceRooms(ctx, userID, spaceID)
	if err != nil {
		return nil, err
	}
	var list []*rooms.Room
	for _, r := range visible {
		if r.Type == rooms.TypeThread && !r.ThreadIsArchived() {
			list = append(list, r)
		}
	}
	return s.threads(ctx, userID, list)
}

// ListArchived is a channel's archived threads the viewer can see, most recently archived
// first; `before` pages by archive time.
func (s *Service) ListArchived(ctx context.Context, userID, channelID int64, before *time.Time, limit int) ([]Thread, bool, error) {
	parent, err := s.loadTextChannel(ctx, channelID)
	if err != nil {
		return nil, false, err
	}
	p, err := s.viewerPerms(ctx, userID, parent)
	if err != nil {
		return nil, false, err
	}
	if !permissions.Has(p, permissions.PermReadMessageHistory) {
		return nil, false, ErrMissingPerm
	}
	mgr := permissions.Has(p, permissions.PermManageThreads)
	all, _, err := s.spaces.SpaceRoomsWithOverrides(ctx, *parent.SpaceID)
	if err != nil {
		return nil, false, err
	}
	joined := map[int64]struct{}{}
	if ids, err := s.rooms.ListUserThreadIDs(ctx, userID); err == nil {
		for _, tid := range ids {
			joined[tid] = struct{}{}
		}
	}
	var list []*rooms.Room
	for _, r := range all {
		if r.Type != rooms.TypeThread || !r.ThreadIsArchived() || r.ParentID == nil || *r.ParentID != channelID {
			continue
		}
		if r.ThreadIsPrivate() && !mgr {
			if _, ok := joined[r.ID]; !ok {
				continue
			}
		}
		if before != nil && r.ThreadArchivedAt != nil && !r.ThreadArchivedAt.Before(*before) {
			continue
		}
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return archivedAt(list[i]).After(archivedAt(list[j])) })
	if limit <= 0 || limit > 50 {
		limit = 25
	}
	hasMore := len(list) > limit
	if hasMore {
		list = list[:limit]
	}
	out, err := s.threads(ctx, userID, list)
	return out, hasMore, err
}

func archivedAt(r *rooms.Room) time.Time {
	if r.ThreadArchivedAt != nil {
		return *r.ThreadArchivedAt
	}
	return r.UpdatedAt
}

func lastActive(r *rooms.Room) time.Time {
	if r.ThreadLastActiveAt != nil {
		return *r.ThreadLastActiveAt
	}
	return r.CreatedAt
}

// Get is one thread as the viewer sees it.
func (s *Service) Get(ctx context.Context, userID, threadID int64) (*Thread, error) {
	room, err := s.loadThread(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if _, err := s.viewerPerms(ctx, userID, room); err != nil {
		return nil, err
	}
	return s.thread(ctx, userID, room)
}

func (s *Service) thread(ctx context.Context, userID int64, room *rooms.Room) (*Thread, error) {
	list, err := s.threads(ctx, userID, []*rooms.Room{room})
	if err != nil {
		return nil, err
	}
	return &list[0], nil
}

func (s *Service) threads(ctx context.Context, userID int64, list []*rooms.Room) ([]Thread, error) {
	ids := make([]int64, 0, len(list))
	for _, r := range list {
		ids = append(ids, r.ID)
	}
	states, err := s.spaces.ThreadStates(ctx, userID, ids)
	if err != nil {
		return nil, err
	}
	out := make([]Thread, 0, len(list))
	for _, r := range list {
		out = append(out, Thread{Room: r, State: states[r.ID]})
	}
	return out, nil
}

// ---- hooks from the messages service ---------------------------------------------------

// OnThreadMessage runs after a message is stored in a thread: the thread is active again
// (unarchived if it was, its auto-archive timer restarted), its count goes up, and the
// sender plus anyone they mentioned who can see the thread become members - Discord's
// auto-join rules.
func (s *Service) OnThreadMessage(ctx context.Context, room *rooms.Room, senderID int64, mentioned []int64) {
	if room == nil || room.Type != rooms.TypeThread || room.SpaceID == nil {
		return
	}
	now := time.Now().UTC()
	patch := rooms.ThreadPatch{LastActiveAt: &now}
	unarchived := false
	if room.ThreadIsArchived() && !room.ThreadIsLocked() {
		f := false
		patch.Archived = &f
		unarchived = true
	}
	if err := s.rooms.UpdateThread(ctx, room.ID, patch); err != nil {
		logger.Err("threads", err, map[string]any{"thread_id": room.ID, "step": "touch"})
	}
	auto := room.ThreadAutoArchiveMin
	if auto <= 0 {
		auto = DefaultAutoArchiveMin
	}
	_ = s.rooms.EnqueueThreadArchive(ctx, room.ID, now.Add(time.Duration(auto)*time.Minute))
	_ = s.rooms.AddThreadMessages(ctx, room.ID, 1)
	join := []int64{senderID}
	for i, uid := range mentioned {
		if i >= MaxAutoJoinMentions {
			break
		}
		if uid == senderID {
			continue
		}
		if p, err := s.spaces.EffectiveChannelPermissions(ctx, uid, *room.SpaceID, room.ID); err == nil && permissions.Has(p, permissions.PermViewRoom) {
			join = append(join, uid)
		}
	}
	if err := s.addMembers(ctx, room, join); err != nil {
		logger.Err("threads", err, map[string]any{"thread_id": room.ID, "step": "auto-join"})
	}
	if unarchived {
		if updated, err := s.loadThread(ctx, room.ID); err == nil {
			if th, err := s.thread(ctx, senderID, updated); err == nil {
				s.publish(ctx, updated, "THREAD_UPDATE", s.threadJSON(th))
			}
		}
	}
}

// OnThreadMessageDeleted keeps message_count honest.
func (s *Service) OnThreadMessageDeleted(ctx context.Context, room *rooms.Room) {
	if room == nil || room.Type != rooms.TypeThread {
		return
	}
	_ = s.rooms.AddThreadMessages(ctx, room.ID, -1)
}

// ---- events -----------------------------------------------------------------------------

// threadJSON is the wire form of a thread: the room map (RoomMap, with its `thread` block)
// plus the counts, and room_id for clients that key events by it.
func (s *Service) threadJSON(t *Thread) map[string]interface{} {
	m := spaces.RoomMap(t.Room)
	spaces.AttachThreadState(m, t.State)
	m["room_id"] = id.Format(t.Room.ID)
	return m
}

// ThreadJSON is threadJSON for handlers.
func (s *Service) ThreadJSON(t *Thread) map[string]interface{} { return s.threadJSON(t) }

// recipients is who learns about a thread: its members for a private thread, everyone who
// can view the parent channel otherwise (nil, true = the whole space).
func (s *Service) recipients(ctx context.Context, room *rooms.Room) recipientSet {
	if room.ThreadIsPrivate() {
		ids, _ := s.rooms.ListThreadMemberIDs(ctx, room.ID)
		return recipientSet{users: ids}
	}
	parentID := room.ID
	if room.ParentID != nil {
		parentID = *room.ParentID
	}
	viewers, everyone := s.spaces.ViewersOf(ctx, *room.SpaceID, parentID)
	return recipientSet{users: viewers, everyone: everyone}
}

type recipientSet struct {
	users    []int64
	everyone bool
}

func (s *Service) publish(ctx context.Context, room *rooms.Room, eventType string, payload map[string]interface{}) {
	s.publishTo(ctx, room, s.recipients(ctx, room), eventType, payload)
}

func (s *Service) publishTo(ctx context.Context, room *rooms.Room, to recipientSet, eventType string, payload map[string]interface{}) {
	if s.redis == nil || room.SpaceID == nil {
		return
	}
	payload["space_id"] = id.Format(*room.SpaceID)
	if _, ok := payload["room_id"]; !ok {
		payload["room_id"] = id.Format(room.ID)
	}
	if room.ParentID != nil {
		payload["parent_id"] = id.Format(*room.ParentID)
	}
	if to.everyone {
		stargate.PublishToSpace(ctx, s.redis, *room.SpaceID, eventType, payload, s.spaces.Region())
		return
	}
	if len(to.users) > 0 {
		stargate.PublishToUsers(ctx, s.redis, to.users, eventType, payload, s.spaces.Region())
	}
}

func (s *Service) publishToUser(ctx context.Context, userID int64, room *rooms.Room, eventType string, payload map[string]interface{}) {
	if s.redis == nil || room.SpaceID == nil {
		return
	}
	payload["space_id"] = id.Format(*room.SpaceID)
	payload["room_id"] = id.Format(room.ID)
	stargate.PublishToUser(ctx, s.redis, userID, eventType, payload, s.spaces.Region())
}

// postNotice stores a system message in room (the parent channel for thread_created, the
// thread for thread_renamed) and fans it out like any other.
func (s *Service) postNotice(ctx context.Context, room *rooms.Room, eventType string, payload map[string]interface{}) {
	if s.notify == nil || room.SpaceID == nil {
		return
	}
	var recipients []int64
	if room.Type == rooms.TypeThread {
		recipients, _ = s.rooms.ListThreadMemberIDs(ctx, room.ID)
	} else {
		recipients, _ = s.spaces.ListSpaceMemberUserIDs(ctx, *room.SpaceID)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if _, err := s.notify(ctx, room.ID, recipients, eventType, string(raw)); err != nil {
		logger.Err("threads", err, map[string]any{"room_id": room.ID, "system_type": eventType})
	}
}

func boolPtr(v bool) *bool { return &v }
