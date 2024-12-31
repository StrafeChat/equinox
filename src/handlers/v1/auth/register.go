package handlers_v1

import (
	"errors"
	"log"
	"os"
	"strconv"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/gocql/gocql"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v3/qb"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/helpers"
	"github.com/StrafeChat/equinox/src/types"
	"github.com/StrafeChat/equinox/src/validation"
)

func RegisterPost(c fiber.Ctx) error {
	body := new(types.RegisterBody)

	if err := c.Bind().Body(body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid JSON format. Example: {\"email\": \"user@example.com\", \"username\": \"user123\", \"discriminator\": \"1234\", \"date_of_birth\": \"2000-01-01T00:00:00Z\", \"password\": \"yourpassword\"}"})
	}

	if err := validateRegistrationInput(body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}

	type checkResult struct {
		exists bool
		err    error
	}
	userExistsCh := make(chan checkResult)
	emailExistsCh := make(chan checkResult)

	/*_ Check user existence concurrently _*/
	go func() {
		exists, err := checkUserExists(body.Username, body.Discriminator)
		userExistsCh <- checkResult{exists, err}
	}()

	/*_ Check email existence concurrently _*/
	go func() {
		exists, err := checkEmailExists(body.Email)
		emailExistsCh <- checkResult{exists, err}
	}()

	userResult := <-userExistsCh
	if userResult.err != nil {
		log.Printf("An error occurred while checking the database: %v", userResult.err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "An error occurred while checking for existing user.",
		})
	}
	if userResult.exists {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error": "A user with the same username and discriminator already exists.",
		})
	}

	emailResult := <-emailExistsCh
	if emailResult.err != nil {
		log.Printf("An error occurred while checking the database for email: %v", emailResult.err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "An error occurred while checking for existing email.",
		})
	}
	if emailResult.exists {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error": "A user with the same email already exists.",
		})
	}

	/*_ Hash password _*/
	passwordHashCh := make(chan struct {
		hash string
		err  error
	})
	go func() {
		passwordHashingSalt, err := strconv.Atoi(os.Getenv("PASSWORD_HASHING_SALT"))
		if err != nil {
			passwordHashCh <- struct {
				hash string
				err  error
			}{"", err}
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), passwordHashingSalt)
		passwordHashCh <- struct {
			hash string
			err  error
		}{string(hash), err}
	}()

	userId := helpers.GenerateUserID()

	passwordResult := <-passwordHashCh
	if passwordResult.err != nil {
		log.Printf("An error occurred while hashing the password: %v", passwordResult.err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "An error occurred while creating the user.",
		})
	}

	errCh := make(chan error, 3)

	go func() {
		err := insertUserByEmail(body.Email, userId.String())
		errCh <- err
	}()

	go func() {
		err := insertUserByUsernameAndDiscriminator(body.Username, body.Discriminator, userId.String())
		errCh <- err
	}()

	newUser := createUserModel(body, userId.String(), passwordResult.hash)
	go func() {
		err := insertUser(newUser)
		errCh <- err
	}()

	for i := 0; i < 3; i++ {
		if err := <-errCh; err != nil {
			log.Printf("Error in database operation: %v", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "An error occurred while creating the user.",
			})
		}
	}

	token := helpers.GenerateSessionToken()
	encryptedIP, err := helpers.Encrypt([]byte(c.IP()))
	if err != nil {
		log.Printf("An error occurred while encrypting a user's IP: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "An internal server error occurred while creating the session.",
		})
	}

	session := models.Session{
		Token:     token,
		UserId:    userId.String(),
		UserAgent: c.Get("User-Agent"),
		Trusted:   false,
		IP:        encryptedIP,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(64 * time.Hour),
	}

	if err := insertSession(session); err != nil {
		log.Printf("An error occurred while creating a session: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "An internal server error occurred.",
		})
	}

	c.Cookie(&fiber.Cookie{
		Name:     "sc_session",
		Value:    token,
		Expires:  time.Now().Add(64 * time.Hour),
		HTTPOnly: true,
		Secure:   true,
		SameSite: "Strict",
	})

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"message": "Registration successful.", "token": token})
}

