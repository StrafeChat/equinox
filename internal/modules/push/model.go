// Package push delivers notifications to signed-in devices while the app is closed: new
// messages, mentions, incoming calls, and "read elsewhere" so a phone can clear what it
// showed. It speaks only standard Web Push - RFC 8030 delivery, RFC 8291 message
// encryption, RFC 8292 VAPID - to whatever endpoint the device registered: the push
// gateway of the app build it runs (which relays to FCM/APNs), a UnifiedPush distributor,
// or a browser's push service. The instance therefore needs no Google or Apple credentials,
// and every body is encrypted to the device's own key, so the gateway and the platform push
// services see ciphertext, one of four size buckets and timing - never who wrote what.
//
// The package is a leaf on purpose: the messages, rooms and voice modules call its
// Enqueue* producers (package-level, taking the Redis client like stargate.PublishToUser),
// so it must not import them back. What the delivery workers need to know about users,
// rooms and spaces comes through the Directory interface, implemented over the real
// repositories by push/pushdir.
package push

import (
	"time"
)

// Payload types (the "t" field). The device ignores anything it does not know.
const (
	TypeMessage = "msg"
	TypeRead    = "read"
	TypeCall    = "call"
	TypeCallEnd = "call_end"
	TypeTest    = "test"
)

// PayloadVersion is the "v" field; a device drops payloads with a version it does not know.
const PayloadVersion = 1

// Room types and per-room notify modes, mirroring rooms.Type* and rooms.NotifyMode* (this
// package cannot import rooms, which imports it).
const (
	roomTypePM         = 1
	roomTypeGroupPM    = 2
	roomTypeSpaceText  = 3
	roomTypeSpaceVoice = 4
	roomTypeThread     = 6

	notifyModeAll      = 1
	notifyModeMentions = 2
	notifyModeNone     = 3
)

// Subscription kinds: which transport the endpoint leads to. Informational for the
// instance (it sends the same Web Push to all of them); shown in the settings status line.
const (
	KindFCM         = "fcm"
	KindAPNs        = "apns"
	KindUnifiedPush = "unifiedpush"
	KindWeb         = "web"
)

// Preference values for Prefs.PM and Prefs.Space.
const (
	ModeAll      = "all"
	ModeMentions = "mentions"
	ModeNone     = "none"
)

// Prefs are the device's own notification choices, sent with the subscription because the
// decision is made here while the app is closed. They mirror the client's notification
// settings: preview text, the PM and space defaults, calls.
type Prefs struct {
	Preview bool   `json:"preview"`
	PM      string `json:"pm"`
	Space   string `json:"space"`
	Calls   bool   `json:"calls"`
}

// DefaultPrefs is what a subscription gets for any preference it leaves out.
func DefaultPrefs() Prefs {
	return Prefs{Preview: true, PM: ModeAll, Space: ModeMentions, Calls: true}
}

// Subscription is a push_subscriptions row: one per session.
type Subscription struct {
	UserID        int64      `db:"user_id"`
	SessionID     int64      `db:"session_id"`
	Endpoint      string     `db:"endpoint"`
	P256dh        string     `db:"p256dh"`
	Auth          string     `db:"auth"`
	Kind          string     `db:"kind"`
	PrefPreview   bool       `db:"pref_preview"`
	PrefPM        string     `db:"pref_pm"`
	PrefSpace     string     `db:"pref_space"`
	PrefCalls     bool       `db:"pref_calls"`
	CreatedAt     time.Time  `db:"created_at"`
	UpdatedAt     time.Time  `db:"updated_at"`
	LastSuccessAt *time.Time `db:"last_success_at"`
	LastError     string     `db:"last_error"`
	Failures      int        `db:"failures"`
}

// Prefs returns the subscription's preferences, with defaults for anything unset.
func (s *Subscription) Prefs() Prefs {
	p := Prefs{Preview: s.PrefPreview, PM: s.PrefPM, Space: s.PrefSpace, Calls: s.PrefCalls}
	d := DefaultPrefs()
	if !validMode(p.PM, false) {
		p.PM = d.PM
	}
	if !validMode(p.Space, true) {
		p.Space = d.Space
	}
	return p
}

func (s *Subscription) setPrefs(p Prefs) {
	s.PrefPreview, s.PrefPM, s.PrefSpace, s.PrefCalls = p.Preview, p.PM, p.Space, p.Calls
}

// validMode reports whether v is a valid PM mode (all/none) or, with space, a valid space
// mode (all/mentions/none). PMs have no "mentions" choice: in a two-person conversation
// every message is addressed to you.
func validMode(v string, space bool) bool {
	switch v {
	case ModeAll, ModeNone:
		return true
	case ModeMentions:
		return space
	}
	return false
}

// View is the API form of a subscription: what the device needs to show its status, and
// nothing that would let a reader push to it (no keys, no full endpoint).
type View struct {
	Kind          string     `json:"kind"`
	EndpointHost  string     `json:"endpoint_host"`
	Prefs         Prefs      `json:"prefs"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	LastSuccessAt *time.Time `json:"last_success_at"`
	LastError     string     `json:"last_error"`
	Failures      int        `json:"failures"`
}

// View returns the API form of the subscription.
func (s *Subscription) View() View {
	return View{
		Kind:          s.Kind,
		EndpointHost:  endpointHost(s.Endpoint),
		Prefs:         s.Prefs(),
		CreatedAt:     s.CreatedAt,
		UpdatedAt:     s.UpdatedAt,
		LastSuccessAt: s.LastSuccessAt,
		LastError:     s.LastError,
		Failures:      s.Failures,
	}
}

// SubscribeInput is the body of PUT /users/@me/push.
type SubscribeInput struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	Kind  string     `json:"kind"`
	Prefs PrefsPatch `json:"prefs"`
}

// PrefsPatch is a partial Prefs: nil fields keep their current (or default) value.
type PrefsPatch struct {
	Preview *bool   `json:"preview"`
	PM      *string `json:"pm"`
	Space   *string `json:"space"`
	Calls   *bool   `json:"calls"`
}

// UpdateInput is the body of PATCH /users/@me/push.
type UpdateInput struct {
	Prefs PrefsPatch `json:"prefs"`
}

// TestResult is the answer to POST /users/@me/push/test.
type TestResult struct {
	Delivered bool   `json:"delivered"`
	Status    int    `json:"status,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Author is the "a" object of a payload: who wrote the message or placed the call.
type Author struct {
	ID     string `json:"id"`
	Name   string `json:"n"`
	Avatar string `json:"av,omitempty"`
}

// Payload is the plaintext inside the encrypted Web Push body. Field names are short on
// purpose: the whole JSON must fit in 2047 bytes (the largest padding bucket less the
// delimiter). Ids are strings, like every other payload.
type Payload struct {
	V   int    `json:"v"`
	T   string `json:"t"`
	UID string `json:"uid"`
	TS  int64  `json:"ts"`

	RoomID    string `json:"rid,omitempty"`
	RoomType  int    `json:"rt,omitempty"`
	SpaceID   string `json:"sid,omitempty"`
	MessageID string `json:"mid,omitempty"`

	Author     *Author `json:"a,omitempty"`
	RoomName   string  `json:"rn,omitempty"`
	ParentName string  `json:"pn,omitempty"`
	SpaceName  string  `json:"sn,omitempty"`
	Encrypted  bool    `json:"e,omitempty"`
	Body       string  `json:"b,omitempty"`
	Attach     int     `json:"att,omitempty"`
	Mentioned  bool    `json:"m,omitempty"`
	Video      bool    `json:"vid,omitempty"`
}
