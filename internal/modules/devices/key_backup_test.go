package devices

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// The whole point of keeping first_message_index and is_verified in the clear is so a second
// device uploading the same session can't overwrite a copy that reads further back in the
// room's history. Get this ordering wrong and a multi-device account silently loses the
// ability to decrypt its older messages.
func TestBetterThan(t *testing.T) {
	base := func() KeyBackupSession {
		return KeyBackupSession{FirstMessageIndex: 10, ForwardedCount: 1, IsVerified: false}
	}

	for _, tc := range []struct {
		name      string
		candidate func(s *KeyBackupSession)
		want      bool
	}{
		{"nothing stored yet", nil, true},
		{"verified beats unverified", func(s *KeyBackupSession) { s.IsVerified = true }, true},
		{"earlier message index wins", func(s *KeyBackupSession) { s.FirstMessageIndex = 0 }, true},
		{"later message index loses", func(s *KeyBackupSession) { s.FirstMessageIndex = 20 }, false},
		{"fewer forwards wins", func(s *KeyBackupSession) { s.ForwardedCount = 0 }, true},
		{"more forwards loses", func(s *KeyBackupSession) { s.ForwardedCount = 5 }, false},
		{"identical is not an improvement", func(s *KeyBackupSession) {}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := base()
			candidate := base()
			if tc.candidate == nil {
				if !candidate.BetterThan(nil) {
					t.Fatal("a session should always be better than no session")
				}
				return
			}
			tc.candidate(&candidate)
			if got := candidate.BetterThan(&stored); got != tc.want {
				t.Fatalf("BetterThan = %v, want %v (candidate %+v, stored %+v)", got, tc.want, candidate, stored)
			}
		})
	}
}

// Verified always wins, even when the unverified copy reads further back. That is the spec's
// ordering, and it matters: an unverified session may have been forwarded by a device whose
// claims about it can't be checked.
func TestBetterThanPrefersVerifiedOverEarlierIndex(t *testing.T) {
	stored := KeyBackupSession{FirstMessageIndex: 0, IsVerified: false}
	candidate := KeyBackupSession{FirstMessageIndex: 100, IsVerified: true}
	if !candidate.BetterThan(&stored) {
		t.Fatal("a verified session should replace an unverified one")
	}
	if stored.BetterThan(&candidate) {
		t.Fatal("an unverified session should not replace a verified one")
	}
}

// PutBackupSessions binds this statement positionally inside a batch, so its arity has to
// match what the loop passes. Taking it from keyBackupSessionsTable.Insert() would silently
// change that the moment a column is added to the metadata - which is exactly how room
// creation broke ("batch statement 2 expected 10 values send got 7").
func TestInsertBackupSessionArity(t *testing.T) {
	const wantValues = 9 // user_id, version, room_id, session_id, session_data, first_message_index, forwarded_count, is_verified, created_at

	if got := strings.Count(insertBackupSessionStmt, "?"); got != wantValues {
		t.Fatalf("statement has %d bind markers, PutBackupSessions passes %d: %s", got, wantValues, insertBackupSessionStmt)
	}
	for _, col := range []string{
		"user_id", "version", "room_id", "session_id", "session_data",
		"first_message_index", "forwarded_count", "is_verified", "created_at",
	} {
		if !strings.Contains(insertBackupSessionStmt, col) {
			t.Errorf("statement is missing column %q: %s", col, insertBackupSessionStmt)
		}
	}
}

// A version whose auth_data has no public key is a backup nothing can ever write to: every
// client declines it silently, so it has to be refused at creation instead.
func TestValidateBackupAuthData(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    string
		wantErr bool
	}{
		{"public key present", `{"public_key":"eU+feAAWLmBZ"}`, false},
		{"extra fields are fine", `{"public_key":"abc","signatures":{}}`, false},
		{"empty object", `{}`, true},
		{"empty public key", `{"public_key":""}`, true},
		{"not an object", `"just a string"`, true},
		{"malformed json", `{`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateBackupAuthData(tc.data); (err != nil) != tc.wantErr {
				t.Fatalf("validateBackupAuthData(%s) = %v, wantErr %v", tc.data, err, tc.wantErr)
			}
		})
	}
}