func validateRegistrationInput(body *types.RegisterBody) error {
	if body.Username == "" || body.Discriminator == 0 || body.Password == "" || body.DateOfBirth.IsZero() || body.Email == "" {
		return errors.New("you must provide all of the required information: email, username, discriminator, date_of_birth & password")
	}

	if len(body.Username) < 3 || len(body.Username) > 20 {
		return errors.New("username must be between 3 and 20 characters")
	}

	if !validation.IsValidEmail(body.Email) {
		return errors.New("invalid email format")
	}

	if !validation.DoBAbove13(body.DateOfBirth) {
		return errors.New("you must be at least 13 years old to use Strafe")
	}

	if body.Discriminator < 1 || body.Discriminator > 9999 {
		return errors.New("discriminator must be a number between 1 and 9999")
	}

	return nil
}

func checkUserExists(username string, discriminator int) (bool, error) {
	existingUserQuery := models.UserByUsernameAndDiscriminatorTable.SelectBuilder().
		Columns("id").
		Where(qb.Eq("username"), qb.Eq("discriminator")).
		Limit(1)

	q := existingUserQuery.Query(*database.Session).BindMap(qb.M{
		"username":      username,
		"discriminator": discriminator,
	})

	err := q.GetRelease(&models.UserByUsernameAndDiscriminator{})
	if err == nil {
		return true, nil
	}
	if errors.Is(gocql.ErrNotFound, err) {
		return false, nil
	}
	return false, err
}

func checkEmailExists(email string) (bool, error) {
	existingEmailQuery := models.UserByEmailTable.SelectBuilder().
		Columns("id").
		Where(qb.Eq("email")).
		Limit(1)

	emailQuery := existingEmailQuery.Query(*database.Session).BindMap(qb.M{
		"email": email,
	})

	err := emailQuery.GetRelease(&models.UserByEmail{})
	if err == nil {
		return true, nil
	}
	if errors.Is(err, gocql.ErrNotFound) {
		return false, nil
	}
	return false, err
}

func insertUserByEmail(email, userId string) error {
	query := models.UserByEmailTable.InsertBuilder().Query(*database.Session).
		BindStruct(models.UserByEmail{
			Email: email,
			ID:    userId,
		})
	return query.ExecRelease()
}

func insertUserByUsernameAndDiscriminator(username string, discriminator int, userId string) error {
	query := models.UserByUsernameAndDiscriminatorTable.InsertBuilder().Query(*database.Session).
		BindStruct(models.UserByUsernameAndDiscriminator{
			Username:      username,
			Discriminator: discriminator,
			ID:            userId,
		})
	return query.ExecRelease()
}

func createUserModel(body *types.RegisterBody, userId, passwordHash string) models.User {
	return models.User{
		ID:            userId,
		Email:         &body.Email,
		Avatar:        nil,
		Banner:        nil,
		Password:      &passwordHash,
		Username:      body.Username,
		Discriminator: body.Discriminator,
		DisplayName:   &body.DisplayName,
		DateOfBirth:   body.DateOfBirth,
		VerifiedEmail: false,
		Bot:           false,
		Bots:          []string{},
		System:        false,
		AboutMe:       nil,
		Bio:           nil,
		Flags:         models.UserFlags(models.EARLY_ADOPTER),
		Locale:        &body.Locale,
		AccentColor:   nil,
		Presence: models.UserPresence{
			Online:       false,
			Status:       "online",
			CustomStatus: nil,
		}.ToMap(),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
}

func insertUser(user models.User) error {
	query := models.UserTable.InsertBuilder().Query(*database.Session).
		BindStruct(user)
	return query.ExecRelease()
}

func insertSession(session models.Session) error {
	query := models.SessionTable.InsertBuilder().Query(*database.Session).
		BindStruct(session)
	return query.ExecRelease()
}
