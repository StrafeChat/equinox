package federation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/users"
	"github.com/StrafeChat/equinox/internal/modules/voice"
	"github.com/StrafeChat/equinox/internal/stargate"
)

// Account deletion across instances.
//
// The home instance (internal/modules/instance/account_delete.go) deletes a local account
// outright and, last before its identity goes, tells every peer that may hold a shadow of it
// with POST /users/delete. Identity is by FID everywhere, never by handle, so a new account
// that takes the freed name has a new id and gets a new, separate shadow on every peer.
//
// A peer does not delete the shadow: messages, reactions and pins it holds name the shadow's
// id, so it becomes a tombstone - row and users_by_remote mapping kept, every profile field
// blank, deleted_at set - after what its users had with the account (friendships, requests,
// memberships of spaces hosted here, group PMs, a call) is torn down. A tombstone is never
// resurrected or renamed (EnsureShadow). A third instance's word that an account is deleted
// is only a hint: the account's home is asked, and only its 404 makes a tombstone.

// DeletionAudience is every peer that may hold a shadow of a local account about to be
// deleted: the audience of a profile update (the homes of everyone it shares a PM or group
// with, the homes of its remote friends, every instance sharing a space with it) plus the
// homes of the remote accounts it has a pending friend request with either way. Computed
// before anything is torn down - afterwards nothing would say where the account was known.
func (s *Service) DeletionAudience(ctx context.Context, u *auth.User, requestCounterparts []int64) map[string]struct{} {
	domains := s.profileAudience(ctx, u)
	if byDomain, _, err := s.remotePeers(ctx, requestCounterparts); err == nil {
		for d := range byDomain {
			domains[d] = struct{}{}
		}
	}
	delete(domains, s.fcfg.Domain)
	return domains
}

// AfterAccountDeleted tells each peer in domains that the local account u was deleted, one
// POST /users/delete each through the durable outbox. The per-peer outbox delivers in order,
// so the notice arrives after the relays the deletion already queued (leaves, unfriends).
func (s *Service) AfterAccountDeleted(ctx context.Context, u *auth.User, domains map[string]struct{}) {
	if u == nil || u.IsRemote() || len(domains) == 0 {
		return
	}
	s.fanOut(ctx, domains, http.MethodPost, "/users/delete", UserDeletedNotice{User: s.LocalFID(u.ID)})
}

// DropMirrorIfEmpty drops a mirrored space nobody here is a member of any more (the spaces
// module's account deletion, after removing a member locally when the origin could not be
// reached).
func (s *Service) DropMirrorIfEmpty(ctx context.Context, spaceID int64) {
	m, err := s.repo.GetSpaceMapping(ctx, spaceID)
	if err != nil || m == nil || s.IsLocalServer(m.OriginDomain) {
		return
	}
	s.dropMirrorIfEmpty(ctx, m)
}

// UserDeleted POST /users/delete - a peer's account was deleted: forget it here.
func (h *Handler) UserDeleted(c fiber.Ctx) error {
	var body UserDeletedNotice
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	requester := RequesterDomain(c)
	originID, domain, err := ParseFID(body.User)
	if err != nil {
		return fail(c, ErrInvalidFID, nil)
	}
	// A peer speaks for its own users only.
	if domain != requester || h.svc.IsLocalServer(domain) {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "an instance may only report its own accounts deleted"})
	}
	ctx := context.WithoutCancel(c.Context())
	shadow, err := h.svc.users.GetByRemote(ctx, domain, originID)
	if err != nil {
		return fail(c, err, map[string]any{"peer": requester})
	}
	if shadow == nil || shadow.IsDeleted() {
		// Nothing known here, or already forgotten: idempotent.
		return c.SendStatus(http.StatusNoContent)
	}
	if err := h.svc.ApplyRemoteUserDeleted(ctx, shadow, requester); err != nil {
		return fail(c, err, map[string]any{"peer": requester, "user_id": shadow.ID})
	}
	return c.SendStatus(http.StatusNoContent)
}

