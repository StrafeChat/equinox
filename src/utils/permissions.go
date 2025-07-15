package utils

import (
	"strconv"
	"strings"
)

// Permission constants
const (
	// General permissions
	ADMINISTRATOR = "ADMINISTRATOR"
	VIEW_CHANNELS = "VIEW_CHANNELS"
	MANAGE_CHANNELS = "MANAGE_CHANNELS"
	MANAGE_ROLES = "MANAGE_ROLES"
	MANAGE_SPACE = "MANAGE_SPACE"
	KICK_MEMBERS = "KICK_MEMBERS"
	BAN_MEMBERS = "BAN_MEMBERS"
	MANAGE_NICKNAMES = "MANAGE_NICKNAMES"
	MANAGE_WEBHOOKS = "MANAGE_WEBHOOKS"
	VIEW_AUDIT_LOG = "VIEW_AUDIT_LOG"

	// Text channel permissions
	SEND_MESSAGES = "SEND_MESSAGES"
	MANAGE_MESSAGES = "MANAGE_MESSAGES"
	READ_MESSAGE_HISTORY = "READ_MESSAGE_HISTORY"
	MENTION_EVERYONE = "MENTION_EVERYONE"
	USE_EXTERNAL_EMOJIS = "USE_EXTERNAL_EMOJIS"
	ADD_REACTIONS = "ADD_REACTIONS"
	ATTACH_FILES = "ATTACH_FILES"
	EMBED_LINKS = "EMBED_LINKS"

	// Voice channel permissions
	CONNECT = "CONNECT"
	SPEAK = "SPEAK"
	MUTE_MEMBERS = "MUTE_MEMBERS"
	DEAFEN_MEMBERS = "DEAFEN_MEMBERS"
	MOVE_MEMBERS = "MOVE_MEMBERS"
	USE_VOICE_ACTIVATION = "USE_VOICE_ACTIVATION"
	PRIORITY_SPEAKER = "PRIORITY_SPEAKER"
	STREAM = "STREAM"
)

// Permission represents a permission with metadata
type Permission struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Category    string `json:"category"`
}

// GetAllPermissions returns all available permissions
func GetAllPermissions() []Permission {
	return []Permission{
		// General permissions
		{ADMINISTRATOR, "Administrator", "All permissions", "general"},
		{VIEW_CHANNELS, "View Channels", "View channels", "general"},
		{MANAGE_CHANNELS, "Manage Channels", "Create, edit, and delete channels", "general"},
		{MANAGE_ROLES, "Manage Roles", "Create, edit, and delete roles", "general"},
		{MANAGE_SPACE, "Manage Space", "Edit space settings", "general"},
		{KICK_MEMBERS, "Kick Members", "Remove members from space", "general"},
		{BAN_MEMBERS, "Ban Members", "Ban members from space", "general"},
		{MANAGE_NICKNAMES, "Manage Nicknames", "Change other members' nicknames", "general"},
		{MANAGE_WEBHOOKS, "Manage Webhooks", "Create, edit, and delete webhooks", "general"},
		{VIEW_AUDIT_LOG, "View Audit Log", "View space audit log", "general"},

		// Text channel permissions
		{SEND_MESSAGES, "Send Messages", "Send messages in text channels", "text"},
		{MANAGE_MESSAGES, "Manage Messages", "Delete and edit messages", "text"},
		{READ_MESSAGE_HISTORY, "Read Message History", "Read previous messages", "text"},
		{MENTION_EVERYONE, "Mention @everyone", "Use @everyone and @here mentions", "text"},
		{USE_EXTERNAL_EMOJIS, "Use External Emojis", "Use emojis from other spaces", "text"},
		{ADD_REACTIONS, "Add Reactions", "Add reactions to messages", "text"},
		{ATTACH_FILES, "Attach Files", "Upload files and media", "text"},
		{EMBED_LINKS, "Embed Links", "Links sent will be embedded", "text"},

		// Voice channel permissions
		{CONNECT, "Connect", "Connect to voice channels", "voice"},
		{SPEAK, "Speak", "Speak in voice channels", "voice"},
		{MUTE_MEMBERS, "Mute Members", "Mute members in voice channels", "voice"},
		{DEAFEN_MEMBERS, "Deafen Members", "Deafen members in voice channels", "voice"},
		{MOVE_MEMBERS, "Move Members", "Move members between voice channels", "voice"},
		{USE_VOICE_ACTIVATION, "Use Voice Activation", "Use voice activation in voice channels", "voice"},
		{PRIORITY_SPEAKER, "Priority Speaker", "Priority speaker in voice channels", "voice"},
		{STREAM, "Stream", "Stream in voice channels", "voice"},
	}
}

