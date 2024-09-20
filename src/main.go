package main

import (
	"log"

	db "github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/routes"
	"github.com/joho/godotenv"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/cors"
	"github.com/gofiber/fiber/v3/middleware/helmet"
	"github.com/gofiber/fiber/v3/middleware/logger"
	"github.com/gofiber/fiber/v3/middleware/recover"
)

func main() {
    
	/*_ Load env vairlbes from file _*/
	err := godotenv.Load(".env")
    if err != nil {
      log.Fatal(err)
    }

    /*_ Initialize Databases _*/
	db.InitDB()
	defer db.Session.Close()

	app := fiber.New()
    
	/*_ Use protection _*/
	app.Use(helmet.New())
	app.Use(recover.New())
	app.Use(cors.New(cors.Config{
		AllowOrigins: []string{"https://*.strafe.chat"},
	}))

	/*_ Log all incoming requests _*/
	app.Use(logger.New(logger.Config{
		Format: "[${time}] ${status} ${latency} ${remote_ip} ${method} ${path}\n",
	}))

	/*_ Setup all routes _*/
	routes.SetupRoutes(app)

	log.Fatal(app.Listen(":443"))
}