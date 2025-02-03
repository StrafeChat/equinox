package handlers_v1

import (
	"encoding/json"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/StrafeChat/equinox/src/utils"
	"github.com/gofiber/fiber/v3"
	"github.com/scylladb/gocqlx/v3/qb"
)

type UpdateStatusRequest struct {
	Status       *string `json:"status,omitempty"`
	CustomStatus *string `json:"custom_status,omitempty"`
}

func UpdateStatus(c fiber.Ctx) error {
	user := c.Locals("user").(models.User)
	var req UpdateStatusRequest

	if err := c.Bind().Body(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{
			"message": "Invalid request body",
		})
	}

	presence := models.UserPresence{
		Online: user.Presence["online"].(bool),
	}

	// Update status if provided
	if req.Status != nil {
		if !utils.IsValidStatus(*req.Status) {
			return c.Status(400).JSON(fiber.Map{
				"message": "Invalid status",
			})
		}
		presence.Status = *req.Status
	} else {
		presence.Status = user.Presence["status"].(string)
	}

	// Update custom status if provided
	if req.CustomStatus != nil {
		presence.CustomStatus = req.CustomStatus
	} else if cs, ok := user.Presence["custom_status"]; ok {
		if cs != nil {
			str := cs.(string)
			presence.CustomStatus = &str
		}
	}

	// Update user's presence and timestamp
	user.Presence = presence.ToMap()
	user.UpdatedAt = time.Now()

	// Update in database
	stmt := models.UserTable.UpdateBuilder().
		Set("presence", "updated_at").
		Where(qb.Eq("id")).
		Query(*database.Session).
		BindStruct(&user)

	if err := stmt.ExecRelease(); err != nil {
		return c.Status(500).JSON(fiber.Map{
			"message": "Failed to update status",
		})
	}

	// Publish presence update to Redis for Stargate
	event := map[string]interface{}{
		"type": "PRESENCE_UPDATE",
		"data": map[string]interface{}{
			"user_id":  user.ID,
			"presence": user.Presence,
		},
	}

	eventJson, err := json.Marshal(event)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{
			"message": "Failed to marshal presence event",
		})
	}

	if err := database.Rdb.Publish("USER_EVENTS", string(eventJson)).Err(); err != nil {
		return c.Status(500).JSON(fiber.Map{
			"message": "Failed to publish presence update",
		})
	}

	return c.Status(200).JSON(user)
}
