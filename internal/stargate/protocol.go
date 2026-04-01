package stargate

// Opcodes: client -> server
const (
	OpHeartbeat  = 0
	OpSubscribe  = 1
	OpSend       = 2
	OpUnsubscribe = 5
	OpPing       = 6
)

// Opcodes: server -> client
const (
	OpEvent = 3
	OpReady = 4
	OpPong = 7
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
	SpaceID  string      `json:"space_id"`
	Type     string      `json:"type,omitempty"` // e.g. "message", "typing"
	Content  interface{} `json:"content"`
	ReplyTo  string      `json:"reply_to,omitempty"`
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
}

// Server outbound: EVENT
type EventPayload struct {
	Type      string      `json:"t"`       // e.g. MESSAGE, TYPING_START
	SpaceID   string      `json:"space_id,omitempty"`   // legacy; for rooms same as room_id
	RoomID    string      `json:"room_id,omitempty"`    // room_id for room/PM events
	UserID    string      `json:"user_id,omitempty"`
	Data      interface{} `json:"d"`
	Origin    string      `json:"origin,omitempty"`    // federation: source server
	Region    string      `json:"region,omitempty"`    // instance region
}

// Server outbound: ERROR
type ErrorPayload struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Redis message envelope (published to Redis, received by all instances)
type RedisEnvelope struct {
	Type    string      `json:"t"`
	SpaceID string      `json:"space_id,omitempty"`
	UserID  string      `json:"user_id,omitempty"`
	From    int64       `json:"from"`     // sender user id
	Data    interface{} `json:"d"`
	Origin  string      `json:"origin,omitempty"`
	Region  string      `json:"region,omitempty"`
}
