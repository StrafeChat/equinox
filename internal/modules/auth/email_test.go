package auth

import (
	"net/http"
	"testing"
)

func TestValidEmailTokenShape(t *testing.T) {
	tok, err := newEmailToken()
	if err != nil {
		t.Fatal(err)
	}
	if !validEmailToken(tok) {
		t.Errorf("a freshly issued token must be valid in shape: %q", tok)
	}
	for _, bad := range []string{"", "abc", tok[:63], tok + "0", "zz" + tok[2:]} {
		if validEmailToken(bad) {
			t.Errorf("%q should be rejected before Redis is asked", bad)
		}
	}
}

// Only a local account with a real address can be written to: shadows of remote users
// are their home instance's to mail, and a bot's address is synthetic.
func TestCanBeMailed(t *testing.T) {
	remote := int64(5)
	cases := map[string]struct {
		u    *User
		want bool
	}{
		"nil":    {nil, false},
		"local":  {&User{Email: "sam@example.org"}, true},
		"bot":    {&User{Email: "bot+1@bots.invalid", Bot: true}, false},
		"remote": {&User{Email: "x@y", HomeDomain: "other.test", RemoteID: &remote}, false},
		"noaddr": {&User{Email: ""}, false},
	}
	for name, tc := range cases {
		if got := canBeMailed(tc.u); got != tc.want {
			t.Errorf("%s: canBeMailed = %v, want %v", name, got, tc.want)
		}
	}
}

func TestEmailErrorStatus(t *testing.T) {
	cases := []struct {
		err  error
		code int
		wire string
	}{
		{ErrEmailDisabled, http.StatusServiceUnavailable, "email_disabled"},
		{ErrEmailAlreadyVerified, http.StatusConflict, "email_already_verified"},
		{ErrEmailCooldown, http.StatusTooManyRequests, "email_cooldown"},
		{ErrEmailTokenInvalid, http.StatusBadRequest, "email_token_invalid"},
		{ErrResetTokenInvalid, http.StatusBadRequest, "reset_token_invalid"},
		{ErrPasswordTooShort, http.StatusBadRequest, "weak_password"},
		{ErrPasswordTooLong, http.StatusBadRequest, "weak_password"},
	}
	for _, tc := range cases {
		code, wire := emailErrorStatus(tc.err)
		if code != tc.code || wire != tc.wire {
			t.Errorf("%v: got %d %q, want %d %q", tc.err, code, wire, tc.code, tc.wire)
		}
	}
	if code, _ := emailErrorStatus(ErrInvalidCredentials); code != 0 {
		t.Errorf("unrelated errors must fall through to the 500 path, got %d", code)
	}
}

// Login's "verify first" answer is a typed error so the handler can say whether a link was
// just sent; it must never be confused with a plain credential failure.
func TestEmailUnverifiedErrorIsDistinct(t *testing.T) {
	var err error = &EmailUnverifiedError{Sent: true}
	if err.Error() != "email not verified" {
		t.Errorf("message = %q", err.Error())
	}
	if err == ErrInvalidCredentials {
		t.Error("must not compare equal to the credentials error")
	}
}