// ApplyRemoteUserDeleted forgets a remote account its home deleted: local clients are told
// (USER_UPDATE deleted), friendships and requests with local users end, the shadow leaves
// the spaces hosted here and its group PMs (the others' clients rotate their group keys),
// its call ends, its relayed activity goes, and the shadow becomes a tombstone. Nothing
// relays back to the home (requester). Each step is best-effort and logged; the tombstone
// is what makes the next notice a no-op, so it is written last.
func (s *Service) ApplyRemoteUserDeleted(ctx context.Context, shadow *auth.User, requester string) error {
	if shadow == nil || !shadow.IsRemote() {
		return errors.New("federation: not a shadow")
	}
	if shadow.IsDeleted() {
		return nil
	}
	ctx = WithExcludedPeer(ctx, requester)
	note := func(stage string, err error) {
		if err != nil {
			logger.Err("federation", err, map[string]any{"stage": stage, "peer": requester, "user_id": shadow.ID})
		}
	}

	// a. Local clients: the placeholder replaces the profile at once.
	users.BroadcastDeleted(ctx, s.redis, s.roomRepo, s.region(), shadow)

	// b. Friendships and requests with local users (RELATIONSHIP_REMOVE on our side; the
	// shadow has no sessions here, and nothing is relayed for a remote actor).
	if s.relSvc != nil {
		_, err := s.relSvc.PurgeUser(ctx, shadow)
		note("relationships", err)
	}

	// c. Spaces hosted here. A mirrored space's origin relays its own removal.
	if s.spaceSvc != nil {
		spaceIDs, err := s.spaceSvc.AccountSpaceIDs(ctx, shadow.ID, shadow.Spaces)
		note("spaces_list", err)
		for _, sid := range spaceIDs {
			if m, err := s.repo.GetSpaceMapping(ctx, sid); err == nil && m != nil && !s.IsLocalServer(m.OriginDomain) {
				continue
			}
			if _, err := s.spaceSvc.RemoveAccount(ctx, shadow.ID, sid); err != nil {
				// An owner here with a current home cannot happen (it refuses the deletion).
				note("space_remove", err)
			}
		}
	}

	// d. Group PMs. A 1:1 PM keeps the shadow as its other participant (a tombstone).
	s.leaveGroupsAsDeleted(ctx, shadow, note)

	// e. A call it is still in.
	if s.voice != nil {
		if err := s.voice.Leave(ctx, shadow.ID); err != nil && !errors.Is(err, voice.ErrNotInVoice) {
			note("voice", err)
		}
	}

	// f. Its relayed activity and presence entries.
	note("presence", stargate.ForgetUser(ctx, s.redis, s.cfg.Database.Redis.CachePrefix, shadow.ID))

	// g. The tombstone.
	if err := s.users.TombstoneShadow(ctx, shadow.ID); err != nil {
		return err
	}
	logger.Info("federation", "account %s deleted by its home; shadow %d tombstoned", s.FIDOf(shadow), shadow.ID)
	return nil
}

// leaveGroupsAsDeleted removes a deleted remote account from every group PM it is in here:
// ApplyParticipants publishes ROOM_UPDATE/ROOM_LEAVE and SESSION_ROTATE to the local
// participants, and for a group hosted here the change is relayed to the other instances in
// it (the origin is the one entitled to).
func (s *Service) leaveGroupsAsDeleted(ctx context.Context, shadow *auth.User, note func(string, error)) {
	rows, err := s.roomRepo.ListByUser(ctx, shadow.ID)
	if err != nil {
		note("rooms_list", err)
		return
	}
	for _, row := range rows {
		room, err := s.roomRepo.GetByID(ctx, row.RoomID)
		if err != nil || room == nil || room.Type != rooms.TypeGroupPM {
			continue
		}
		current, err := s.roomRepo.GetParticipants(ctx, room.ID)
		if err != nil {
			note("room_participants", err)
			continue
		}
		target := make([]int64, 0, len(current))
		for _, pid := range current {
			if pid != shadow.ID {
				target = append(target, pid)
			}
		}
		if len(target) == len(current) {
			continue
		}
		if len(target) == 0 {
			// Nobody left: just drop the shadow's participation.
			note("room_leave", s.roomRepo.RemoveParticipant(ctx, room.ID, shadow.ID))
			continue
		}
		rwp, err := s.roomSvc.ApplyParticipants(ctx, room.ID, target)
		if err != nil {
			note("room_apply", err)
			continue
		}
		if m, err := s.repo.GetRoomMapping(ctx, room.ID); err == nil && (m == nil || s.IsLocalServer(m.OriginDomain)) {
			s.AfterParticipantsChanged(ctx, rwp, []int64{shadow.ID})
		}
	}
}

