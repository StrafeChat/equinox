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
	"github.com/StrafeChat/equinox/src/routes"
	// "github.com/resend/resend-go/v2"
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
		EnableTrustedProxyCheck: true,
		// Include Cloudflare IP ranges along with local proxies
		TrustedProxies: []string{
			// Local proxies
			"127.0.0.1", "::1", "172.24.0.1",
			// Cloudflare IPv4 ranges
			"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
			"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
			"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
			"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
			// Cloudflare IPv6 ranges (partial list)
			"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
			"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
		},
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
		Format: "[${time}] ${status} ${latency} ${ip} ${method} ${path}\n",
	}))

	/*_ Setup all routes _*/
	routes.SetupRoutes(app)

	log.Fatal(app.Listen("0.0.0.0:" + os.Getenv("PORT")))
}
