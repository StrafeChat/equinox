package handlers_v1

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gocql/gocql"
	"github.com/gofiber/fiber/v3"
	"github.com/mssola/user_agent"
	"github.com/resend/resend-go/v2"
	"github.com/scylladb/gocqlx/v3/qb"
	"golang.org/x/crypto/bcrypt"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/middleware"
	"github.com/StrafeChat/equinox/src/types"
	"github.com/StrafeChat/equinox/src/validation"
)

// GenerateResetCode generates a random 6-character code for password reset
func GenerateResetCode() string {
	b := make([]byte, 4)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)[:6]
}

// getDeviceInfo extracts device information from user agent string
func getDeviceInfo(userAgentString string) string {
	ua := user_agent.New(userAgentString)
	browser, version := ua.Browser()
	os := ua.OS()
	deviceType := "Desktop"
	if ua.Mobile() {
		deviceType = "Mobile"
	}

	return fmt.Sprintf("%s on %s (%s %s)", deviceType, os, browser, version)
}

func getCityFromIP(ipAddress string) string {
	// Skip for private/local IPs
	if strings.HasPrefix(ipAddress, "127.") || strings.HasPrefix(ipAddress, "192.168.") ||
		strings.HasPrefix(ipAddress, "10.") || strings.HasPrefix(ipAddress, "172.") {
		return "Local Network"
	}

	// Use a free IP geolocation API
	apiURL := "https://ipapi.co/" + ipAddress + "/json/"
	resp, err := http.Get(apiURL)
	if err != nil {
		log.Printf("Error fetching IP data: %v", err)
		return "Unknown Location"
	}
	defer resp.Body.Close()

	// Parse response
	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("Error decoding IP API response: %v", err)
		return "Unknown Location"
	}

	city, cityExists := result["city"].(string)
	region, regionExists := result["region"].(string)
	country, countryExists := result["country_name"].(string)

	// Construct location string
	if !cityExists && !regionExists && !countryExists {
		return "Unknown Location"
	}

	locationParts := []string{}
	if cityExists {
		locationParts = append(locationParts, city)
	}
	if regionExists {
		locationParts = append(locationParts, region)
	}
	if countryExists {
		locationParts = append(locationParts, country)
	}

	return strings.Join(locationParts, ", ")
}

