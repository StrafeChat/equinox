package utils

// Valid status values
var validStatuses = map[string]bool{
	"online":    true,
	"idle":      true,
	"dnd":       true,
	"invisible": true,
	"offline":   true,
}

// IsValidStatus checks if the given status is valid
func IsValidStatus(status string) bool {
	_, exists := validStatuses[status]
	return exists
}
