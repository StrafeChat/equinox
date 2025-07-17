package handlers_v1

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/gocql/gocql"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v2/qb"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/helpers"
	"github.com/StrafeChat/equinox/src/types"
)

func LoginPost(c fiber.Ctx) error {
	body := new(types.LoginBody)

	if err := c.Bind().Body(body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid JSON format."})
	}

	if body.Email == "" || body.Password == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "You need to provide an email and password."})
	}

	userByEmail := models.UserByEmailTable.SelectBuilder().
		Columns("id").
		Where(qb.Eq("email")).
		Limit(1)

	userByEmailQuery := userByEmail.Query(*database.Session).
		BindStruct(models.UserByEmail{
			Email: body.Email,
		})

	var emailUser models.UserByEmail
	err := userByEmailQuery.GetRelease(&emailUser)
	if err != nil {
		if errors.Is(err, gocql.ErrNotFound) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "Invalid email or password."})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "An error occurred while checking for the user."})
	}

	userById := models.UserTable.SelectBuilder().
		Columns("id", "password", "verified_email").
		Where(qb.Eq("id")).
		Limit(1)

	userByIdQuery := userById.Query(*database.Session).
		BindStruct(models.User{
			ID: emailUser.ID,
		})

	var user models.User
	err1 := userByIdQuery.GetRelease(&user)
	if err1 != nil {
		if errors.Is(err1, gocql.ErrNotFound) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "User not found."})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "An error occurred while checking for the user."})
	}

	if user.Password == nil || *user.Password == "" {
		fmt.Printf("User %s found without a password, possible data corruption or bot account.", user.ID)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Account configuration error."})
	}

	err = bcrypt.CompareHashAndPassword([]byte(*user.Password), []byte(body.Password))
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Invalid email or password."})
	}

	// Check if email verification is enabled and if user's email is verified
	emailVerificationEnabled := strings.ToLower(os.Getenv("ENABLE_EMAIL_VERIFICATION")) == "true"
	if emailVerificationEnabled && !user.VerifiedEmail {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "Please verify your email address before logging in. Check your email for a verification link.",
		})
	}

	token := helpers.GenerateSessionToken()

	encryptedIP, err := helpers.Encrypt([]byte(c.Locals("original_ip").(string)))
	if err != nil {
		fmt.Printf("An error occurred while encrypting a user's IP: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "An internal server error occurred while creating the session.",
		})
	}

	// fmt.Println(encryptedIP, c.IP())

	// Log the user ID for the session
	fmt.Printf("Creating session for user ID: %s\n", user.ID)

	session := models.Session{
		Token:     token,
		UserId:    user.ID,
		UserAgent: c.Get("User-Agent"),
		Trusted:   false,
		IP:        encryptedIP,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(64 * time.Hour),
	}

	/*_ Create a session and return the cookie.  _*/
	// Insert into sessions table
	createSessionQuery := models.SessionTable.InsertBuilder()
	sessionTableQuery := createSessionQuery.Query(*database.Session).
		BindStruct(session)

	if err := sessionTableQuery.ExecRelease(); err != nil {
		fmt.Printf("An error occurred while creating a session: %v\nQuery: %v", err, sessionTableQuery)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "An internal server error occurred.",
		})
	}

	// Insert into sessions_by_user table
	sessionByUser := models.SessionByUser{
		UserId:    session.UserId,
		Token:     session.Token,
		IP:        session.IP,
		UserAgent: session.UserAgent,
		Trusted:   session.Trusted,
		CreatedAt: session.CreatedAt,
		ExpiresAt: session.ExpiresAt,
	}

	createSessionByUserQuery := models.SessionByUserTable.InsertBuilder()
	sessionByUserTableQuery := createSessionByUserQuery.Query(*database.Session).
		BindStruct(sessionByUser)

	if err := sessionByUserTableQuery.ExecRelease(); err != nil {
		fmt.Printf("An error occurred while creating a session_by_user: %v\nQuery: %v", err, sessionByUserTableQuery)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "An internal server error occurred.",
		})
	}

	// Set response headers
	c.Set("Access-Control-Expose-Headers", "X-Session-Token")
	c.Set("X-Session-Token", token)

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"token":   token,
		"message": "Login successful.",
	})
}
