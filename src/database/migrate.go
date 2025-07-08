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

// MigrateMessageSchema creates the new message types and updates the messages table