// PasswordResetRequestPost handles the request to reset a password
func PasswordResetRequestPost(c fiber.Ctx) error {
	body := new(types.PasswordResetRequestBody)

	if err := c.Bind().Body(body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid JSON format."})
	}

	if body.Email == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "You need to provide an email."})
	}

	if !validation.IsValidEmail(body.Email) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid email format."})
	}

	// Find user by email
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
			// Don't reveal if email exists or not for security reasons
			return c.Status(fiber.StatusOK).JSON(fiber.Map{"message": "If your email is registered, you will receive a password reset link."})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "An error occurred while checking for the user."})
	}

	// Check for existing reset codes for this user
	log.Printf("Building query to check for existing reset codes with user_id: %s", emailUser.ID)
	// Use the PasswordResetByUserTable which has user_id as the partition key
	existingResetQuery := models.PasswordResetByUserTable.SelectBuilder().
		Columns("code", "created_at", "user_id")

	log.Printf("Preparing query execution for existing reset codes")
	existingResetQueryExec := existingResetQuery.Query(*database.Session)
	log.Printf("Binding user_id: %s to find existing reset codes", emailUser.ID)
	existingResetQueryExec = existingResetQueryExec.BindMap(qb.M{
		"user_id": emailUser.ID,
	})

	log.Printf("Checking for existing reset codes for user ID: %s", emailUser.ID)
	log.Printf("Query structure: %+v", existingResetQuery)

	log.Printf("Query execution prepared with binding: %+v", qb.M{
		"user_id": emailUser.ID,
	})

	var existingResets []models.PasswordReset
	log.Printf("Executing SelectRelease to get existing reset codes")
	err = existingResetQueryExec.SelectRelease(&existingResets)
	if err != nil {
		if errors.Is(err, gocql.ErrNotFound) {
			log.Printf("No existing reset codes found for user ID: %s", emailUser.ID)
			// Continue with empty slice
		} else {
			log.Printf("Error checking existing reset codes: %v", err)
			log.Printf("Query details: %+v", existingResetQuery)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "An error occurred while processing your request."})
		}
	} else {
		log.Printf("Found %d existing reset codes for user ID: %s", len(existingResets), emailUser.ID)
	}

	// Check if there are too many recent reset requests
	if len(existingResets) > 0 {
		// Find the most recent reset request
		var mostRecent time.Time
		for _, reset := range existingResets {
			if reset.CreatedAt.After(mostRecent) {
				mostRecent = reset.CreatedAt
			}
		}

		// If the most recent request is less than 15 minutes old, reject the new request
		if time.Since(mostRecent) < 15*time.Minute {
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{"error": "Too many password reset requests. Please try again later."})
		}

		// If there are more than 3 existing reset codes, delete the oldest ones
		if len(existingResets) >= 3 {
			log.Printf("Cleaning up old reset codes for user %s", emailUser.ID)
			// Sort by creation time (oldest first)
			sort.Slice(existingResets, func(i, j int) bool {
				return existingResets[i].CreatedAt.Before(existingResets[j].CreatedAt)
			})

			// Delete all but the most recent one
			for i := 0; i < len(existingResets)-1; i++ {
				deleteQuery := models.PasswordResetTable.DeleteBuilder().
					Where(qb.Eq("code"))

				deleteQueryExec := deleteQuery.Query(*database.Session).
					BindMap(qb.M{
						"code": existingResets[i].Code,
					})

				if err := deleteQueryExec.ExecRelease(); err != nil {
					log.Printf("Error deleting old reset code: %v", err)
					// Continue anyway
				}
			}
		}
	}

	// Generate a reset code
	resetCode := GenerateResetCode()

	// Get IP address and user agent
	ipAddress := c.Locals("original_ip").(string)
	userAgentString := c.Get("User-Agent")

	// Get device information and location
	deviceInfo := getDeviceInfo(userAgentString)
	location := getCityFromIP(ipAddress)

	// Set expiration time (1 hour from now)
	createdAt := time.Now()
	expiresAt := createdAt.Add(time.Hour)

	// Store the reset code in the database
	resetData := models.PasswordReset{
		UserId:    emailUser.ID,
		Code:      resetCode,
		Ip:        ipAddress,
		UserAgent: userAgentString,
		CreatedAt: createdAt,
		ExpiresAt: expiresAt,
	}

	// Insert into the main password_resets table
	query := models.PasswordResetTable.InsertBuilder().Query(*database.Session).
		BindStruct(resetData)

	if err := query.ExecRelease(); err != nil {
		log.Printf("Error storing password reset code in main table: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "An error occurred while processing your request."})
	}

	// Insert into the password_resets_by_user table
	userQuery := models.PasswordResetByUserTable.InsertBuilder().Query(*database.Session).
		BindStruct(resetData)

	if err := userQuery.ExecRelease(); err != nil {
		log.Printf("Error storing password reset code in by_user table: %v", err)
		// Continue anyway as the main table insert succeeded
	}

	// Insert into the password_resets_by_expiration table
	expirationQuery := models.PasswordResetByExpirationTable.InsertBuilder().Query(*database.Session).
		BindStruct(resetData)

	if err := expirationQuery.ExecRelease(); err != nil {
		log.Printf("Error storing password reset code in by_expiration table: %v", err)
		// Continue anyway as the main table insert succeeded
	}

	// Send email with reset code
	apiKey := os.Getenv("RESEND_API_KEY")
	if apiKey == "" {
		log.Printf("RESEND_API_KEY environment variable is not set")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Email service configuration error."})
	}

	client := resend.NewClient(apiKey)

	// Get the frontend URL from environment or use default
	frontendURL := os.Getenv("FRONTEND_URL")
	if frontendURL == "" {
		frontendURL = "https://alpha.strafechat.dev"
	}

	// Create reset link
	resetLink := fmt.Sprintf("%s/password-reset/verify?userId=%s&code=%s", frontendURL, emailUser.ID, resetCode)

	// Format the time in a user-friendly way
	timeFormatted := createdAt.Format("Monday, January 2, 2006 at 3:04 PM MST")

	// Create HTML email content with enhanced styling and security information
	htmlContent := fmt.Sprintf(`
	<div style="font-family: Arial, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px; border: 1px solid #e0e0e0; border-radius: 8px;">
		<div style="text-align: center; margin-bottom: 20px;">
			<h1 style="color: #21c35e; margin-bottom: 5px;">Strafe Chat</h1>
			<div style="height: 4px; background-color: #21c35e; width: 100px; margin: 0 auto;"></div>
		</div>

		<h2 style="color: #21c35e; margin-top: 30px;">Password Reset Request</h2>
		
		<p>Hello,</p>
		<p>We received a request to reset your Strafe Chat password. <strong>If you didn't make this request, please ignore this email or contact support immediately.</strong></p>
		
		<div style="background-color: #f9f9f9; border-left: 4px solid #21c35e; padding: 15px; margin: 20px 0;">
			<p style="margin: 0; font-weight: bold;">Security Information:</p>
			<p style="margin: 5px 0;">• Request made on: %s</p>
			<p style="margin: 5px 0;">• Device: %s</p>
			<p style="margin: 5px 0;">• Location: %s</p>
			<p style="margin: 5px 0;">• IP Address: %s</p>
		</div>

		<p>Your password reset code is:</p>
		<div style="background-color: #f0f0f0; padding: 15px; text-align: center; font-size: 24px; letter-spacing: 5px; font-family: monospace; border-radius: 5px; margin: 20px 0;">
			<strong>%s</strong>
		</div>

		<p>Or click the button below to reset your password:</p>
		<div style="text-align: center; margin: 30px 0;">
			<a href="%s" style="display: inline-block; background-color: #21c35e; color: white; padding: 12px 25px; text-decoration: none; border-radius: 5px; font-weight: bold; font-size: 16px;">Reset Password</a>
		</div>

		<p>This link and code will expire in 1 hour.</p>

		<p style="margin-top: 20px;">If you're having trouble clicking the button, copy and paste the URL below into your web browser:</p>
		<p style="word-break: break-all; background-color: #f9f9f9; padding: 10px; border-radius: 5px; font-size: 14px;">%s</p>

		<div style="margin-top: 40px; padding-top: 20px; border-top: 1px solid #e0e0e0; color: #666;">
			<p>Best regards,<br>The Strafe Chat Team</p>
			<p style="font-size: 12px; color: #999;">If you didn't request a password reset, you can safely ignore this email.</p>
		</div>
	</div>
	`, timeFormatted, deviceInfo, location, ipAddress, resetCode, resetLink, resetLink)

	params := &resend.SendEmailRequest{
		From:    "Strafe Chat <no-reply@strafe.chat>",
		To:      []string{body.Email},
		Subject: "Password Reset Request",
		Html:    htmlContent,
	}

	_, err = client.Emails.Send(params)
	if err != nil {
		log.Printf("Error sending password reset email: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to send password reset email."})
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{"message": "If your email is registered, you will receive a password reset link."})
}

