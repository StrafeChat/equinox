package routes_v1

import (
	spaces "github.com/StrafeChat/equinox/src/handlers/v1/spaces"
	"github.com/StrafeChat/equinox/src/middleware"
	"github.com/gofiber/fiber/v3"
)

func SetupInvitesRoutes(versionRouter *fiber.Group) {
	// Public invite routes (no auth required)
	publicRouter := versionRouter.Group("/invite")
	publicRouter.Get("/:code", spaces.GetInviteInfo)

	// Authenticated invite routes
	authRouter := versionRouter.Group("/invite")
	authRouter.Use(middleware.VerifyAuth())
	authRouter.Post("/:code/use", spaces.UseInvite)
}
