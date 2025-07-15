package services

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/resend/resend-go/v2"
)

type EmailService struct{}

func NewEmailService() *EmailService {
	return &EmailService{}
}

func (es *EmailService) SendVerificationEmail(userID, email, username string) error {
	// Generate verification token
	token, err := generateVerificationToken()
	if err != nil {
		return fmt.Errorf("failed to generate verification token: %w", err)
	}

	// Store verification token in database
	verificationToken := models.VerificationToken{
		Token:     token,
		UserID:    userID,
		Email:     email,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(24 * time.Hour), // Token expires in 24 hours
	}

	query := models.VerificationTokenTable.InsertBuilder().Query(*database.Session).
		BindStruct(verificationToken)

	if err := query.ExecRelease(); err != nil {
		return fmt.Errorf("failed to store verification token: %w", err)
	}

	// Create verification URL pointing to client
	clientDomain := os.Getenv("DOMAIN")
	verificationURL := fmt.Sprintf("%s/verify-email?token=%s", clientDomain, token)

	// Send actual email using Resend
	return es.sendEmailWithResend(email, username, verificationURL)
}

func (es *EmailService) sendEmailWithResend(toEmail, username, verificationURL string) error {
	// Get Resend API key from environment
	apiKey := os.Getenv("RESEND_API_KEY")
	if apiKey == "" {
		// Fallback to logging if no API key is configured
		log.Printf("RESEND_API_KEY not configured, logging email instead:")
		log.Printf("To: %s", toEmail)
		log.Printf("Subject: Verify your Strafe Chat account")
		log.Printf("Body: Hi %s,\n\nPlease click the following link to verify your email address:\n%s\n\nThis link will expire in 24 hours.\n\nBest regards,\nThe Strafe Chat Team", username, verificationURL)
		return nil
	}

	// Get from email from environment
	fromEmail := os.Getenv("FROM_EMAIL")
	if fromEmail == "" {
		fromEmail = "noreply@strafe.chat" // Default fallback
	}

	// Initialize Resend client
	client := resend.NewClient(apiKey)

	// Create email content
	subject := "Verify your Strafe Chat account"
	htmlContent := fmt.Sprintf(`
		<html>
		<body>
			<h2>Welcome to Strafe Chat, %s!</h2>
			<p>Please click the button below to verify your email address:</p>
			<a href="%s" style="background-color: #007bff; color: white; padding: 12px 24px; text-decoration: none; border-radius: 4px; display: inline-block;">Verify Email</a>
			<p>Or copy and paste this link into your browser:</p>
			<p><a href="%s">%s</a></p>
			<p>This link will expire in 24 hours.</p>
			<p>Best regards,<br>The Strafe Chat Team</p>
		</body>
		</html>
	`, username, verificationURL, verificationURL, verificationURL)

	textContent := fmt.Sprintf(`Hi %s,

Please click the following link to verify your email address:
%s

This link will expire in 24 hours.

Best regards,
The Strafe Chat Team`, username, verificationURL)

	// Send email
	params := &resend.SendEmailRequest{
		From:    fromEmail,
		To:      []string{toEmail},
		Subject: subject,
		Html:    htmlContent,
		Text:    textContent,
	}

	_, err := client.Emails.Send(params)
	if err != nil {
		return fmt.Errorf("failed to send verification email: %w", err)
	}

	log.Printf("Verification email sent successfully to %s", toEmail)
	return nil
}

func generateVerificationToken() (string, error) {
	bytes := make([]byte, 32)
	_, err := rand.Read(bytes)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}
