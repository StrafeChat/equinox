package spaces

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/nebula"
	"github.com/StrafeChat/equinox/internal/safego"
)

// Space profiles (Discord's server profiles). In each space a member may set a nickname and
// their own avatar, banner, About Me, bio and pronouns; each one replaces the global profile
// inside that space only. The values live on the member row (space_members), so leaving,
// being removed or the space being deleted takes the whole space profile with it - nothing
// about a former member's space profile is kept.
//
// Who may change what, as on Discord: a member sets their own profile; the nickname needs
// Change Nickname (or Manage Nicknames). A moderator with Manage Nicknames who outranks the
// member may change their nickname and reset - clear, never set - the rest of their space
// profile, for when someone puts something abusive in it. Only the owner touches the owner's.
// A moderator's changes are audit-logged; a member's own never are.
//
// The REST member object carries the space profile as its own keys (nick, space_avatar, ...)
// beside the unchanged global ones, so a client's global-profile update can never overwrite
// a space value, and every SPACE_MEMBER_UPDATE is the member's full state.

const (
	MaxNicknameRunes       = 32
	MaxMemberAboutMeRunes  = 190
	MaxMemberBioRunes      = 190
	MaxMemberPronounsRunes = 40
	MaxMemberImageURLRunes = 256
)

var (
	ErrMemberNotFound            = errors.New("member not found")
	ErrInvalidNickname           = errors.New("nickname must be at most 32 characters, on one line")
	ErrInvalidMemberAboutMe      = errors.New("about_me must be at most 190 characters")
	ErrInvalidMemberBio          = errors.New("bio must be at most 190 characters")
	ErrInvalidMemberPronouns     = errors.New("pronouns must be at most 40 characters")
	ErrInvalidProfileImage       = errors.New("avatar and banner must be uploaded, or cleared with null")
	ErrCannotChangeNickname      = errors.New("missing permission to change your nickname in this space")
	ErrCannotChangeOwnerNickname = errors.New("only the space owner can change the owner's nickname")
	ErrNotOwnProfile             = errors.New("only the member can change their own space avatar, banner, pronouns, about me or bio")
	// ErrCannotResetOwnerProfile: someone other than the owner tried to reset (clear) the
	// owner's space avatar, banner, pronouns, About Me or bio. ErrNotOwnProfile (and its
	// cannot_edit_member_profile code) stays reserved for giving those fields a value on
	// another member, which the request alone decides, so a mirror and the origin agree on it.
	ErrCannotResetOwnerProfile = errors.New("only the space owner can change the owner's space profile")
	// ErrMemberProfileChanged: the member's avatar or banner kept changing underneath two
	// attempts at the write (concurrent uploads). Nothing was written; trying again is safe.
	ErrMemberProfileChanged = errors.New("the space profile changed while saving; try again")
)

// errInvalidProfileJSON is parseMemberProfileBody's answer to a body that is not JSON.
var errInvalidProfileJSON = errors.New("invalid json")

// MemberProfile is the space-profile part of a member row. "" = no override.
type MemberProfile struct{ Nick, Avatar, Banner, AboutMe, Bio, Pronouns string }

// Profile is the member's space profile.
func (m *SpaceMember) Profile() MemberProfile {
	return MemberProfile{
		Nick: m.Nickname, Avatar: m.Avatar, Banner: m.Banner,
		AboutMe: m.AboutMe, Bio: m.Bio, Pronouns: m.Pronouns,
	}
}

// MemberProfilePatch: nil = unchanged, pointer to "" = clear.
type MemberProfilePatch struct{ Nick, Avatar, Banner, AboutMe, Bio, Pronouns *string }

// Empty reports whether the patch names no field at all.
func (p *MemberProfilePatch) Empty() bool {
	return p.Nick == nil && p.Avatar == nil && p.Banner == nil && p.AboutMe == nil && p.Bio == nil && p.Pronouns == nil
}

// OnlyNick reports whether the nickname is the one field the patch names.
func (p *MemberProfilePatch) OnlyNick() bool {
	return p.Nick != nil && p.Avatar == nil && p.Banner == nil && p.AboutMe == nil && p.Bio == nil && p.Pronouns == nil
}

