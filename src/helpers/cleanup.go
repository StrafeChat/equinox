package helpers

import (
	"log"
	"time"

	"github.com/scylladb/gocqlx/v3/qb"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
)

// CleanupExpiredResetCodes removes all expired password reset codes from the database
func CleanupExpiredResetCodes() {
	// Get current time
	now := time.Now()

	// Find all expired reset codes using the table with expires_at as the partition key
	expiredCodesQuery := models.PasswordResetByExpirationTable.SelectBuilder().
		Where(qb.Lt("expires_at")).
		AllowFiltering()

	log.Printf("Executing query to find expired reset codes with expires_at < %v", now)
	expiredCodesQueryExec := expiredCodesQuery.Query(*database.Session).
		BindMap(qb.M{
			"expires_at": now,
		})

	var expiredResets []models.PasswordReset
	if err := expiredCodesQueryExec.SelectRelease(&expiredResets); err != nil {
		log.Printf("Error finding expired reset codes: %v", err)
		return
	}

	log.Printf("Found %d expired reset codes to clean up", len(expiredResets))

	// Delete each expired code from all tables
	for _, reset := range expiredResets {
		// Delete from the main password_resets table
		deleteQuery := models.PasswordResetTable.DeleteBuilder().
			Where(qb.Eq("code"))

		deleteQueryExec := deleteQuery.Query(*database.Session).
			BindMap(qb.M{
				"code": reset.Code,
			})

		if err := deleteQueryExec.ExecRelease(); err != nil {
			log.Printf("Error deleting expired reset code from main table: %v", err)
			// Continue with the next one
		}

		// Delete from the password_resets_by_user table if we have the data
		if reset.UserId != "" && !reset.CreatedAt.IsZero() {
			deleteByUserQuery := models.PasswordResetByUserTable.DeleteBuilder().
				Where(qb.Eq("user_id"), qb.Eq("created_at"))

			deleteByUserQueryExec := deleteByUserQuery.Query(*database.Session).
				BindMap(qb.M{
					"user_id":    reset.UserId,
					"created_at": reset.CreatedAt,
				})

			if err := deleteByUserQueryExec.ExecRelease(); err != nil {
				log.Printf("Error deleting expired reset code from by_user table: %v", err)
				// Continue anyway
			}
		}

		// Delete from the password_resets_by_expiration table if we have the data
		if !reset.ExpiresAt.IsZero() {
			deleteByExpirationQuery := models.PasswordResetByExpirationTable.DeleteBuilder().
				Where(qb.Eq("expires_at"), qb.Eq("code"))

			deleteByExpirationQueryExec := deleteByExpirationQuery.Query(*database.Session).
				BindMap(qb.M{
					"expires_at": reset.ExpiresAt,
					"code":       reset.Code,
				})

			if err := deleteByExpirationQueryExec.ExecRelease(); err != nil {
				log.Printf("Error deleting expired reset code from by_expiration table: %v", err)
				// Continue anyway
			}
		}
	}
}

// ScheduleCleanupTask starts a goroutine that periodically cleans up expired reset codes
func ScheduleCleanupTask() {
	go func() {
		// Run cleanup every hour
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				log.Println("Running scheduled cleanup of expired password reset codes")
				CleanupExpiredResetCodes()
			}
		}
	}()

	// Also run once at startup
	log.Println("Running initial cleanup of expired password reset codes")
	CleanupExpiredResetCodes()
}
