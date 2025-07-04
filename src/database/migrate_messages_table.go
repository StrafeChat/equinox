package database

import (
	"log"
)

// MigrateMessagesTableSchema recreates the messages table with the correct system_data column type
// WARNING: This will drop and recreate the messages table, causing data loss!
// Only run this migration if you have backed up your data or are working with a fresh database
func MigrateMessagesTableSchema() error {
	log.Println("WARNING: This migration will drop and recreate the messages table!")
	log.Println("Make sure you have backed up your data before proceeding.")
	
	// Drop the existing messages table
	if err := Session.ExecStmt(`DROP TABLE IF EXISTS messages`); err != nil {
		log.Printf("Failed to drop messages table: %v", err)
		return err
	}
	log.Println("Dropped existing messages table")
	
	// Recreate the messages table with the correct schema
	if err := Session.ExecStmt(`CREATE TABLE IF NOT EXISTS messages (
		id text PRIMARY KEY,
		content text,
		author_id text,
		room_id text,
		created_at timestamp,
		nonce text,
		space_id text,
		system boolean,
		tts boolean,
		attachments list<FROZEN<message_attachment>>,
		embeds list<FROZEN<message_embed>>,
		flags int,
		mention_everyone boolean,
		mention_roles list<text>,
		mention_rooms list<text>,
		mentions list<text>,
		message_references list<text>,
		pinned boolean,
		edited_at timestamp,
		type int,
		system_type text,
		system_data SET<FROZEN<message_system_data>>,
		sudo FROZEN<message_sudo>
	)`); err != nil {
		log.Printf("Failed to create messages table: %v", err)
		return err
	}
	log.Println("Created messages table with updated schema")
	
	return nil
}

// MigrateMessagesTableSafe attempts to migrate data from old to new messages table
// This is a safer approach that preserves existing data
func MigrateMessagesTableSafe() error {
	log.Println("Starting safe migration of messages table...")
	
	// Create a backup table with the old schema
	if err := Session.ExecStmt(`CREATE TABLE IF NOT EXISTS messages_backup AS SELECT * FROM messages`); err != nil {
		log.Printf("Failed to create backup table: %v", err)
		return err
	}
	log.Println("Created backup of existing messages table")
	
	// Create new table with updated schema
	if err := Session.ExecStmt(`CREATE TABLE IF NOT EXISTS messages_new (
		id text PRIMARY KEY,
		content text,
		author_id text,
		room_id text,
		created_at timestamp,
		nonce text,
		space_id text,
		system boolean,
		tts boolean,
		attachments list<FROZEN<message_attachment>>,
		embeds list<FROZEN<message_embed>>,
		flags int,
		mention_everyone boolean,
		mention_roles list<text>,
		mention_rooms list<text>,
		mentions list<text>,
		message_references list<text>,
		pinned boolean,
		edited_at timestamp,
		type int,
		system_type text,
		system_data SET<FROZEN<message_system_data>>,
		sudo FROZEN<message_sudo>
	)`); err != nil {
		log.Printf("Failed to create new messages table: %v", err)
		return err
	}
	log.Println("Created new messages table with updated schema")
	
	log.Println("Manual data migration required:")
	log.Println("1. Migrate data from 'messages' to 'messages_new' table")
	log.Println("2. Convert text system_data to SET<FROZEN<message_system_data>> format")
	log.Println("3. Drop old 'messages' table: DROP TABLE messages;")
	log.Println("4. Rename new table: ALTER TABLE messages_new RENAME TO messages;")
	log.Println("5. Update application code to use new schema")
	
	return nil
}