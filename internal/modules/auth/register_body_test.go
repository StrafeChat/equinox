package auth

import "testing"

// Regression: zog resolves a shape key to a struct field by the *entire* json tag string,
// so a `,omitempty` suffix makes the field unreachable and the value is silently dropped.
// That is how a client-chosen discriminator was being ignored at registration.
func TestParseRegisterBodyReadsEveryField(t *testing.T) {
	var in RegisterInput
	body := []byte(`{
		"email":"A@B.co","username":"abc","password":"correcthorsebattery",
		"date_of_birth":"2000-01-01T00:00:00.000Z",
		"discriminator":4242,"captcha_token":"TOK123"
	}`)
	if errs := ParseRegisterBody(body, &in); errs != nil {
		t.Fatalf("parse errors: %v", errs)
	}
	if in.CaptchaToken != "TOK123" {
		t.Errorf("CaptchaToken = %q, want TOK123", in.CaptchaToken)
	}
	if in.Discriminator == nil || *in.Discriminator != 4242 {
		t.Errorf("Discriminator = %v, want 4242", in.Discriminator)
	}
	if in.DateOfBirth.IsZero() {
		t.Error("DateOfBirth was dropped")
	}
}
