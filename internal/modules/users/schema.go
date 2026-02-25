package users

import (
	"encoding/json"
	"strings"

	"github.com/StrafeChat/equinox/internal/modules/auth"
)

var validStatuses = map[string]struct{}{
	"online": {}, "offline": {}, "dnd": {}, "idle": {},
}

const (
	maxDisplayName = 32
	maxBio         = 190
	maxAboutMe     = 190
	maxAvatar      = 256
	maxBanner      = 256
	maxAccentColor = 32
	maxCustomStatus = 128
)

// ParsePatchMeBody parses JSON body into ProfileUpdate with validation.
// Returns (nil, true) on success, (error message, false) on failure.
func ParsePatchMeBody(body []byte, out *auth.ProfileUpdate) (string, bool) {
	type patchInput struct {
		DisplayName *string `json:"display_name,omitempty"`
		Bio         *string `json:"bio,omitempty"`
		AboutMe     *string `json:"about_me,omitempty"`
		Avatar      *string `json:"avatar,omitempty"`
		Banner      *string `json:"banner,omitempty"`
		AccentColor *string `json:"accent_color,omitempty"`
		Presence    *struct {
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
		if len(s) > maxDisplayName {
			return "display_name must be at most 32 characters", false
		}
		out.DisplayName = &s
	}
	if tmp.Bio != nil {
		s := strings.TrimSpace(*tmp.Bio)
		if len(s) > maxBio {
			return "bio must be at most 190 characters", false
		}
		out.Bio = &s
	}
	if tmp.AboutMe != nil {
		s := strings.TrimSpace(*tmp.AboutMe)
		if len(s) > maxAboutMe {
			return "about_me must be at most 190 characters", false
		}
		out.AboutMe = &s
	}
	if tmp.Avatar != nil {
		s := strings.TrimSpace(*tmp.Avatar)
		if len(s) > maxAvatar {
			return "avatar must be at most 256 characters", false
		}
		out.Avatar = &s
	}
	if tmp.Banner != nil {
		s := strings.TrimSpace(*tmp.Banner)
		if len(s) > maxBanner {
			return "banner must be at most 256 characters", false
		}
		out.Banner = &s
	}
	if tmp.AccentColor != nil {
		s := strings.TrimSpace(*tmp.AccentColor)
		if len(s) > maxAccentColor {
			return "accent_color must be at most 32 characters", false
		}
		out.AccentColor = &s
	}
	if tmp.Presence != nil {
		out.Presence = &auth.PresenceUpdate{}
		if tmp.Presence.Online != nil {
			out.Presence.Online = tmp.Presence.Online
		}
		if tmp.Presence.Status != nil {
			s := strings.ToLower(strings.TrimSpace(*tmp.Presence.Status))
			if _, ok := validStatuses[s]; !ok {
				return "status must be one of: online, offline, dnd, idle", false
			}
			out.Presence.Status = &s
		}
		if tmp.Presence.CustomStatus != nil {
			s := strings.TrimSpace(*tmp.Presence.CustomStatus)
			if len(s) > maxCustomStatus {
				return "custom_status must be at most 128 characters", false
			}
			out.Presence.CustomStatus = &s
		}
		if out.Presence.Online == nil && out.Presence.Status == nil && out.Presence.CustomStatus == nil {
			out.Presence = nil
		}
	}
	return "", true
}