// A restore pages through the partition by clustering position. The cursor has to survive a
// round trip exactly, including ids that contain regular base64 or URL metacharacters, or a
// restore silently resumes in the wrong place and loses whatever it skipped.
func TestBackupCursorRoundTrip(t *testing.T) {
	for _, tc := range []struct{ room, session string }{
		{"!123:a.local", "AbC+/dEf=="},
		{"!room-with-dash:example.org", "sess?ion&id=1"},
		{"!r:d", ""},
	} {
		room, session, err := decodeBackupCursor(encodeBackupCursor(tc.room, tc.session))
		if err != nil {
			t.Fatalf("decode(%q, %q): %v", tc.room, tc.session, err)
		}
		if room != tc.room || session != tc.session {
			t.Errorf("round trip gave (%q, %q), want (%q, %q)", room, session, tc.room, tc.session)
		}
	}
}

// An empty cursor means "start at the beginning", and ("", "") sorts before every real row.
func TestEmptyBackupCursorStartsAtBeginning(t *testing.T) {
	room, session, err := decodeBackupCursor("")
	if err != nil || room != "" || session != "" {
		t.Fatalf("got (%q, %q, %v), want empty start", room, session, err)
	}
}

func TestMalformedBackupCursorIsRejected(t *testing.T) {
	for _, cursor := range []string{"!!!not-base64!!!", "bm8tc2VwYXJhdG9y"} {
		if _, _, err := decodeBackupCursor(cursor); err == nil {
			t.Errorf("cursor %q was accepted, want an error", cursor)
		}
	}
}

// The paging query has to constrain the partition *and* slice on the clustering columns.
// Without the slice it returns the same first page forever and a restore never finishes.
func TestListBackupSessionsPagesByClusteringPosition(t *testing.T) {
	if !strings.Contains(listBackupSessionsPageStmt, "(room_id,session_id)>(?,?)") {
		t.Errorf("statement does not slice on the clustering columns: %s", listBackupSessionsPageStmt)
	}
	if !strings.Contains(listBackupSessionsPageStmt, "user_id=?") || !strings.Contains(listBackupSessionsPageStmt, "version=?") {
		t.Errorf("statement does not constrain the partition key: %s", listBackupSessionsPageStmt)
	}
	if !strings.Contains(listBackupSessionsPageStmt, "LIMIT") {
		t.Errorf("statement is unbounded, so one page could be the whole backup: %s", listBackupSessionsPageStmt)
	}
}

// A whole version's keys are deleted by partition, not by primary key: the caller has no room
// or session id to give, and one tombstone is the point. table.DeleteBuilder() would add the
// clustering columns to the WHERE clause and turn this into a statement nothing can bind.
func TestDeleteBackupSessionsIsPartitionWide(t *testing.T) {
	if got := strings.Count(deleteBackupSessionsStmt, "?"); got != 2 {
		t.Fatalf("expected user_id and version only, got %d bind markers: %s", got, deleteBackupSessionsStmt)
	}
	for _, col := range []string{"room_id", "session_id"} {
		if strings.Contains(deleteBackupSessionsStmt, col) {
			t.Errorf("statement constrains %q, so it can no longer delete a whole version: %s", col, deleteBackupSessionsStmt)
		}
	}
}

// --- Version retention

// fakeBackupRepo is the in-memory slice of Repository the backup-version code touches. The
// nil Repository embed makes every other method panic, which is what a stray call deserves.
type fakeBackupRepo struct {
	Repository
	versions map[int64][]KeyBackupVersion           // per user, in insertion order
	sessions map[int64]map[int64][]KeyBackupSession // per user, per version
}

func newFakeBackupRepo() *fakeBackupRepo {
	return &fakeBackupRepo{
		versions: map[int64][]KeyBackupVersion{},
		sessions: map[int64]map[int64][]KeyBackupSession{},
	}
}

