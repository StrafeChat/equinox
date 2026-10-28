package stargate

import (
	"context"

	"github.com/redis/go-redis/v9"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

const presenceUpdateEvent = "PRESENCE_UPDATE"

// OnlinePresenceKey is a Redis set of the user ids with at least one live gateway
// connection. Maintained here (SADD on connect, SREM on last disconnect) so a separate
// process - the API's admin stats - can read the count with SCARD without a database scan.
// Cleared on gateway startup (a fresh gateway has no clients), which self-heals a crash on
// a single-instance deployment; a multi-instance deployment would want per-node sets.
const OnlinePresenceKey = "presence:online"

// PresenceFederator relays a presence change to the home instances of the user's remote
// friends. Implemented by internal/federation; nil when this instance doesn't federate.
type PresenceFederator interface {
	AfterPresenceChanged(ctx context.Context, u *auth.User)
}

// DefaultPresenceNotifier updates user presence in the DB and publishes to friends on connect/disconnect.
type DefaultPresenceNotifier struct {
	userRepo  auth.UserRepository
	redis     *redis.Client
	region    string
	federator PresenceFederator
}

func NewDefaultPresenceNotifier(userRepo auth.UserRepository, redis *redis.Client, region string) *DefaultPresenceNotifier {
	if region == "" {
		region = "default"
	}
	return &DefaultPresenceNotifier{userRepo: userRepo, redis: redis, region: region}
}

// SetFederator wires outbound federation for connect/disconnect presence changes.
func (p *DefaultPresenceNotifier) SetFederator(f PresenceFederator) {
	p.federator = f
}

func (p *DefaultPresenceNotifier) OnConnect(ctx context.Context, userID int64) {
	p.setOnline(ctx, userID, true)
}

func (p *DefaultPresenceNotifier) OnDisconnect(ctx context.Context, userID int64) {
	p.setOnline(ctx, userID, false)
}

func (p *DefaultPresenceNotifier) setOnline(ctx context.Context, userID int64, online bool) {
	upd := &auth.ProfileUpdate{
		Presence: &auth.PresenceUpdate{Online: &online},
	}
	updated, err := p.userRepo.UpdateProfile(ctx, userID, upd)
	if err != nil {
		logger.Err("stargate", err, map[string]any{"user_id": userID, "online": online})
		return
	}
	// Track membership of the online set for the admin dashboard's live count. SADD is
	// idempotent (a second device re-adds harmlessly); SREM fires only on last disconnect,
	// because the hub calls OnDisconnect only when no other client for the user remains.
	if p.redis != nil {
		if online {
			p.redis.SAdd(ctx, OnlinePresenceKey, id.Format(userID))
		} else {
			p.redis.SRem(ctx, OnlinePresenceKey, id.Format(userID))
		}
	}
	if updated == nil {
		return
	}
	PublishPresenceUpdate(ctx, p.redis, p.region, updated)
	if p.federator != nil {
		p.federator.AfterPresenceChanged(ctx, updated)
	}
}

// ActivityChanged publishes a user's changed activity (op 9, a closed connection's cleanup)
// under the invisible rule (PublishActivityChange), and relays it to other instances only
// when the people here were told too.
func (p *DefaultPresenceNotifier) ActivityChanged(ctx context.Context, userID int64) {
	if p.userRepo == nil {
		return
	}
	u, err := p.userRepo.GetByID(ctx, userID)
	if err != nil {
		logger.Err("stargate", err, map[string]any{"user_id": userID})
		return
	}
	if u == nil {
		return
	}
	if PublishActivityChange(ctx, p.redis, p.region, u) && p.federator != nil {
		p.federator.AfterPresenceChanged(ctx, u)
	}
}

// RelayActivity sends the user's presence, with what they are playing, to other instances
// again - the hourly keepalive's every activityRelayTicks-th tick - so the copy a peer keeps
// (12 h) outlives a long game. Nobody here is told: nothing changed for them. A user who
// reads offline to others is not relayed, as an activity change would not be.
func (p *DefaultPresenceNotifier) RelayActivity(ctx context.Context, userID int64) {
	if p.userRepo == nil || p.federator == nil {
		return
	}
	u, err := p.userRepo.GetByID(ctx, userID)
	if err != nil {
		logger.Err("stargate", err, map[string]any{"user_id": userID})
		return
	}
	if visibleToOthers(u) {
		p.federator.AfterPresenceChanged(ctx, u)
	}
}

// PublishActivityChange publishes a change of the user's activity alone - their status is
// what it was - and reports whether anyone but the user was told, which is when the change
// may go on to other instances as well.
//
// While others see the user as offline (invisible, or not connected) only the user's own
// devices hear about it. A user who really is offline produces no frame at all, so a
// PRESENCE_UPDATE reaching friends, spaces or another instance for an activity change -
// even one saying "offline" - would give an invisible user away, and tell when they switch
// games. A user who is not connected anywhere is told nothing either: the connect or
// disconnect publishes their presence, activity included, on its own.
func PublishActivityChange(ctx context.Context, rdb *redis.Client, region string, u *auth.User) bool {
	if u == nil {
		return false
	}
	if !visibleToOthers(u) {
		if rdb != nil && auth.ToPublicPresence(u.Presence, false).Status != "offline" {
			self, _ := presencePayloads(ctx, u)
			PublishToUser(ctx, rdb, u.ID, presenceUpdateEvent, self, defaultRegion(region))
		}
		return false
	}
	PublishPresenceUpdate(ctx, rdb, region, u)
	return true
}

// SelfPresence is the user's own view of their presence (READY, GET /users/@me): their real status,
// invisible included, with what their devices report they are doing.
func SelfPresence(ctx context.Context, user *auth.User) auth.PublicPresence {
	p := auth.ToPublicPresence(user.Presence, false)
	if p.Status != "offline" {
		p = auth.WithActivities(p, ActivitiesOf(ctx, user.ID))
	}
	return p
}

// connectedSelfPresence is SelfPresence for the READY of a socket that is open: the user
// was read before it registered, and OnConnect writes "online" in the background, so the
// record may still say offline - which READY must never tell a live socket.
func connectedSelfPresence(ctx context.Context, user *auth.User) auth.PublicPresence {
	u := *user
	u.Presence.Online = true
	return SelfPresence(ctx, &u)
}

// presencePayloads builds PRESENCE_UPDATE's data for the user's own devices (real status)
// and for everyone else (invisible reads as offline), each with the user's current
// activity unless that view is offline.
func presencePayloads(ctx context.Context, user *auth.User) (self, others map[string]interface{}) {
	presenceSelf := auth.ToPublicPresence(user.Presence, false)
	presenceOthers := auth.ToPublicPresence(user.Presence, true)
	if presenceSelf.Status != "offline" {
		acts := ActivitiesOf(ctx, user.ID)
		presenceSelf = auth.WithActivities(presenceSelf, acts)
		presenceOthers = auth.WithActivities(presenceOthers, acts)
	}
	self = map[string]interface{}{"user_id": id.Format(user.ID), "presence": presenceSelf}
	others = map[string]interface{}{"user_id": id.Format(user.ID), "presence": presenceOthers}
	return self, others
}

// PublishPresenceUpdate broadcasts a user's presence to everyone who should see it: the
// user themselves (their real status, so multiple devices stay in sync), their friends, and
// every space they belong to (reaching all co-members subscribed to that space). Friends and
// spaces get the "others" view, where invisible reads as offline. Broadcasting to spaces -
// not just friends - is what keeps a member's status correct for people who share a space
// with them but are not friends; without it those statuses only ever reflected load time.
// Both views carry the user's current activity (the activity store, which both binaries
// share), the others view only while it is not offline.
func PublishPresenceUpdate(ctx context.Context, rdb *redis.Client, region string, user *auth.User) {
	if rdb == nil || user == nil {
		return
	}
	region = defaultRegion(region)
	payloadSelf, payloadOthers := presencePayloads(ctx, user)

	// The user's own devices (real status).
	PublishToUser(ctx, rdb, user.ID, presenceUpdateEvent, payloadSelf, region)
	// Friends (DM list / friends list).
	if len(user.Relationships) > 0 {
		PublishToUsers(ctx, rdb, user.Relationships, presenceUpdateEvent, payloadOthers, region)
	}
	// Every space the user is a member of - one publish per space, received by every
	// co-member subscribed to it. user.Spaces is maintained on join/leave.
	for _, spaceID := range user.Spaces {
		PublishToSpace(ctx, rdb, spaceID, presenceUpdateEvent, payloadOthers, region)
	}
}
