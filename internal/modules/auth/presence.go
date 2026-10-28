package auth

// PublicPresence is the presence object exposed to the API and WebSocket.
// It never includes the internal "online" boolean so others cannot detect invisible users.
type PublicPresence struct {
	Status       string `json:"status"`                  // "online" | "idle" | "dnd" | "offline"
	CustomStatus string `json:"custom_status,omitempty"` // Optional status text
	// Activities is what the user is doing right now ("Playing X"): ephemeral, kept in
	// Redis only, attached with WithActivities - never by ToPublicPresence, which knows
	// nothing about it - and absent whenever the status reads offline.
	Activities []Activity `json:"activities,omitempty"`
}

// ToPublicPresence converts UserPresence to PublicPresence for API/WS responses.
// When forOthers is true, "invisible" is mapped to "offline" so others cannot tell if you are invisible.
// When the user is offline (Online=false), status is always "offline" so disconnect updates are correct.
func ToPublicPresence(p UserPresence, forOthers bool) PublicPresence {
	var status string
	if !p.Online {
		status = "offline"
	} else if p.Status == "" || p.Status == "offline" {
		// "offline" was the old default and is not a real choice; when Online it means the
		// account simply never picked a status, so show plain online.
		status = "online"
	} else {
		status = p.Status
	}
	if forOthers && status == "invisible" {
		status = "offline"
	}
	return PublicPresence{Status: status, CustomStatus: p.CustomStatus}
}
