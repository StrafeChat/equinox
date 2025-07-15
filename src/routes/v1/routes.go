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
<<<<<<< HEAD
	SetupSpacesRoutes(versionRouter)
	SetupInvitesRoutes(versionRouter)
=======
	SetupPortalRoutes(versionRouter)
>>>>>>> 51ab100ae19eff33e3733c6032e4d71977faaa27
}
