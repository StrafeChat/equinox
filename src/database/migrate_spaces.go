package database

import (
	"log"
)

// MigrateSpacesSchema adds missing columns to the spaces table
func MigrateSpacesSchema() error {
	log.Println("Migrating spaces table schema...")

	// Try to add banner column if it doesn't exist
	if err := Session.ExecStmt(`ALTER TABLE spaces ADD banner text`); err != nil {
		log.Printf("Banner column might already exist in spaces table: %v", err)
	}

	log.Println("Spaces table migration completed")
	return nil
}