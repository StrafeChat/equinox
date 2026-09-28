package devices

import (
	"strings"
	"testing"
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
