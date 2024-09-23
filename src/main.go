package main

import (
	"log"
	"os"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/cors"
	"github.com/gofiber/fiber/v3/middleware/helmet"
	"github.com/gofiber/fiber/v3/middleware/logger"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/joho/godotenv"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/routes"
)

func main() {
	/*_ Load env variables from file _*/
	err := godotenv.Load(".env")
	if err != nil {
		log.Fatal("Error while loading enviroment variables: " + err.Error())
	}

	/*_ Initialize Databases _*/
	database.InitDB()
	defer database.Session.Close()

	app := fiber.New()

	/*_ Use protection _*/
	app.Use(helmet.New())
	app.Use(recover.New())
	app.Use(cors.New(cors.Config{
		AllowOrigins: []string{os.Getenv("DOMAIN")},
	}))

	/*_ Log all incoming requests _*/
	app.Use(logger.New(logger.Config{
		Format: "[${time}] ${status} ${latency} ${remote_ip} ${method} ${path}\n",
	}))

	/*_ Setup all routes _*/
	routes.SetupRoutes(app)

	log.Fatal(app.Listen(":443"))
}