func (r *fakeBackupRepo) CreateBackupVersion(_ context.Context, v *KeyBackupVersion) error {
	r.versions[v.UserID] = append(r.versions[v.UserID], *v)
	return nil
}

// ListBackupVersions mirrors the table's CLUSTERING ORDER BY (version DESC): newest first.
func (r *fakeBackupRepo) ListBackupVersions(_ context.Context, userID int64, limit int) ([]KeyBackupVersion, error) {
	out := append([]KeyBackupVersion(nil), r.versions[userID]...)
	sort.Slice(out, func(i, j int) bool { return out[i].Version > out[j].Version })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *fakeBackupRepo) GetBackupVersion(_ context.Context, userID, version int64) (*KeyBackupVersion, error) {
	for _, v := range r.versions[userID] {
		if v.Version == version {
			v := v
			return &v, nil
		}
	}
	return nil, nil
}

func (r *fakeBackupRepo) DeleteBackupVersion(_ context.Context, userID, version int64) error {
	kept := r.versions[userID][:0]
	for _, v := range r.versions[userID] {
		if v.Version != version {
			kept = append(kept, v)
		}
	}
	r.versions[userID] = kept
	return nil
}

func (r *fakeBackupRepo) GetBackupSessions(_ context.Context, userID, version int64, roomID string, sessionIDs []string) (map[string]KeyBackupSession, error) {
	want := make(map[string]bool, len(sessionIDs))
	for _, id := range sessionIDs {
		want[id] = true
	}
	out := map[string]KeyBackupSession{}
	for _, s := range r.sessions[userID][version] {
		if s.RoomID == roomID && want[s.SessionID] {
			out[s.SessionID] = s
		}
	}
	return out, nil
}

func (r *fakeBackupRepo) PutBackupSessions(_ context.Context, sessions []KeyBackupSession) error {
	for _, s := range sessions {
		if r.sessions[s.UserID] == nil {
			r.sessions[s.UserID] = map[int64][]KeyBackupSession{}
		}
		rows := r.sessions[s.UserID][s.Version]
		replaced := false
		for i, prev := range rows {
			if prev.RoomID == s.RoomID && prev.SessionID == s.SessionID {
				rows[i] = s
				replaced = true
			}
		}
		if !replaced {
			rows = append(rows, s)
		}
		r.sessions[s.UserID][s.Version] = rows
	}
	return nil
}

func (r *fakeBackupRepo) ListBackupSessions(_ context.Context, userID, version int64, _, _ string) ([]KeyBackupSession, error) {
	return append([]KeyBackupSession(nil), r.sessions[userID][version]...), nil
}

func (r *fakeBackupRepo) CountBackupSessions(_ context.Context, userID, version int64) (int, error) {
	return len(r.sessions[userID][version]), nil
}

func (r *fakeBackupRepo) DeleteBackupSessions(_ context.Context, userID, version int64) error {
	delete(r.sessions[userID], version)
	return nil
}

// backdate moves a version's created_at, standing in for the passage of time.
func (r *fakeBackupRepo) backdate(userID, version int64, by time.Duration) {
	for i := range r.versions[userID] {
		if r.versions[userID][i].Version == version {
			r.versions[userID][i].CreatedAt = time.Now().UTC().Add(-by)
		}
	}
}

// versionIDs is what the store holds for a user, newest first.
func (r *fakeBackupRepo) versionIDs(userID int64) []int64 {
	rows, _ := r.ListBackupVersions(context.Background(), userID, 100)
	out := make([]int64, 0, len(rows))
	for _, v := range rows {
		out = append(out, v.Version)
	}
	return out
}

const testBackupUser int64 = 7

func newBackupService() (*Service, *fakeBackupRepo) {
	repo := newFakeBackupRepo()
	return NewService(repo, nil, nil), repo
}

