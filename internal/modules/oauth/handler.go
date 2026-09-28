package oauth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/applications"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func scopeError(c fiber.Ctx, err error) error {
	switch err {
	case ErrInvalidClient:
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid_client"})
	case ErrInvalidGrant:
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid_grant"})
	case ErrInvalidScope:
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid_scope"})
	case ErrInvalidRedirect:
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid_request", "error_description": "invalid redirect_uri"})
	case ErrUnsupported:
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "unsupported_grant_type"})
	default:
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "server_error"})
	}
}

// Info GET /oauth2/authorize/info - the consent screen's data (auth required: the user must
// be signed in to consent).
func (h *Handler) Info(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	if c.Query("response_type") != "code" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "unsupported_response_type"})
	}
	clientID, err := id.Parse(c.Query("client_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid_client"})
	}
	app, scopes, err := h.svc.AuthorizeInfo(c.Context(), clientID, c.Query("redirect_uri"), c.Query("scope"))
	if err != nil {
		return scopeError(c, err)
	}
	return c.JSON(fiber.Map{
		"application": appPublic(app),
		"scopes":      scopes,
	})
}

// Authorize POST /oauth2/authorize - the user has consented; issue a code and hand back the
// redirect the client page should send the browser to.
func (h *Handler) Authorize(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	var in struct {
		ClientID     string `json:"client_id"`
		RedirectURI  string `json:"redirect_uri"`
		Scope        string `json:"scope"`
		State        string `json:"state"`
		ResponseType string `json:"response_type"`
	}
	if len(c.Body()) == 0 || json.Unmarshal(c.Body(), &in) != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid_request"})
	}
	if in.ResponseType != "code" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "unsupported_response_type"})
	}
	clientID, err := id.Parse(in.ClientID)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid_client"})
	}
	code, err := h.svc.Authorize(c.Context(), user.ID, clientID, in.RedirectURI, in.Scope)
	if err != nil {
		return scopeError(c, err)
	}
	sep := "?"
	if strings.Contains(in.RedirectURI, "?") {
		sep = "&"
	}
	location := in.RedirectURI + sep + "code=" + url.QueryEscape(code)
	if in.State != "" {
		location += "&state=" + url.QueryEscape(in.State)
	}
	return c.JSON(fiber.Map{"location": location})
}

// Token POST /oauth2/token - the OAuth2 token endpoint. No auth middleware: the client
// authenticates with its id and secret in the body. Accepts form or JSON.
func (h *Handler) Token(c fiber.Ctx) error {
	get := tokenParams(c)
	clientID, err := id.Parse(get("client_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid_client"})
	}
	secret := get("client_secret")
	var res *TokenResult
	switch get("grant_type") {
	case "authorization_code":
		res, err = h.svc.Exchange(c.Context(), clientID, secret, get("code"), get("redirect_uri"))
	case "refresh_token":
		res, err = h.svc.Refresh(c.Context(), clientID, secret, get("refresh_token"))
	default:
		return scopeError(c, ErrUnsupported)
	}
	if err != nil {
		return scopeError(c, err)
	}
	return c.JSON(fiber.Map{
		"access_token":  res.AccessToken,
		"token_type":    "Bearer",
		"expires_in":    res.ExpiresIn,
		"refresh_token": res.RefreshToken,
		"scope":         strings.Join(res.Scopes, " "),
	})
}

// Grants GET /oauth2/@me/grants - the apps this account has authorised.
func (h *Handler) Grants(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	grants, err := h.svc.ListGrants(c.Context(), user.ID)
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	if grants == nil {
		grants = []Grant{}
	}
	return c.JSON(grants)
}

// RevokeGrant DELETE /oauth2/@me/grants/:app_id
func (h *Handler) RevokeGrant(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	appID, err := id.Parse(c.Params("app_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid application id"})
	}
	if err := h.svc.RevokeGrant(c.Context(), user.ID, appID); err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	return c.SendStatus(http.StatusNoContent)
}

// tokenParams reads a field from a form body (the OAuth2 standard) or a JSON body.
func tokenParams(c fiber.Ctx) func(string) string {
	ct := string(c.Request().Header.ContentType())
	if strings.Contains(ct, "application/json") {
		var m map[string]string
		_ = json.Unmarshal(c.Body(), &m)
		return func(k string) string { return strings.TrimSpace(m[k]) }
	}
	return func(k string) string { return strings.TrimSpace(c.FormValue(k)) }
}

func appPublic(a *applications.Application) fiber.Map {
	return fiber.Map{
		"id":          id.Format(a.ID),
		"name":        a.Name,
		"description": a.Description,
		"icon":        a.Icon,
		"bot":         a.HasBot(),
	}
}

var _ = fmt.Sprintf
