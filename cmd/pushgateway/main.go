package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/pushgateway"
	"github.com/StrafeChat/equinox/internal/safego"
	"github.com/joho/godotenv"
)

// main runs the push gateway: Web Push in, FCM / APNs out. It is configured entirely by
// PUSHGW_* environment variables (see internal/pushgateway/config.go) and keeps no state,
// so it can be restarted or replicated freely.
func main() {
	// Like the API, an env file may seed the environment for local runs; a missing file is
	// fine, containers pass configuration directly.
	envFile := os.Getenv("ENV_FILE")
	if envFile == "" {
		envFile = ".env"
	}
	if err := godotenv.Load(envFile); err != nil && !os.IsNotExist(err) {
		log.Fatal(err)
	}
	logger.InitFromEnv()

	cfg, err := pushgateway.LoadConfig(os.Getenv)
	if err != nil {
		log.Fatalf("pushgateway: %v", err)
	}
	srv, err := pushgateway.NewServer(cfg, pushgateway.Options{})
	if err != nil {
		log.Fatalf("pushgateway: %v", err)
	}
	platforms := strings.Join(srv.Platforms(), ",")
	if platforms == "" {
		platforms = "none"
		log.Printf("[pushgateway] warning: no platform configured; set PUSHGW_FCM_CREDENTIALS and/or PUSHGW_APNS_*")
	}
	if cfg.Debug {
		log.Printf("[pushgateway] warning: PUSHGW_DEBUG is on - pushes to debug endpoints are kept in memory and listed at /v1/debug/pushes; never run this in production")
	}

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       20 * time.Second,
		// Long enough for a forward to Google/Apple, OAuth exchange included.
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  2 * time.Minute,
		// Sealed endpoints make long request lines; nothing legitimate needs more.
		MaxHeaderBytes: 64 << 10,
		ErrorLog:       log.New(os.Stderr, "[pushgateway][http] ", log.LstdFlags),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	safego.Go("pushgateway", func() { srv.LogStats(ctx, 5*time.Minute) })

	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()
	log.Printf("[pushgateway] listening on %s, public URL %s, platforms: %s",
		cfg.Listen, cfg.PublicURL, platforms)

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("pushgateway: %v", err)
		}
	case <-ctx.Done():
		// Let in-flight forwards finish: a push the sender already handed us should reach
		// Google/Apple rather than be cut off mid-request by a deploy.
		log.Printf("[pushgateway] shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			log.Printf("[pushgateway] shutdown: %v", err)
		}
	}
}
