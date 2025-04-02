package handlers_v1

import (
	"errors"
	"fmt"

	"github.com/gocql/gocql"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v3/qb"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/helpers"
)

// GetUserSessions retrieves all active sessions for the current user
func GetUserSessions(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	currentToken := c.Get("X-Session-Token")

	// Log the user ID we're querying for
	fmt.Printf("Querying sessions for user ID (string): %s\n", user.ID)
	// Query to get sessions by user_id using the new sessions_by_user table
	sessionsByUserQuery := models.SessionByUserTable.SelectBuilder().
		Columns("session_token", "user_id", "ip", "user_agent", "trusted", "created_at", "expires_at").
		Where(qb.Eq("user_id")).
		Query(*database.Session).
		BindMap(qb.M{"user_id": user.ID})

	var sessions []models.SessionByUser
	if err := sessionsByUserQuery.SelectRelease(&sessions); err != nil {
		fmt.Printf("Error getting sessions: %v\n", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "An error occurred while retrieving sessions.",
		})
	}

	// Check if we need to debug the session table structure
	if len(sessions) == 0 {
		// Query a sample of sessions to see what's in the table
		allSessionsQuery := models.SessionTable.SelectBuilder().
			Columns("session_token", "user_id").
			Limit(5).
			Query(*database.Session)

		var sampleSessions []models.Session
		if err := allSessionsQuery.SelectRelease(&sampleSessions); err != nil {
			fmt.Printf("Error getting sample sessions: %v\n", err)
		} else {
			fmt.Printf("Sample sessions in the database:\n")
			for i, s := range sampleSessions {
				fmt.Printf("  %d. Token: %s, User ID: %s\n", i+1, s.Token, s.UserId)
			}
		}
	}

	// Log the number of sessions found
	fmt.Printf("Found %d sessions for user ID: %s\n", len(sessions), user.ID)

	// Format the response
	response := make([]fiber.Map, 0, len(sessions))
	for _, session := range sessions {
		// Decrypt IP if it's encrypted
		decryptedIP := session.IP
		if decrypted, err := helpers.Decrypt(session.IP); err == nil {
			decryptedIP = string(decrypted)
		}

		response = append(response, fiber.Map{
			"token":      session.Token,
			"user_id":    session.UserId,
			"ip":         decryptedIP,
			"user_agent": session.UserAgent,
			"trusted":    session.Trusted,
			"created_at": session.CreatedAt,
			"expires_at": session.ExpiresAt,
			"current":    session.Token == currentToken,
		})
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"sessions": response,
	})
}

// RevokeSession revokes a specific session by token
func RevokeSession(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	currentToken := c.Get("X-Session-Token")
	tokenToRevoke := c.Params("token")

	if tokenToRevoke == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Session token is required.",
		})
	}

	// Prevent revoking the current session
	if tokenToRevoke == currentToken {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Cannot revoke the current session.",
		})
	}

	// Verify the session belongs to the user
	sessionByToken := models.SessionTable.SelectBuilder().
		Columns("user_id").
		Where(qb.Eq("session_token")).
		Limit(1)

	sessionByTokenQuery := sessionByToken.Query(*database.Session).
		BindStruct(models.Session{
			Token: tokenToRevoke,
		})

	var session models.Session
	if err := sessionByTokenQuery.GetRelease(&session); err != nil {
		if errors.Is(err, gocql.ErrNotFound) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"error": "Session not found.",
			})
		}
		fmt.Printf("Error getting session: %v\n", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "An error occurred while retrieving the session.",
		})
	}

	// Ensure the session belongs to the user
	if session.UserId != user.ID {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"error": "You do not have permission to revoke this session.",
		})
	}

	// Delete from both session tables
	// Delete from sessions table
	deleteSession := models.SessionTable.DeleteBuilder().
		Where(qb.Eq("session_token"))

	deleteSessionQuery := deleteSession.Query(*database.Session).
		BindStruct(models.Session{
			Token: tokenToRevoke,
		})

	if err := deleteSessionQuery.ExecRelease(); err != nil {
		fmt.Printf("Error deleting from sessions table: %v\n", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "An error occurred while revoking the session.",
		})
	}

	// Delete from sessions_by_user table
	deleteSessionByUser := models.SessionByUserTable.DeleteBuilder().
		Where(qb.Eq("user_id"), qb.Eq("session_token"))

	deleteSessionByUserQuery := deleteSessionByUser.Query(*database.Session).
		BindMap(qb.M{
			"user_id":       user.ID,
			"session_token": tokenToRevoke,
		})

	if err := deleteSessionByUserQuery.ExecRelease(); err != nil {
		fmt.Printf("Error deleting from sessions_by_user table: %v\n", err)
		// Log error but don't return since the main sessions table was updated
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"message": "Session revoked successfully.",
	})
}

// RevokeAllSessions revokes all sessions for the user except the current one
func RevokeAllSessions(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	currentToken := c.Get("X-Session-Token")

	// Query all sessions for the user
	sessionsByUser := models.SessionByUserTable.SelectBuilder().
		Columns("user_id", "session_token").
		Where(qb.Eq("user_id"))

	// Log the user ID we're querying for
	fmt.Printf("Querying sessions for user ID (string): %s\n", user.ID)

	sessionsByUserQuery := sessionsByUser.Query(*database.Session).
		BindMap(qb.M{
			"user_id": user.ID,
		})

	var sessions []models.SessionByUser
	if err := sessionsByUserQuery.SelectRelease(&sessions); err != nil {
		fmt.Printf("Error getting sessions: %v\n", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "An error occurred while retrieving sessions.",
		})
	}

	// Check if we need to debug the session table structure
	if len(sessions) == 0 {
		// Query a sample of sessions to see what's in the table
		allSessionsQuery := models.SessionTable.SelectBuilder().
			Columns("session_token", "user_id").
			Limit(5).
			Query(*database.Session)

		var sampleSessions []models.Session
		if err := allSessionsQuery.SelectRelease(&sampleSessions); err != nil {
			fmt.Printf("Error getting sample sessions: %v\n", err)
		} else {
			fmt.Printf("Sample sessions in the database:\n")
			for i, s := range sampleSessions {
				fmt.Printf("  %d. Token: %s, User ID: %s\n", i+1, s.Token, s.UserId)
			}
		}
	}

	// Delete all sessions except the current one
	for _, session := range sessions {
		if session.Token == currentToken {
			continue
		}

		// Delete from sessions table
		deleteSession := models.SessionTable.DeleteBuilder().
			Where(qb.Eq("session_token"))

		deleteSessionQuery := deleteSession.Query(*database.Session).
			BindStruct(models.Session{
				Token: session.Token,
			})

		if err := deleteSessionQuery.ExecRelease(); err != nil {
			fmt.Printf("Error deleting from sessions table for token %s: %v\n", session.Token, err)
			// Continue with other sessions even if one fails
		}

		// Delete from sessions_by_user table
		deleteSessionByUser := models.SessionByUserTable.DeleteBuilder().
			Where(qb.Eq("user_id"), qb.Eq("session_token"))

		deleteSessionByUserQuery := deleteSessionByUser.Query(*database.Session).
			BindMap(qb.M{
				"user_id":       user.ID,
				"session_token": session.Token,
			})

		if err := deleteSessionByUserQuery.ExecRelease(); err != nil {
			fmt.Printf("Error deleting from sessions_by_user table for token %s: %v\n", session.Token, err)
			// Continue with other sessions even if one fails
		}
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"message": "All other sessions revoked successfully.",
	})
}
