package handlers_v1

import (
	"time"

	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/types"
	"github.com/gocql/gocql"
	"github.com/gofiber/fiber/v3"
)

func CreateRoom(c fiber.Ctx) error {
	user := c.Locals("user").(*models.User)
	body := new(types.CreateRoomInput)

	var input types.CreateRoomInput
	if err := c.Bind().Body(body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Invalid request body",
		})
	}

	// Validate the input using a hypothetical validation function
	// if err := utils.ValidateInput(input); err != nil {
	// 	return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
	// 		"message": "Validation failed",
	// 		"errors":  utils.GetValidationErrors(err),
	// 	})
	// }

	// Create new room
	room := types.Room{
		ID:         gocql.TimeUUID(),
		Recipients: append(input.Recipients, user.ID),
		CreatedAt:  time.Now(),
	}

	// Set creator if it's a group
	if input.IsGroup {
		room.Creator = &user.ID
	}

	// // Insert room into database using gocqlx
	// q := models.RoomTable.InsertQuery(utils.DB.Session)
	// if err := q.BindStruct(room).ExecRelease(); err != nil {
	// 	return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
	// 		"message": "Failed to create room",
	// 	})
	// }

	return c.Status(fiber.StatusCreated).JSON(room)
}
