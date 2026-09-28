package app

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/redis/go-redis/v9"
	"github.com/scylladb/gocqlx/v3"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/db"
	"github.com/StrafeChat/equinox/internal/federation"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/middleware"
	"github.com/StrafeChat/equinox/internal/routes"
)

type App struct {
	Config *config.Config
	Fiber  *fiber.App
	Scylla gocqlx.Session
	Redis  *redis.Client
}

func New(cfg *config.Config) (*App, error) {
	bodyLimit := cfg.HTTP.BodyLimitKB * 1024
	if bodyLimit <= 0 {
		bodyLimit = 1024 * 1024
	}
	fiberCfg := fiber.Config{
		BodyLimit: int(bodyLimit),
	}
	if len(cfg.HTTP.TrustedProxies) > 0 {
		// Behind Caddy/nginx the TCP peer is always the proxy; take the client address
		// from X-Forwarded-For, but only when the request really came from one of the
		// configured proxies (anyone else could set the header to dodge rate limits).
		fiberCfg.ProxyHeader = fiber.HeaderXForwardedFor
		fiberCfg.TrustProxy = true
		for _, p := range cfg.HTTP.TrustedProxies {
			switch strings.ToLower(p) {
			case "loopback":
				fiberCfg.TrustProxyConfig.Loopback = true
			case "linklocal":
				fiberCfg.TrustProxyConfig.LinkLocal = true
			case "private":
				fiberCfg.TrustProxyConfig.Private = true
			default:
				fiberCfg.TrustProxyConfig.Proxies = append(fiberCfg.TrustProxyConfig.Proxies, p)
			}
		}
	}
	fiber := fiber.New(fiberCfg)

	scylla, err := db.NewScylla(cfg.Database.Scylla)
	if err != nil {
		return nil, err
	}

	redis := db.NewRedis(cfg.Database.Redis)

	app := &App{
		Config: cfg,
		Fiber:  fiber,
		Scylla: scylla,
		Redis:  redis,
	}

	if err := app.register(); err != nil {
		return nil, err
	}

	return app, nil
}

func (a *App) register() error {
	a.Fiber.Use(recover.New(recover.Config{EnableStackTrace: a.Config.Log.Level == logger.LevelDebug}))
	a.Fiber.Use(middleware.SecurityHeaders())
	a.Fiber.Use(middleware.CORS(a.Config.HTTP.CORSOrigins))
	a.Fiber.Use(middleware.RequestLog())

	var fed *federation.Service
	if a.Config.Federation.Enabled {
		var err error
		fed, err = federation.New(a.Config, a.Scylla, a.Redis)
		if err != nil {
			return fmt.Errorf("federation: %w", err)
		}
	}

	routes.SetupRoutes(routes.Deps{
		App:        a.Fiber,
		Config:     a.Config,
		Scylla:     a.Scylla,
		Redis:      a.Redis,
		Federation: fed,
	})
	return nil
}

func (a *App) Start() error {
	addr := fmt.Sprintf(":%s", a.Config.HTTP.Port)

	go func() {
		logger.Info("server", "starting on %s", addr)

		if err := a.Fiber.Listen(addr); err != nil {
			logger.Error("server", "stopped: %v", err)
		}
	}()

	return a.gracefulShutdown()
}

func (a *App) gracefulShutdown() error {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	<-stop

	logger.Info("server", "stopping...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	a.Scylla.Close()
	_ = a.Redis.Close()

	return a.Fiber.ShutdownWithContext(ctx)
}
