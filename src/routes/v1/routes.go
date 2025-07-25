package routes_v1

import (
	"log"

	"github.com/gofiber/fiber/v3"
)

func SetupRoutes(app *fiber.App) {
	log.Println("Loading V1 routes.")
	versionRouter := app.Group("/v1").(*fiber.Group)

	SetupAuthRoutes(versionRouter)
	SetupUsersRoutes(versionRouter)
	SetupRoomsRoutes(versionRouter)
	SetupPortalRoutes(versionRouter)
	SetupSpacesRoutes(versionRouter)
	SetupInvitesRoutes(versionRouter)
	SetupBotsRoutes(versionRouter)
}
