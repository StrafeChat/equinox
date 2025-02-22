package handlers_v1

import (
	"fmt"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/helpers"
	"github.com/StrafeChat/equinox/src/types"
	"github.com/gofiber/fiber/v3"
)

func CreateRoom(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	body := new(types.CreateRoomInput)

	if err := c.Bind().Body(body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Invalid request body",
		})
	}

	if body.Recipients[0] == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"message": "Invalid request body, recipients is required.",
		})
	}

	room := types.Room{
		ID:            helpers.GenerateRoomID().String(),
		Recipients:    append(body.Recipients, user.ID),
		LastMessageId: fmt.Sprint(-1),
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}

	fmt.Println(body)

	if body.IsGroup {
		room.Creator = &user.ID
	}
	fmt.Println(room)

	q := models.RoomTable.InsertQuery(*database.Session)
	if err := q.BindStruct(room).ExecRelease(); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"message": "Failed to create room.",
			"error":   err.Error(),
		})
	}

	return c.Status(fiber.StatusCreated).JSON(room)
}
