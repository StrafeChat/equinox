package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/StrafeChat/equinox/internal/modules/instance"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/db"
	"github.com/StrafeChat/equinox/internal/federation"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/applications"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/relationships"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
	"github.com/StrafeChat/equinox/internal/modules/voice"
	"github.com/StrafeChat/equinox/internal/stargate"
	"github.com/joho/godotenv"
)

// lazyPresenceFederator builds the federation client on first use and keeps retrying
// while the API has not written the shared signing key yet (a fresh install starts both
// processes at once), instead of leaving presence relay off until the next restart.
type lazyPresenceFederator struct {
	build func() (*federation.Service, error)
	mu    sync.Mutex
	fed   *federation.Service
	retry time.Time
}

func (l *lazyPresenceFederator) AfterPresenceChanged(ctx context.Context, u *auth.User) {
	l.mu.Lock()
	fed := l.fed
	if fed == nil && time.Now().After(l.retry) {
		var err error
		if fed, err = l.build(); err != nil {
			l.retry = time.Now().Add(30 * time.Second)
			logger.Warn("stargate", "presence federation not ready: %v", err)
		} else {
			l.fed = fed
		}
	}
	l.mu.Unlock()
	if fed != nil {
		fed.AfterPresenceChanged(ctx, u)
	}
}

// readyDataProvider fetches rooms, relationships, spaces, space rooms and the live voice
// states for the READY payload.
type readyDataProvider struct {
	roomSvc  *rooms.Service
	relSvc   *relationships.Service
	spaceSvc *spaces.Service
	// voice is nil when the instance has no LiveKit configured.
	voice *voice.Store
}

// roomChannelAuthorizer implements stargate.ChannelAuthorizer by delegating to the same
// participant/space-membership check the REST API uses (rooms.Service.CanAccess), so
// WebSocket subscribe and REST access can't drift out of sync.
//
// The "space" channel prefix is overloaded: clients subscribe to it with a room ID for
// room/PM events, AND with a space's own ID for space-wide broadcast events (role
// changes, member add/remove, space renames - see spaces.Service.publishSpaceEvent /
// stargate.PublishToSpaceChannel). Both go out on stargate:space:{id} using the raw id, so
// the authorizer has to accept whichever kind of id it's actually looking at.
type roomChannelAuthorizer struct {
	roomSvc  *rooms.Service
	spaceSvc *spaces.Service
}

func (a *roomChannelAuthorizer) CanAccessRoom(ctx context.Context, userID int64, roomID string) (bool, error) {
	ok, _, err := a.CanAccessChannel(ctx, userID, roomID)
	return ok, err
}

// CanAccessChannel implements stargate.ChannelScoper: the same check, which has read the
// room (or found the id is a space's) and so also says what the channel is.
func (a *roomChannelAuthorizer) CanAccessChannel(ctx context.Context, userID int64, channelID string) (bool, stargate.Scope, error) {
	rid, err := id.Parse(channelID)
	if err != nil {
		return false, stargate.Scope{}, nil
	}
	room, ok, err := a.roomSvc.CheckAccess(ctx, userID, rid)
	if err == nil {
		return ok, stargate.InRoom(room.SpaceID, room.ID), nil
	}
	if errors.Is(err, spaces.ErrNotMember) {
		return false, stargate.Scope{}, nil
	}
	if err != rooms.ErrRoomNotFound {
		return false, stargate.Scope{}, err
	}
	// Not a room id - try it as a space id (space-wide broadcast channel).
	ok, err = a.spaceSvc.IsMember(ctx, rid, userID)
	return ok, stargate.InSpace(rid), err
}

