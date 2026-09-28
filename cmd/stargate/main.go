package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/db"
	"github.com/StrafeChat/equinox/internal/federation"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/relationships"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
	"github.com/StrafeChat/equinox/internal/modules/voice"
	"github.com/StrafeChat/equinox/internal/stargate"
	"github.com/joho/godotenv"
)

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
// PublishToSpace(spaceID, ...)). Both go out on stargate:space:{id} using the raw id, so
// the authorizer has to accept whichever kind of id it's actually looking at.
type roomChannelAuthorizer struct {
	roomSvc  *rooms.Service
	spaceSvc *spaces.Service
}

func (a *roomChannelAuthorizer) CanAccessRoom(ctx context.Context, userID int64, roomID string) (bool, error) {
	rid, err := id.Parse(roomID)
	if err != nil {
		return false, nil
	}
	ok, err := a.roomSvc.CanAccess(ctx, userID, rid)
	if err == nil {
		return ok, nil
	}
	if err != rooms.ErrRoomNotFound {
		return false, err
	}
	// Not a room id - try it as a space id (space-wide broadcast channel).
	return a.spaceSvc.IsMember(ctx, rid, userID)
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
	for _, r := range roomList {
		if r.Type == rooms.TypePM || r.Type == rooms.TypeGroupPM {
			voiceRoomIDs = append(voiceRoomIDs, r.ID)
		}
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
		e2eeEnabled := true
		if r.E2EEEnabled != nil {
			e2eeEnabled = *r.E2EEEnabled
		}
		m["e2ee_enabled"] = e2eeEnabled
		if len(r.Participants) > 0 {
			m["participants"] = r.Participants
		}
		if r.Federation != nil {
			m["federation"] = r.Federation
		}
		out = append(out, m)
	}

	// Spaces and space rooms. Like Discord's GUILD_CREATE, each space carries its roles
	// and each room its permission overrides, so the client can compute permissions
	// locally and never has to fetch them when opening a channel.
	var spacesList []map[string]interface{}
	spaceRoomsMap := make(map[string]interface{})
	if p.spaceSvc != nil {
		mentionCounts, _ := p.roomSvc.GetMentionCounts(ctx, userID)
		userRows, _ := p.roomSvc.UserRoomRows(ctx, userID)
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
				roomListForSpace, snap, err := p.spaceSvc.SpaceRoomsWithOverrides(ctx, space.ID)
				if err != nil {
					continue
				}
				spacesList = append(spacesList, spaceToReadyMap(space, snap))
				roomMaps := make([]map[string]interface{}, 0, len(roomListForSpace))
				for _, r := range roomListForSpace {
					if r.Type == rooms.TypeSpaceVoice {
						voiceRoomIDs = append(voiceRoomIDs, r.ID)
					}
					m := spaces.AttachOverrides(spaces.RoomMap(r), snap.RoomOverridesFor(r.ID))
					ur := userRows[r.ID]
					if ur != nil {
						if ur.LastReadMessageID != nil {
							m["last_read_message_id"] = id.Format(*ur.LastReadMessageID)
						}
						if ur.LastMessageID != nil {
							m["last_message_id"] = id.Format(*ur.LastMessageID)
						}
					}
					if mc := ur.DisplayMentionCount(mentionCounts[r.ID]); mc > 0 {
						m["mention_count"] = mc
					}
					roomMaps = append(roomMaps, m)
				}
				spaceRoomsMap[id.Format(space.ID)] = roomMaps
			}
		}
	}

	data := &stargate.ReadyData{Rooms: out, Relationships: relList, Spaces: spacesList, SpaceRooms: spaceRoomsMap}
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
		// Megolm sessions by it (see rooms.Federation).
		roomSvc.SetFederationInfo(federation.NewInfoProvider(scylla))
	}
	relRepo := relationships.NewRepository(scylla)
	relSvc := relationships.NewService(relRepo, userRepo, redis, cfg)
	readyProvider := &readyDataProvider{roomSvc: roomSvc, relSvc: relSvc, spaceSvc: spaceSvc}
	if cfg.Voice.Enabled {
		readyProvider.voice = voice.NewStore(redis, cfg.Database.Redis.CachePrefix)
	}

	presenceNotifier := stargate.NewDefaultPresenceNotifier(userRepo, redis, cfg.Stargate.Region)
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
	hub.Run(ctx)

	resolver := stargate.NewResolver(sessionRepo, userRepo)

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