// setsNonNickValue reports whether the patch gives any field other than the nickname a value
// (not a clear) - what only the member may do to their own profile. Normalized patches only.
func (p *MemberProfilePatch) setsNonNickValue() bool {
	for _, v := range []*string{p.Avatar, p.Banner, p.AboutMe, p.Bio, p.Pronouns} {
		if v != nil && *v != "" {
			return true
		}
	}
	return false
}

// Apply is old with the patch's fields replaced.
func (p MemberProfilePatch) Apply(old MemberProfile) MemberProfile {
	next := old
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	set(&next.Nick, p.Nick)
	set(&next.Avatar, p.Avatar)
	set(&next.Banner, p.Banner)
	set(&next.AboutMe, p.AboutMe)
	set(&next.Bio, p.Bio)
	set(&next.Pronouns, p.Pronouns)
	return next
}

// profileDiff is the patch that turns old into next, naming only the fields that differ.
func profileDiff(old, next MemberProfile) MemberProfilePatch {
	var p MemberProfilePatch
	pick := func(a, b string) *string {
		if a == b {
			return nil
		}
		return &b
	}
	p.Nick = pick(old.Nick, next.Nick)
	p.Avatar = pick(old.Avatar, next.Avatar)
	p.Banner = pick(old.Banner, next.Banner)
	p.AboutMe = pick(old.AboutMe, next.AboutMe)
	p.Bio = pick(old.Bio, next.Bio)
	p.Pronouns = pick(old.Pronouns, next.Pronouns)
	return p
}

// nilIfEmpty is a CQL NULL (and an omitted audit side) for "".
func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// normalizeMemberProfilePatch trims every set field and validates it. Limits are runes, as
// for the global profile (users/schema.go). An image is "" or an http(s) URL: the upload
// endpoint and a federation op are the only things that set one, and both hand over a URL.
func normalizeMemberProfilePatch(p *MemberProfilePatch) error {
	trim := func(v *string) {
		if v != nil {
			*v = strings.TrimSpace(*v)
		}
	}
	for _, v := range []*string{p.Nick, p.Avatar, p.Banner, p.AboutMe, p.Bio, p.Pronouns} {
		trim(v)
	}
	if p.Nick != nil && *p.Nick != "" && !validNickname(*p.Nick) {
		return ErrInvalidNickname
	}
	if p.AboutMe != nil && utf8.RuneCountInString(*p.AboutMe) > MaxMemberAboutMeRunes {
		return ErrInvalidMemberAboutMe
	}
	if p.Bio != nil && utf8.RuneCountInString(*p.Bio) > MaxMemberBioRunes {
		return ErrInvalidMemberBio
	}
	if p.Pronouns != nil && utf8.RuneCountInString(*p.Pronouns) > MaxMemberPronounsRunes {
		return ErrInvalidMemberPronouns
	}
	for _, v := range []*string{p.Avatar, p.Banner} {
		if v != nil && *v != "" && !validProfileImageURL(*v) {
			return ErrInvalidProfileImage
		}
	}
	return nil
}

// validNickname: at most 32 runes on one line, and something visible. A nickname is shown
// in place of the member's name everywhere in the space, so it must not be able to pass for
// something else: no control characters (newlines, tabs) or Unicode line and paragraph
// separators, no bidi embeddings, overrides or isolates (which reverse how the name - and
// the text after it - reads), and not made only of blanks, invisible formatting characters
// and combining marks with nothing to combine with (an "empty" name). Joiners and variation
// selectors stay allowed beside visible characters: emoji sequences need them.
func validNickname(s string) bool {
	if utf8.RuneCountInString(s) > MaxNicknameRunes {
		return false
	}
	visible := false
	for _, r := range s {
		if forbiddenNicknameRune(r) {
			return false
		}
		if !unicode.IsSpace(r) && !unicode.In(r, unicode.Cf, unicode.Mn, unicode.Me) && !isBlankFiller(r) {
			visible = true
		}
	}
	return visible
}

