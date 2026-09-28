package messages

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// Create handles POST /rooms/:id/messages
func (h *Handler) Create(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	var in CreateMessageInput
	if err := json.Unmarshal(c.Body(), &in); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid json"})
	}
	if in.Ciphertext == "" && in.Plaintext == "" && len(in.Attachments) == 0 {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "ciphertext, plaintext, or attachments required"})
	}
	msg, err := h.svc.Create(c.Context(), user.ID, roomID, &in)
	if err != nil {
		if err == ErrNotParticipant {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
		}
		if err == ErrForbidden {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "missing permission to send messages in this channel"})
		}
		if err == ErrInvalidInput {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "plaintext required when E2EE is off; ciphertext required when E2EE is on"})
		}
		if err == ErrTooManyAttachments || err == ErrAttachmentNotFound || err == ErrContentTooLong || err == ErrTooManyMentions {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}
		var slow *SlowmodeError
		if errors.As(err, &slow) {
			secs := int(slow.RetryAfter.Round(time.Second) / time.Second)
			if secs < 1 {
				secs = 1
			}
			c.Set("Retry-After", strconv.Itoa(secs))
			return c.Status(http.StatusTooManyRequests).JSON(fiber.Map{
				"error":       "slowmode is on in this channel",
				"retry_after": secs,
			})
		}
		logger.Err("messages", err, map[string]any{"room_id": roomID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.Status(http.StatusCreated).JSON(messageToJSON(msg, nil))
}

// UploadAttachment handles POST /rooms/:id/attachments (multipart: "file", plus optional
// "width"/"height" for images and "encrypted"=1 for client-encrypted blobs). Returns the
// attachment record whose id the client then passes in the message's `attachments`.
func (h *Handler) UploadAttachment(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	if !h.svc.UploadsConfigured() {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "file uploads are not configured"})
	}
	fh, err := c.FormFile("file")
	if err != nil || fh == nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "missing file field"})
	}
	if fh.Size <= 0 {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "empty file"})
	}
	if fh.Size > h.svc.MaxAttachmentBytes() {
		return c.Status(http.StatusRequestEntityTooLarge).JSON(fiber.Map{
			"error":     "attachment is too large",
			"max_bytes": h.svc.MaxAttachmentBytes(),
		})
	}
	src, err := fh.Open()
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "could not read file"})
	}
	defer src.Close()

	enc := c.FormValue("encrypted")
	in := &UploadAttachmentInput{
		Filename:    fh.Filename,
		ContentType: fh.Header.Get("Content-Type"),
		Size:        fh.Size,
		Width:       atoiOrZero(c.FormValue("width")),
		Height:      atoiOrZero(c.FormValue("height")),
		Encrypted:   enc == "1" || enc == "true",
		Body:        src,
	}
	att, err := h.svc.UploadAttachment(c.Context(), user.ID, roomID, in)
	if err != nil {
		switch err {
		case ErrNotParticipant:
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
		case ErrForbidden:
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "missing permission to send messages in this channel"})
		case ErrRoomNotFound:
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "room not found"})
		case ErrAttachmentTooLarge:
			return c.Status(http.StatusRequestEntityTooLarge).JSON(fiber.Map{"error": err.Error()})
		case ErrUploadsDisabled:
			return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": err.Error()})
		case ErrInvalidInput:
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid upload"})
		case ErrAttachmentBadUpload:
			return c.Status(http.StatusBadGateway).JSON(fiber.Map{"error": err.Error()})
		}
		logger.Err("messages", err, map[string]any{"room_id": roomID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.Status(http.StatusCreated).JSON(att)
}

func atoiOrZero(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// Get handles GET /rooms/:id/messages/:msg_id
func (h *Handler) Get(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	msgID, err := id.Parse(c.Params("msg_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid message id"})
	}
	msg, err := h.svc.Get(c.Context(), user.ID, roomID, msgID)
	if err != nil {
		if err == ErrNotParticipant {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
		}
		if err == ErrForbidden {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "missing permission to read this channel"})
		}
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	if msg == nil {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "message not found"})
	}
	reactions, err := h.svc.Reactions(c.Context(), user.ID, roomID, msgID)
	if err != nil {
		logger.Err("messages", err, map[string]any{"room_id": roomID, "message_id": msgID})
	}
	return c.JSON(messageToJSON(msg, reactions))
}

