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
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/stargate"
	"github.com/joho/godotenv"
)

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

	hub := stargate.NewHub(redis, cfg.Stargate.Region)
	ctx := context.Background()
	hub.Run(ctx)

	sessionRepo := auth.NewCachedSessionRepository(auth.NewSessionRepository(scylla), redis, cfg)
	userRepo := auth.NewCachedUserRepository(auth.NewUserRepository(scylla), redis, cfg)
	resolver := stargate.NewResolver(sessionRepo, userRepo)

	srv := stargate.NewServer(stargate.ServerConfig{
		Hub:             hub,
		Resolver:        resolver,
		AllowedOrigins:  cfg.Stargate.AllowedOrigins,
		ReadBufferSize:  cfg.Stargate.ReadBufferSize,
		WriteBufferSize: cfg.Stargate.WriteBufferSize,
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
