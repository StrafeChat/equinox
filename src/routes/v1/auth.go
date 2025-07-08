package routes_v1

import (
	"github.com/gofiber/fiber/v3"

	authHandler "github.com/StrafeChat/equinox/src/handlers/v1/auth"
)

func SetupAuthRoutes(verisonRouter *fiber.Group) {
	router := verisonRouter.Group("/auth")

	router.Post("/login", authHandler.LoginPost)
	router.Post("/register", authHandler.RegisterPost)
	router.Get("/verify-email", authHandler.VerifyEmailGet)
	router.Post("/resend-verification", authHandler.ResendVerificationPost)
	router.Post("/password-reset", authHandler.PasswordResetRequestPost)
	router.Post("/password-reset/verify", authHandler.PasswordResetVerifyPost)
	router.Post("/password-reset/complete", authHandler.PasswordResetCompletePost)
}
