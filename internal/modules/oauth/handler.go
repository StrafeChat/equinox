package oauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/applications"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// oauthError maps a service error to the RFC 6749 error shape. Errors from the spaces
// module (a bot install the user may not perform) come back as access_denied with the
// space's own wording, so the consent screen can say why.
func oauthError(c fiber.Ctx, err error) error {
	// A refusal from the spaces module - including one passed through from the instance
	// hosting the space (an install into a mirrored space is decided there) - keeps its
	// status and wording.
	if status, msg, ok := spaces.HTTPError(err); ok {
		code := "invalid_request"
		switch {
		case status == http.StatusForbidden:
			code = "access_denied"
		case status >= 500:
			code = "server_error"
		}
		return c.Status(status).JSON(fiber.Map{"error": code, "error_description": msg})
	}
	switch {
	case errors.Is(err, ErrInvalidClient):
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid_client"})
	case errors.Is(err, ErrInvalidGrant):
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid_grant"})
	case errors.Is(err, ErrInvalidScope):
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid_scope"})
	case errors.Is(err, ErrInvalidRedirect):
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid_request", "error_description": "invalid redirect_uri"})
	case errors.Is(err, ErrUnsupported):
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "unsupported_grant_type"})
	case errors.Is(err, ErrNoBot):
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid_scope", "error_description": err.Error()})
	case errors.Is(err, ErrNoSpace), errors.Is(err, ErrInvalidBody), errors.Is(err, spaces.ErrInvalidBot):
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid_request", "error_description": err.Error()})
	case errors.Is(err, ErrBotPrivate), errors.Is(err, spaces.ErrMissingPerm), errors.Is(err, spaces.ErrNotMember),
		errors.Is(err, spaces.ErrBanned), errors.Is(err, spaces.ErrPermissionEscalation):
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "access_denied", "error_description": err.Error()})
	case errors.Is(err, spaces.ErrSpaceNotFound):
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "invalid_request", "error_description": err.Error()})
	default:
		logger.Err("oauth", err, nil)
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "server_error"})
	}
}

// flexInt64 accepts a JSON number or a numeric string, the way ids and permission masks
// arrive from JavaScript clients (a string keeps 64-bit values exact).
type flexInt64 int64

func (f *flexInt64) UnmarshalJSON(b []byte) error {
	t := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if t == "" || t == "null" {
		return nil
	}
	v, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return err
	}
	*f = flexInt64(v)
	return nil
}

// parsePermissions reads a permissions query/body value; absent or unparsable is 0.
func parsePermissions(raw string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
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
	view, err := h.svc.AuthorizeInfo(c.Context(), user.ID, clientID, c.Query("redirect_uri"), c.Query("scope"), parsePermissions(c.Query("permissions")))
	if err != nil {
		return oauthError(c, err)
	}
	out := fiber.Map{
		"application": applications.PublicJSON(view.App, view.Bot),
		"scopes":      view.Scopes,
	}
	if view.Bot != nil {
		out["bot"] = botPublic(view.Bot)
		out["permissions"] = view.Permissions
		targets := make([]fiber.Map, 0, len(view.Targets))
		for _, t := range view.Targets {
			row := fiber.Map{
				"id":                    id.Format(t.Space.ID),
				"name":                  t.Space.Name,
				"name_acronym":          t.Space.NameAcronym,
				"icon":                  t.Space.Icon,
				"grantable_permissions": t.Grantable,
			}
			if t.Space.Federation != nil {
				// A space hosted on another instance: the bot is added through its origin.
				row["hosted_on"] = t.Space.Federation.OriginDomain
			}
			targets = append(targets, row)
		}
		out["spaces"] = targets
	}
	return c.JSON(out)
}

