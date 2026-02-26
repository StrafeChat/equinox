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
	"github.com/StrafeChat/equinox/internal/stargate"
	"github.com/joho/godotenv"
)

// readyDataProvider fetches rooms and relationships for the READY payload.
type readyDataProvider struct {
	roomSvc *rooms.Service
	relSvc  *relationships.Service
}

func (p *readyDataProvider) GetReadyData(ctx context.Context, userID int64) (interface{}, interface{}, error) {
	roomList, err := p.roomSvc.ListRooms(ctx, userID)
	if err != nil {
		return nil, nil, fmt.Errorf("list rooms: %w", err)
	}
	relList, err := p.relSvc.ListRelationships(ctx, userID)
	if err != nil {
		return nil, nil, fmt.Errorf("list relationships: %w", err)
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
		if len(r.Participants) > 0 {
			m["participants"] = r.Participants
		}
		out = append(out, m)
	}
	return out, relList, nil
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
	roomSvc := rooms.NewService(roomRepo, userRepo, redis, cfg)
	relRepo := relationships.NewRepository(scylla)
	relSvc := relationships.NewService(relRepo, userRepo, redis, cfg)
	readyProvider := &readyDataProvider{roomSvc: roomSvc, relSvc: relSvc}

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
