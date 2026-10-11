package push

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/StrafeChat/equinox/internal/config"
)

// ---- a fake push service: an httptest TLS server that records what it receives ----------

type receivedPush struct {
	Header http.Header
	Body   []byte
}

type fakePushService struct {
	srv *httptest.Server

	mu       sync.Mutex
	received []receivedPush
	// statuses are answered in order; once used up, 201.
	statuses []int
	// retryAfter is sent with a 429.
	retryAfter string
}

func newFakePushService(t *testing.T, statuses ...int) *fakePushService {
	t.Helper()
	f := &fakePushService{statuses: statuses}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.received = append(f.received, receivedPush{Header: r.Header.Clone(), Body: body})
		status := http.StatusCreated
		if len(f.statuses) > 0 {
			status, f.statuses = f.statuses[0], f.statuses[1:]
		}
		retryAfter := f.retryAfter
		f.mu.Unlock()
		if status == http.StatusTooManyRequests && retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePushService) pushes() []receivedPush {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]receivedPush(nil), f.received...)
}

// ---- a device: receiver keys and the subscription that names them ----------------------

type testDevice struct {
	priv *ecdh.PrivateKey
	auth []byte
}

func newTestDevice(t *testing.T) *testDevice {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	return &testDevice{priv: priv, auth: auth}
}

