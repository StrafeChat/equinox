package rooms

import (
	"strings"
	"testing"
)

// Create and AddParticipant bind rooms_by_user positionally, so the statement's arity has
// to match the values they pass. It previously came from roomsByUserTable.Insert(), which
// covers *every* column in the metadata - adding the per-room mute settings there silently
// took it from 7 placeholders to 10, and every room creation started failing with
// "batch statement 2 expected 10 values send got 7" the first time the path ran.
func TestInsertRoomsByUserArityMatchesCallers(t *testing.T) {
	const wantValues = 7 // user_id, room_id, last_message_id, last_read_message_id, mention_count, joined_at, mention_count_baseline

	if got := strings.Count(insertRoomsByUserStmt, "?"); got != wantValues {
		t.Fatalf("statement has %d bind markers, callers pass %d: %s", got, wantValues, insertRoomsByUserStmt)
	}
	for _, col := range []string{
		"user_id", "room_id", "last_message_id", "last_read_message_id",
		"mention_count", "joined_at", "mention_count_baseline",
	} {
		if !strings.Contains(insertRoomsByUserStmt, col) {
			t.Errorf("statement is missing column %q: %s", col, insertRoomsByUserStmt)
		}
	}
}

// Create and CreateSpaceRoom bind the rooms row positionally too; the voice columns
// (user_limit, bitrate), then permissions_synced, then the eight thread columns were added
// after the rest, and both callers pass them.
func TestInsertRoomArityMatchesCallers(t *testing.T) {
	const wantValues = 24
	if got := strings.Count(insertRoomStmt, "?"); got != wantValues {
		t.Fatalf("statement has %d bind markers, callers pass %d: %s", got, wantValues, insertRoomStmt)
	}
	for _, col := range roomsTable.Metadata().Columns {
		if !strings.Contains(insertRoomStmt, col) {
			t.Errorf("statement is missing column %q: %s", col, insertRoomStmt)
		}
	}
	// The bind order in Create/CreateSpaceRoom follows the table metadata order; keep
	// the statement's column order identical so a reorder cannot silently swap values.
	want := "INSERT INTO rooms (" + strings.Join(roomsTable.Metadata().Columns, ",") + ")"
	if !strings.HasPrefix(insertRoomStmt, want) {
		t.Errorf("column order drifted from roomsTable metadata:\n got %s\nwant %s...", insertRoomStmt, want)
	}
}

// The columns deliberately left out of the create-time insert are per-user preferences
// that mean "no override" when absent. If one of them ever needs a value at creation, the
// callers have to change too - this keeps that decision explicit.
func TestInsertRoomsByUserOmitsPreferenceColumns(t *testing.T) {
	for _, col := range []string{"muted", "muted_until", "notify_mode"} {
		if strings.Contains(insertRoomsByUserStmt, col) {
			t.Errorf("column %q is now written at create time; update Create/AddParticipant to bind it: %s", col, insertRoomsByUserStmt)
		}
	}
}
