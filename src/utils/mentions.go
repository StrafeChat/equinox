package utils

import (
	"regexp"
	"strings"
)

// MentionType represents the type of mention
type MentionType int

const (
	MentionTypeUser MentionType = iota
	MentionTypeRole
	MentionTypeRoom
	MentionTypeEveryone
)

// Mention represents a parsed mention
type Mention struct {
	Type  MentionType `json:"type"`
	ID    string      `json:"id"`
	Raw   string      `json:"raw"`
	Start int         `json:"start"`
	End   int         `json:"end"`
}

// MentionParseResult contains the parsed mentions and cleaned content
type MentionParseResult struct {
	UserMentions    []string  `json:"user_mentions"`
	RoleMentions    []string  `json:"role_mentions"`
	RoomMentions    []string  `json:"room_mentions"`
	MentionEveryone bool      `json:"mention_everyone"`
	AllMentions     []Mention `json:"all_mentions"`
	CleanedContent  string    `json:"cleaned_content"`
}

// Regular expressions for different mention types
var (
	// User mentions: <@userid>
	userMentionRegex = regexp.MustCompile(`<@([0-9]+)>`)
	// Role mentions: <@&roleid>
	roleMentionRegex = regexp.MustCompile(`<@&([0-9]+)>`)
	// Room mentions: <#roomid>
	roomMentionRegex = regexp.MustCompile(`<#([0-9]+)>`)
	// @everyone mention
	everyoneMentionRegex = regexp.MustCompile(`@everyone`)
)

// ParseMentions extracts all mentions from message content
func ParseMentions(content string) MentionParseResult {
	result := MentionParseResult{
		UserMentions:    []string{},
		RoleMentions:    []string{},
		RoomMentions:    []string{},
		MentionEveryone: false,
		AllMentions:     []Mention{},
		CleanedContent:  content,
	}

	// Parse user mentions
	userMatches := userMentionRegex.FindAllStringSubmatch(content, -1)
	userMatchIndices := userMentionRegex.FindAllStringIndex(content, -1)
	for i, match := range userMatches {
		if len(match) > 1 {
			userID := match[1]
			result.UserMentions = append(result.UserMentions, userID)
			if i < len(userMatchIndices) {
				result.AllMentions = append(result.AllMentions, Mention{
					Type:  MentionTypeUser,
					ID:    userID,
					Raw:   match[0],
					Start: userMatchIndices[i][0],
					End:   userMatchIndices[i][1],
				})
			}
		}
	}

	// Parse role mentions
	roleMatches := roleMentionRegex.FindAllStringSubmatch(content, -1)
	roleMatchIndices := roleMentionRegex.FindAllStringIndex(content, -1)
	for i, match := range roleMatches {
		if len(match) > 1 {
			roleID := match[1]
			result.RoleMentions = append(result.RoleMentions, roleID)
			if i < len(roleMatchIndices) {
				result.AllMentions = append(result.AllMentions, Mention{
					Type:  MentionTypeRole,
					ID:    roleID,
					Raw:   match[0],
					Start: roleMatchIndices[i][0],
					End:   roleMatchIndices[i][1],
				})
			}
		}
	}

	// Parse room mentions
	roomMatches := roomMentionRegex.FindAllStringSubmatch(content, -1)
	roomMatchIndices := roomMentionRegex.FindAllStringIndex(content, -1)
	for i, match := range roomMatches {
		if len(match) > 1 {
			roomID := match[1]
			result.RoomMentions = append(result.RoomMentions, roomID)
			if i < len(roomMatchIndices) {
				result.AllMentions = append(result.AllMentions, Mention{
					Type:  MentionTypeRoom,
					ID:    roomID,
					Raw:   match[0],
					Start: roomMatchIndices[i][0],
					End:   roomMatchIndices[i][1],
				})
			}
		}
	}

	// Parse @everyone mentions
	everyoneMatches := everyoneMentionRegex.FindAllStringIndex(content, -1)
	if len(everyoneMatches) > 0 {
		result.MentionEveryone = true
		for _, match := range everyoneMatches {
			result.AllMentions = append(result.AllMentions, Mention{
				Type:  MentionTypeEveryone,
				ID:    "everyone",
				Raw:   "@everyone",
				Start: match[0],
				End:   match[1],
			})
		}
	}

	// Remove duplicates
	result.UserMentions = removeDuplicates(result.UserMentions)
	result.RoleMentions = removeDuplicates(result.RoleMentions)
	result.RoomMentions = removeDuplicates(result.RoomMentions)

	return result
}

// removeDuplicates removes duplicate strings from a slice
func removeDuplicates(slice []string) []string {
	keys := make(map[string]bool)
	result := []string{}
	for _, item := range slice {
		if !keys[item] {
			keys[item] = true
			result = append(result, item)
		}
	}
	return result
}

// ValidateUserMention checks if a user ID is valid and accessible
func ValidateUserMention(userID string, spaceID *int64) bool {
	// TODO: Implement user validation logic
	// Check if user exists and is accessible in the current context
	return true
}

// ValidateRoleMention checks if a role ID is valid and accessible
func ValidateRoleMention(roleID string, spaceID *int64) bool {
	// TODO: Implement role validation logic
	// Check if role exists in the space
	return true
}

// ValidateRoomMention checks if a room ID is valid and accessible
func ValidateRoomMention(roomID string, userID string, spaceID *int64) bool {
	// TODO: Implement room validation logic
	// Check if room exists and user has access to it
	return true
}

// ValidateEveryoneMention checks if user has permission to use @everyone
func ValidateEveryoneMention(userPermissions []string) bool {
	return HasPermission(userPermissions, MENTION_EVERYONE)
}

// FormatMentionForDisplay formats a mention for display in the frontend
func FormatMentionForDisplay(mention Mention, displayName string) string {
	switch mention.Type {
	case MentionTypeUser:
		return "@" + displayName
	case MentionTypeRole:
		return "@" + displayName
	case MentionTypeRoom:
		return "#" + displayName
	case MentionTypeEveryone:
		return "@everyone"
	default:
		return mention.Raw
	}
}

// GetMentionAutocompletePrefix extracts the mention prefix being typed
func GetMentionAutocompletePrefix(content string, cursorPosition int) (string, MentionType, bool) {
	if cursorPosition <= 0 || cursorPosition > len(content) {
		return "", MentionTypeUser, false
	}

	// Look backwards from cursor to find mention start
	start := cursorPosition - 1
	for start >= 0 && content[start] != ' ' && content[start] != '\n' {
		start--
	}
	start++ // Move to the character after the space/newline

	if start >= cursorPosition {
		return "", MentionTypeUser, false
	}

	prefix := content[start:cursorPosition]

	// Check for different mention types
	if strings.HasPrefix(prefix, "@&") {
		// Role mention
		return strings.TrimPrefix(prefix, "@&"), MentionTypeRole, true
	} else if strings.HasPrefix(prefix, "@") {
		// User mention or @everyone
		return strings.TrimPrefix(prefix, "@"), MentionTypeUser, true
	} else if strings.HasPrefix(prefix, "#") {
		// Room mention
		return strings.TrimPrefix(prefix, "#"), MentionTypeRoom, true
	}

	return "", MentionTypeUser, false
}
