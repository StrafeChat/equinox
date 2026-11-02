package stargate

import "github.com/StrafeChat/equinox/internal/modules/auth"

// Opcodes: client -> server
const (
	OpHeartbeat   = 0
	OpSubscribe   = 1
	OpSend        = 2
	OpUnsubscribe = 5
	OpPing        = 6
	// OpActivity sets or clears what this connection reports the user is doing
	// ({"activities": [{"type": "playing", "name": ..., "started_at": ...}]}; null or [] clears).
	OpActivity = 9
)

// Opcodes: server -> client
const (
	OpEvent = 3
	OpReady = 4
	OpPong  = 7
	OpError = 8
)

// Client inbound message
type ClientMessage struct {
	Op int         `json:"op"`
	D  interface{} `json:"d,omitempty"`
}

// Subscribe payload
type SubscribePayload struct {
	SpaceID string `json:"space_id,omitempty"`
	UserID  string `json:"user_id,omitempty"` // for DMs
}

// Send payload (publish to room/space; space_id = room_id for PMs)
type SendPayload struct {
	SpaceID string      `json:"space_id"`
	Type    string      `json:"type,omitempty"` // e.g. "message", "typing"
	Content interface{} `json:"content"`
	ReplyTo string      `json:"reply_to,omitempty"`
}

// ActivityPayload is op 9's d. At most auth.MaxActivities entries; see auth.NormalizeActivity
// for the rules each one must meet.
type ActivityPayload struct {
	Activities []auth.Activity `json:"activities"`
}

// Unsubscribe payload
type UnsubscribePayload struct {
	SpaceID string `json:"space_id,omitempty"`
	UserID  string `json:"user_id,omitempty"`
}

// Server outbound: READY (after successful auth)
// When ReadyDataProvider is set, rooms, relationships, spaces, and space_rooms are included for initial load.
type ReadyPayload struct {
	User          interface{} `json:"user"`
	SessionID     string      `json:"session_id"`
	Rooms         interface{} `json:"rooms,omitempty"`
	Relationships interface{} `json:"relationships,omitempty"`
	Spaces        interface{} `json:"spaces,omitempty"`
	SpaceRooms    interface{} `json:"space_rooms,omitempty"` // map: space_id -> []room
	// VoiceStates is everyone currently in a voice room the user can see (their spaces'
	// voice rooms, their PMs); Calls the ringing/running PM calls among those rooms.
	VoiceStates interface{} `json:"voice_states,omitempty"`
	Calls       interface{} `json:"calls,omitempty"`
}

// ReadyData is what a ReadyDataProvider contributes to READY.
type ReadyData struct {
	Rooms         interface{}
	Relationships interface{}
	Spaces        interface{}
	SpaceRooms    interface{}
	VoiceStates   interface{}
	Calls         interface{}
	// ChannelIDs is every "space" channel id the user may subscribe to - each space's own
	// id plus every room id (space rooms and PMs). Not sent; the server uses it to
	// auto-subscribe bot connections.
	ChannelIDs []string
	// ChannelScopes says what each of ChannelIDs is (its space, and its room unless it is a
	// space's own channel), recorded with a bot's auto-subscriptions so its op 2 sends carry
	// the right ids. Not sent; an id missing here relays unscoped.
	ChannelScopes map[string]Scope
}

// Server outbound: EVENT
type EventPayload struct {
	Type string `json:"t"` // e.g. MESSAGE, TYPING_START
	// SpaceID is the space the event belongs to; absent for PM / group PM events and for
	// account events about no space.
	SpaceID string `json:"space_id,omitempty"`
	// RoomID is the room (channel, thread, PM) the event is about; absent for space-wide events.
	RoomID string `json:"room_id,omitempty"`
	// UserID is set only on events delivered on an account's own channel, and names that
	// account (the recipient, never the actor).
	UserID string      `json:"user_id,omitempty"`
	Data   interface{} `json:"d"`
	Origin string      `json:"origin,omitempty"` // federation: source server
	Region string      `json:"region,omitempty"` // instance region
}

// Server outbound: ERROR
type ErrorPayload struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Redis message envelope (published to Redis, received by all instances)
type RedisEnvelope struct {
	Type string `json:"t"`
	// SpaceID and UserID are the ROUTING channel, not what the event is about: SpaceID names a
	// stargate:space:<id> channel (a room's id for a room's events, a space's own id for
	// space-wide ones), UserID a stargate:user:<id> channel. They keep these keys and this
	// meaning so a gateway one release behind still routes a room's events to the room alone
	// while API and gateway are upgraded one after the other.
	SpaceID string      `json:"space_id,omitempty"`
	UserID  string      `json:"user_id,omitempty"`
	From    int64       `json:"from"` // sender user id
	Data    interface{} `json:"d"`
	Origin  string      `json:"origin,omitempty"`
	Region  string      `json:"region,omitempty"`
	// Scoped says ScopeSpaceID / ScopeRoomID name the space and room the event is about (each
	// empty when there is none). An envelope without it comes from a publisher that predates
	// them, and the gateway falls back to the routing channel for both ids.
	Scoped       bool   `json:"scoped,omitempty"`
	ScopeSpaceID string `json:"scope_space_id,omitempty"`
	ScopeRoomID  string `json:"scope_room_id,omitempty"`
	// Subscribe lists the "space" channels a server-originated event gives its recipients
	// access to. The gateway's own business; never forwarded to clients.
	Subscribe []string `json:"subscribe,omitempty"`
}