// forbiddenNicknameRune is what a nickname may never hold anywhere in it: control
// characters, U+2028/U+2029 (which break the line as a newline does) and bidi controls.
func forbiddenNicknameRune(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp) || isBidiControl(r)
}

// isBidiControl is U+202A-202E (embeddings and overrides) and U+2066-2069 (isolates).
func isBidiControl(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

// isBlankFiller: letters and symbols that render as nothing (the Hangul fillers, the blank
// braille pattern) - the usual way to fake an empty name once whitespace is refused.
func isBlankFiller(r rune) bool {
	switch r {
	case 0x115F, 0x1160, 0x3164, 0xFFA0, 0x2800:
		return true
	}
	return false
}

func validProfileImageURL(s string) bool {
	if utf8.RuneCountInString(s) > MaxMemberImageURLRunes {
		return false
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// memberProfileKeys are the PATCH body keys, in the order a patch is read.
var memberProfileKeys = []string{"nick", "avatar", "banner", "about_me", "bio", "pronouns"}

// parseMemberProfileBody decodes PATCH /spaces/:id/members/:userId. Each known key is a
// string (set) or null (clear); an absent key is unchanged and unknown keys are ignored. An
// avatar or banner may only be cleared here - an image is set by uploading it.
func parseMemberProfileBody(body []byte) (*MemberProfilePatch, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, errInvalidProfileJSON
	}
	p := &MemberProfilePatch{}
	targets := map[string]**string{
		"nick": &p.Nick, "avatar": &p.Avatar, "banner": &p.Banner,
		"about_me": &p.AboutMe, "bio": &p.Bio, "pronouns": &p.Pronouns,
	}
	for _, key := range memberProfileKeys {
		v, ok := raw[key]
		if !ok {
			continue
		}
		var s *string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, fmt.Errorf("%s must be a string or null", key)
		}
		if s == nil {
			empty := ""
			s = &empty
		}
		if (key == "avatar" || key == "banner") && strings.TrimSpace(*s) != "" {
			return nil, ErrInvalidProfileImage
		}
		*targets[key] = s
	}
	if p.Empty() {
		return nil, ErrNothingToPatch
	}
	return p, nil
}

// nicknamePermission decides whether actor may change target's nickname - and, by the same
// rule, reset the rest of another member's space profile. The only implementation of it
// (the web client's canEditNicknameOf mirrors it).
// actorBase = SpacePermissionBase(actor); *Highest = highestRolePosition (excl. @everyone, -1 = none).
func nicknamePermission(actorID, targetID, ownerID, actorBase int64, actorHighest, targetHighest int) error {
	if actorID == ownerID {
		return nil // the owner: anyone, themself included
	}
	if targetID == ownerID {
		return ErrCannotChangeOwnerNickname // Administrator is not enough
	}
	admin := permissions.Has(actorBase, permissions.PermAdministrator)
	if actorID == targetID {
		if admin || permissions.Has(actorBase, permissions.PermChangeNickname) || permissions.Has(actorBase, permissions.PermManageNicknames) {
			return nil
		}
		return ErrCannotChangeNickname
	}
	if !admin && !permissions.Has(actorBase, permissions.PermManageNicknames) {
		return ErrMissingPerm
	}
	// Strict, as for kick and ban: an equal rank is refused too.
	if targetHighest >= actorHighest {
		return ErrRoleHierarchy
	}
	return nil
}

// AssertMember is ErrNotMember unless userID is a member of spaceID.
func (s *Service) AssertMember(ctx context.Context, userID, spaceID int64) error {
	ok, err := s.repo.IsMember(ctx, spaceID, userID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotMember
	}
	return nil
}

// GetMemberWithUser is GET /spaces/:id/members/:userId: one member with their profile, for
// a caller in the space. On a mirror it is the local copy.
func (s *Service) GetMemberWithUser(ctx context.Context, actorID, spaceID, targetID int64) (*SpaceMember, *auth.User, error) {
	if err := s.AssertMember(ctx, actorID, spaceID); err != nil {
		return nil, nil, err
	}
	m, err := s.repo.GetMember(ctx, spaceID, targetID)
	if err != nil {
		return nil, nil, err
	}
	if m == nil {
		return nil, nil, ErrMemberNotFound
	}
	u, err := s.userRepo.GetByID(ctx, targetID)
	if err != nil {
		return nil, nil, err
	}
	if u == nil {
		return nil, nil, ErrMemberNotFound
	}
	return m, u, nil
}

// UpdateMemberProfile applies a patch to target's space profile as actor. Yourself: any
// field (the nickname needs Change Nickname or Manage Nicknames). Another member: their
// nickname, and clearing any other field, with Manage Nicknames over them. An unchanged
// result writes nothing and announces nothing.
func (s *Service) UpdateMemberProfile(ctx context.Context, actorID, spaceID, targetID int64, in *MemberProfilePatch) (*SpaceMember, error) {
	if in == nil {
		return nil, ErrNothingToPatch
	}
	if err := normalizeMemberProfilePatch(in); err != nil {
		return nil, err
	}
	if in.Empty() {
		return nil, ErrNothingToPatch
	}
	if origin, actor, err := s.remoteSpace(ctx, actorID, spaceID); err != nil {
		return nil, err
	} else if origin != "" {
		// The origin runs every check below as this member, and the relay its write causes
		// has already updated the row here by the time the call returns.
		if err := s.fed.RemoteUpdateMemberProfile(ctx, origin, spaceID, actor, targetID, in); err != nil {
			return nil, err
		}
		m, err := s.repo.GetMember(ctx, spaceID, targetID)
		if err != nil {
			return nil, err
		}
		if m == nil {
			return nil, ErrMemberNotFound
		}
		return m, nil
	}
	sp, err := s.repo.GetByID(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	if sp == nil {
		return nil, ErrSpaceNotFound
	}
	if err := s.AssertMember(ctx, actorID, spaceID); err != nil {
		return nil, err
	}
	cur, err := s.repo.GetMember(ctx, spaceID, targetID)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, ErrMemberNotFound
	}
	self := actorID == targetID
	if !self && in.setsNonNickValue() {
		return nil, ErrNotOwnProfile
	}

	// Write only what changes, tied to the membership just read. A miss with the member
	// still in place means an image changed underneath (another upload); one retry from a
	// fresh read settles it, so what gets released below is what was really overwritten.
	var before, next MemberProfile
	for attempt := 0; ; attempt++ {
		// Anything on another member needs Manage Nicknames over them; on yourself only a new
		// nickname needs a bit (re-sending the one you have, as a whole-form save does, is not
		// a change). Checked against the row this attempt writes: a retry's fresh read may
		// hold a nickname a moderator just set (which re-sending the old one would undo) or
		// different roles.
		if !self || (in.Nick != nil && *in.Nick != strings.TrimSpace(cur.Nickname)) {
			if err := s.memberProfilePermission(ctx, sp, actorID, cur, in); err != nil {
				return nil, err
			}
		}
		before = cur.Profile()
		next = in.Apply(before)
		if next == before {
			return cur, nil
		}
		applied, err := s.repo.UpdateMemberProfile(ctx, spaceID, targetID, cur, profileDiff(before, next))
		if err != nil {
			return nil, err
		}
		if applied {
			break
		}
		fresh, err := s.repo.GetMember(ctx, spaceID, targetID)
		if err != nil {
			return nil, err
		}
		if fresh == nil || !fresh.JoinedAt.Equal(cur.JoinedAt) {
			// Left (or left and came back): the membership the checks were made for is gone.
			return nil, ErrMemberNotFound
		}
		if attempt >= 1 {
			return nil, ErrMemberProfileChanged
		}
		cur = fresh
	}
	s.releaseMemberBlobs(ctx, spaceID, targetID, before, next)

	m, err := s.repo.GetMember(ctx, spaceID, targetID)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, ErrMemberNotFound
	}
	s.publishMemberUpdate(ctx, spaceID, m)
	s.fedMemberUpdated(ctx, spaceID, targetID)
	if !self {
		if before.Nick != next.Nick {
			s.audit(ctx, spaceID, actorID, AuditMemberNickUpdate, id.Format(targetID), map[string]change{
				"nick": {Old: nilIfEmpty(before.Nick), New: nilIfEmpty(next.Nick)},
			}, "")
		}
		if fields := resetFields(before, next); len(fields) > 0 {
			s.auditFields(ctx, spaceID, actorID, AuditMemberProfileReset, id.Format(targetID), fields, "")
		}
	}
	return m, nil
}