// PasswordResetVerifyPost verifies a password reset code
func PasswordResetVerifyPost(c fiber.Ctx) error {
	body := new(types.PasswordResetVerifyBody)

	log.Printf("[PasswordResetVerify] Received request from IP: %s, User-Agent: %s", middleware.GetRealIP(c), c.Get("User-Agent"))

	if err := c.Bind().Body(body); err != nil {
		log.Printf("[PasswordResetVerify] Error parsing request body: %v", err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid JSON format."})
	}

	log.Printf("[PasswordResetVerify] Request body: Code=%s, UserId=%s", body.Code, body.UserId)

	if body.Code == "" {
		log.Printf("[PasswordResetVerify] Error: Code is required")
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Code is required."})
	}

	// Check if the reset code exists and is valid
	log.Printf("[PasswordResetVerify] Querying password_resets table with code: %s", body.Code)
	resetQuery := models.PasswordResetTable.SelectBuilder().
		Where(qb.Eq("code")).
		Limit(1)

	resetQueryExec := resetQuery.Query(*database.Session).
		BindMap(qb.M{
			"code": body.Code,
		})

	var resetData models.PasswordReset
	err := resetQueryExec.GetRelease(&resetData)
	if err != nil {
		log.Printf("[PasswordResetVerify] Database error: %v", err)
		if errors.Is(err, gocql.ErrNotFound) {
			log.Printf("[PasswordResetVerify] Reset code not found: %s", body.Code)
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid or expired password reset code."})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "An error occurred while verifying the reset code."})
	}

	// Code is already verified since we queried by it as primary key
	// No need to compare again
	log.Printf("[PasswordResetVerify] Reset code verified successfully. Associated user ID: %s", resetData.UserId)

	return c.Status(fiber.StatusOK).JSON(fiber.Map{"valid": true})
}

// PasswordResetCompletePost completes the password reset process
func PasswordResetCompletePost(c fiber.Ctx) error {
	body := new(types.PasswordResetCompleteBody)

	if err := c.Bind().Body(body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid JSON format."})
	}

	if body.Code == "" || body.NewPassword == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Code and new password are required."})
	}

	// Check if the reset code exists and is valid
	resetQuery := models.PasswordResetTable.SelectBuilder().
		Where(qb.Eq("code")).
		Limit(1)

	resetQueryExec := resetQuery.Query(*database.Session).
		BindMap(qb.M{
			"code": body.Code,
		})

	var resetData models.PasswordReset
	err := resetQueryExec.GetRelease(&resetData)
	if err != nil {
		if errors.Is(err, gocql.ErrNotFound) {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid or expired password reset code."})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "An error occurred while verifying the reset code."})
	}

	// Code is already verified since we queried by it as primary key
	// No need to compare again

	// Hash the new password
	passwordHashingSalt, err := bcrypt.GenerateFromPassword([]byte(body.NewPassword), 10)
	if err != nil {
		log.Printf("Error hashing password: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "An error occurred while processing your request."})
	}

	// Update the user's password
	passwordStr := string(passwordHashingSalt)
	updateQuery := models.UserTable.UpdateBuilder().
		Set("password").
		Where(qb.Eq("id"))

	updateQueryExec := updateQuery.Query(*database.Session).
		BindMap(qb.M{
			"id":       resetData.UserId,
			"password": passwordStr,
		})

	if err := updateQueryExec.ExecRelease(); err != nil {
		log.Printf("Error updating password: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "An error occurred while updating your password."})
	}

	// Delete all reset codes for this user
	// First, delete the specific code used for this reset
	// Get the reset data to have information for deleting from secondary tables
	var resetDataForDeletion models.PasswordReset
	getResetQuery := models.PasswordResetTable.SelectBuilder().
		Where(qb.Eq("code")).
		Limit(1)

	getResetQueryExec := getResetQuery.Query(*database.Session).
		BindMap(qb.M{
			"code": body.Code,
		})

	err = getResetQueryExec.GetRelease(&resetDataForDeletion)
	if err != nil {
		log.Printf("Error getting reset data for deletion: %v", err)
		// Continue anyway
	}

	// Delete from the main password_resets table
	deleteCodeQuery := models.PasswordResetTable.DeleteBuilder().
		Where(qb.Eq("code"))

	deleteCodeQueryExec := deleteCodeQuery.Query(*database.Session).
		BindMap(qb.M{
			"code": body.Code,
		})

	if err := deleteCodeQueryExec.ExecRelease(); err != nil {
		log.Printf("Error deleting reset code from main table: %v", err)
		// Continue anyway as the password has been updated
	}

	// Delete from the password_resets_by_user table if we have the data
	if resetDataForDeletion.UserId != "" && !resetDataForDeletion.CreatedAt.IsZero() {
		deleteByUserQuery := models.PasswordResetByUserTable.DeleteBuilder().
			Where(qb.Eq("user_id"), qb.Eq("created_at"))

		deleteByUserQueryExec := deleteByUserQuery.Query(*database.Session).
			BindMap(qb.M{
				"user_id":    resetDataForDeletion.UserId,
				"created_at": resetDataForDeletion.CreatedAt,
			})

		if err := deleteByUserQueryExec.ExecRelease(); err != nil {
			log.Printf("Error deleting reset code from by_user table: %v", err)
			// Continue anyway
		}
	}

	// Delete from the password_resets_by_expiration table if we have the data
	if !resetDataForDeletion.ExpiresAt.IsZero() {
		deleteByExpirationQuery := models.PasswordResetByExpirationTable.DeleteBuilder().
			Where(qb.Eq("expires_at"), qb.Eq("code"))

		deleteByExpirationQueryExec := deleteByExpirationQuery.Query(*database.Session).
			BindMap(qb.M{
				"expires_at": resetDataForDeletion.ExpiresAt,
				"code":       body.Code,
			})

		if err := deleteByExpirationQueryExec.ExecRelease(); err != nil {
			log.Printf("Error deleting reset code from by_expiration table: %v", err)
			// Continue anyway
		}
	}

	// Then, find and delete any other reset codes for this user
	log.Printf("Building query to find other reset codes for user ID: %s", resetData.UserId)
	// Use the PasswordResetByUserTable which has user_id as the partition key
	otherCodesQuery := models.PasswordResetByUserTable.SelectBuilder().
		Where(qb.Eq("user_id"))

	log.Printf("Preparing query execution for other reset codes")
	otherCodesQueryExec := otherCodesQuery.Query(*database.Session)
	log.Printf("Binding user_id: %s to find other reset codes", resetData.UserId)
	otherCodesQueryExec = otherCodesQueryExec.BindMap(qb.M{
		"user_id": resetData.UserId,
	})
	log.Printf("Query execution prepared for finding other reset codes")

	var otherResets []models.PasswordReset
	log.Printf("Executing SelectRelease to find other reset codes")
	err = otherCodesQueryExec.SelectRelease(&otherResets)
	if err != nil {
		if errors.Is(err, gocql.ErrNotFound) {
			log.Printf("No other reset codes found for user ID: %s", resetData.UserId)
			// Continue with empty slice
		} else {
			log.Printf("Error finding other reset codes: %v", err)
			log.Printf("Query details: %+v", otherCodesQueryExec)
			// Continue anyway as the password has been updated
		}
	} else {
		log.Printf("Found %d other reset codes for user ID: %s", len(otherResets), resetData.UserId)
	}

	// Delete any other reset codes found
	for _, reset := range otherResets {
		deleteOtherQuery := models.PasswordResetTable.DeleteBuilder().
			Where(qb.Eq("code"))

		deleteOtherQueryExec := deleteOtherQuery.Query(*database.Session).
			BindMap(qb.M{
				"code": reset.Code,
			})

		if err := deleteOtherQueryExec.ExecRelease(); err != nil {
			log.Printf("Error deleting other reset code: %v", err)
			// Continue anyway
		}
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{"message": "Your password has been successfully reset."})
}
