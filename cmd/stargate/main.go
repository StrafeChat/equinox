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
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/relationships"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
	"github.com/StrafeChat/equinox/internal/stargate"
	"github.com/joho/godotenv"
)

// readyDataProvider fetches rooms, relationships, spaces, and space rooms for the READY payload.
type readyDataProvider struct {
	roomSvc  *rooms.Service
	relSvc   *relationships.Service
	spaceSvc *spaces.Service
}

func (p *readyDataProvider) GetReadyData(ctx context.Context, userID int64) (interface{}, interface{}, interface{}, interface{}, error) {
	roomList, err := p.roomSvc.ListRooms(ctx, userID)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("list rooms: %w", err)
	}
	relList, err := p.relSvc.ListRelationships(ctx, userID)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("list relationships: %w", err)
	}
	out := make([]map[string]interface{}, 0, len(roomList))
	for _, r := range roomList {
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
		out = append(out, m)
	}

	// Spaces and space rooms
	var spacesList []map[string]interface{}
	spaceRoomsMap := make(map[string]interface{})
	if p.spaceSvc != nil {
		spaceRows, err := p.spaceSvc.ListSpacesForUser(ctx, userID)
		if err == nil {
			for _, row := range spaceRows {
				space, err := p.spaceSvc.GetSpace(ctx, row.SpaceID)
				if err != nil || space == nil {
					continue
				}
				spacesList = append(spacesList, spaceToReadyMap(space))
				roomListForSpace, err := p.spaceSvc.ListSpaceRooms(ctx, userID, row.SpaceID)
				if err != nil {
					continue
				}
				roomMaps := make([]map[string]interface{}, 0, len(roomListForSpace))
				for _, r := range roomListForSpace {
					m := roomToReadyMap(r)
					if ur, _ := p.roomSvc.GetUserRoomRow(ctx, userID, r.ID); ur != nil {
						if ur.LastReadMessageID != nil {
							m["last_read_message_id"] = id.Format(*ur.LastReadMessageID)
						}
						if ur.LastMessageID != nil {
							m["last_message_id"] = id.Format(*ur.LastMessageID)
						}
						if ur.MentionCount > 0 {
							m["mention_count"] = ur.MentionCount
						}
					}
					roomMaps = append(roomMaps, m)
				}
				spaceRoomsMap[id.Format(row.SpaceID)] = roomMaps
			}
		}
	}

	return out, relList, spacesList, spaceRoomsMap, nil
}

func spaceToReadyMap(s *spaces.Space) map[string]interface{} {
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
	return m
}

func roomToReadyMap(r *rooms.Room) map[string]interface{} {
	m := map[string]interface{}{
		"id":         id.Format(r.ID),
		"type":       r.Type,
		"name":       r.Name,
		"topic":      r.Topic,
		"position":   r.Position,
		"created_at": r.CreatedAt,
		"updated_at": r.UpdatedAt,
	}
	if r.SpaceID != nil {
		m["space_id"] = id.Format(*r.SpaceID)
	}
	if r.ParentID != nil {
		m["parent_id"] = id.Format(*r.ParentID)
	}
	if r.LastMessageID != nil {
		m["last_message_id"] = id.Format(*r.LastMessageID)
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
	err := godotenv.Load()
	if err != nil {
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

	presenceNotifier := stargate.NewDefaultPresenceNotifier(userRepo, redis, cfg.Stargate.Region)
	hub := stargate.NewHubWithConfig(stargate.HubConfig{
		Redis:            redis,
		Region:           cfg.Stargate.Region,
		PresenceNotifier: presenceNotifier,
	})
	ctx := context.Background()
	hub.Run(ctx)

	resolver := stargate.NewResolver(sessionRepo, userRepo)

	roomRepo := rooms.NewRepository(scylla)
	spaceRepo := spaces.NewRepository(scylla)
	spaceSvc := spaces.NewService(spaceRepo, roomRepo, userRepo, redis, cfg)
	roomSvc := rooms.NewService(roomRepo, userRepo, redis, cfg, nil, spaceSvc)
	relRepo := relationships.NewRepository(scylla)
	relSvc := relationships.NewService(relRepo, userRepo, redis, cfg)
	readyProvider := &readyDataProvider{roomSvc: roomSvc, relSvc: relSvc, spaceSvc: spaceSvc}

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
