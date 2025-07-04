package routes_v1

import (
	handlers_v1_auth "github.com/StrafeChat/equinox/src/handlers/v1/auth"
	handlers_v1_rooms "github.com/StrafeChat/equinox/src/handlers/v1/rooms"
	handlers_v1_users "github.com/StrafeChat/equinox/src/handlers/v1/users"
	"github.com/StrafeChat/equinox/src/middleware"
	"github.com/gofiber/fiber/v3"
)

func SetupUsersRoutes(verisonRouter *fiber.Group) {
	router := verisonRouter.Group("/users")

	router.Use(middleware.VerifyAuth())

	router.Get("/@me", handlers_v1_users.MeGet)
	router.Post("/bulk", handlers_v1_users.BulkGetUsers)
	router.Get("/:id", handlers_v1_users.GetUser)
	router.Post("/@me/relationships", handlers_v1_users.RelationshipsPost)
	router.Put("/@me/relationships/:id", handlers_v1_users.RelationshipsPut)
	router.Delete("/@me/relationships/:id", handlers_v1_users.RelationshipsDelete)
	router.Post("/@me/rooms", handlers_v1_rooms.CreateRoom)
	router.Get("/@me/rooms", handlers_v1_users.GetUserRooms)
	router.Patch("/@me/avatar", handlers_v1_users.UpdateAvatar)
	router.Patch("/@me/banner", handlers_v1_users.UpdateBanner)
	router.Patch("/@me/bio", handlers_v1_users.UpdateBio)
	router.Patch("/@me/status", handlers_v1_users.UpdateStatus)
	router.Patch("/@me/profile", handlers_v1_users.UpdateProfile)

	// Session management routes
	router.Get("/@me/sessions", handlers_v1_auth.GetUserSessions)
	router.Delete("/@me/sessions/:token", handlers_v1_auth.RevokeSession)
	router.Delete("/@me/sessions", handlers_v1_auth.RevokeAllSessions)
}
