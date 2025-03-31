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
	// apiKey := """

	// client := resend.NewClient(apiKey)

	// params := &resend.SendEmailRequest{
	//     From:    "no-reply@strafe.chat",
	//     To:      []string{"brydenisnotsmart@proton.me"},
	//     Subject: "Hello World",
	//     Html:    "<p>Congrats on sending your <strong>first email</strong>!</p>",
	// }

	// _, err := client.Emails.Send(params)
	// if err != nil {
	// 	panic("Error while sending email: " + err.Error())
	// }

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
		TrustedProxies:          []string{"127.0.0.1", "::1", "172.24.0.1", "172.69.205.133"}, // Get the real IP from the request and not nginx
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

	/*_ Log all incoming requests _*/
	app.Use(logger.New(logger.Config{
		Format: "[${time}] ${status} ${latency} ${ip} ${method} ${path}\n",
	}))

	/*_ Setup all routes _*/
	routes.SetupRoutes(app)

	log.Fatal(app.Listen("0.0.0.0:" + os.Getenv("PORT")))
}
