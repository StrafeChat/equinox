package auth

import "testing"

// Regression: zog resolves a shape key to a struct field by the *entire* json tag string,
// so a `,omitempty` suffix makes the field unreachable and the value is silently dropped.
// That is how a client-chosen field was once ignored at registration.
func TestParseRegisterBodyReadsEveryField(t *testing.T) {
	var in RegisterInput
	body := []byte(`{
		"email":"A@B.co","username":"abc","password":"correcthorsebattery",
		"date_of_birth":"2000-01-01T00:00:00.000Z",
		"captcha_token":"TOK123","invite":"  aB3dE6gH9k  "
	}`)
	if errs := ParseRegisterBody(body, &in); errs != nil {
		t.Fatalf("parse errors: %v", errs)
	}
	if in.CaptchaToken != "TOK123" {
		t.Errorf("CaptchaToken = %q, want TOK123", in.CaptchaToken)
	}
	if in.DateOfBirth.IsZero() {
		t.Error("DateOfBirth was dropped")
	}
	// Same trap as the fields above, plus the Trim() that lets someone paste a code with
	// a stray space and still get in.
	if in.Invite != "aB3dE6gH9k" {
		t.Errorf("Invite = %q, want aB3dE6gH9k", in.Invite)
	}
}
