package spaces

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// stmtColumns pulls the comma-separated names between from and to out of a statement.
func stmtColumns(t *testing.T, stmt, from, to string) []string {
	t.Helper()
	i := strings.Index(stmt, from)
	j := strings.Index(stmt, to)
	if i < 0 || j < 0 || j < i {
		t.Fatalf("cannot find %q ... %q in %q", from, to, stmt)
	}
	var out []string
	for _, c := range strings.Split(stmt[i+len(from):j], ",") {
		out = append(out, strings.TrimSpace(c))
	}
	return out
}

// The two statements that spell space_members' columns out must agree with the table
// metadata every other member read goes through - a mismatch scans the wrong columns into
// the wrong fields, and nothing catches it until runtime (the positional trap).
func TestSpaceMemberColumnsMatchPageSelect(t *testing.T) {
	want := spaceMembersTable.Metadata().Columns
	if got := stmtColumns(t, listMembersPageStmt, "SELECT ", " FROM"); !reflect.DeepEqual(got, want) {
		t.Errorf("ListMembersPage selects %v, table has %v", got, want)
	}
	if got := stmtColumns(t, insertMemberStmt, "space_members (", ") VALUES"); !reflect.DeepEqual(got, want) {
		t.Errorf("AddMember inserts %v, table has %v", got, want)
	}
	binds := strings.Count(insertMemberStmt, "?")
	values := memberRowValues(1, 2, nil, MemberProfile{}, time.Now())
	if binds != len(want) || len(values) != len(want) {
		t.Errorf("AddMember: %d columns, %d placeholders, %d values", len(want), binds, len(values))
	}
}

func TestMemberRowValues(t *testing.T) {
	joined := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	got := memberRowValues(1, 2, []int64{3}, MemberProfile{Nick: "n", Bio: "b"}, joined)
	want := []interface{}{int64(1), int64(2), []int64{3}, "n", nil, nil, nil, "b", nil, joined}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("memberRowValues = %#v", got)
	}
	// A fresh join binds every profile column NULL, clearing whatever an earlier row left.
	for i, v := range memberRowValues(1, 2, nil, MemberProfile{}, joined)[3:9] {
		if v != nil {
			t.Errorf("profile column %d = %#v, want nil", i, v)
		}
	}
}