// Authorize POST /oauth2/authorize - the user has consented; install the bot if that was
// asked, issue a code, and hand back the redirect the client page should send the browser
// to. A bare bot install (scope=bot, no redirect_uri) has no redirect: `location` is ""
// and `space_id` says where the bot went.
func (h *Handler) Authorize(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	var in struct {
		ClientID     string    `json:"client_id"`
		RedirectURI  string    `json:"redirect_uri"`
		Scope        string    `json:"scope"`
		State        string    `json:"state"`
		ResponseType string    `json:"response_type"`
		SpaceID      flexInt64 `json:"space_id"`
		Permissions  flexInt64 `json:"permissions"`
		// PKCE (optional): passed straight through from the authorization URL.
		CodeChallenge       string `json:"code_challenge"`
		CodeChallengeMethod string `json:"code_challenge_method"`
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
	res, err := h.svc.Authorize(c.Context(), user.ID, clientID, in.RedirectURI, in.Scope, int64(in.SpaceID), int64(in.Permissions), in.CodeChallenge, in.CodeChallengeMethod)
	if err != nil {
		return oauthError(c, err)
	}
	out := fiber.Map{"location": ""}
	if res.Installed {
		out["space_id"] = id.Format(res.SpaceID)
		out["permissions"] = res.Permissions
	}
	if res.Code != "" {
		q := url.Values{}
		q.Set("code", res.Code)
		if in.State != "" {
			q.Set("state", in.State)
		}
		if res.Installed {
			q.Set("space_id", id.Format(res.SpaceID))
			q.Set("permissions", strconv.FormatInt(res.Permissions, 10))
		}
		sep := "?"
		if strings.Contains(in.RedirectURI, "?") {
			sep = "&"
		}
		out["location"] = strings.TrimSpace(in.RedirectURI) + sep + q.Encode()
	}
	return c.JSON(out)
}

// Token POST /oauth2/token - the OAuth2 token endpoint. No auth middleware: the client
// authenticates with its id and secret, either in the body or as HTTP Basic. Accepts
// form or JSON.
func (h *Handler) Token(c fiber.Ctx) error {
	get := tokenParams(c)
	clientID, secret, err := clientCredentials(c, get)
	if err != nil {
		return oauthError(c, err)
	}
	var res *TokenResult
	switch get("grant_type") {
	case "authorization_code":
		res, err = h.svc.Exchange(c.Context(), clientID, secret, get("code"), get("redirect_uri"), get("code_verifier"))
	case "refresh_token":
		res, err = h.svc.Refresh(c.Context(), clientID, secret, get("refresh_token"))
	default:
		return oauthError(c, ErrUnsupported)
	}
	if err != nil {
		return oauthError(c, err)
	}
	c.Set("Cache-Control", "no-store")
	return c.JSON(fiber.Map{
		"access_token":  res.AccessToken,
		"token_type":    "Bearer",
		"expires_in":    res.ExpiresIn,
		"refresh_token": res.RefreshToken,
		"scope":         strings.Join(res.Scopes, " "),
	})
}

// Revoke POST /oauth2/token/revoke - RFC 7009. Client-authenticated like Token; the
// `token` may be an access or a refresh token, and an unknown one still gets a 200.
func (h *Handler) Revoke(c fiber.Ctx) error {
	get := tokenParams(c)
	clientID, secret, err := clientCredentials(c, get)
	if err != nil {
		return oauthError(c, err)
	}
	if err := h.svc.Revoke(c.Context(), clientID, secret, get("token")); err != nil {
		return oauthError(c, err)
	}
	return c.JSON(fiber.Map{})
}

// Me GET /oauth2/@me - describes the access token making the request: the application it
// belongs to, its scopes and expiry, and the user (with identify).
func (h *Handler) Me(c fiber.Ctx) error {
	raw := strings.TrimSpace(strings.TrimPrefix(c.Get("Authorization"), "Bearer "))
	info, err := h.svc.Introspect(c.Context(), raw)
	if err != nil {
		if errors.Is(err, ErrInvalidGrant) || errors.Is(err, ErrInvalidClient) {
			return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_token"})
		}
		return oauthError(c, err)
	}
	out := fiber.Map{
		"application": applications.PublicJSON(info.App, nil),
		"scopes":      info.Scopes,
		"expires":     info.ExpiresAt,
	}
	if info.User != nil {
		out["user"] = fiber.Map{
			"id":            id.Format(info.User.ID),
			"username":      info.User.Username,
			"discriminator": fmt.Sprintf("%04d", info.User.Discriminator),
			"display_name":  info.User.DisplayName,
			"avatar":        info.User.Avatar,
			"public_flags":  auth.PublicFlags(info.User),
		}
	}
	return c.JSON(out)
}

// Grants GET /oauth2/@me/grants - the apps this account has authorised.
func (h *Handler) Grants(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	grants, err := h.svc.ListGrants(c.Context(), user.ID)
	if err != nil {
		return oauthError(c, err)
	}
	out := make([]fiber.Map, 0, len(grants))
	for _, g := range grants {
		m := fiber.Map{
			"application_id": id.Format(g.Grant.ApplicationID),
			"scopes":         g.Grant.Scopes,
			"created_at":     g.Grant.CreatedAt,
		}
		if g.App != nil {
			m["application"] = applications.PublicJSON(g.App, nil)
		}
		out = append(out, m)
	}
	return c.JSON(out)
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
		return oauthError(c, err)
	}
	return c.SendStatus(http.StatusNoContent)
}

