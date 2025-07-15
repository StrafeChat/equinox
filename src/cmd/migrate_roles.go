package main

import (
	"log"
	"os"

	"github.com/StrafeChat/equinox/src/database"
)

func main() {
	// Initialize database
	if err := database.InitDB(); err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}

	// Run migration
	if err := database.MigrateSpaceRoles(); err != nil {
		log.Fatalf("Migration failed: %v", err)
		os.Exit(1)
	}

	log.Println("Migration completed successfully")
}