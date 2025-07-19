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
	"github.com/StrafeChat/equinox/src/helpers"
	"github.com/StrafeChat/equinox/src/middleware"
	"github.com/StrafeChat/equinox/src/portal"
	"github.com/StrafeChat/equinox/src/routes"
)

func main() {

	/*_ Load env variables from file _*/
	errEnv := godotenv.Load(".env")
	if errEnv != nil {
		panic("Error while loading enviroment variables: " + errEnv.Error())
	}

	/*_ Initialize Databases _*/
	database.InitDB()
	defer database.Session.Close()

	/*_ Initialize Livekit Connection _*/
	portal.InitPortal()

	// Start the password reset cleanup task
	helpers.ScheduleCleanupTask()

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
		ProxyHeader: "X-Forwarded-For",
		// Performance optimizations
		ReadBufferSize:    16384,      // 16KB read buffer
		WriteBufferSize:   16384,      // 16KB write buffer
		Concurrency:       256 * 1024, // Handle more concurrent connections
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		BodyLimit:         50 * 1024 * 1024, // 50MB body limit
		DisableKeepalive:  false,
		ReduceMemoryUsage: false, // Keep false for better performance
	})

	/*_ Use protection _*/
	app.Use(helmet.New())
	app.Use(recover.New(recover.Config{
		EnableStackTrace: true,
	}))

	app.Use(limiter.New(limiter.Config{
		Max:        25,
		Expiration: 5 * time.Second,
		KeyGenerator: func(c fiber.Ctx) string {
			return middleware.GetRealIP(c)
		},
		LimitReached: func(c fiber.Ctx) error {
			return c.SendStatus(fiber.StatusTooManyRequests)
		},
		SkipFailedRequests:     false,
		SkipSuccessfulRequests: false,
	}))

	domain := os.Getenv("DOMAIN")
	if domain == "" {
		domain = "https://alpha.strafechat.dev" // Default to production domain if env not set
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
		MaxAge:        7200,
	}))

	/*_ Apply Cloudflare IP detection middleware _*/
	app.Use(middleware.CloudflareIP())

	/*_ Log all incoming requests _*/
	app.Use(logger.New(logger.Config{
		Format: "[${time}] ${status} ${latency} ${locals:original_ip} ${method} ${path}\n",
	}))

	/*_ Setup all routes _*/
	routes.SetupRoutes(app)

	log.Fatal(app.Listen("0.0.0.0:" + os.Getenv("PORT")))
}