// List handles GET /rooms/:id/messages?before=id&limit=50
func (h *Handler) List(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	var beforeID *int64
	if b := c.Query("before"); b != "" {
		bid, err := id.Parse(b)
		if err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid before"})
		}
		beforeID = &bid
	}
	limit := 50
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	msgs, err := h.svc.List(c.Context(), user.ID, roomID, beforeID, limit)
	if err != nil {
		if err == ErrNotParticipant {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
		}
		if err == ErrForbidden {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "missing permission to read this channel"})
		}
		logger.Err("messages", err, map[string]any{"room_id": roomID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	ids := make([]int64, len(msgs))
	for i := range msgs {
		ids[i] = msgs[i].ID
	}
	reactionsByMsg, rerr := h.svc.ReactionsForMessages(c.Context(), roomID, ids, user.ID)
	if rerr != nil {
		// Non-fatal: history is still useful without reaction counts.
		logger.Err("messages", rerr, map[string]any{"room_id": roomID})
	}
	out := make([]fiber.Map, len(msgs))
	for i := range msgs {
		out[i] = messageToJSON(&msgs[i], reactionsByMsg[msgs[i].ID])
	}
	return c.JSON(out)
}

// Edit handles PATCH /rooms/:id/messages/:msg_id
func (h *Handler) Edit(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	msgID, err := id.Parse(c.Params("msg_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid message id"})
	}
	var in EditMessageInput
	if err := json.Unmarshal(c.Body(), &in); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid json"})
	}
	if in.Ciphertext == "" && in.Plaintext == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "ciphertext or plaintext required"})
	}
	msg, err := h.svc.Edit(c.Context(), user.ID, roomID, msgID, &in)
	if err != nil {
		if err == ErrNotParticipant {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
		}
		if err == ErrMessageNotFound {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "message not found"})
		}
		if err == ErrForbidden {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "cannot edit this message"})
		}
		if err == ErrInvalidInput {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "plaintext required when E2EE is off; ciphertext required when E2EE is on"})
		}
		if err == ErrContentTooLong {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}
		logger.Err("messages", err, nil)
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.JSON(messageToJSON(msg, nil))
}

// Delete handles DELETE /rooms/:id/messages/:msg_id
func (h *Handler) Delete(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	msgID, err := id.Parse(c.Params("msg_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid message id"})
	}
	if err := h.svc.Delete(c.Context(), user.ID, roomID, msgID); err != nil {
		if err == ErrNotParticipant {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
		}
		if err == ErrMessageNotFound {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "message not found"})
		}
		if err == ErrForbidden {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "cannot delete this message"})
		}
		logger.Err("messages", err, nil)
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.Status(http.StatusNoContent).Send(nil)
}

func messageToJSON(m *Message, reactions []ReactionSummary) fiber.Map {
	out := fiber.Map{
		"room_id":          id.Format(m.RoomID),
		"id":               id.Format(m.ID),
		"sender_id":        id.Format(m.SenderID),
		"sender_device_id": id.Format(m.SenderDeviceID),
		"ciphertext":       m.Ciphertext,
		"created_at":       m.CreatedAt,
		"updated_at":       m.UpdatedAt,
	}
	if m.Plaintext != "" {
		out["plaintext"] = m.Plaintext
	}
	if m.ReplyToID != nil {
		out["reply_to_id"] = id.Format(*m.ReplyToID)
	}
	if len(m.Mentions) > 0 {
		out["mentions"] = formatIDs(m.Mentions)
	}
	if m.MentionEveryone {
		out["mention_everyone"] = true
	}
	if len(m.MentionRoles) > 0 {
		out["mention_roles"] = formatIDs(m.MentionRoles)
	}
	if atts := m.Attachments(); len(atts) > 0 {
		out["attachments"] = atts
	}
	if m.DeletedAt != nil && !m.DeletedAt.IsZero() {
		out["deleted_at"] = m.DeletedAt
	}
	if m.SystemType != "" {
		out["system_type"] = m.SystemType
		out["system_payload"] = m.SystemPayload
	}
	if len(reactions) > 0 {
		out["reactions"] = reactions
	}
	return out
}