// memberProfilePermission applies nicknamePermission to a patch on target. A reset of the
// owner's profile that does not touch the nickname is refused as what it is - a change to
// the owner's profile - rather than with the nickname-specific error.
func (s *Service) memberProfilePermission(ctx context.Context, sp *Space, actorID int64, target *SpaceMember, in *MemberProfilePatch) error {
	base, err := s.SpacePermissionBase(ctx, actorID, sp.ID)
	if err != nil {
		return err
	}
	actorHighest, targetHighest := -1, -1
	if actorID != target.UserID && actorID != sp.OwnerID {
		everyoneID, byID, highest, err := s.memberRoleContext(ctx, sp.ID, actorID)
		if err != nil {
			return err
		}
		actorHighest, targetHighest = highest, highestRolePosition(target.RoleIDs, everyoneID, byID)
	}
	err = nicknamePermission(actorID, target.UserID, sp.OwnerID, base, actorHighest, targetHighest)
	if errors.Is(err, ErrCannotChangeOwnerNickname) && in.Nick == nil {
		return ErrCannotResetOwnerProfile
	}
	return err
}

// resetFields names the non-nickname fields set in before and cleared in next, as the
// PATCH body spells them - the member_profile_reset audit entry.
func resetFields(before, next MemberProfile) []string {
	var out []string
	for _, f := range []struct {
		key        string
		was, isNow string
	}{
		{"avatar", before.Avatar, next.Avatar},
		{"banner", before.Banner, next.Banner},
		{"about_me", before.AboutMe, next.AboutMe},
		{"bio", before.Bio, next.Bio},
		{"pronouns", before.Pronouns, next.Pronouns},
	} {
		if f.was != "" && f.isNow == "" {
			out = append(out, f.key)
		}
	}
	return out
}

