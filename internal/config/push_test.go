package config

import (
	"strings"
	"testing"
)

// Nothing set: push is on, with four workers and a subject derived from the instance's
// own name - no operator action needed.
func TestPushDefaults(t *testing.T) {
	p := loadPushConfig("https://web.example.com", "chat.example.com")
	if !p.Enabled || p.Workers != 4 || p.AllowInsecure || len(p.AllowedHosts) != 0 {
		t.Fatalf("unexpected defaults: %+v", p)
	}
	if p.VAPIDSubject != "https://chat.example.com" {
		t.Errorf("subject = %q, want the federation domain", p.VAPIDSubject)
	}
	if err := validatePush(p); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}
	if got := loadPushConfig("https://web.example.com:8443/app", "").VAPIDSubject; got != "https://web.example.com:8443" {
		t.Errorf("subject without federation = %q, want the WEB_URL host", got)
	}
	if got := loadPushConfig("", "").VAPIDSubject; got != "https://localhost" {
		t.Errorf("bare subject = %q", got)
	}
}

func TestPushEnv(t *testing.T) {
	t.Setenv("PUSH_ALLOWED_HOSTS", " Push.Strafe.Chat, ntfy.sh ,")
	t.Setenv("PUSH_VAPID_SUBJECT", "mailto:admin@example.com")
	t.Setenv("PUSH_WORKERS", "8")
	t.Setenv("PUSH_ALLOW_INSECURE", "true")
	p := loadPushConfig("", "chat.example.com")
	if strings.Join(p.AllowedHosts, ",") != "push.strafe.chat,ntfy.sh" {
		t.Errorf("allowed hosts = %v", p.AllowedHosts)
	}
	if p.VAPIDSubject != "mailto:admin@example.com" || p.Workers != 8 || !p.AllowInsecure {
		t.Errorf("env not applied: %+v", p)
	}
}

func TestPushValidation(t *testing.T) {
	p := loadPushConfig("", "chat.example.com")
	p.VAPIDSubject = "admin@example.com"
	if err := validatePush(p); err == nil || !strings.Contains(err.Error(), "PUSH_VAPID_SUBJECT") {
		t.Errorf("bare address accepted as subject: %v", err)
	}
	p = loadPushConfig("", "chat.example.com")
	p.Workers = 0
	if err := validatePush(p); err == nil {
		t.Error("zero workers accepted")
	}
	// A key made with the openssl recipe in deploy/.env.example is accepted; anything that
	// is not 32 bytes on the curve is refused at boot.
	p = loadPushConfig("", "chat.example.com")
	p.VAPIDPrivateKey = "NKquTGVXyK6ZbmMzVRgFhko9iJBmbEr_AvDbimuP1pI"
	if err := validatePush(p); err != nil {
		t.Errorf("valid key refused: %v", err)
	}
	for _, bad := range []string{"short", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "NKquTGVXyK6ZbmMzVRgFhko9iJBmbEr_AvDbimuP1pIAA"} {
		p.VAPIDPrivateKey = bad
		if err := validatePush(p); err == nil || !strings.Contains(err.Error(), "PUSH_VAPID_PRIVATE_KEY") {
			t.Errorf("key %q: %v", bad, err)
		}
	}
	// Switched off, nothing else is checked.
	p.Enabled = false
	if err := validatePush(p); err != nil {
		t.Errorf("disabled push must validate: %v", err)
	}
}