// GetDefaultEveryonePermissions returns the default permissions for @everyone role
func GetDefaultEveryonePermissions() []string {
	return []string{
		VIEW_CHANNELS,
		SEND_MESSAGES,
		READ_MESSAGE_HISTORY,
		ADD_REACTIONS,
		ATTACH_FILES,
		EMBED_LINKS,
		CONNECT,
		SPEAK,
		USE_VOICE_ACTIVATION,
	}
}

// HasPermission checks if a user has a specific permission
func HasPermission(userPermissions []string, permission string) bool {
	// Administrator has all permissions
	for _, perm := range userPermissions {
		if perm == ADMINISTRATOR {
			return true
		}
	}

	// Check for specific permission
	for _, perm := range userPermissions {
		if perm == permission {
			return true
		}
	}

	return false
}

// CalculatePermissions calculates the final permissions for a user based on their roles
func CalculatePermissions(rolePermissions [][]string) []string {
	permissionSet := make(map[string]bool)

	// Combine all permissions from all roles
	for _, rolePerms := range rolePermissions {
		for _, perm := range rolePerms {
			permissionSet[perm] = true
		}
	}

	// Convert map to slice
	permissions := make([]string, 0, len(permissionSet))
	for perm := range permissionSet {
		permissions = append(permissions, perm)
	}

	return permissions
}

// PermissionsToString converts permissions slice to a comma-separated string
func PermissionsToString(permissions []string) string {
	return strings.Join(permissions, ",")
}

// PermissionsFromString converts a comma-separated string to permissions slice
func PermissionsFromString(permissionsStr string) []string {
	if permissionsStr == "" {
		return []string{}
	}
	return strings.Split(permissionsStr, ",")
}

// PermissionsFromMap converts a map[string]interface{} to []string
// Only includes permissions that have a truthy value
func PermissionsFromMap(permissionsMap map[string]interface{}) []string {
	var permissions []string
	for perm, value := range permissionsMap {
		if isTruthy(value) {
			permissions = append(permissions, perm)
		}
	}
	return permissions
}

// isTruthy checks if a value is considered "true"
func isTruthy(value interface{}) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return v != "" && v != "false" && v != "0"
	case int, int8, int16, int32, int64:
		return v != 0
	case float32, float64:
		return v != 0.0
	default:
		return value != nil
	}
}

// PermissionValue represents a permission as a bit flag
type PermissionValue int64

// Permission bit flags
const (
	PermissionAdministrator PermissionValue = 1 << iota
	PermissionViewChannels
	PermissionManageChannels
	PermissionManageRoles
	PermissionManageSpace
	PermissionKickMembers
	PermissionBanMembers
	PermissionSendMessages
	PermissionManageMessages
	PermissionReadMessageHistory
	PermissionMentionEveryone
	PermissionConnect
	PermissionSpeak
	PermissionMuteMembers
	PermissionDeafenMembers
	PermissionMoveMembers
)