// ---- wire forms -------------------------------------------------------------------------

// memberProfileFields is the space profile on every member serialization: always all six
// keys, null when there is no override, so a client can tell "cleared" from "not sent".
func memberProfileFields(m *SpaceMember) map[string]interface{} {
	val := func(s string) interface{} { return nilIfEmpty(strings.TrimSpace(s)) }
	return map[string]interface{}{
		"nick":           val(m.Nickname),
		"space_avatar":   val(m.Avatar),
		"space_banner":   val(m.Banner),
		"space_about_me": val(m.AboutMe),
		"space_bio":      val(m.Bio),
		"space_pronouns": val(m.Pronouns),
	}
}

// memberJSON is the REST member object: the global profile (memberUserFields, unchanged -
// the global keys are always the global profile), the membership, and the space profile.
func (s *Service) memberJSON(m *SpaceMember, u *auth.User) map[string]interface{} {
	out := s.memberUserFields(u)
	out["joined_at"] = m.JoinedAt
	out["roles"] = formatRoleIDStrings(m.RoleIDs)
	for k, v := range memberProfileFields(m) {
		out[k] = v
	}
	return out
}

// memberUpdatePayload is SPACE_MEMBER_UPDATE's data: always the member's full state. role_ids
// is never left out - clients older than space profiles read a missing role_ids as "no
// roles" - and the six profile keys are always there for the same reason.
func memberUpdatePayload(m *SpaceMember) map[string]interface{} {
	out := memberProfileFields(m)
	out["user_id"] = id.Format(m.UserID)
	out["role_ids"] = formatRoleIDStrings(m.RoleIDs)
	return out
}

// publishMemberUpdate tells the space a member's roles or space profile changed. Every
// SPACE_MEMBER_UPDATE goes through here.
func (s *Service) publishMemberUpdate(ctx context.Context, spaceID int64, m *SpaceMember) {
	if m == nil {
		return
	}
	s.publishSpaceEvent(ctx, spaceID, "SPACE_MEMBER_UPDATE", memberUpdatePayload(m))
}

// ---- images and removal ------------------------------------------------------------------

