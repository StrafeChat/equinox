package routes_v1

import (
	handlers_v1 "github.com/StrafeChat/equinox/src/handlers/v1/users"
	"github.com/StrafeChat/equinox/src/middleware"
	"github.com/gofiber/fiber/v3"
)

func SetupUsersRoutes(app *fiber.App) {
	router := app.Group("/users")

	router.Use(middleware.VerifyAuth())

	router.Get("/@me", handlers_v1.MeGet)
	router.Get("/:id", handlers_v1.GetUser)
	router.Post("/@me/relationships", handlers_v1.RelationshipsPost)
	router.Put("/@me/relationships/:id", handlers_v1.RelationshipsPut)
	router.Delete("/@me/relationships/:id", handlers_v1.RelationshipsDelete)
}
