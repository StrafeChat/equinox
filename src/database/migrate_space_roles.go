package database

import (
	"log"

	"github.com/StrafeChat/equinox/src/repository"
)

// MigrateSpaceRoles ensures all existing spaces have the @everyone role
func MigrateSpaceRoles() error {
	log.Println("Starting space roles migration...")

	// Get all spaces
	spaceRepo := repository.NewSpaceRepository(Session)
	spaces, err := spaceRepo.GetAllSpaces()
	if err != nil {
		log.Printf("Failed to get spaces for migration: %v", err)
		return err
	}

	// Create @everyone role for each space that doesn't have it
	spaceRolesRepo := repository.NewSpaceRolesRepository(Session)
	for _, space := range spaces {
		// Check if @everyone role exists
		_, err := spaceRolesRepo.GetRole(space.ID, "@everyone")
		if err != nil {
			// Role doesn't exist, create it
			log.Printf("Creating @everyone role for space %d (%s)", space.ID, space.Name)
			if err := spaceRolesRepo.CreateDefaultEveryoneRole(space.ID); err != nil {
				log.Printf("Failed to create @everyone role for space %d: %v", space.ID, err)
				continue
			}
			log.Printf("Successfully created @everyone role for space %d", space.ID)
		} else {
			log.Printf("Space %d already has @everyone role", space.ID)
		}
	}

	log.Println("Space roles migration completed")
	return nil
}