func (d *testDevice) subscription(userID, sessionID int64, endpoint string, prefs Prefs) Subscription {
	s := Subscription{
		UserID: userID, SessionID: sessionID, Endpoint: endpoint,
		P256dh: b64(d.priv.PublicKey().Bytes()), Auth: b64(d.auth), Kind: KindFCM,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.setPrefs(prefs)
	return s
}

// open decrypts a received push and decodes its payload.
func (d *testDevice) open(t *testing.T, p receivedPush) Payload {
	t.Helper()
	plain := decryptBody(t, p.Body, d.priv, d.auth)
	var out Payload
	if err := json.Unmarshal(plain, &out); err != nil {
		t.Fatalf("payload is not JSON: %q", plain)
	}
	return out
}

// ---- a sender that trusts the fake push service ----------------------------------------

func testVAPID(t *testing.T) *VAPID {
	t.Helper()
	k, _ := ecdh.P256().GenerateKey(rand.Reader)
	v, err := NewVAPID(b64(k.Bytes()), "mailto:ops@example.com")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// testPushConfig allows the loopback test server, as PUSH_ALLOW_INSECURE does in dev.
func testPushConfig() config.PushConfig {
	return config.PushConfig{Enabled: true, AllowInsecure: true, Workers: 1, VAPIDSubject: "mailto:ops@example.com"}
}

func trustServer(t *testing.T, s *Sender, srv *httptest.Server) {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	s.client.Transport.(*http.Transport).TLSClientConfig.RootCAs = pool
	s.sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() } // no real waits
}

// verifyVAPID checks an Authorization header the way a push service does (RFC 8292) and
// returns the JWT claims.
func verifyVAPID(t *testing.T, header, wantKey string) map[string]any {
	t.Helper()
	if !strings.HasPrefix(header, "vapid ") {
		t.Fatalf("Authorization %q is not a vapid one", header)
	}
	var token, key string
	for _, part := range strings.Split(strings.TrimPrefix(header, "vapid "), ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "t="):
			token = part[2:]
		case strings.HasPrefix(part, "k="):
			key = part[2:]
		}
	}
	if key != wantKey {
		t.Fatalf("k = %q, want the instance key %q", key, wantKey)
	}
	rawKey, err := decodeB64(key)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), rawKey)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT %q has %d parts", token, len(parts))
	}
	hdr, _ := decodeB64(parts[0])
	if string(hdr) != `{"typ":"JWT","alg":"ES256"}` {
		t.Errorf("JWT header = %s", hdr)
	}
	sig, _ := decodeB64(parts[2])
	if len(sig) != 64 {
		t.Fatalf("signature is %d bytes, want raw r||s (64)", len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, digest[:], r, s) {
		t.Fatal("VAPID signature does not verify with k")
	}
	rawClaims, _ := decodeB64(parts[1])
	var claims map[string]any
	if err := json.Unmarshal(rawClaims, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

// ---- in-memory Repository and Directory -------------------------------------------------

type memRepo struct {
	mu   sync.Mutex
	subs map[[2]int64]Subscription
	key  string
}

func newMemRepo(subs ...Subscription) *memRepo {
	r := &memRepo{subs: map[[2]int64]Subscription{}}
	for _, s := range subs {
		r.subs[[2]int64{s.UserID, s.SessionID}] = s
	}
	return r
}

func (r *memRepo) Put(_ context.Context, s *Subscription) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.subs[[2]int64{s.UserID, s.SessionID}] = *s
	return nil
}

func (r *memRepo) Get(_ context.Context, userID, sessionID int64) (*Subscription, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.subs[[2]int64{userID, sessionID}]
	if !ok {
		return nil, nil
	}
	return &s, nil
}

func (r *memRepo) ListByUser(_ context.Context, userID int64) ([]Subscription, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Subscription
	for k, s := range r.subs {
		if k[0] == userID {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out, nil
}

func (r *memRepo) Delete(_ context.Context, userID, sessionID int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.subs, [2]int64{userID, sessionID})
	n := 0
	for k := range r.subs {
		if k[0] == userID {
			n++
		}
	}
	return n, nil
}

func (r *memRepo) update(userID, sessionID int64, fn func(*Subscription)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := [2]int64{userID, sessionID}
	s := r.subs[k] // an upsert, like the CQL UPDATE
	s.UserID, s.SessionID = userID, sessionID
	fn(&s)
	r.subs[k] = s
}

func (r *memRepo) UpdatePrefs(_ context.Context, userID, sessionID int64, p Prefs, at time.Time) error {
	r.update(userID, sessionID, func(s *Subscription) { s.setPrefs(p); s.UpdatedAt = at })
	return nil
}

func (r *memRepo) RecordSuccess(_ context.Context, userID, sessionID int64, at time.Time) error {
	r.update(userID, sessionID, func(s *Subscription) { s.LastSuccessAt, s.LastError, s.Failures = &at, "", 0 })
	return nil
}

func (r *memRepo) RecordFailure(_ context.Context, userID, sessionID int64, reason string, failures int) error {
	r.update(userID, sessionID, func(s *Subscription) { s.LastError, s.Failures = reason, failures })
	return nil
}

func (r *memRepo) ForEachUser(_ context.Context, fn func(int64) error) error {
	r.mu.Lock()
	seen := map[int64]bool{}
	for k := range r.subs {
		seen[k[0]] = true
	}
	r.mu.Unlock()
	for uid := range seen {
		if err := fn(uid); err != nil {
			return err
		}
	}
	return nil
}

func (r *memRepo) ClaimVAPIDKey(_ context.Context, candidate string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.key == "" {
		r.key = candidate
	}
	return r.key, nil
}

type memDir struct {
	users    map[int64]*UserInfo
	rooms    map[int64]*RoomInfo
	members  map[int64][]int64
	settings map[[2]int64]*RoomSettings
	spaces   map[int64]*SpaceInfo
	roles    map[int64]map[int64]string
	// hidden: (user, room) pairs the user may not view.
	hidden map[[2]int64]bool
	// revoked sessions.
	revoked map[[2]int64]bool
}

func newMemDir() *memDir {
	return &memDir{
		users: map[int64]*UserInfo{}, rooms: map[int64]*RoomInfo{}, members: map[int64][]int64{},
		settings: map[[2]int64]*RoomSettings{}, spaces: map[int64]*SpaceInfo{}, roles: map[int64]map[int64]string{},
		hidden: map[[2]int64]bool{}, revoked: map[[2]int64]bool{},
	}
}

func (d *memDir) User(_ context.Context, id int64) (*UserInfo, error) { return d.users[id], nil }
func (d *memDir) SessionActive(_ context.Context, userID, sessionID int64) (bool, error) {
	return !d.revoked[[2]int64{userID, sessionID}], nil
}
func (d *memDir) Room(_ context.Context, id int64) (*RoomInfo, error) { return d.rooms[id], nil }
func (d *memDir) RoomParticipants(_ context.Context, id int64) ([]int64, error) {
	return d.members[id], nil
}
func (d *memDir) RoomSettings(_ context.Context, userID, roomID int64) (*RoomSettings, error) {
	return d.settings[[2]int64{userID, roomID}], nil
}
func (d *memDir) Space(_ context.Context, id int64) (*SpaceInfo, error) { return d.spaces[id], nil }
func (d *memDir) RoleNames(_ context.Context, id int64) (map[int64]string, error) {
	return d.roles[id], nil
}
func (d *memDir) CanView(_ context.Context, userID int64, room *RoomInfo) (bool, error) {
	return !d.hidden[[2]int64{userID, room.ID}], nil
}

// newTestService is a Service over in-memory storage whose sender trusts the fake push
// service. No Redis: the producers' side is exercised separately.
func newTestService(t *testing.T, f *fakePushService, repo *memRepo, dir *memDir) *Service {
	t.Helper()
	svc := NewService(repo, dir, nil, testPushConfig(), testVAPID(t))
	trustServer(t, svc.sender, f.srv)
	return svc
}
