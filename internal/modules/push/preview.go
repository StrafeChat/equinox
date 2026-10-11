package push

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The body preview ("b") of a message push from a room that is not end-to-end encrypted:
// the plaintext as a person would read it, with the markup that only makes sense to the
// client (mention and emoji tokens) turned into names. Markdown stays as written - the
// device shows plain text and asterisks read fine. An end-to-end encrypted room never gets
// here: the server has no plaintext for it.

const (
	previewMaxRunes = 300
	// previewMaxLookups bounds the name lookups one preview may cost, per kind.
	previewMaxLookups = 10
)

var (
	previewUserRe  = regexp.MustCompile(`<@!?(\d+)>`)
	previewFIDRe   = regexp.MustCompile(`<@!?@\d+:[A-Za-z0-9.\-]+(?::\d+)?>`)
	previewRoleRe  = regexp.MustCompile(`<@&(\d+)>`)
	previewRoomRe  = regexp.MustCompile(`<#(\d+)>`)
	previewEmojiRe = regexp.MustCompile(`<a?:([A-Za-z0-9_]{2,32}):\d+>`)
)

// previewNames is what rendering needs to resolve tokens; the worker backs it with the
// job's cached Directory lookups.
type previewNames interface {
	userName(ctx context.Context, userID int64) (string, bool)
	roleName(ctx context.Context, roleID int64) (string, bool)
	roomName(ctx context.Context, roomID int64) (string, bool)
}

// renderPreview turns message plaintext into the preview text: mentions as @name, room
// links as #name, custom emoji as :name:, whitespace collapsed, at most 300 runes.
func renderPreview(ctx context.Context, text string, names previewNames) string {
	if text == "" {
		return ""
	}
	looked := map[int64]string{}
	text = previewUserRe.ReplaceAllStringFunc(text, func(tok string) string {
		uid, err := strconv.ParseInt(previewUserRe.FindStringSubmatch(tok)[1], 10, 64)
		if err != nil {
			return ""
		}
		n, seen := looked[uid]
		if !seen {
			// Past the lookup budget a mention is dropped, as an unknown one is.
			if len(looked) < previewMaxLookups {
				if name, ok := names.userName(ctx, uid); ok {
					n = "@" + name
				}
			}
			looked[uid] = n
		}
		return n
	})
	// A federated mention that could not be resolved when the message arrived names a
	// person this instance knows nothing about; showing the raw id would only be noise.
	text = previewFIDRe.ReplaceAllString(text, "")
	roles := 0
	text = previewRoleRe.ReplaceAllStringFunc(text, func(tok string) string {
		rid, err := strconv.ParseInt(previewRoleRe.FindStringSubmatch(tok)[1], 10, 64)
		roles++
		if err == nil && roles <= previewMaxLookups {
			if n, ok := names.roleName(ctx, rid); ok {
				if strings.HasPrefix(n, "@") {
					return n // @everyone is already spelled with its @
				}
				return "@" + n
			}
		}
		return "@role"
	})
	rooms := 0
	text = previewRoomRe.ReplaceAllStringFunc(text, func(tok string) string {
		rid, err := strconv.ParseInt(previewRoomRe.FindStringSubmatch(tok)[1], 10, 64)
		rooms++
		if err == nil && rooms <= previewMaxLookups {
			if n, ok := names.roomName(ctx, rid); ok {
				return "#" + n
			}
		}
		return "#room"
	})
	text = previewEmojiRe.ReplaceAllString(text, ":$1:")
	return truncateRunes(strings.Join(strings.FieldsFunc(text, unicode.IsSpace), " "), previewMaxRunes)
}

// truncateRunes cuts s to at most max runes, ending with an ellipsis when it was cut.
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	cut := 0
	for i := range s {
		if cut == max-1 {
			return strings.TrimRightFunc(s[:i], unicode.IsSpace) + "…"
		}
		cut++
	}
	return s
}