// AddSpaceMember PUT /spaces/:id/members/:user_id - the `spaces.join` scope: the caller
// (a bot with Create Invite, or a member with it) adds a user who authorised the app,
// proving it with that user's access token in the body. 201 when added, 204 when they
// were already a member.
func (h *Handler) AddSpaceMember(c fiber.Ctx) error {
	actor := auth.GetUser(c)
	if actor == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	userID, err := id.Parse(c.Params("user_id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}
	var in struct {
		AccessToken string `json:"access_token"`
	}
	if len(c.Body()) == 0 || json.Unmarshal(c.Body(), &in) != nil || strings.TrimSpace(in.AccessToken) == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid_request", "error_description": "access_token required"})
	}
	added, err := h.svc.AddSpaceMember(c.Context(), actor.ID, spaceID, userID, strings.TrimSpace(in.AccessToken))
	if err != nil {
		if errors.Is(err, ErrInvalidGrant) {
			return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "invalid_grant", "error_description": "the access token is not this user's or lacks the spaces.join scope"})
		}
		return oauthError(c, err)
	}
	if !added {
		return c.SendStatus(http.StatusNoContent)
	}
	return c.Status(http.StatusCreated).JSON(fiber.Map{"space_id": id.Format(spaceID), "user_id": id.Format(userID)})
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

// clientCredentials takes the client id and secret from HTTP Basic auth (RFC 6749 §2.3.1)
// when present, otherwise from the body.
func clientCredentials(c fiber.Ctx, get func(string) string) (int64, string, error) {
	rawID, secret := get("client_id"), get("client_secret")
	if h := c.Get("Authorization"); strings.HasPrefix(h, "Basic ") {
		dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(strings.TrimPrefix(h, "Basic ")))
		if err != nil {
			return 0, "", ErrInvalidClient
		}
		u, p, ok := strings.Cut(string(dec), ":")
		if !ok {
			return 0, "", ErrInvalidClient
		}
		if rawID, err = url.QueryUnescape(u); err != nil {
			return 0, "", ErrInvalidClient
		}
		if secret, err = url.QueryUnescape(p); err != nil {
			return 0, "", ErrInvalidClient
		}
	}
	clientID, err := id.Parse(rawID)
	if err != nil {
		return 0, "", ErrInvalidClient
	}
	return clientID, secret, nil
}

func botPublic(u *auth.User) fiber.Map {
	return fiber.Map{
		"id":            id.Format(u.ID),
		"username":      u.Username,
		"discriminator": fmt.Sprintf("%04d", u.Discriminator),
		"display_name":  u.DisplayName,
		"avatar":        u.Avatar,
		"bot":           true,
	}
}
