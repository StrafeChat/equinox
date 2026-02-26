package auth

// PublicPresence is the presence object exposed to the API and WebSocket.
// It never includes the internal "online" boolean so others cannot detect invisible users.
type PublicPresence struct {
	Status       string `json:"status"`                   // "online" | "idle" | "dnd" | "offline"
	CustomStatus string `json:"custom_status,omitempty"`   // Optional status text
}

// ToPublicPresence converts UserPresence to PublicPresence for API/WS responses.
// When forOthers is true, "invisible" is mapped to "offline" so others cannot tell if you are invisible.
func ToPublicPresence(p UserPresence, forOthers bool) PublicPresence {
	status := p.Status
	if status == "" {
		if p.Online {
			status = "online"
		} else {
			status = "offline"
		}
	}
	if forOthers && status == "invisible" {
		status = "offline"
	}
	return PublicPresence{Status: status, CustomStatus: p.CustomStatus}
}
