package database

import (
	"log"
)

// MigrateRoomSchema adds missing columns to the rooms table
func MigrateRoomSchema() error {
	// Try to add name column if it doesn't exist
	if err := Session.ExecStmt(`ALTER TABLE rooms ADD name text`); err != nil {
		log.Printf("Name column might already exist: %v", err)
	}

	// Try to add topic column if it doesn't exist
	if err := Session.ExecStmt(`ALTER TABLE rooms ADD topic text`); err != nil {
		log.Printf("Topic column might already exist: %v", err)
	}

	// Try to add icon column if it doesn't exist
	if err := Session.ExecStmt(`ALTER TABLE rooms ADD icon text`); err != nil {
		log.Printf("Icon column might already exist: %v", err)
	}

	return nil
}

// MigrateMessageSchema creates the new message types and updates the messages table
