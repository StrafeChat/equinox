package routes

func SetupRoutes(d Deps) {
	SetupMiscRoutes(d)
	SetupAuthRoutes(d)
	SetupUsersRoutes(d)
	SetupRoomsRoutes(d)
	SetupDevicesRoutes(d)
	SetupMessagesRoutes(d)
}
