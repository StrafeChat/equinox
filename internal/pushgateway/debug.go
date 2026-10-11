package pushgateway

import (
	"context"
	"sync"
)

// debugCapacity is how many debug pushes are kept; older ones fall off.
const debugCapacity = 200

// DebugPush is one push received for a "debug" registration, as listed by
// GET /v1/debug/pushes.
type DebugPush struct {
	Seq     uint64 `json:"seq"`
	Token   string `json:"token"`
	Slot    string `json:"slot"`
	Urgency string `json:"urgency"`
	TTL     int    `json:"ttl"`
	Topic   string `json:"topic"`
	// P is the encrypted body, base64url - the same value FCM would carry in data.p.
	P string `json:"p"`
}

// debugStore is the development-only "debug" platform: instead of reaching a phone
// through Google or Apple, pushes are kept in memory for a developer (or the integration
// harness) to poll and replay into an emulator's debug receiver. That exercises the whole
// chain - instance encryption, VAPID, sealing, the app's decryption and rendering - except
// the last platform hop, which the FCM/APNs tests cover against httptest servers.
type debugStore struct {
	mu     sync.Mutex
	seq    uint64
	pushes []DebugPush
}

func (d *debugStore) Send(_ context.Context, m *Message) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seq++
	d.pushes = append(d.pushes, DebugPush{
		Seq:     d.seq,
		Token:   m.Reg.Token,
		Slot:    m.Reg.Slot,
		Urgency: m.Urgency,
		TTL:     m.TTL,
		Topic:   m.Topic,
		P:       b64.EncodeToString(m.Body),
	})
	if over := len(d.pushes) - debugCapacity; over > 0 {
		d.pushes = append(d.pushes[:0:0], d.pushes[over:]...)
	}
	return nil
}

// after lists the kept pushes with a sequence number greater than seq, oldest first.
func (d *debugStore) after(seq uint64) []DebugPush {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]DebugPush, 0, len(d.pushes))
	for _, p := range d.pushes {
		if p.Seq > seq {
			out = append(out, p)
		}
	}
	return out
}
