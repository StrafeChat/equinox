package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/redis/go-redis/v9"
	"github.com/scylladb/gocqlx/v3"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/db"
	"github.com/StrafeChat/equinox/internal/routes"
)

type App struct {
	Config *config.Config
	Fiber  *fiber.App
	Scylla gocqlx.Session
	Redis  *redis.Client
}

func New(cfg *config.Config) (*App, error) {
	fiber := fiber.New()

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

	app.register()

	return app, nil
}

func (a *App) register() {
	routes.SetupRoutes(routes.Deps{
		App:    a.Fiber,
		Config: a.Config,
		Scylla: a.Scylla,
		Redis:  a.Redis,
	})
}

func (a *App) Start() error {
	addr := fmt.Sprintf(":%s", a.Config.HTTP.Port)

	go func() {
		log.Printf("[SERVER] starting on %s", addr)

		if err := a.Fiber.Listen(addr); err != nil {
			log.Printf("[SERVER] stopped: %v", err)
		}
	}()

	return a.gracefulShutdown()
}

func (a *App) gracefulShutdown() error {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	<-stop

	log.Println("[SERVER] stopping...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	a.Scylla.Close()
	_ = a.Redis.Close()

	return a.Fiber.ShutdownWithContext(ctx)
}
