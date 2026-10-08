package instance

import (
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

// moderationError maps the moderation errors; anything unknown falls through to errorFor.
func moderationError(c fiber.Ctx, err error) error {
	switch err {
	case ErrUserNotFound, ErrSpaceNotFound, ErrReportNotFound, ErrNotBanned, ErrIPBanNotFound:
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
	case ErrCannotBanSelf, ErrCannotBanRemote, ErrReportSelf:
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": err.Error()})
	case ErrAlreadyBanned, ErrReportClosed, ErrDuplicateReport, ErrIPBanExists, ErrEmailTaken:
		return c.Status(http.StatusConflict).JSON(fiber.Map{"error": err.Error()})
	case ErrInvalidBan, ErrInvalidReport, ErrInvalidAction, ErrInvalidQuery, ErrInvalidBadges, ErrInvalidCIDR, ErrCIDRTooWide,
		ErrInvalidEmail, ErrBotAccount, ErrInvalidNotice, ErrCannotNotice, ErrNoticesUnavailable:
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	case ErrRecoveryUnavailable, ErrFederationDisabled:
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": err.Error()})
	case ErrInvalidPolicyKind, ErrInvalidPeerDomain:
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	default:
		return errorFor(c, err)
	}
}

// userSummaryJSON is the public shape - what the client already knows a user as.
func userSummaryJSON(u *auth.User) fiber.Map {
	if u == nil {
		return nil
	}
	return fiber.Map{
		"id":           id.Format(u.ID),
		"username":     u.Username,
		"display_name": u.DisplayName,
		"avatar":       u.Avatar,
		"home_domain":  u.HomeDomain,
		"public_flags": auth.PublicFlags(u),
		"bot":          u.Bot,
	}
}

// userAdminJSON adds what only an administrator should see.
func userAdminJSON(u *auth.User) fiber.Map {
	m := userSummaryJSON(u)
	if m == nil {
		return nil
	}
	m["email"] = u.Email
	m["created_at"] = u.CreatedAt
	m["bot"] = u.Bot
	m["verified_email"] = u.VerifiedEmail
	return m
}

func spaceSummaryJSON(sp *spaces.Space) fiber.Map {
	if sp == nil {
		return nil
	}
	return fiber.Map{
		"id":         id.Format(sp.ID),
		"name":       sp.Name,
		"icon":       sp.Icon,
		"owner_id":   id.Format(sp.OwnerID),
		"official":   sp.Official,
		"created_at": sp.CreatedAt,
	}
}

func parseIDParam(c fiber.Ctx, name string) (int64, bool) {
	n, err := id.Parse(c.Params(name))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func decodeBody(c fiber.Ctx, out interface{}) bool {
	if len(c.Body()) == 0 {
		return true
	}
	return json.Unmarshal(c.Body(), out) == nil
}

// Stats GET /instance/stats
func (h *Handler) Stats(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	st, err := h.svc.GetStats(c.Context(), user.ID)
	if err != nil {
		return moderationError(c, err)
	}
	return c.JSON(st)
}

// SearchUsers GET /instance/users?q=
func (h *Handler) SearchUsers(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	users, err := h.svc.SearchUsers(c.Context(), user.ID, c.Query("q"))
	if err != nil {
		return moderationError(c, err)
	}
	out := make([]fiber.Map, 0, len(users))
	for _, u := range users {
		out = append(out, userAdminJSON(u))
	}
	return c.JSON(out)
}

// GetUser GET /instance/users/:id
func (h *Handler) GetUser(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	uid, ok := parseIDParam(c, "id")
	if !ok {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}
	d, err := h.svc.GetUserDetail(c.Context(), user.ID, uid)
	if err != nil {
		return moderationError(c, err)
	}
	sessions := make([]fiber.Map, 0, len(d.Sessions))
	for _, s := range d.Sessions {
		sessions = append(sessions, fiber.Map{
			"session_id": id.Format(s.SessionID),
			"created_at": s.CreatedAt,
			"expires_at": s.ExpiresAt,
			"ip_address": s.IPAddress,
			"user_agent": s.UserAgent,
		})
	}
	sps := make([]fiber.Map, 0, len(d.Spaces))
	for _, sp := range d.Spaces {
		sps = append(sps, spaceSummaryJSON(sp))
	}
	if d.Reports == nil {
		d.Reports = []ReportSummary{}
	}
	return c.JSON(fiber.Map{
		"user":           userAdminJSON(d.User),
		"ban":            d.Ban,
		"instance_admin": d.InstanceAdmin,
		"sessions":       sessions,
		"spaces":         sps,
		"reports":        d.Reports,
	})
}

// BanUser POST /instance/users/:id/ban
func (h *Handler) BanUser(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	uid, ok := parseIDParam(c, "id")
	if !ok {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}
	var in BanInput
	if !decodeBody(c, &in) {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	b, err := h.svc.BanUser(c.Context(), user.ID, uid, in)
	if err != nil {
		return moderationError(c, err)
	}
	return c.Status(http.StatusCreated).JSON(b)
}

// SetBadges PATCH /instance/users/:id/badges
func (h *Handler) SetBadges(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	uid, ok := parseIDParam(c, "id")
	if !ok {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}
	var in struct {
		Flags int `json:"flags"`
	}
	if !decodeBody(c, &in) {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	flags, err := h.svc.SetUserBadges(c.Context(), user.ID, uid, in.Flags)
	if err != nil {
		return moderationError(c, err)
	}
	return c.JSON(fiber.Map{"public_flags": flags})
}

// SetEmail PATCH /instance/users/:id/email - admin moves an account to a new address.
func (h *Handler) SetEmail(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	uid, ok := parseIDParam(c, "id")
	if !ok {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}
	var in struct {
		Email string `json:"email"`
	}
	if !decodeBody(c, &in) {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	updated, err := h.svc.SetUserEmail(c.Context(), user.ID, uid, in.Email)
	if err != nil {
		return moderationError(c, err)
	}
	return c.JSON(userAdminJSON(updated))
}

// SendNotice POST /instance/users/:id/notice - admin sends a free-text official message to
// a user, delivered as a direct message from the instance's official account.
func (h *Handler) SendNotice(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	uid, ok := parseIDParam(c, "id")
	if !ok {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}
	var in struct {
		Message string `json:"message"`
	}
	if !decodeBody(c, &in) {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	if err := h.svc.SendUserNotice(c.Context(), user.ID, uid, in.Message); err != nil {
		return moderationError(c, err)
	}
	return c.SendStatus(http.StatusNoContent)
}

// RegenerateRecoveryCodes POST /instance/users/:id/recovery_codes - admin regenerates a user's
// 2FA recovery codes and emails them; when email is off they come back in the response for the
// admin to hand over.
func (h *Handler) RegenerateRecoveryCodes(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	uid, ok := parseIDParam(c, "id")
	if !ok {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}
	codes, emailed, err := h.svc.RegenerateUserRecoveryCodes(c.Context(), user.ID, uid)
	if err != nil {
		return moderationError(c, err)
	}
	return c.JSON(fiber.Map{"codes": codes, "emailed": emailed})
}

// SetSpaceOfficial PATCH /instance/spaces/:id/official - mark a space as part of this
// instance (or not). Admin only.
func (h *Handler) SetSpaceOfficial(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	sid, ok := parseIDParam(c, "id")
	if !ok {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	var in struct {
		Official bool `json:"official"`
	}
	if !decodeBody(c, &in) {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	sp, err := h.svc.SetSpaceOfficial(c.Context(), user.ID, sid, in.Official)
	if err != nil {
		return moderationError(c, err)
	}
	return c.JSON(fiber.Map{"official": sp.Official})
}

// UnbanUser DELETE /instance/users/:id/ban
func (h *Handler) UnbanUser(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	uid, ok := parseIDParam(c, "id")
	if !ok {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}
	if err := h.svc.UnbanUser(c.Context(), user.ID, uid); err != nil {
		return moderationError(c, err)
	}
	return c.SendStatus(http.StatusNoContent)
}

// ListBans GET /instance/bans
func (h *Handler) ListBans(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	bans, err := h.svc.ListBans(c.Context(), user.ID)
	if err != nil {
		return moderationError(c, err)
	}
	// The bans page shows who, not just which id: hydrate the users in one read.
	ids := make([]int64, 0, len(bans))
	for _, b := range bans {
		ids = append(ids, b.UserID)
	}
	byID := map[int64]*auth.User{}
	if len(ids) > 0 && h.svc.mod != nil {
		if users, err := h.svc.mod.Users.GetByIDs(c.Context(), ids); err == nil {
			for _, u := range users {
				byID[u.ID] = u
			}
		}
	}
	out := make([]fiber.Map, 0, len(bans))
	for _, b := range bans {
		out = append(out, fiber.Map{"ban": b, "user": userSummaryJSON(byID[b.UserID])})
	}
	return c.JSON(out)
}

// GetSpace GET /instance/spaces/:id
func (h *Handler) GetSpace(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	sid, ok := parseIDParam(c, "id")
	if !ok {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	d, err := h.svc.GetSpaceDetail(c.Context(), user.ID, sid)
	if err != nil {
		return moderationError(c, err)
	}
	if d.Reports == nil {
		d.Reports = []ReportSummary{}
	}
	return c.JSON(fiber.Map{
		"space":        spaceSummaryJSON(d.Space),
		"owner":        userSummaryJSON(d.Owner),
		"member_count": d.MemberCount,
		"reports":      d.Reports,
	})
}

// TakeDownSpace DELETE /instance/spaces/:id
func (h *Handler) TakeDownSpace(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	sid, ok := parseIDParam(c, "id")
	if !ok {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if !decodeBody(c, &in) {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	if err := h.svc.TakeDownSpace(c.Context(), user.ID, sid, in.Reason); err != nil {
		return moderationError(c, err)
	}
	return c.SendStatus(http.StatusNoContent)
}

// CreateReport POST /reports - any signed-in account.
func (h *Handler) CreateReport(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	var in CreateReportInput
	if len(c.Body()) == 0 || json.Unmarshal(c.Body(), &in) != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	rep, err := h.svc.CreateReport(c.Context(), user.ID, in)
	if err != nil {
		return moderationError(c, err)
	}
	return c.Status(http.StatusCreated).JSON(rep)
}

// ListReports GET /instance/reports?status=open
func (h *Handler) ListReports(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	reports, err := h.svc.ListReports(c.Context(), user.ID, c.Query("status", ReportOpen))
	if err != nil {
		return moderationError(c, err)
	}
	// Names for the queue rows, in one read per side.
	ids := make([]int64, 0, len(reports)*2)
	for _, r := range reports {
		ids = append(ids, r.ReporterID)
		if r.TargetType == TargetUser {
			ids = append(ids, r.TargetID)
		}
	}
	byID := map[int64]*auth.User{}
	if len(ids) > 0 && h.svc.mod != nil {
		if users, err := h.svc.mod.Users.GetByIDs(c.Context(), ids); err == nil {
			for _, u := range users {
				byID[u.ID] = u
			}
		}
	}
	out := make([]fiber.Map, 0, len(reports))
	for i := range reports {
		r := &reports[i]
		row := fiber.Map{"report": r, "reporter": userSummaryJSON(byID[r.ReporterID])}
		if r.TargetType == TargetUser {
			row["target_user"] = userSummaryJSON(byID[r.TargetID])
		} else if h.svc.mod != nil {
			sp, _ := h.svc.mod.Spaces.GetByID(c.Context(), r.TargetID)
			row["target_space"] = spaceSummaryJSON(sp)
		}
		out = append(out, row)
	}
	return c.JSON(out)
}

// GetReport GET /instance/reports/:id
func (h *Handler) GetReport(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	rid, ok := parseIDParam(c, "id")
	if !ok {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid report id"})
	}
	d, err := h.svc.GetReport(c.Context(), user.ID, rid)
	if err != nil {
		return moderationError(c, err)
	}
	out := fiber.Map{
		"report":       d.Report,
		"reporter":     userSummaryJSON(d.Reporter),
		"target_user":  userSummaryJSON(d.TargetUser),
		"target_space": spaceSummaryJSON(d.TargetSpace),
	}
	if d.Room != nil {
		out["room"] = fiber.Map{"id": id.Format(d.Room.ID), "name": d.Room.Name, "type": d.Room.Type}
	}
	if d.Message != nil {
		msg := fiber.Map{
			"id":         id.Format(d.Message.ID),
			"sender_id":  id.Format(d.Message.SenderID),
			"created_at": d.Message.CreatedAt,
			"readable":   d.MessageReadable,
			"deleted":    d.Message.DeletedAt != nil,
		}
		if d.MessageReadable {
			msg["text"] = d.Message.Plaintext
		}
		out["message"] = msg
	}
	return c.JSON(out)
}

// ResolveReport POST /instance/reports/:id/resolve
func (h *Handler) ResolveReport(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	rid, ok := parseIDParam(c, "id")
	if !ok {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid report id"})
	}
	var in ResolveInput
	if !decodeBody(c, &in) {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	rep, err := h.svc.ResolveReport(c.Context(), user.ID, rid, in)
	if err != nil {
		return moderationError(c, err)
	}
	return c.JSON(rep)
}

// ListAudit GET /instance/audit
func (h *Handler) ListAudit(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	entries, err := h.svc.ListAudit(c.Context(), user.ID)
	if err != nil {
		return moderationError(c, err)
	}
	ids := make([]int64, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ActorID)
	}
	byID := map[int64]*auth.User{}
	if len(ids) > 0 && h.svc.mod != nil {
		if users, err := h.svc.mod.Users.GetByIDs(c.Context(), ids); err == nil {
			for _, u := range users {
				byID[u.ID] = u
			}
		}
	}
	out := make([]fiber.Map, 0, len(entries))
	for i := range entries {
		e := &entries[i]
		out = append(out, fiber.Map{"entry": e, "actor": userSummaryJSON(byID[e.ActorID])})
	}
	return c.JSON(out)
}

// --------------------------------------------------------------- federation policy ----

func (h *Handler) ListFederationPolicy(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	v, err := h.svc.ListFederationPolicy(c.Context(), user.ID)
	if err != nil {
		return moderationError(c, err)
	}
	allow := make([]fiber.Map, 0)
	block := make([]fiber.Map, 0)
	for _, e := range v.Entries {
		row := fiber.Map{"domain": e.Domain, "created_at": e.CreatedAt, "added_by": id.Format(e.AddedBy)}
		if e.Kind == PolicyKindBlock {
			block = append(block, row)
		} else {
			allow = append(allow, row)
		}
	}
	return c.JSON(fiber.Map{
		"enabled": v.Enabled,
		"domain":  v.Domain,
		// Editable, admin-managed entries.
		"allow": allow,
		"block": block,
		// Read-only entries from FEDERATION_ALLOWLIST / FEDERATION_BLOCKLIST (env), shown so the
		// admin understands the full effective policy; a non-empty allowlist means allowlist mode.
		"env_allow": v.EnvAllow,
		"env_block": v.EnvBlock,
	})
}

func (h *Handler) SetFederationPolicy(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	var in PeerPolicyInput
	if !decodeBody(c, &in) {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	if err := h.svc.SetFederationPolicy(c.Context(), user.ID, in.Domain, in.Kind); err != nil {
		return moderationError(c, err)
	}
	return c.SendStatus(http.StatusNoContent)
}

func (h *Handler) RemoveFederationPolicy(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	domain, derr := url.PathUnescape(c.Params("domain"))
	if derr != nil {
		domain = c.Params("domain")
	}
	if err := h.svc.RemoveFederationPolicy(c.Context(), user.ID, domain); err != nil {
		return moderationError(c, err)
	}
	return c.SendStatus(http.StatusNoContent)
}
