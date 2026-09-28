// Package voice runs voice and video calls on top of LiveKit: it hands out room-scoped
// tokens, keeps the per-room voice states (who is connected, muted, deafened, sharing
// video) that clients render, enforces the space's voice permissions and moderator
// actions, and rings people for PM / group PM calls.
//
// LiveKit carries the media; this package is the source of truth for *who is where*.
// A state is written when a token is issued, confirmed when LiveKit reports the
// participant joined (webhook), and removed when they leave, are removed, or the
// reconciler finds them gone.
package voice

import (
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// UserSummary is the bit of profile every client needs to draw a participant tile,
// carried on the state so voice rooms render without a member-list fetch.
type UserSummary struct {
	ID            string `json:"id"`
	Username      string `json:"username"`
	Discriminator int    `json:"discriminator"`
	DisplayName   string `json:"display_name"`
	Avatar        string `json:"avatar,omitempty"`
}

func summaryOf(u *auth.User) *UserSummary {
	if u == nil {
		return nil
	}
	return &UserSummary{
		ID:            id.Format(u.ID),
		Username:      u.Username,
		Discriminator: u.Discriminator,
		DisplayName:   u.DisplayName,
		Avatar:        u.Avatar,
	}
}

// State is one user's presence in one voice room - Discord's VoiceState. A user is in
// at most one voice room across the whole instance. Self* flags are what the user
// chose; Mute/Deaf are moderator-imposed; Suppress means they lack the Speak
// permission there.
type State struct {
	UserID  int64 `json:"user_id,string"`
	RoomID  int64 `json:"room_id,string"`
	SpaceID int64 `json:"space_id,string,omitempty"`
	// SessionID makes the LiveKit identity (user.session) unique per join, so a late
	// participant_left webhook from an earlier connection can't remove a newer state.
	SessionID  string    `json:"session_id"`
	SelfMute   bool      `json:"self_mute"`
	SelfDeaf   bool      `json:"self_deaf"`
	Mute       bool      `json:"mute"`
	Deaf       bool      `json:"deaf"`
	SelfVideo  bool      `json:"self_video"`
	SelfStream bool      `json:"self_stream"`
	Suppress   bool      `json:"suppress"`
	Priority   bool      `json:"priority_speaker"`
	Connected  bool      `json:"connected"`
	JoinedAt   time.Time `json:"joined_at"`
	// IssuedAt is when the current token was issued; the reconciler gives a state this
	// long to show up in LiveKit before treating it as abandoned.
	IssuedAt time.Time    `json:"token_issued_at"`
	User     *UserSummary `json:"user,omitempty"`
}

// Identity is the LiveKit participant identity for this state.
func (s *State) Identity() string {
	return id.Format(s.UserID) + "." + s.SessionID
}

// Call is a ringing / running call in a PM or group PM. Space voice rooms have no
// call object - people just come and go.
type Call struct {
	RoomID    int64     `json:"room_id,string"`
	StartedBy int64     `json:"started_by,string"`
	StartedAt time.Time `json:"started_at"`
	// Ringing lists the participants still being called. Answering, declining or the
	// call ending removes them; Ring puts them back.
	Ringing []int64 `json:"ringing"`
}

func (c *Call) ringingStrings() []string {
	out := make([]string, 0, len(c.Ringing))
	for _, uid := range c.Ringing {
		out = append(out, id.Format(uid))
	}
	return out
}

func (c *Call) isRinging(uid int64) bool {
	for _, r := range c.Ringing {
		if r == uid {
			return true
		}
	}
	return false
}

func (c *Call) stopRinging(uid int64) bool {
	for i, r := range c.Ringing {
		if r == uid {
			c.Ringing = append(c.Ringing[:i], c.Ringing[i+1:]...)
			return true
		}
	}
	return false
}

// CallView is the wire form of a Call: ids as strings, like every other payload. Call
// itself is the Redis form (int64 ids) and must never be marshalled to a client - a
// snowflake in a JSON number loses precision in JavaScript.
type CallView struct {
	RoomID    string    `json:"room_id"`
	StartedBy string    `json:"started_by"`
	StartedAt time.Time `json:"started_at"`
	Ringing   []string  `json:"ringing"`
}

func (c *Call) View() *CallView {
	if c == nil {
		return nil
	}
	return &CallView{
		RoomID:    id.Format(c.RoomID),
		StartedBy: id.Format(c.StartedBy),
		StartedAt: c.StartedAt,
		Ringing:   c.ringingStrings(),
	}
}

// toMap is View() as a map, for events that add fields.
func (c *Call) toMap() map[string]interface{} {
	v := c.View()
	return map[string]interface{}{
		"room_id":    v.RoomID,
		"started_by": v.StartedBy,
		"started_at": v.StartedAt,
		"ringing":    v.Ringing,
	}
}

// Perms is the voice slice of a member's effective permissions in one room.
type Perms struct {
	Connect, Speak, Video, MuteMembers, DeafenMembers, MoveMembers, VAD, Priority bool
}

// JoinInput is the body of POST /rooms/:id/voice/join.
type JoinInput struct {
	SelfMute bool `json:"self_mute"`
	SelfDeaf bool `json:"self_deaf"`
}

// SelfInput is the body of PATCH /voice/state. Only present fields change.
type SelfInput struct {
	SelfMute   *bool `json:"self_mute,omitempty"`
	SelfDeaf   *bool `json:"self_deaf,omitempty"`
	SelfVideo  *bool `json:"self_video,omitempty"`
	SelfStream *bool `json:"self_stream,omitempty"`
}

// ModerateInput is the body of PATCH /spaces/:id/members/:userId/voice.
type ModerateInput struct {
	Mute *bool `json:"mute,omitempty"`
	Deaf *bool `json:"deaf,omitempty"`
	// RoomID moves the member to another voice room in the same space.
	RoomID *string `json:"room_id,omitempty"`
}

// JoinResult is what a client needs to connect.
type JoinResult struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	// RoomName is the LiveKit room the token is for.
	RoomName string `json:"room_name"`
	State    *State `json:"state"`
	// States is everyone in the room (including the caller) at the moment of joining.
	States []State `json:"states"`
	// Bitrate is the audio bitrate clients should publish at (bits per second).
	Bitrate int       `json:"bitrate"`
	Call    *CallView `json:"call,omitempty"`
}
