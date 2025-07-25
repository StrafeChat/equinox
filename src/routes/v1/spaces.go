package routes_v1

import (
	bots "github.com/StrafeChat/equinox/src/handlers/v1/bots"
	spaces "github.com/StrafeChat/equinox/src/handlers/v1/spaces"
	"github.com/StrafeChat/equinox/src/middleware"
	"github.com/gofiber/fiber/v3"
)

func SetupSpacesRoutes(versionRouter *fiber.Group) {
	router := versionRouter.Group("/spaces")

	router.Use(middleware.VerifyAuth())

	// POST /spaces/ - Create a new space
	router.Post("/", spaces.CreateSpace)

	// GET /spaces/ - Get user's spaces
	router.Get("/", spaces.GetUserSpaces)

	// GET /spaces/:id - Get specific space details
	router.Get("/:id", spaces.GetSpace)

	// PATCH /spaces/:id - Update space details
	router.Patch("/:id", spaces.UpdateSpace)

	// Space Members Routes
	router.Get("/:id/members", spaces.GetSpaceMembers)
	router.Patch("/:id/members/:userId/roles", spaces.UpdateMemberRoles)
	router.Delete("/:id/members/:userId", spaces.KickMember)
	router.Delete("/:id/leave", spaces.LeaveSpace)

	// Space Roles Routes
	router.Get("/:id/roles", spaces.GetSpaceRoles)
	router.Post("/:id/roles", spaces.CreateRole)
	router.Patch("/:id/roles/:roleId", spaces.UpdateRole)
	router.Delete("/:id/roles/:roleId", spaces.DeleteRole)
	router.Post("/:id/members/:userId/roles/:roleId", spaces.AddRoleToMember)
	router.Delete("/:id/members/:userId/roles/:roleId", spaces.RemoveRoleFromMember)

	// Space Invites Routes
	router.Get("/:id/invites", spaces.GetSpaceInvites)
	router.Post("/:id/invites", spaces.CreateInvite)
	router.Delete("/:id/invites/:inviteId", spaces.DeleteInvite)

	// Space Rooms Routes
	router.Post("/:id/rooms", spaces.CreateSpaceRoom)

	// Space Bots Routes
	router.Post("/:id/bots", bots.AddBotToSpace)
	router.Delete("/:id/bots/:botId", bots.RemoveBotFromSpace)
	router.Get("/:id/bots", bots.GetSpaceBots)

	// Permission checking route
	router.Get("/:id/permissions/:permission", spaces.CheckUserPermission)

	versionRouter.Get("/permissions", spaces.GetAvailablePermissions)
}