// resolveSender is resolveProfile for a message's sender, where a deleted account must not
// lose its messages (every message stays, as on the home instance):
//
//   - our own FID: the row is gone here, so the message keeps the id and renders as deleted;
//   - the vouching instance's own user: EnsureShadow tombstones it (or makes a tombstone);
//   - a third instance's user: a shadow we have is used as it is; otherwise its home is
//     asked - a profile means the hint was wrong (never trust a third party's "deleted"), a
//     404 means it is gone and a tombstone is made.
func (s *Service) resolveSender(ctx context.Context, p Profile, from string) (*auth.User, error) {
	if !p.Deleted {
		return s.resolveProfile(ctx, p, from)
	}
	originID, domain, err := ParseFID(p.FID)
	if err != nil {
		return nil, err
	}
	if s.IsLocalServer(domain) {
		u, err := s.users.GetByID(ctx, originID)
		if err != nil {
			return nil, err
		}
		if u == nil {
			now := time.Now().UTC()
			return &auth.User{ID: originID, DeletedAt: &now}, nil
		}
		if u.IsRemote() {
			return nil, ErrRemoteUserNotFound
		}
		return u, nil
	}
	if domain == from {
		return s.EnsureShadow(ctx, domain, p)
	}
	if existing, err := s.users.GetByRemote(ctx, domain, originID); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}
	if !s.fcfg.IsAllowedPeer(domain) {
		return nil, ErrPeerNotAllowed
	}
	var fetched Profile
	if _, err := s.client.Do(ctx, domain, http.MethodGet, "/users/"+id.Format(originID), nil, &fetched); err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.Status == http.StatusNotFound {
			return s.tombstoneFor(ctx, domain, originID, nil)
		}
		return nil, err
	}
	return s.EnsureShadow(ctx, domain, fetched)
}

// tombstoneFor makes the shadow of (domain, originID) a tombstone: the full
// ApplyRemoteUserDeleted for one that exists, or a new row that is a tombstone from the
// start (new id, users_by_remote, deleted_at) - so its messages keep a sender and a later
// reference to the same FID never fetches a profile.
func (s *Service) tombstoneFor(ctx context.Context, domain string, originID int64, existing *auth.User) (*auth.User, error) {
	if existing != nil {
		if existing.IsDeleted() {
			return existing, nil
		}
		if err := s.ApplyRemoteUserDeleted(ctx, existing, domain); err != nil {
			return nil, err
		}
		if u, err := s.users.GetByRemote(ctx, domain, originID); err == nil && u != nil {
			return u, nil
		}
		return existing, nil
	}
	now := time.Now().UTC()
	rid := originID
	u := &auth.User{
		ID: id.Next(), HomeDomain: domain, RemoteID: &rid, CreatedAt: now, UpdatedAt: now,
		DeletedAt: &now, Presence: auth.UserPresence{Status: "offline"},
	}
	if err := s.users.UpsertShadow(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

// region is the STARGATE_REGION events published from here are tagged with.
func (s *Service) region() string {
	if s.cfg != nil && s.cfg.Stargate.Region != "" {
		return s.cfg.Stargate.Region
	}
	return "default"
}
