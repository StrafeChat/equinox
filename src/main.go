package main

import (
	"log"

	db "go-webserver/src/database"
	"go-webserver/src/routes"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
)

func main() {
	db.InitDB()
	defer db.Session.Close()

	app := fiber.New()

	app.Use(recover.New())
	app.Use(logger.New())

	routes.SetupUsersRoutes(app)

	log.Fatal(app.Listen(":443"))
}
