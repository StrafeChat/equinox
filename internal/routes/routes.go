package routes

func SetupRoutes(d Deps) {
	SetupMiscRoutes(d)
	SetupAuthRoutes(d)
	SetupUsersRoutes(d)
	SetupRoomsRoutes(d)
	SetupSpacesRoutes(d)
	SetupDevicesRoutes(d)
	SetupMessagesRoutes(d)
	SetupFederationRoutes(d)
	SetupVoiceRoutes(d)
}
