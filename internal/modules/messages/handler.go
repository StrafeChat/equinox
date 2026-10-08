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
		if res, ok := originError(c, err); ok {
			return res
		}
		if err == ErrBlocked {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
		}
		if err == ErrSystemReadOnly {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
		}
		if err == ErrNotParticipant {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
		}
		if err == ErrAttachForbidden {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
		}
		if err == ErrThreadLocked || err == ErrThreadArchived {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
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
		// The space's raid protection / automod said no: the code and detail let the
		// client phrase it in the user's language.
		var verr *VerificationError
		if errors.As(err, &verr) {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{
				"error":       verificationMessage(verr),
				"code":        "verification_level",
				"requirement": verr.Requirement,
				"level":       verr.Level,
			})
		}
		var aerr *AutomodError
		if errors.As(err, &aerr) {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{
				"error": automodMessage(aerr),
				"code":  "automod_blocked",
				"rule":  aerr.Rule,
			})
		}
		logger.Err("messages", err, map[string]any{"room_id": roomID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.Status(http.StatusCreated).JSON(h.svc.MessageJSON(c.Context(), msg, nil))
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
		case ErrAttachForbidden:
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
		case ErrThreadLocked, ErrThreadArchived:
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
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
	msg, reactions, err := h.svc.Get(c.Context(), user.ID, roomID, msgID)
	if err != nil {
		if res, ok := originError(c, err); ok {
			return res
		}
		if err == ErrNotParticipant {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
		}
		if err == ErrThreadLocked || err == ErrThreadArchived {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
		}
		if err == ErrForbidden {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "missing permission to read this channel"})
		}
		if err == ErrMessageNotFound {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "message not found"})
		}
		logger.Err("messages", err, map[string]any{"room_id": roomID, "message_id": msgID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	if msg == nil {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "message not found"})
	}
	return c.JSON(h.svc.MessageJSON(c.Context(), msg, reactions))
}

// originError answers a write or read that a space's hosting instance refused or could
// not be asked (channels of spaces hosted elsewhere).
func originError(c fiber.Ctx, err error) (error, bool) {
	var oe *OriginError
	if errors.As(err, &oe) {
		return c.Status(oe.Status).JSON(fiber.Map{"error": oe.Message}), true
	}
	if errors.Is(err, ErrOriginUnavailable) {
		return c.Status(http.StatusBadGateway).JSON(fiber.Map{"error": err.Error()}), true
	}
	return nil, false
}

// List handles GET /rooms/:id/messages. Cursor is one of (Discord-style, mutually exclusive):
//
//	?before=<id>  older than id (default history paging)
//	?after=<id>   newer than id, oldest-first (resume downward scroll from a jumped-to window)
//	?around=<id>  a window centred on id (jump to a reply target / search hit not yet loaded)
func (h *Handler) List(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	roomID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid room id"})
	}
	parseCursor := func(name string) (*int64, error) {
		v := c.Query(name)
		if v == "" {
			return nil, nil
		}
		cid, perr := id.Parse(v)
		if perr != nil {
			return nil, perr
		}
		return &cid, nil
	}
	beforeID, err := parseCursor("before")
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid before"})
	}
	afterID, err := parseCursor("after")
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid after"})
	}
	aroundID, err := parseCursor("around")
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid around"})
	}
	limit := 50
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	var msgs []Message
	var reactionsByMsg map[int64][]ReactionSummary
	switch {
	case aroundID != nil:
		msgs, reactionsByMsg, err = h.svc.ListAroundWithReactions(c.Context(), user.ID, roomID, *aroundID, limit)
	case afterID != nil:
		msgs, reactionsByMsg, err = h.svc.ListAfterWithReactions(c.Context(), user.ID, roomID, *afterID, limit)
	default:
		msgs, reactionsByMsg, err = h.svc.ListWithReactions(c.Context(), user.ID, roomID, beforeID, limit)
	}
	if err != nil {
		if res, ok := originError(c, err); ok {
			return res
		}
		if err == ErrNotParticipant {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
		}
		if err == ErrThreadLocked || err == ErrThreadArchived {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
		}
		if err == ErrForbidden {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "missing permission to read this channel"})
		}
		logger.Err("messages", err, map[string]any{"room_id": roomID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.JSON(h.svc.MessagesJSON(c.Context(), msgs, reactionsByMsg))
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
		if res, ok := originError(c, err); ok {
			return res
		}
		if err == ErrNotParticipant {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
		}
		if err == ErrMessageNotFound {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "message not found"})
		}
		if err == ErrThreadLocked || err == ErrThreadArchived {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
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
	return c.JSON(h.svc.MessageJSON(c.Context(), msg, nil))
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
		if res, ok := originError(c, err); ok {
			return res
		}
		if err == ErrNotParticipant {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "not a participant"})
		}
		if err == ErrMessageNotFound {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "message not found"})
		}
		if err == ErrThreadLocked || err == ErrThreadArchived {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
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
	if m.PinnedAt != nil && !m.PinnedAt.IsZero() {
		out["pinned"] = true
		out["pinned_at"] = m.PinnedAt
		if m.PinnedBy != 0 {
			out["pinned_by"] = id.Format(m.PinnedBy)
		}
	}
	if m.ThreadID != nil {
		out["thread_id"] = id.Format(*m.ThreadID)
	}
	if len(reactions) > 0 {
		out["reactions"] = reactions
	}
	return out
}