func (p *readyDataProvider) GetReadyData(ctx context.Context, userID int64) (*stargate.ReadyData, error) {
	roomList, err := p.roomSvc.ListRooms(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list rooms: %w", err)
	}
	relList, err := p.relSvc.ListRelationships(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list relationships: %w", err)
	}
	// Every room whose voice states the user may see: their PMs and group PMs, and the
	// voice rooms of their spaces (collected below).
	voiceRoomIDs := make([]int64, 0, len(roomList))
	out := make([]map[string]interface{}, 0, len(roomList))
	// Every "space" channel the user may subscribe to, and what each is (see
	// stargate.ReadyData.ChannelIDs / ChannelScopes).
	channelIDs := make([]string, 0, len(roomList))
	channelScopes := make(map[string]stargate.Scope, len(roomList))
	addChannel := func(sc stargate.Scope) {
		cid := sc.RoomID
		if cid == 0 {
			cid = sc.SpaceID
		}
		channelIDs = append(channelIDs, id.Format(cid))
		channelScopes[id.Format(cid)] = sc
	}
	// A space's rooms and threads wait for the per-space visibility check below: a
	// rooms_by_user row exists for every member a message fanned out to, whether or not
	// they may view the room, so the row alone cannot admit a room to READY (or, for a bot,
	// to its auto-subscriptions).
	var spaceRows []rooms.RoomWithParticipants
	for _, r := range roomList {
		if r.SpaceID != nil {
			spaceRows = append(spaceRows, r)
			continue
		}
		if r.Type == rooms.TypePM || r.Type == rooms.TypeGroupPM {
			voiceRoomIDs = append(voiceRoomIDs, r.ID)
		}
		out = append(out, readyRoomMap(r))
		addChannel(stargate.InRoom(r.SpaceID, r.ID))
	}
	visible := map[int64]struct{}{}

	// Spaces and space rooms. Like Discord's GUILD_CREATE, each space carries its roles
	// and each room its permission overrides, so the client can compute permissions
	// locally and never has to fetch them when opening a channel.
	var spacesList []map[string]interface{}
	spaceRoomsMap := make(map[string]interface{})
	if p.spaceSvc != nil {
		readState := p.spaceSvc.LoadReadState(ctx, userID)
		spaceRows, err := p.spaceSvc.ListSpacesForUser(ctx, userID)
		if err == nil {
			ids := make([]int64, 0, len(spaceRows))
			for _, row := range spaceRows {
				ids = append(ids, row.SpaceID)
			}
			spaceObjs, err := p.spaceSvc.GetSpaces(ctx, ids)
			if err != nil {
				spaceObjs = nil
			}
			for _, space := range spaceObjs {
				if space == nil {
					continue
				}
				// Only the rooms this user may actually view: a channel they lack
				// PermViewRoom for is neither advertised in space_rooms (so it never shows
				// in their client) nor added to channelIDs (so a bot is never auto-
				// subscribed to it). The realtime authorizer enforces the same gate. The
				// entries are built by the code SPACE_CREATE's rooms are, so the two agree.
				roomMaps, roomListForSpace, snap, err := p.spaceSvc.ViewerSpaceRooms(ctx, userID, space.ID, readState)
				if err != nil {
					continue
				}
				spacesList = append(spacesList, spaceToReadyMap(space, snap))
				addChannel(stargate.InSpace(space.ID))
				for _, r := range roomListForSpace {
					visible[r.ID] = struct{}{}
					if r.Type == rooms.TypeSpaceVoice {
						voiceRoomIDs = append(voiceRoomIDs, r.ID)
					}
					addChannel(stargate.InSpace(space.ID).About(r.ID))
				}
				spaceRoomsMap[id.Format(space.ID)] = roomMaps
			}
		}
	}

	for _, r := range spaceRows {
		if _, ok := visible[r.ID]; !ok && p.spaceSvc != nil {
			// Not in the active, viewable set. An archived thread the user joined is still
			// theirs to see; a channel they lost View on, or a room of a space they left, is not.
			if r.Type != rooms.TypeThread || !p.canView(ctx, userID, r) {
				continue
			}
		}
		out = append(out, readyRoomMap(r))
		addChannel(stargate.InRoom(r.SpaceID, r.ID))
	}

	data := &stargate.ReadyData{Rooms: out, Relationships: relList, Spaces: spacesList, SpaceRooms: spaceRoomsMap, ChannelIDs: channelIDs, ChannelScopes: channelScopes}
	if p.voice != nil && len(voiceRoomIDs) > 0 {
		states, calls, err := p.voice.StatesForRooms(ctx, voiceRoomIDs)
		if err != nil {
			logger.Warn("stargate", "voice states for READY: %v", err)
		} else {
			if states == nil {
				states = []voice.State{}
			}
			views := make([]*voice.CallView, 0, len(calls))
			for i := range calls {
				views = append(views, calls[i].View())
			}
			data.VoiceStates = states
			data.Calls = views
		}
	}
	return data, nil
}

// canView reports whether the user may view a space room that is not in the space's active
// room list (an archived thread): the same permission resolution the authorizer uses.
func (p *readyDataProvider) canView(ctx context.Context, userID int64, r rooms.RoomWithParticipants) bool {
	if r.SpaceID == nil {
		return false
	}
	perms, err := p.spaceSvc.EffectiveChannelPermissions(ctx, userID, *r.SpaceID, r.ID)
	return err == nil && permissions.Has(perms, permissions.PermViewRoom)
}

