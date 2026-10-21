package spaces

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/nebula"
)

// memberTarget reads :userId, where "@me" is the caller (Discord's spelling for your own
// member). The app runs with UnescapePath off, so a client that encodes the "@" is decoded.
func memberTarget(c fiber.Ctx, caller int64) (int64, bool) {
	raw := c.Params("userId")
	if decoded, err := url.PathUnescape(raw); err == nil {
		raw = decoded
	}
	if raw == "@me" {
		return caller, true
	}
	uid, err := id.Parse(raw)
	if err != nil {
		return 0, false
	}
	return uid, true
}

// GetMember GET /spaces/:id/members/:userId - one member object, for a member of the space.
func (h *Handler) GetMember(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	targetID, ok := memberTarget(c, user.ID)
	if !ok {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}
	m, u, err := h.svc.GetMemberWithUser(c.Context(), user.ID, spaceID, targetID)
	if err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	return c.JSON(h.svc.memberJSON(m, u))
}

// PatchMember PATCH /spaces/:id/members/:userId - set your own space profile (any of nick,
// about_me, bio, pronouns; avatar and banner only cleared, with null), or, as a moderator,
// another member's nickname and clearing the rest of their space profile.
func (h *Handler) PatchMember(c fiber.Ctx) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	targetID, ok := memberTarget(c, user.ID)
	if !ok {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid user id"})
	}
	patch, err := parseMemberProfileBody(c.Body())
	if err != nil {
		// An image for someone else is refused as not yours to set, before "upload it instead".
		if errors.Is(err, ErrInvalidProfileImage) && targetID != user.ID {
			return notOwnProfile(c)
		}
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	m, err := h.svc.UpdateMemberProfile(c.Context(), user.ID, spaceID, targetID, patch)
	if err != nil {
		if errors.Is(err, ErrNotOwnProfile) {
			return notOwnProfile(c)
		}
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	u := user
	if targetID != user.ID {
		if u, err = h.svc.userRepo.GetByID(c.Context(), targetID); err != nil {
			return spaceError(c, err, map[string]any{"space_id": spaceID})
		}
		if u == nil {
			return spaceError(c, ErrMemberNotFound, nil)
		}
	}
	return c.JSON(h.svc.memberJSON(m, u))
}

// notOwnProfile is the 403 for setting a value on someone else's space profile, with a code
// a client can branch on.
func notOwnProfile(c fiber.Ctx) error {
	return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": ErrNotOwnProfile.Error(), "code": "cannot_edit_member_profile"})
}

// PostMemberAvatar POST /spaces/:id/members/@me/avatar (multipart field "file").
func (h *Handler) PostMemberAvatar(c fiber.Ctx) error { return h.postMemberImage(c, "avatar") }

// PostMemberBanner POST /spaces/:id/members/@me/banner (multipart field "file").
func (h *Handler) PostMemberBanner(c fiber.Ctx) error { return h.postMemberImage(c, "banner") }

// postMemberImage stores a space avatar or banner on this instance's Nebula and points the
// caller's space profile at it - PostSpaceIcon's and users.uploadProfileImage's shape. The key
// is spaces/members/<space>/<user>/<kind>-<random>.<ext>, so the image can be told apart as
// this member's (ownsMemberBlobURL) and deleted when it is replaced, cleared or the member
// leaves. On a mirror the image stays on the mirror's Nebula and its URL goes to the origin.
func (h *Handler) postMemberImage(c fiber.Ctx, kind string) error {
	user := auth.GetUser(c)
	if user == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	cfg := h.cfg
	if cfg == nil || strings.TrimSpace(cfg.Nebula.BaseURL) == "" || strings.TrimSpace(cfg.Nebula.UploadSecret) == "" {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "file uploads are not configured"})
	}
	spaceID, err := id.Parse(c.Params("id"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid space id"})
	}
	// Membership first, upload second: a stranger must not be able to fill our CDN. No
	// permission bit is needed - Discord gates only the nickname.
	if err := h.svc.AssertMember(c.Context(), user.ID, spaceID); err != nil {
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}

	fh, err := c.FormFile("file")
	if err != nil || fh == nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "missing file field"})
	}
	maxBytes := cfg.Nebula.AvatarMaxBytes
	if kind == "banner" && cfg.Nebula.BannerMaxBytes > 0 {
		maxBytes = cfg.Nebula.BannerMaxBytes
	}
	if fh.Size <= 0 || fh.Size > int64(maxBytes) {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid file size"})
	}
	ext := strings.ToLower(path.Ext(fh.Filename))
	if ext == "" {
		ext = ".png"
	}
	mime := spaceIconMIME[ext]
	if mime == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "unsupported image type (use jpg, png, gif, or webp)"})
	}
	src, err := fh.Open()
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "could not read file"})
	}
	defer src.Close()

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		logger.Err("spaces", err, map[string]any{"space_id": spaceID})
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
	key := memberBlobPrefix(spaceID, user.ID) + kind + "-" + hex.EncodeToString(suffix[:]) + ext
	if err := nebula.Put(c.Context(), cfg.Nebula.BaseURL, cfg.Nebula.UploadSecret, key, src, mime, fh.Size); err != nil {
		logger.Err("spaces", err, map[string]any{"space_id": spaceID, "key": key})
		return c.Status(http.StatusBadGateway).JSON(fiber.Map{"error": "could not store image"})
	}

	imageURL := nebula.PublicBase(cfg.Nebula.BaseURL, cfg.Nebula.PublicURL) + "/v1/" + key
	patch := &MemberProfilePatch{}
	if kind == "banner" {
		patch.Banner = &imageURL
	} else {
		patch.Avatar = &imageURL
	}
	m, err := h.svc.UpdateMemberProfile(c.Context(), user.ID, spaceID, user.ID, patch)
	if err != nil {
		// Delete the new image only when the write was refused outright. A timeout, an
		// unreachable origin or a 5xx may have landed anyway, and deleting then would leave
		// the profile pointing at a missing image; an orphaned image is the lesser harm.
		if profileWriteRefused(err) {
			if delErr := nebula.Delete(c.Context(), cfg.Nebula.BaseURL, cfg.Nebula.UploadSecret, key); delErr != nil {
				logger.Err("spaces", delErr, map[string]any{"space_id": spaceID, "key": key})
			}
		}
		return spaceError(c, err, map[string]any{"space_id": spaceID})
	}
	return c.JSON(h.svc.memberJSON(m, user))
}

// profileWriteRefused reports whether err means a space-profile write certainly did not
// happen: refused by validation, permissions or membership, or lost to a concurrent change
// after its retry. Anything else (storage timeouts, federation failures) is ambiguous.
func profileWriteRefused(err error) bool {
	for _, refused := range []error{
		ErrMemberNotFound, ErrNotMember, ErrSpaceNotFound, ErrInvalidNickname, ErrInvalidMemberAboutMe,
		ErrInvalidMemberBio, ErrInvalidMemberPronouns, ErrInvalidProfileImage, ErrCannotChangeNickname,
		ErrCannotChangeOwnerNickname, ErrNotOwnProfile, ErrCannotResetOwnerProfile, ErrMemberProfileChanged,
	} {
		if errors.Is(err, refused) {
			return true
		}
	}
	return false
}
