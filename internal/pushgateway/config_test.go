package pushgateway

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func baseEnv(t *testing.T) map[string]string {
	return map[string]string{
		"PUSHGW_PUBLIC_URL": "https://push.example.com/",
		"PUSHGW_SEAL_KEY":   base64.StdEncoding.EncodeToString(randomKey(t)),
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig(envMap(baseEnv(t)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":4020" || cfg.PublicURL.String() != "https://push.example.com" || len(cfg.SealKey) != 32 {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.FCM != nil || cfg.APNs != nil || cfg.Debug {
		t.Fatal("nothing optional should be on by default")
	}
}

func TestLoadConfigSealKeys(t *testing.T) {
	key := randomKey(t)
	for name, enc := range map[string]string{
		"std padded":   base64.StdEncoding.EncodeToString(key),
		"url unpadded": base64.RawURLEncoding.EncodeToString(key),
	} {
		env := baseEnv(t)
		env["PUSHGW_SEAL_KEY"] = enc
		env["PUSHGW_SEAL_KEYS_OLD"] = base64.StdEncoding.EncodeToString(randomKey(t)) + ", " + base64.RawURLEncoding.EncodeToString(randomKey(t))
		cfg, err := LoadConfig(envMap(env))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(cfg.SealKey) != string(key) || len(cfg.OldSealKeys) != 2 {
			t.Fatalf("%s: keys not decoded", name)
		}
	}

	for name, mutate := range map[string]func(map[string]string){
		"no public URL":     func(e map[string]string) { delete(e, "PUSHGW_PUBLIC_URL") },
		"relative URL":      func(e map[string]string) { e["PUSHGW_PUBLIC_URL"] = "push.example.com" },
		"URL with query":    func(e map[string]string) { e["PUSHGW_PUBLIC_URL"] = "https://push.example.com/?a=b" },
		"no seal key":       func(e map[string]string) { delete(e, "PUSHGW_SEAL_KEY") },
		"short seal key":    func(e map[string]string) { e["PUSHGW_SEAL_KEY"] = base64.StdEncoding.EncodeToString(make([]byte, 16)) },
		"bad old key":       func(e map[string]string) { e["PUSHGW_SEAL_KEYS_OLD"] = "nope!" },
		"bad proxy":         func(e map[string]string) { e["PUSHGW_TRUSTED_PROXIES"] = "everyone" },
		"bad debug flag":    func(e map[string]string) { e["PUSHGW_DEBUG"] = "maybe" },
		"debug on public":   func(e map[string]string) { e["PUSHGW_DEBUG"] = "true" },
		"half APNs":         func(e map[string]string) { e["PUSHGW_APNS_KEY_ID"] = "ABC" },
		"missing FCM file":  func(e map[string]string) { e["PUSHGW_FCM_CREDENTIALS"] = "/nonexistent/sa.json" },
		"missing APNs file": func(e map[string]string) { setAPNsEnv(e, "/nonexistent/key.p8") },
	} {
		env := baseEnv(t)
		mutate(env)
		if _, err := LoadConfig(envMap(env)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func setAPNsEnv(e map[string]string, path string) {
	e["PUSHGW_APNS_KEY"] = path
	e["PUSHGW_APNS_KEY_ID"] = "ABC123DEFG"
	e["PUSHGW_APNS_TEAM_ID"] = "TEAM123456"
	e["PUSHGW_APNS_TOPIC"] = "chat.strafe.mobile"
}

func TestLoadConfigDebugOnlyLocally(t *testing.T) {
	for _, u := range []string{"http://127.0.0.1:4029", "https://localhost:4020", "http://10.0.2.2:4020", "https://[::1]:4020", "http://pushgw.test:4020"} {
		env := baseEnv(t)
		env["PUSHGW_PUBLIC_URL"] = u
		env["PUSHGW_DEBUG"] = "true"
		cfg, err := LoadConfig(envMap(env))
		if err != nil || !cfg.Debug {
			t.Errorf("%s: debug refused: %v", u, err)
		}
	}
	// Plain http does not make a public name a development gateway: anyone could list the
	// pushes it keeps.
	for _, u := range []string{"http://push.example.com", "http://8.8.8.8:4020", "https://push.strafe.chat"} {
		env := baseEnv(t)
		env["PUSHGW_PUBLIC_URL"] = u
		env["PUSHGW_DEBUG"] = "true"
		if _, err := LoadConfig(envMap(env)); err == nil {
			t.Errorf("%s: debug allowed on a public URL", u)
		}
	}
	// Real platform credentials mean real devices: never together with the debug platform.
	dir := t.TempDir()
	ec, err := x509.MarshalPKCS8PrivateKey(newP256(t))
	if err != nil {
		t.Fatal(err)
	}
	p8Path := filepath.Join(dir, "AuthKey_ABC123DEFG.p8")
	if err := os.WriteFile(p8Path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ec}), 0o600); err != nil {
		t.Fatal(err)
	}
	env := baseEnv(t)
	env["PUSHGW_PUBLIC_URL"] = "http://127.0.0.1:4020"
	env["PUSHGW_DEBUG"] = "true"
	setAPNsEnv(env, p8Path)
	if _, err := LoadConfig(envMap(env)); err == nil {
		t.Error("debug allowed beside APNs credentials")
	}
}

func TestLoadConfigCredentials(t *testing.T) {
	dir := t.TempDir()

	pkcs8, err := x509.MarshalPKCS8PrivateKey(rsaKey(t))
	if err != nil {
		t.Fatal(err)
	}
	sa, _ := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "strafe-test",
		"private_key_id": "kid-1",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})),
		"client_email":   "push@strafe-test.iam.gserviceaccount.com",
		"token_uri":      "https://oauth2.googleapis.com/token",
	})
	saPath := filepath.Join(dir, "sa.json")
	if err := os.WriteFile(saPath, sa, 0o600); err != nil {
		t.Fatal(err)
	}

	ec, err := x509.MarshalPKCS8PrivateKey(newP256(t))
	if err != nil {
		t.Fatal(err)
	}
	p8Path := filepath.Join(dir, "AuthKey_ABC123DEFG.p8")
	if err := os.WriteFile(p8Path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ec}), 0o600); err != nil {
		t.Fatal(err)
	}

	env := baseEnv(t)
	env["PUSHGW_FCM_CREDENTIALS"] = saPath
	setAPNsEnv(env, p8Path)
	cfg, err := LoadConfig(envMap(env))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FCM == nil || cfg.FCM.ProjectID != "strafe-test" || cfg.FCM.PrivateKeyID != "kid-1" || cfg.FCM.PrivateKey == nil {
		t.Fatalf("FCM = %+v", cfg.FCM)
	}
	if cfg.APNs == nil || cfg.APNs.KeyID != "ABC123DEFG" || cfg.APNs.TeamID != "TEAM123456" || cfg.APNs.Topic != "chat.strafe.mobile" {
		t.Fatalf("APNs = %+v", cfg.APNs)
	}

	// An RSA key where APNs wants P-256, and an EC key where FCM wants RSA, are refused.
	if _, err := ParseAPNsKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})); err == nil {
		t.Error("an RSA .p8 must be refused")
	}
	var m map[string]string
	_ = json.Unmarshal(sa, &m)
	m["private_key"] = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ec}))
	bad, _ := json.Marshal(m)
	if _, err := ParseFCMCredentials(bad); err == nil {
		t.Error("an EC service-account key must be refused")
	}
	if _, err := ParseFCMCredentials([]byte(`{"type":"authorized_user","project_id":"p","client_email":"e","private_key":"x"}`)); err == nil {
		t.Error("a non-service-account credential must be refused")
	}
}