func mustCreateBackup(t *testing.T, svc *Service) *KeyBackupVersion {
	t.Helper()
	v, err := svc.CreateBackupVersion(context.Background(), testBackupUser, backupAlgorithm, `{"public_key":"pk"}`, "wrapped", "salt")
	if err != nil {
		t.Fatalf("CreateBackupVersion: %v", err)
	}
	return v
}

// backupKeysInput builds a one-session upload body the way the client does: through JSON,
// since PutBackupKeysInput.Rooms is an anonymous struct type.
func backupKeysInput(t *testing.T, room, session string) *PutBackupKeysInput {
	t.Helper()
	raw := fmt.Sprintf(`{"rooms":{%q:{"sessions":{%q:{"first_message_index":0,"forwarded_count":0,"is_verified":true,"session_data":{"ciphertext":"x"}}}}}}`, room, session)
	var in PutBackupKeysInput
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatalf("building upload body: %v", err)
	}
	return &in
}

func mustPutBackupKey(t *testing.T, svc *Service, version int64, room, session string) {
	t.Helper()
	if _, _, err := svc.PutBackupKeys(context.Background(), testBackupUser, version, backupKeysInput(t, room, session)); err != nil {
		t.Fatalf("PutBackupKeys(%d): %v", version, err)
	}
}

func assertVersions(t *testing.T, repo *fakeBackupRepo, want ...int64) {
	t.Helper()
	got := repo.versionIDs(testBackupUser)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("stored versions = %v, want %v", got, want)
	}
}

// Creating a version no longer destroys the one it replaces. That was the whole exploit:
// anyone who could POST a version wiped the user's only copy of their history in the same
// request. The previous version - keys included - has to survive the replacement.
func TestCreateBackupVersionRetainsThePrevious(t *testing.T) {
	svc, repo := newBackupService()
	v1 := mustCreateBackup(t, svc)
	mustPutBackupKey(t, svc, v1.Version, "!r:a", "s1")

	v2 := mustCreateBackup(t, svc)
	assertVersions(t, repo, v2.Version, v1.Version)
	if n, _ := repo.CountBackupSessions(context.Background(), testBackupUser, v1.Version); n != 1 {
		t.Fatalf("previous version lost its keys: %d sessions, want 1", n)
	}

	current, previous, err := svc.BackupVersions(context.Background(), testBackupUser)
	if err != nil || current == nil || previous == nil {
		t.Fatalf("BackupVersions = (%v, %v, %v), want both set", current, previous, err)
	}
	if current.Version != v2.Version || previous.Version != v1.Version {
		t.Fatalf("BackupVersions = (%d, %d), want (%d, %d)", current.Version, previous.Version, v2.Version, v1.Version)
	}
}

// Exactly one superseded version is kept: the retention exists to undo one bad replacement,
// not to accumulate every recovery code the account ever had.
func TestThirdBackupVersionDropsTheOldest(t *testing.T) {
	svc, repo := newBackupService()
	v1 := mustCreateBackup(t, svc)
	mustPutBackupKey(t, svc, v1.Version, "!r:a", "s1")
	v2 := mustCreateBackup(t, svc)
	v3 := mustCreateBackup(t, svc)

	assertVersions(t, repo, v3.Version, v2.Version)
	if n, _ := repo.CountBackupSessions(context.Background(), testBackupUser, v1.Version); n != 0 {
		t.Fatalf("oldest version's keys were left behind: %d sessions", n)
	}
}

