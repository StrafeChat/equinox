package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
	"github.com/StrafeChat/equinox/internal/mail"
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
		// An error nothing handled must not reach the client as text: a gocql or Redis
		// message names tables and hosts. Fiber's own *fiber.Error values (404, 405, 413,
		// ...) are safe and kept as JSON; anything else - including a recovered panic -
		// becomes a generic 500 and is logged here with the real cause.
		ErrorHandler: func(c fiber.Ctx, err error) error {
			var fe *fiber.Error
			if errors.As(err, &fe) && fe.Code < http.StatusInternalServerError {
				return c.Status(fe.Code).JSON(fiber.Map{"error": fe.Message})
			}
			logger.Err("http", err, map[string]any{"method": c.Method(), "path": c.Path()})
			return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
		},
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
		// Mirrors of spaces hosted elsewhere are brought back in line with their origin
		// now and then, in case a relay was missed while this instance was down.
		fed.StartMaintenance(context.Background())
	}

	// A nil interface, not a nil *mail.SMTP in an interface - route setup tests the
	// interface against nil to decide whether email features exist.
	var mailer mail.Mailer
	if a.Config.Mail.Enabled {
		m, err := mail.NewSMTP(a.Config.Mail)
		if err != nil {
			return fmt.Errorf("mail: %w", err)
		}
		mailer = m
		policy := "verification optional"
		if a.Config.Mail.VerificationRequired {
			policy = "verification required to sign in"
		}
		logger.Info("mail", "sending through %s:%d (%s) as %s - %s", a.Config.Mail.Host, a.Config.Mail.Port, a.Config.Mail.TLS, a.Config.Mail.From, policy)
	}

	routes.SetupRoutes(routes.Deps{
		App:        a.Fiber,
		Config:     a.Config,
		Scylla:     a.Scylla,
		Redis:      a.Redis,
		Federation: fed,
		Mailer:     mailer,
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
