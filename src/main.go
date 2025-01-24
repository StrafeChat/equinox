package main

import (
	"log"
	"os"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/cors"
	"github.com/gofiber/fiber/v3/middleware/helmet"
	"github.com/gofiber/fiber/v3/middleware/limiter"
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
		panic("Error while loading enviroment variables: " + err.Error())
	}

	/*_ Initialize Databases _*/
	database.InitDB()
	defer database.Session.Close()

	app := fiber.New(fiber.Config{
		ErrorHandler: func(c fiber.Ctx, err error) error {
			log.Printf("Error in request: %v", err)
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			return c.Status(code).JSON(fiber.Map{
				"error": err.Error(),
			})
		},
	})

	/*_ Use protection _*/
	app.Use(helmet.New())
	app.Use(recover.New(recover.Config{
		EnableStackTrace: true,
	}))

	app.Use(limiter.New(limiter.Config{
		Max:        10,
		Expiration: 5 * time.Second,
		KeyGenerator: func(c fiber.Ctx) string {
			return c.IP()
		},
		LimitReached: func(c fiber.Ctx) error {
			return c.SendStatus(fiber.StatusTooManyRequests)
		},
		SkipFailedRequests:     false,
		SkipSuccessfulRequests: false,
	}))

	domain := os.Getenv("DOMAIN")
	if domain == "" {
		domain = "https://alpha.strafechat.dev"  // Default to production domain if env not set
	}

	app.Use(cors.New(cors.Config{
		AllowOrigins: []string{"*"},
		AllowMethods: []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "HEAD", "PATCH"},
		AllowHeaders: []string{
			"Origin", 
			"Content-Type", 
			"Accept", 
			"Content-Length", 
			"Accept-Language", 
			"Accept-Encoding", 
			"Connection", 
			"Access-Control-Allow-Origin", 
			"Access-Control-Allow-Methods",
			"Access-Control-Allow-Headers",
			"Access-Control-Allow-Credentials",
			"X-Session-Token",
		},
		ExposeHeaders: []string{"X-Session-Token"},
		MaxAge:       7200, // 2 hours
	}))

	/*_ Log all incoming requests _*/
	app.Use(logger.New(logger.Config{
		Format: "[${time}] ${status} ${latency} ${remote_ip} ${method} ${path}\n",
	}))

	/*_ Setup all routes _*/
	routes.SetupRoutes(app)

	log.Fatal(app.Listen(":" + os.Getenv("PORT")))
}