// The retained version lives for a week after it was superseded, measured by the current
// version's age. The sweep is lazy, on writes and the recovery read; the hot version poll
// must only stop *reporting* the expired version, never do the deleting itself.
func TestSupersededBackupVersionExpiresAfterRetention(t *testing.T) {
	t.Run("put sweeps it", func(t *testing.T) {
		svc, repo := newBackupService()
		v1 := mustCreateBackup(t, svc)
		v2 := mustCreateBackup(t, svc)
		repo.backdate(testBackupUser, v2.Version, supersededBackupRetention+time.Hour)

		_, previous, err := svc.BackupVersions(context.Background(), testBackupUser)
		if err != nil || previous != nil {
			t.Fatalf("expired previous still reported: %v, %v", previous, err)
		}
		assertVersions(t, repo, v2.Version, v1.Version) // reported gone, but the poll does not delete

		mustPutBackupKey(t, svc, v2.Version, "!r:a", "s1")
		assertVersions(t, repo, v2.Version)
	})

	t.Run("recovery read sweeps it", func(t *testing.T) {
		svc, repo := newBackupService()
		mustCreateBackup(t, svc)
		v2 := mustCreateBackup(t, svc)
		repo.backdate(testBackupUser, v2.Version, supersededBackupRetention+time.Hour)

		got, err := svc.RecoverableBackupVersion(context.Background(), testBackupUser, 0)
		if err != nil || got == nil || got.Version != v2.Version {
			t.Fatalf("RecoverableBackupVersion(0) = %v, %v, want %d", got, err, v2.Version)
		}
		assertVersions(t, repo, v2.Version)
	})

	t.Run("create keeps the one it replaces even when that one is old", func(t *testing.T) {
		svc, repo := newBackupService()
		mustCreateBackup(t, svc)
		v2 := mustCreateBackup(t, svc)
		repo.backdate(testBackupUser, v2.Version, supersededBackupRetention+time.Hour)

		v3 := mustCreateBackup(t, svc)
		// v2 served for over a week; it is freshly superseded, so it stays. v1 goes.
		assertVersions(t, repo, v3.Version, v2.Version)
	})

	t.Run("a day short is still retained", func(t *testing.T) {
		svc, repo := newBackupService()
		v1 := mustCreateBackup(t, svc)
		v2 := mustCreateBackup(t, svc)
		repo.backdate(testBackupUser, v2.Version, supersededBackupRetention-24*time.Hour)

		mustPutBackupKey(t, svc, v2.Version, "!r:a", "s1")
		assertVersions(t, repo, v2.Version, v1.Version)
	})
}

// Uploads go to the current version only. A device still pointed at the superseded one has
// to find out the version moved, and the 404 is how one that missed KEY_BACKUP_UPDATE does -
// while the keys already in that version stay readable for a restore.
func TestPutBackupKeysOnlyAcceptsTheCurrentVersion(t *testing.T) {
	svc, _ := newBackupService()
	v1 := mustCreateBackup(t, svc)
	mustPutBackupKey(t, svc, v1.Version, "!r:a", "s1")
	mustCreateBackup(t, svc)

	if _, _, err := svc.PutBackupKeys(context.Background(), testBackupUser, v1.Version, backupKeysInput(t, "!r:a", "s2")); err != ErrBackupVersionNotFound {
		t.Fatalf("upload to the superseded version: err = %v, want ErrBackupVersionNotFound", err)
	}
	rooms, _, err := svc.GetBackupKeys(context.Background(), testBackupUser, v1.Version, "")
	if err != nil || len(rooms) != 1 {
		t.Fatalf("superseded version is not readable: rooms = %v, err = %v", rooms, err)
	}
}

// A restore may name the current version or the retained one, nothing else - the endpoint
// hands out wrapped recovery material and should not become a way to probe for old rows.
func TestRecoverableBackupVersion(t *testing.T) {
	ctx := context.Background()
	svc, _ := newBackupService()

	if got, err := svc.RecoverableBackupVersion(ctx, testBackupUser, 0); err != nil || got != nil {
		t.Fatalf("no backup yet: got %v, %v, want nil, nil", got, err)
	}
	if _, err := svc.RecoverableBackupVersion(ctx, testBackupUser, 12345); err != ErrBackupVersionNotFound {
		t.Fatalf("no backup yet, named version: err = %v, want ErrBackupVersionNotFound", err)
	}

	v1 := mustCreateBackup(t, svc)
	v2 := mustCreateBackup(t, svc)
	for _, tc := range []struct {
		name    string
		ask     int64
		want    int64
		wantErr error
	}{
		{"unspecified is the current one", 0, v2.Version, nil},
		{"current by id", v2.Version, v2.Version, nil},
		{"retained previous by id", v1.Version, v1.Version, nil},
		{"anything else", v1.Version - 1, 0, ErrBackupVersionNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := svc.RecoverableBackupVersion(ctx, testBackupUser, tc.ask)
			if err != tc.wantErr {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && (got == nil || got.Version != tc.want) {
				t.Fatalf("got %v, want version %d", got, tc.want)
			}
		})
	}
}

