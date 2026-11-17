package instance

import (
	"errors"
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// deletionError maps the account-deletion errors, then falls back to moderationError.
func deletionError(c fiber.Ctx, err error) error {
	var blocked *DeletionBlockedError
	var stage *DeletionStageError
	switch {
	case errors.As(err, &blocked):
		return c.Status(http.StatusConflict).JSON(fiber.Map{"error": blocked.Error(), "code": "blocked", "blockers": blockersJSON(blocked.Blockers)})
	case errors.As(err, &stage):
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "the deletion did not finish - it is safe to try again", "stage": stage.Stage})
	case errors.Is(err, ErrConfirmMismatch):
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error(), "code": "confirm_mismatch"})
	case errors.Is(err, ErrCannotBanRemote):
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error(), "code": RefusalRemote})
	case errors.Is(err, ErrDeleteSelf):
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error(), "code": RefusalSelf})
	case errors.Is(err, ErrDeleteSystem):
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error(), "code": RefusalSystem})
	case errors.Is(err, ErrDeleteAdmin):
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error(), "code": RefusalAdmin})
	case errors.Is(err, ErrDeletionInProgress):
		return c.Status(http.StatusConflict).JSON(fiber.Map{"error": err.Error(), "code": "in_progress"})
	case errors.Is(err, ErrDeletionUnavailable):
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": err.Error()})
	}
	return moderationError(c, err)
}

func blockersJSON(b DeletionBlockers) fiber.Map {
	spaces := make([]fiber.Map, 0, len(b.OwnedSpaces))
	for _, sp := range b.OwnedSpaces {
		var hosted interface{}
		if sp.HostedOn != "" {
			hosted = sp.HostedOn
		}
		spaces = append(spaces, fiber.Map{"id": id.Format(sp.ID), "name": sp.Name, "member_count": sp.MemberCount, "hosted_on": hosted})
	}
	apps := make([]fiber.Map, 0, len(b.OwnedApplications))
	for _, a := range b.OwnedApplications {
		m := fiber.Map{"id": id.Format(a.ID), "name": a.Name, "bot_user_id": nil, "bot_username": nil, "bot_space_count": a.BotSpaceCount}
		if a.BotUserID != 0 {
			m["bot_user_id"] = id.Format(a.BotUserID)
			m["bot_username"] = a.BotUsername
		}
		apps = append(apps, m)
	}
	return fiber.Map{"owned_spaces": spaces, "owned_applications": apps}
}

// DeletionPreview GET /instance/users/:id/deletion-preview
func (h *Handler) DeletionPreview(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	uid, ok := parseIDParam(c, "id")
	if !ok {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}
	p, err := h.svc.PreviewAccountDeletion(c.Context(), user.ID, uid)
	if err != nil {
		return deletionError(c, err)
	}
	u := userAdminJSON(p.User)
	u["instance_admin"] = p.InstanceAdmin
	var refusal interface{}
	if p.Refusal != "" {
		refusal = p.Refusal
	}
	var app interface{}
	if p.Application != nil {
		if p.Application.Orphan {
			app = fiber.Map{"id": nil, "name": nil, "owner": nil, "orphan": true}
		} else {
			app = fiber.Map{"id": id.Format(p.Application.ID), "name": p.Application.Name, "owner": userAdminJSON(p.Application.Owner), "orphan": false}
		}
	}
	return c.JSON(fiber.Map{
		"user":        u,
		"kind":        p.Kind,
		"refusal":     refusal,
		"blockers":    blockersJSON(p.Blockers),
		"application": app,
		"counts":      p.Counts,
	})
}

// DeleteAccount DELETE /instance/users/:id
func (h *Handler) DeleteAccount(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	uid, ok := parseIDParam(c, "id")
	if !ok {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}
	var in DeleteAccountInput
	if !decodeBody(c, &in) {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	res, err := h.svc.DeleteAccount(c.Context(), user.ID, uid, in)
	if err != nil {
		return deletionError(c, err)
	}
	spaces := make([]fiber.Map, 0, len(res.SpacesTakenDown))
	for _, sp := range res.SpacesTakenDown {
		spaces = append(spaces, fiber.Map{"id": id.Format(sp.ID), "name": sp.Name})
	}
	apps := make([]fiber.Map, 0, len(res.ApplicationsDeleted))
	for _, a := range res.ApplicationsDeleted {
		m := fiber.Map{"id": id.Format(a.ID), "name": a.Name, "bot_user_id": nil}
		if a.BotUserID != 0 {
			m["bot_user_id"] = id.Format(a.BotUserID)
		}
		apps = append(apps, m)
	}
	warnings := res.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	out := fiber.Map{
		"deleted":              true,
		"user_id":              id.Format(res.UserID),
		"username":             res.Username,
		"kind":                 res.Kind,
		"counts":               res.Counts,
		"spaces_taken_down":    spaces,
		"applications_deleted": apps,
		"warnings":             warnings,
		"ban_removed":          res.BanRemoved,
	}
	if res.BanRemoved {
		// The ban row is keyed by the deleted id and meant nothing any more; only an IP ban
		// keeps the person from registering again.
		out["ban_note"] = "the account's instance ban was removed with it; keep or add an IP ban if the person must stay out"
	}
	return c.JSON(out)
}
