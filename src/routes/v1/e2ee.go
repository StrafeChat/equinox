package routes_v1

import (
	"github.com/StrafeChat/equinox/src/handlers"
	"github.com/StrafeChat/equinox/src/middleware"
	"github.com/gofiber/fiber/v3"
)

// SetupE2EERoutes sets up all E2EE-related routes
func SetupE2EERoutes(versionRouter *fiber.Group) {
	router := versionRouter.Group("/e2ee")

	// All E2EE routes require authentication
	router.Use(middleware.VerifyAuth())

	// Key management routes
	router.Post("/initialize", handlers.InitializeE2EE)
	router.Get("/status", handlers.GetE2EEStatus)
	router.Get("/users/:userId/status", handlers.GetUserE2EEStatus)
	router.Get("/users/:userId/bundle", handlers.GetPreKeyBundle)
	router.Post("/prekeys/refresh", handlers.RefreshPreKeys)

	// Direct message encryption routes
	router.Post("/encrypt", handlers.EncryptMessage)
	router.Post("/decrypt", handlers.DecryptMessage)

	// Group message encryption routes
	router.Post("/groups/:roomId/session", handlers.CreateGroupSession)
	router.Post("/groups/:roomId/encrypt", handlers.EncryptGroupMessage)
	router.Post("/groups/:roomId/decrypt", handlers.DecryptGroupMessage)
}