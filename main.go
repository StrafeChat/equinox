package main

import (
	"log"

	"github.com/StrafeChat/equinox/config"
	"github.com/StrafeChat/equinox/routes"
	"github.com/gofiber/fiber/v3"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	app := fiber.New()
	routes.SetupRoutes(app, cfg)
	app.Listen(":3000")
}
