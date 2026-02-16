package main

import (
	"log"

	"github.com/StrafeChat/equinox/internal/app"
	"github.com/StrafeChat/equinox/internal/config"
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

	a, err := app.New(cfg)
	if err != nil {
		log.Fatal(err)
	}

	if err := a.Start(); err != nil {
		log.Fatal(err)
	}
}