// PermissionsToBitfield converts permission strings to a bitfield
func PermissionsToBitfield(permissions []string) PermissionValue {
	var bitfield PermissionValue

	for _, perm := range permissions {
		switch perm {
		case ADMINISTRATOR:
			bitfield |= PermissionAdministrator
		case VIEW_CHANNELS:
			bitfield |= PermissionViewChannels
		case MANAGE_CHANNELS:
			bitfield |= PermissionManageChannels
		case MANAGE_ROLES:
			bitfield |= PermissionManageRoles
		case MANAGE_SPACE:
			bitfield |= PermissionManageSpace
		case KICK_MEMBERS:
			bitfield |= PermissionKickMembers
		case BAN_MEMBERS:
			bitfield |= PermissionBanMembers
		case SEND_MESSAGES:
			bitfield |= PermissionSendMessages
		case MANAGE_MESSAGES:
			bitfield |= PermissionManageMessages
		case READ_MESSAGE_HISTORY:
			bitfield |= PermissionReadMessageHistory
		case MENTION_EVERYONE:
			bitfield |= PermissionMentionEveryone
		case CONNECT:
			bitfield |= PermissionConnect
		case SPEAK:
			bitfield |= PermissionSpeak
		case MUTE_MEMBERS:
			bitfield |= PermissionMuteMembers
		case DEAFEN_MEMBERS:
			bitfield |= PermissionDeafenMembers
		case MOVE_MEMBERS:
			bitfield |= PermissionMoveMembers
		}
	}

	return bitfield
}

// BitfieldToPermissions converts a bitfield to permission strings
func BitfieldToPermissions(bitfield PermissionValue) []string {
	var permissions []string

	if bitfield&PermissionAdministrator != 0 {
		permissions = append(permissions, ADMINISTRATOR)
	}
	if bitfield&PermissionViewChannels != 0 {
		permissions = append(permissions, VIEW_CHANNELS)
	}
	if bitfield&PermissionManageChannels != 0 {
		permissions = append(permissions, MANAGE_CHANNELS)
	}
	if bitfield&PermissionManageRoles != 0 {
		permissions = append(permissions, MANAGE_ROLES)
	}
	if bitfield&PermissionManageSpace != 0 {
		permissions = append(permissions, MANAGE_SPACE)
	}
	if bitfield&PermissionKickMembers != 0 {
		permissions = append(permissions, KICK_MEMBERS)
	}
	if bitfield&PermissionBanMembers != 0 {
		permissions = append(permissions, BAN_MEMBERS)
	}
	if bitfield&PermissionSendMessages != 0 {
		permissions = append(permissions, SEND_MESSAGES)
	}
	if bitfield&PermissionManageMessages != 0 {
		permissions = append(permissions, MANAGE_MESSAGES)
	}
	if bitfield&PermissionReadMessageHistory != 0 {
		permissions = append(permissions, READ_MESSAGE_HISTORY)
	}
	if bitfield&PermissionMentionEveryone != 0 {
		permissions = append(permissions, MENTION_EVERYONE)
	}
	if bitfield&PermissionConnect != 0 {
		permissions = append(permissions, CONNECT)
	}
	if bitfield&PermissionSpeak != 0 {
		permissions = append(permissions, SPEAK)
	}
	if bitfield&PermissionMuteMembers != 0 {
		permissions = append(permissions, MUTE_MEMBERS)
	}
	if bitfield&PermissionDeafenMembers != 0 {
		permissions = append(permissions, DEAFEN_MEMBERS)
	}
	if bitfield&PermissionMoveMembers != 0 {
		permissions = append(permissions, MOVE_MEMBERS)
	}

	return permissions
}

// PermissionBitfieldToString converts a permission bitfield to string
func PermissionBitfieldToString(bitfield PermissionValue) string {
	return strconv.FormatInt(int64(bitfield), 10)
}

// PermissionBitfieldFromString converts a string to permission bitfield
func PermissionBitfieldFromString(bitfieldStr string) (PermissionValue, error) {
	if bitfieldStr == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(bitfieldStr, 10, 64)
	if err != nil {
		return 0, err
	}
	return PermissionValue(value), nil
}