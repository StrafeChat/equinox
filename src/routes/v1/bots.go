package routes_v1

import (
	bots "github.com/StrafeChat/equinox/src/handlers/v1/bots"
	"github.com/StrafeChat/equinox/src/middleware"
	"github.com/gofiber/fiber/v3"
)

func SetupBotsRoutes(versionRouter *fiber.Group) {
	router := versionRouter.Group("/bots")

	router.Use(middleware.VerifyAuth())

	// Bot CRUD operations
	router.Post("/", bots.CreateBot)           // POST /v1/bots - Create a new bot
	router.Get("/", bots.GetMyBots)            // GET /v1/bots - Get all bots owned by user
	router.Get("/discoverable", bots.GetDiscoverableBots) // GET /v1/bots/discoverable - Get all public and discoverable bots
	router.Get("/:id", bots.GetBot)            // GET /v1/bots/:id - Get specific bot details
	router.Patch("/:id", bots.UpdateBot)       // PATCH /v1/bots/:id - Update bot details
	router.Delete("/:id", bots.DeleteBot)      // DELETE /v1/bots/:id - Delete bot

	// Bot avatar management
	router.Post("/:id/avatar", bots.UpdateBotAvatar) // POST /v1/bots/:id/avatar - Update bot avatar

	// Bot token management
	router.Post("/:id/regenerate-token", bots.RegenerateToken) // POST /v1/bots/:id/regenerate-token - Regenerate bot token
}