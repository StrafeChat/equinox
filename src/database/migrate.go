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

	// Try to add space_id column if it doesn't exist
	if err := Session.ExecStmt(`ALTER TABLE rooms ADD space_id bigint`); err != nil {
		log.Printf("Space_id column might already exist: %v", err)
	}

	// Try to add parent_id column if it doesn't exist
	if err := Session.ExecStmt(`ALTER TABLE rooms ADD parent_id text`); err != nil {
		log.Printf("Parent_id column might already exist: %v", err)
	}

	// Try to add position column if it doesn't exist
	if err := Session.ExecStmt(`ALTER TABLE rooms ADD position int`); err != nil {
		log.Printf("Position column might already exist: %v", err)
	}

	return nil
}

// MigrateVerificationTokensSchema recreates the verification_tokens table with the correct column name
func MigrateVerificationTokensSchema() error {
	log.Println("Migrating verification_tokens table schema...")

	// Drop the existing verification_tokens table if it exists
	if err := Session.ExecStmt(`DROP TABLE IF EXISTS verification_tokens`); err != nil {
		log.Printf("Failed to drop verification_tokens table: %v", err)
		return err
	}
	log.Println("Dropped existing verification_tokens table")

	// Recreate the verification_tokens table with the correct schema
	if err := Session.ExecStmt(`CREATE TABLE IF NOT EXISTS verification_tokens (
		verification_token text PRIMARY KEY,
		user_id bigint,
		email text,
		created_at timestamp,
		expires_at timestamp
	)`); err != nil {
		log.Printf("Failed to create verification_tokens table: %v", err)
		return err
	}
	log.Println("Created verification_tokens table with updated schema")

	return nil
}

// MigrateMessageAttachmentSchema updates the message_attachment UDT to include size field
func MigrateMessageAttachmentSchema() error {
	log.Println("Migrating message_attachment UDT schema...")

	// First, try to add the size field to the existing UDT
	if err := Session.ExecStmt(`ALTER TYPE message_attachment ADD size bigint`); err != nil {
		log.Printf("Size field might already exist in message_attachment: %v", err)
		// If the field already exists, that's fine
	} else {
		log.Println("Added size field to message_attachment type")
	}

	return nil
}

// MigrateFileSchema adds width and height columns to the files table
func MigrateFileSchema() error {
	log.Println("Migrating files table schema...")

	// Try to add width column if it doesn't exist
	if err := Session.ExecStmt(`ALTER TABLE files ADD width int`); err != nil {
		log.Printf("Width column might already exist in files table: %v", err)
	}

	// Try to add height column if it doesn't exist
	if err := Session.ExecStmt(`ALTER TABLE files ADD height int`); err != nil {
		log.Printf("Height column might already exist in files table: %v", err)
	}

	log.Println("Files table migration completed")
	return nil
}

// MigrateUserSchema adds missing columns to the users table
func MigrateUserSchema() error {
	// Try to add spaces column if it doesn't exist
	if err := Session.ExecStmt(`ALTER TABLE users ADD spaces set<bigint>`); err != nil {
		log.Printf("Spaces column might already exist: %v", err)
	}

	// Try to add bots column if it doesn't exist
	if err := Session.ExecStmt(`ALTER TABLE users ADD bots set<text>`); err != nil {
		log.Printf("Bots column might already exist: %v", err)
	}

	return nil
}

// MigrateSpaceIDSchema migrates space_id from bigint to text
// WARNING: This migration requires manual data conversion
func MigrateSpaceIDSchema() error {
	log.Println("WARNING: Migrating space_id from bigint to text requires manual intervention!")
	log.Println("This migration will:")
	log.Println("1. Add a new space_id_text column")
	log.Println("2. You need to manually copy data from space_id to space_id_text")
	log.Println("3. Drop the old space_id column")
	log.Println("4. Rename space_id_text to space_id")

	// Step 1: Add new text column
	if err := Session.ExecStmt(`ALTER TABLE rooms ADD space_id_text text`); err != nil {
		log.Printf("space_id_text column might already exist: %v", err)
	}

	// Step 2: Copy data (this needs to be done manually or with a data migration script)
	log.Println("Please run the following CQL to copy data:")
	log.Println("UPDATE rooms SET space_id_text = CAST(space_id AS text) WHERE space_id IS NOT NULL;")

	// Note: Steps 3 and 4 should be done after data verification
	// ALTER TABLE rooms DROP space_id;
	// ALTER TABLE rooms RENAME space_id_text TO space_id;

	return nil
}

// MigrateAllSchemas runs all available migrations
func MigrateAllSchemas() error {
	log.Println("Running all database schema migrations...")

	// Run individual migrations
	if err := MigrateRoomSchema(); err != nil {
		return err
	}

	if err := MigrateVerificationTokensSchema(); err != nil {
		return err
	}

	if err := MigrateMessageAttachmentSchema(); err != nil {
		return err
	}

	if err := MigrateFileSchema(); err != nil {
		return err
	}

	if err := MigrateUserSchema(); err != nil {
		return err
	}

	// Run permissions migration
	if err := MigratePermissionsToBitmap(); err != nil {
		return err
	}

	log.Println("All schema migrations completed successfully")
	return nil
}

// MigrateE2EESchema adds missing columns to E2EE tables
func MigrateE2EESchema() error {
	log.Println("Migrating E2EE tables schema...")

	// Try to add key_type column to e2ee_identity_keys if it doesn't exist
	if err := Session.ExecStmt(`ALTER TABLE e2ee_identity_keys ADD key_type text`); err != nil {
		log.Printf("key_type column might already exist in e2ee_identity_keys: %v", err)
	} else {
		log.Println("Added key_type column to e2ee_identity_keys table")
	}

	log.Println("E2EE tables migration completed")
	return nil
}

// MigrateMessageSchema creates the new message types and updates the messages table
