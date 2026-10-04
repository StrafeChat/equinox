package applications

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func errorFor(c fiber.Ctx, err error) error {
	switch err {
	case ErrNotFound, ErrNoBot:
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
	case ErrNotOwner:
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
	case ErrTooMany, ErrHasBot:
		return c.Status(http.StatusConflict).JSON(fiber.Map{"error": err.Error()})
	case ErrInvalidName, ErrInvalidRedirect, ErrInvalidField:
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	default:
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
}

// appJSON is the owner's view of an application. client_id is the application id; the
// secret is never here. `bot` carries the bot account's public profile when one exists.
func (h *Handler) appJSON(ctx context.Context, a *Application) fiber.Map {
	m := fiber.Map{
		"id":            id.Format(a.ID),
		"client_id":     id.Format(a.ID),
		"owner_id":      id.Format(a.OwnerID),
		"name":          a.Name,
		"description":   a.Description,
		"icon":          a.Icon,
		"redirect_uris": a.RedirectURIs,
		"has_bot":       a.HasBot(),
		"bot_public":    a.BotPublic,
		"created_at":    a.CreatedAt,
	}
	if a.RedirectURIs == nil {
		m["redirect_uris"] = []string{}
	}
	if a.HasBot() {
		m["bot_user_id"] = id.Format(a.BotUserID)
		if bot, err := h.svc.BotUser(ctx, a); err == nil && bot != nil {
			m["bot"] = botJSON(bot)
		}
	}
	return m
}

// PublicJSON is what anyone may see of an application: enough to recognise it on a consent
// screen or a bot's profile, and nothing an owner would consider private.
func PublicJSON(a *Application, bot *auth.User) fiber.Map {
	m := fiber.Map{
		"id":          id.Format(a.ID),
		"name":        a.Name,
		"description": a.Description,
		"icon":        a.Icon,
		"has_bot":     a.HasBot(),
		"bot_public":  a.BotPublic,
	}
	if bot != nil {
		m["bot"] = botJSON(bot)
	}
	return m
}

func botJSON(u *auth.User) fiber.Map {
	return fiber.Map{
		"id":           id.Format(u.ID),
		"username":     u.Username,
		"display_name": u.DisplayName,
		"avatar":       u.Avatar,
		"banner":       u.Banner,
		"bio":          u.Bio,
		"about_me":     u.AboutMe,
		"accent_color": u.AccentColor,
		"public_flags": auth.PublicFlags(u),
		"bot":          true,
	}
}

func (h *Handler) Create(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	var in struct {
		Name string `json:"name"`
	}
	if len(c.Body()) == 0 || json.Unmarshal(c.Body(), &in) != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	a, secret, err := h.svc.Create(c.Context(), user.ID, in.Name)
	if err != nil {
		return errorFor(c, err)
	}
	m := h.appJSON(c.Context(), a)
	m["client_secret"] = secret // shown once
	return c.Status(http.StatusCreated).JSON(m)
}

func (h *Handler) List(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	apps, err := h.svc.List(c.Context(), user.ID)
	if err != nil {
		return errorFor(c, err)
	}
	out := make([]fiber.Map, 0, len(apps))
	for i := range apps {
		out = append(out, h.appJSON(c.Context(), &apps[i]))
	}
	return c.JSON(out)
}

func (h *Handler) Get(c fiber.Ctx) error {
	user, appID, ok := h.owned(c)
	if !ok {
		return nil
	}
	a, err := h.svc.Get(c.Context(), user.ID, appID)
	if err != nil {
		return errorFor(c, err)
	}
	return c.JSON(h.appJSON(c.Context(), a))
}

// GetPublic GET /applications/:id/public - the public profile of any application, for the
// surfaces that show an app to people other than its owner (a bot's "Add to space").
func (h *Handler) GetPublic(c fiber.Ctx) error {
	appID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid application id"})
	}
	a, err := h.svc.GetPublic(c.Context(), appID)
	if err != nil {
		return errorFor(c, err)
	}
	bot, _ := h.svc.BotUser(c.Context(), a)
	return c.JSON(PublicJSON(a, bot))
}

func (h *Handler) Patch(c fiber.Ctx) error {
	user, appID, ok := h.owned(c)
	if !ok {
		return nil
	}
	var in UpdateInput
	if len(c.Body()) > 0 && json.Unmarshal(c.Body(), &in) != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	a, err := h.svc.Update(c.Context(), user.ID, appID, in)
	if err != nil {
		return errorFor(c, err)
	}
	return c.JSON(h.appJSON(c.Context(), a))
}

func (h *Handler) Delete(c fiber.Ctx) error {
	user, appID, ok := h.owned(c)
	if !ok {
		return nil
	}
	if err := h.svc.Delete(c.Context(), user.ID, appID); err != nil {
		return errorFor(c, err)
	}
	return c.SendStatus(http.StatusNoContent)
}

func (h *Handler) ResetSecret(c fiber.Ctx) error {
	user, appID, ok := h.owned(c)
	if !ok {
		return nil
	}
	secret, err := h.svc.ResetSecret(c.Context(), user.ID, appID)
	if err != nil {
		return errorFor(c, err)
	}
	return c.JSON(fiber.Map{"client_secret": secret})
}

func (h *Handler) AddBot(c fiber.Ctx) error {
	user, appID, ok := h.owned(c)
	if !ok {
		return nil
	}
	bot, token, err := h.svc.AddBot(c.Context(), user.ID, appID)
	if err != nil {
		return errorFor(c, err)
	}
	m := botJSON(bot)
	m["token"] = token // shown once
	return c.Status(http.StatusCreated).JSON(m)
}

func (h *Handler) ResetBotToken(c fiber.Ctx) error {
	user, appID, ok := h.owned(c)
	if !ok {
		return nil
	}
	token, err := h.svc.ResetBotToken(c.Context(), user.ID, appID)
	if err != nil {
		return errorFor(c, err)
	}
	return c.JSON(fiber.Map{"token": token})
}

// owned resolves the caller and the :id param, writing the error response itself when either
// is missing. ok=false means a response was already sent.
func (h *Handler) owned(c fiber.Ctx) (*auth.User, int64, bool) {
	user := auth.GetUser(c)
	if user == nil {
		_ = c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
		return nil, 0, false
	}
	appID, err := id.Parse(c.Params("id"))
	if err != nil {
		_ = c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid application id"})
		return nil, 0, false
	}
	return user, appID, true
}
