package users

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/StrafeChat/equinox/internal/modules/auth"
)

var validStatuses = map[string]struct{}{
	"online": {}, "offline": {}, "dnd": {}, "idle": {}, "invisible": {},
}

// Limits are in characters (runes), not bytes: "at most 32 characters" must mean the same
// thing for a name written in Arabic or with emoji as for one in ASCII.
const (
	maxDisplayName  = 32
	maxBio          = 190
	maxAboutMe      = 190
	maxAvatar       = 256
	maxBanner       = 256
	maxCustomStatus = 128
	maxPronouns     = 40
)

// accentColorRe: clients put this straight into CSS, so only a plain hex colour is allowed.
var accentColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func tooLong(s string, max int) bool {
	return utf8.RuneCountInString(s) > max
}

// ParsePatchMeBody parses JSON body into ProfileUpdate with validation.
// Returns (nil, true) on success, (error message, false) on failure.
func ParsePatchMeBody(body []byte, out *auth.ProfileUpdate) (string, bool) {
	type patchInput struct {
		DisplayName *string `json:"display_name,omitempty"`
		Bio         *string `json:"bio,omitempty"`
		AboutMe     *string `json:"about_me,omitempty"`
		Avatar      *string `json:"avatar,omitempty"`
		Banner      *string `json:"banner,omitempty"`
		AccentColor   *string `json:"accent_color,omitempty"`
		Pronouns      *string `json:"pronouns,omitempty"`
		BirthdayOptIn *bool   `json:"birthday_opt_in,omitempty"`
		Presence      *struct {
			Online       *bool   `json:"online,omitempty"`
			Status       *string `json:"status,omitempty"`
			CustomStatus *string `json:"custom_status,omitempty"`
		} `json:"presence,omitempty"`
	}
	var tmp patchInput
	if err := json.Unmarshal(body, &tmp); err != nil {
		return "invalid json", false
	}
	// Validate and copy
	if tmp.DisplayName != nil {
		s := strings.TrimSpace(*tmp.DisplayName)
		if tooLong(s, maxDisplayName) {
			return "display_name must be at most 32 characters", false
		}
		out.DisplayName = &s
	}
	if tmp.Bio != nil {
		s := strings.TrimSpace(*tmp.Bio)
		if tooLong(s, maxBio) {
			return "bio must be at most 190 characters", false
		}
		out.Bio = &s
	}
	if tmp.AboutMe != nil {
		s := strings.TrimSpace(*tmp.AboutMe)
		if tooLong(s, maxAboutMe) {
			return "about_me must be at most 190 characters", false
		}
		out.AboutMe = &s
	}
	if tmp.Avatar != nil {
		s := strings.TrimSpace(*tmp.Avatar)
		if tooLong(s, maxAvatar) {
			return "avatar must be at most 256 characters", false
		}
		out.Avatar = &s
	}
	if tmp.Banner != nil {
		s := strings.TrimSpace(*tmp.Banner)
		if tooLong(s, maxBanner) {
			return "banner must be at most 256 characters", false
		}
		out.Banner = &s
	}
	if tmp.AccentColor != nil {
		s := strings.TrimSpace(*tmp.AccentColor)
		if s != "" && !accentColorRe.MatchString(s) {
			return "accent_color must be a hex colour like #1e90ff", false
		}
		out.AccentColor = &s
	}
	if tmp.Pronouns != nil {
		s := strings.TrimSpace(*tmp.Pronouns)
		if tooLong(s, maxPronouns) {
			return "pronouns must be at most 40 characters", false
		}
		out.Pronouns = &s
	}
	if tmp.BirthdayOptIn != nil {
		out.BirthdayOptIn = tmp.BirthdayOptIn
	}
	if tmp.Presence != nil {
		out.Presence = &auth.PresenceUpdate{}
		// `online` is owned by the gateway (set on connect/disconnect); a client can't
		// declare itself online over REST. Use status "invisible" to appear offline.
		if tmp.Presence.Status != nil {
			s := strings.ToLower(strings.TrimSpace(*tmp.Presence.Status))
			if _, ok := validStatuses[s]; !ok {
				return "status must be one of: online, offline, dnd, idle, invisible", false
			}
			out.Presence.Status = &s
		}
		if tmp.Presence.CustomStatus != nil {
			s := strings.TrimSpace(*tmp.Presence.CustomStatus)
			if tooLong(s, maxCustomStatus) {
				return "custom_status must be at most 128 characters", false
			}
			out.Presence.CustomStatus = &s
		}
		if out.Presence.Status == nil && out.Presence.CustomStatus == nil {
			out.Presence = nil
		}
	}
	return "", true
}
