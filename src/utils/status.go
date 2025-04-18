package utils
// IsValidStatus checks if the given status is valid
func IsValidStatus(status string) bool {
	switch status {
	case "online", "idle", "dnd", "offline":
		return true
	default:
		return false
	}
}
