package routes_v1

import (
	spaces "github.com/StrafeChat/equinox/src/handlers/v1/spaces"
	"github.com/StrafeChat/equinox/src/middleware"
	"github.com/gofiber/fiber/v3"
)

func SetupInvitesRoutes(versionRouter *fiber.Group) {
	// Authenticated invite routes
	authRouter := versionRouter.Group("/invite")
	authRouter.Use(middleware.VerifyAuth())
	authRouter.Post("/:code/use", spaces.UseInvite)
	authRouter.Get("/:code", spaces.GetInviteInfo)
}
