package applications

import (
	"encoding/json"
	"fmt"
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

// appJSON is the public shape. client_id is the application id; the secret is never here.
func appJSON(a *Application) fiber.Map {
	m := fiber.Map{
		"id":            id.Format(a.ID),
		"client_id":     id.Format(a.ID),
		"owner_id":      id.Format(a.OwnerID),
		"name":          a.Name,
		"description":   a.Description,
		"icon":          a.Icon,
		"redirect_uris": a.RedirectURIs,
		"has_bot":       a.HasBot(),
		"created_at":    a.CreatedAt,
	}
	if a.HasBot() {
		m["bot_user_id"] = id.Format(a.BotUserID)
	}
	if a.RedirectURIs == nil {
		m["redirect_uris"] = []string{}
	}
	return m
}

func botJSON(u *auth.User) fiber.Map {
	return fiber.Map{
		"id":            id.Format(u.ID),
		"username":      u.Username,
		"discriminator": fmt.Sprintf("%04d", u.Discriminator),
		"display_name":  u.DisplayName,
		"bot":           true,
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
	m := appJSON(a)
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
		out = append(out, appJSON(&apps[i]))
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
	return c.JSON(appJSON(a))
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
	return c.JSON(appJSON(a))
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