func spaceToReadyMap(s *spaces.Space, snap *spaces.Snapshot) map[string]interface{} {
	m := map[string]interface{}{
		"id":                            id.Format(s.ID),
		"name":                          s.Name,
		"name_acronym":                  s.NameAcronym,
		"description":                   s.Description,
		"icon":                          s.Icon,
		"banner":                        s.Banner,
		"owner_id":                      id.Format(s.OwnerID),
		"verification_level":            s.VerificationLevel,
		"official":                      s.Official,
		"default_message_notifications": s.DefaultMessageNotif,
		"explicit_content_filter":       s.ExplicitContentFilter,
		"features":                      s.Features,
		"afk_timeout":                   s.AFKTimeout,
		"system_room_flags":             s.SystemRoomFlags,
		"max_presences":                 s.MaxPresences,
		"max_members":                   s.MaxMembers,
		"vanity_url_code":               s.VanityURLCode,
		"preferred_locale":              s.PreferredLocale,
		"max_video_room_users":          s.MaxVideoRoomUsers,
		"widget_enabled":                s.WidgetEnabled,
		"automod_flags":                 s.AutomodFlags,
		"automod_mention_limit":         s.AutomodMentionLimit,
		"created_at":                    s.CreatedAt,
		"updated_at":                    s.UpdatedAt,
	}
	if s.AFKRoomID != nil {
		m["afk_room_id"] = id.Format(*s.AFKRoomID)
	}
	if s.SystemRoomID != nil {
		m["system_room_id"] = id.Format(*s.SystemRoomID)
	}
	if s.RulesRoomID != nil {
		m["rules_room_id"] = id.Format(*s.RulesRoomID)
	}
	if s.PublicUpdatesRoomID != nil {
		m["public_updates_room_id"] = id.Format(*s.PublicUpdatesRoomID)
	}
	if s.WidgetRoomID != nil {
		m["widget_room_id"] = id.Format(*s.WidgetRoomID)
	}
	if s.Federation != nil {
		m["federation"] = s.Federation
	}
	if snap != nil {
		if snap.EveryoneRoleID != 0 {
			m["everyone_role_id"] = id.Format(snap.EveryoneRoleID)
		}
		m["roles"] = spaces.RoleMaps(snap.Roles)
	} else if s.EveryoneRoleID != 0 {
		m["everyone_role_id"] = id.Format(s.EveryoneRoleID)
	}
	return m
}

// readyRoomMap is the READY payload's form of one of the user's rooms.
func readyRoomMap(r rooms.RoomWithParticipants) map[string]interface{} {
	m := map[string]interface{}{
		"id":         id.Format(r.ID),
		"type":       r.Type,
		"recipients": recipientIDsToSlice(r.ParticipantIDs),
		"created_at": r.CreatedAt,
	}
	if r.SpaceID != nil {
		m["space_id"] = id.Format(*r.SpaceID)
	}
	if r.ParentID != nil {
		m["parent_id"] = id.Format(*r.ParentID)
	}
	if r.Name != "" {
		m["name"] = r.Name
	}
	if r.Topic != "" {
		m["topic"] = r.Topic
	}
	if r.Position != 0 {
		m["position"] = r.Position
	}
	if r.LastMessageID != nil {
		m["last_message_id"] = id.Format(*r.LastMessageID)
	}
	if r.LastReadMessageID != nil {
		m["last_read_message_id"] = id.Format(*r.LastReadMessageID)
	}
	if r.MentionCount > 0 {
		m["mention_count"] = r.MentionCount
	}
	if r.Muted {
		m["muted"] = true
	}
	if r.MutedUntil != nil {
		m["muted_until"] = *r.MutedUntil
	}
	if r.NotifyMode != 0 {
		m["notify_mode"] = r.NotifyMode
	}
	if !r.UpdatedAt.IsZero() {
		m["updated_at"] = r.UpdatedAt
	}
	if r.Type == rooms.TypeGroupPM {
		m["creator_id"] = id.Format(r.CreatorID)
	} else if r.CreatorID != 0 {
		m["creator_id"] = id.Format(r.CreatorID)
	}
	// A space's rooms (threads the user joined land here too) are plaintext unless turned
	// on; only PMs and group PMs default to encrypted.
	m["e2ee_enabled"] = r.E2EEOn()
	if len(r.Participants) > 0 {
		m["participants"] = r.Participants
	}
	if r.Federation != nil {
		m["federation"] = r.Federation
	}
	return m
}

func recipientIDsToSlice(ids []int64) []string {
	out := make([]string, len(ids))
	for i, uid := range ids {
		out[i] = id.Format(uid)
	}
	return out
}

