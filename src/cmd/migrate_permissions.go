package main

import (
	"log"

	"github.com/StrafeChat/equinox/src/database"
)

func main() {
	// Initialize database
	if err := database.InitDB(); err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}

	// Run permissions migration
	log.Println("Starting permissions migration to bitmap format...")
	if err := database.MigratePermissionsToBitmap(); err != nil {
		log.Fatalf("Permissions migration failed: %v", err)
	}

	// Verify migration
	log.Println("Verifying migration...")
	if err := database.VerifyPermissionsMigration(); err != nil {
		log.Fatalf("Migration verification failed: %v", err)
	}

	log.Println("✅ Permissions migration completed successfully!")
	log.Println("")
	log.Println("Migration Summary:")
	log.Println("- Converted array-based permissions to bitmap format")
	log.Println("- Changed VIEW_CHANNELS permissions to VIEW_ROOMS")
	log.Println("- Created backup tables for safety")
	log.Println("- Verified migration integrity")
	log.Println("")
	log.Println("You can now safely remove backup tables if everything looks correct.")
}