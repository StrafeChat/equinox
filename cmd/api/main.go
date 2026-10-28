package main

import (
	"log"
	"os"

	"github.com/StrafeChat/equinox/internal/app"
	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/stargate"
	"github.com/joho/godotenv"
)

// main runs the REST API. Configuration comes from the environment, optionally seeded
// from an env file; see internal/config for every variable.
func main() {
	// ENV_FILE picks an alternative env file (e.g. a second instance on the same machine).
	// A missing file is fine: containers pass configuration through the environment.
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
	id.Init(cfg.App.SnowflakeNode)

	a, err := app.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	// The gateway writes what people are playing; the API reads it for member lists, the
	// friends list, PATCH /users/@me's PRESENCE_UPDATE and federation, and writes remote
	// users' relayed activities. Same Redis, same prefix as the gateway.
	stargate.SetActivityStore(stargate.NewActivityStore(a.Redis, cfg.Database.Redis.CachePrefix))

	if err := a.Start(); err != nil {
		log.Fatal(err)
	}
}