func main() {
	// Same env-file handling as cmd/api: ENV_FILE selects the file, a missing one is fine.
	envFile := os.Getenv("ENV_FILE")
	if envFile == "" {
		envFile = ".env"
	}
	if err := godotenv.Load(envFile); err != nil && !os.IsNotExist(err) {
		log.Fatal(err)
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	logger.InitDefault(cfg.Log.Level)

	scylla, err := db.NewScylla(cfg.Database.Scylla)
	if err != nil {
		log.Fatal(err)
	}
	redis := db.NewRedis(cfg.Database.Redis)

	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(scylla), redis, cfg)
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(scylla), redis, cfg)

	roomRepo := rooms.NewRepository(scylla)
	spaceRepo := spaces.NewRepository(scylla)
	spaceSvc := spaces.NewService(spaceRepo, roomRepo, userRepo, redis, cfg)
	roomSvc := rooms.NewService(roomRepo, userRepo, redis, cfg, nil, spaceSvc)
	if cfg.Federation.Enabled {
		// READY must carry each federated room's global identity so clients key their
		// Megolm sessions by it (see rooms.Federation), and each mirrored space's origin.
		info := federation.NewInfoProvider(scylla, cfg.Federation.Domain)
		roomSvc.SetFederationInfo(info)
		spaceSvc.SetFederationInfo(info)
	}
	relRepo := relationships.NewRepository(scylla)
	relSvc := relationships.NewService(relRepo, userRepo, redis, cfg)
	readyProvider := &readyDataProvider{roomSvc: roomSvc, relSvc: relSvc, spaceSvc: spaceSvc}
	if cfg.Voice.Enabled {
		readyProvider.voice = voice.NewStore(redis, cfg.Database.Redis.CachePrefix)
	}

	// What people are playing (op 9), in Redis under the deployment's cache prefix - the
	// same store the API reads, so a status change made there carries the activity.
	activities := stargate.NewActivityStore(redis, cfg.Database.Redis.CachePrefix)
	stargate.SetActivityStore(activities)

	presenceNotifier := stargate.NewDefaultPresenceNotifier(userRepo, redis, cfg.Stargate.Region)
	if cfg.Federation.Enabled {
		// Connect/disconnect presence reaches remote friends' instances, signed with the
		// API's identity: the gateway loads that key but never creates it.
		presenceNotifier.SetFederator(&lazyPresenceFederator{build: func() (*federation.Service, error) {
			return federation.NewWithExistingKey(cfg, scylla, redis)
		}})
	}
	hub := stargate.NewHubWithConfig(stargate.HubConfig{
		Redis:            redis,
		Region:           cfg.Stargate.Region,
		PresenceNotifier: presenceNotifier,
		Authorizer:       &roomChannelAuthorizer{roomSvc: roomSvc, spaceSvc: spaceSvc},
	})
	ctx := context.Background()
	// A fresh gateway has no clients, so the online-presence set must start empty; this is
	// what makes a crash self-heal on a single-instance deployment (stale ids from the old
	// process are dropped). See stargate.OnlinePresenceKey.
	if err := redis.Del(ctx, stargate.OnlinePresenceKey).Err(); err != nil {
		logger.Warn("stargate", "could not reset online-presence set: %v", err)
	}
	// The same for the connections' activities: none of those sockets exist any more.
	// Remote users' activities are the API's and stay.
	if err := activities.PurgeConnections(ctx); err != nil {
		logger.Warn("stargate", "could not reset connection activities: %v", err)
	}
	hub.Run(ctx)

	// Bots connect with `Authorization: Bot <token>`; the applications store resolves it.
	appsRepo := applications.NewRepository(scylla)
	// An instance ban has to reach a bot's token the way it reaches a person's sessions.
	bans := instance.NewService(cfg, instance.NewRepository(scylla))
	bans.SetModeration(instance.ModerationDeps{Repo: instance.NewModerationRepository(scylla)})
	resolver := stargate.NewResolver(sessionRepo, userRepo).WithBots(appsRepo).WithBanChecker(bans, func(ctx context.Context, botID int64) (int64, error) {
		app, err := appsRepo.GetByID(ctx, botID)
		if err != nil || app == nil {
			return 0, err
		}
		return app.OwnerID, nil
	})

	srv := stargate.NewServer(stargate.ServerConfig{
		Hub:               hub,
		Resolver:          resolver,
		ReadyDataProvider: readyProvider,
		AllowedOrigins:    cfg.Stargate.AllowedOrigins,
		ReadBufferSize:    cfg.Stargate.ReadBufferSize,
		WriteBufferSize:   cfg.Stargate.WriteBufferSize,
	})

	mux := http.NewServeMux()
	mux.Handle("/events", srv)

	addr := fmt.Sprintf(":%s", cfg.Stargate.Port)
	httpServer := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
	}

	go func() {
		logger.Info("stargate", "listening on %s (/%s)", addr, "events")

		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("stargate", "server stopped: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	logger.Info("stargate", "stopping...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("stargate shutdown: %v", err)
	}

	scylla.Close()
	_ = redis.Close()
}