// An explicit delete is still immediate - "I lost my recovery code" means the keys under it
// are unreachable and should not linger - and deleting the current version promotes the
// retained one, which is how a user undoes a replacement that was not theirs.
func TestDeleteBackupVersionIsImmediate(t *testing.T) {
	ctx := context.Background()
	svc, repo := newBackupService()
	v1 := mustCreateBackup(t, svc)
	mustPutBackupKey(t, svc, v1.Version, "!r:a", "s1")
	v2 := mustCreateBackup(t, svc)
	mustPutBackupKey(t, svc, v2.Version, "!r:a", "s2")

	if err := svc.DeleteBackupVersion(ctx, testBackupUser, v2.Version); err != nil {
		t.Fatalf("DeleteBackupVersion(current): %v", err)
	}
	assertVersions(t, repo, v1.Version)
	if n, _ := repo.CountBackupSessions(ctx, testBackupUser, v2.Version); n != 0 {
		t.Fatalf("deleted version kept %d sessions", n)
	}
	current, previous, err := svc.BackupVersions(ctx, testBackupUser)
	if err != nil || current == nil || current.Version != v1.Version || previous != nil {
		t.Fatalf("after deleting the current version: (%v, %v, %v), want v1 current and no previous", current, previous, err)
	}

	if err := svc.DeleteBackupVersion(ctx, testBackupUser, v1.Version); err != nil {
		t.Fatalf("DeleteBackupVersion(last): %v", err)
	}
	assertVersions(t, repo)
	if err := svc.DeleteBackupVersion(ctx, testBackupUser, v1.Version); err != ErrBackupVersionNotFound {
		t.Fatalf("deleting twice: err = %v, want ErrBackupVersionNotFound", err)
	}
}

func TestRetainedBackupVersions(t *testing.T) {
	now := time.Now().UTC()
	at := func(age time.Duration) time.Time { return now.Add(-age) }
	v := func(version int64, age time.Duration) KeyBackupVersion {
		return KeyBackupVersion{Version: version, CreatedAt: at(age)}
	}
	for _, tc := range []struct {
		name     string
		versions []KeyBackupVersion
		keep     int
		drop     int
	}{
		{"none", nil, 0, 0},
		{"one", []KeyBackupVersion{v(3, time.Hour)}, 1, 0},
		{"fresh current keeps its predecessor", []KeyBackupVersion{v(3, time.Hour), v(2, 48*time.Hour)}, 2, 0},
		{"old current drops its predecessor", []KeyBackupVersion{v(3, 8*24*time.Hour), v(2, 9*24*time.Hour)}, 1, 1},
		{"only the immediate predecessor survives", []KeyBackupVersion{v(3, time.Hour), v(2, time.Hour), v(1, time.Hour), v(0, time.Hour)}, 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keep, drop := retainedBackupVersions(tc.versions, now)
			if len(keep) != tc.keep || len(drop) != tc.drop {
				t.Fatalf("keep %d / drop %d, want %d / %d", len(keep), len(drop), tc.keep, tc.drop)
			}
			for i := 1; i < len(keep); i++ {
				if keep[i].Version > keep[i-1].Version {
					t.Fatalf("keep is not newest-first: %v", keep)
				}
			}
			for _, d := range drop {
				for _, k := range keep {
					if d.Version > k.Version {
						t.Fatalf("dropped %d, newer than kept %d", d.Version, k.Version)
					}
				}
			}
		})
	}
}
