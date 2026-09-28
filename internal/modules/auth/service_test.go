package auth

import "testing"

func TestUsernameGrammar(t *testing.T) {
	ok := []string{"alice", "Bob_2", "a.b-c", "ab", "abcdefghijklmnopqrstuvwxyz012345"}
	bad := []string{"a", "alice#0001", "alice@example", "al:ice", "al ice", "alice\n", "ünïcode", "abcdefghijklmnopqrstuvwxyz0123456"}
	for _, u := range ok {
		if !usernameRe.MatchString(u) {
			t.Errorf("%q should be a valid username", u)
		}
	}
	for _, u := range bad {
		if usernameRe.MatchString(u) {
			t.Errorf("%q should be rejected", u)
		}
	}
}