// memberBlobPrefix is where a member's space images live on this instance's Nebula.
func memberBlobPrefix(spaceID, userID int64) string {
	return "spaces/members/" + id.Format(spaceID) + "/" + id.Format(userID) + "/"
}

// memberBlobNameRe is the file part of a key postMemberImage writes.
var memberBlobNameRe = regexp.MustCompile(`^(avatar|banner)-[0-9a-f]{16}\.(png|jpe?g|gif|webp)$`)

// ownsMemberBlobURL returns the Nebula key of url when it is one of this member's space
// images on this instance's CDN: <base>/v1/spaces/members/<space>/<user>/<kind>-<16 hex>.<ext>
// for one of bases. Anything else - another instance's CDN, another member's or space's
// image, or a URL with anything extra in it (a query, "..", an encoded character) - is not
// ours to delete. URLs arrive from other instances too, so this matches exactly.
func ownsMemberBlobURL(bases []string, spaceID, userID int64, url string) (string, bool) {
	prefix := memberBlobPrefix(spaceID, userID)
	for _, base := range bases {
		base = strings.TrimRight(strings.TrimSpace(base), "/")
		if base == "" {
			continue
		}
		rest, ok := strings.CutPrefix(url, base+"/v1/")
		if !ok {
			continue
		}
		name, ok := strings.CutPrefix(rest, prefix)
		if ok && memberBlobNameRe.MatchString(name) {
			return rest, true
		}
	}
	return "", false
}

// memberBlobReleases caps how many release jobs talk to Nebula at once: deleting a large space
// frees every member's images together, and a self-hosted Nebula need not take them all at
// the same moment.
var memberBlobReleases = make(chan struct{}, 4)

// releaseMemberBlobs deletes, best-effort, each of old's avatar and banner that next no longer
// uses and that this instance stored for this member. It runs off the request: a removal or
// an upload must not wait on Nebula (or fail because it is down). An image left behind is a
// leak, not an error. A no-op without Nebula configured.
func (s *Service) releaseMemberBlobs(ctx context.Context, spaceID, userID int64, old, next MemberProfile) {
	if s.cfg == nil {
		return
	}
	neb := s.cfg.Nebula
	if strings.TrimSpace(neb.BaseURL) == "" || strings.TrimSpace(neb.UploadSecret) == "" {
		return
	}
	bases := []string{neb.PublicURL, neb.BaseURL}
	var keys []string
	for _, pair := range [][2]string{{old.Avatar, next.Avatar}, {old.Banner, next.Banner}} {
		if pair[0] == "" || pair[0] == pair[1] {
			continue
		}
		if key, ok := ownsMemberBlobURL(bases, spaceID, userID, pair[0]); ok {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return
	}
	bg := context.WithoutCancel(ctx)
	safego.Go("spaces", func() {
		memberBlobReleases <- struct{}{}
		defer func() { <-memberBlobReleases }()
		ctx, cancel := context.WithTimeout(bg, 10*time.Second)
		defer cancel()
		for _, key := range keys {
			if err := nebula.Delete(ctx, neb.BaseURL, neb.UploadSecret, key); err != nil {
				logger.Err("spaces", err, map[string]any{"space_id": spaceID, "user_id": userID, "key": key})
			}
		}
	})
}

// removeMemberRow is the one way a membership ends (kick, ban, leave, a mirror's removal, the
// space going): the row goes, and with it the space profile, whose images are released. m is
// the row when the caller already has it; nil reads it first.
func (s *Service) removeMemberRow(ctx context.Context, spaceID, userID int64, m *SpaceMember) error {
	if m == nil {
		var err error
		// Only the images need the row; failing to read it must not keep someone in a space.
		if m, err = s.repo.GetMember(ctx, spaceID, userID); err != nil {
			logger.Err("spaces", err, map[string]any{"space_id": spaceID, "user_id": userID, "step": "read member before removal"})
		}
	}
	if err := s.repo.RemoveMember(ctx, spaceID, userID); err != nil {
		return err
	}
	if m != nil {
		s.releaseMemberBlobs(ctx, spaceID, userID, m.Profile(), MemberProfile{})
	}
	return nil
}
