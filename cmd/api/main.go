package main

import (
	"log"
	"os"

	"github.com/StrafeChat/equinox/internal/app"
	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
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

	if err := a.Start(); err != nil {
		log.Fatal(err)
	}
}
