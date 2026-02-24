package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/db"
	"github.com/StrafeChat/equinox/internal/routes"
	"github.com/gofiber/fiber/v3"
)

type App struct {
	cfg    *config.Config
	fiber  *fiber.App
	scylla any
	redis  any
}

func New(cfg *config.Config) (*App, error) {
	f := fiber.New()

	_, scylla := db.NewScylla(cfg.Database.Scylla)
	redis := db.NewRedis(cfg.Database.Redis)

	app := &App{
		cfg:    cfg,
		fiber:  f,
		scylla: scylla,
		redis:  redis,
	}

	app.register()

	return app, nil
}

func (a *App) register() {

	routes.SetupRoutes(a.fiber, a.cfg)
}

func (a *App) Start() error {
	addr := fmt.Sprintf(":%s", a.cfg.HTTP.Port)

	go func() {
		log.Printf("[SERVER] Starting on %s", addr)

		if err := a.fiber.Listen(addr); err != nil {
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

	return a.fiber.ShutdownWithContext(ctx)
}
