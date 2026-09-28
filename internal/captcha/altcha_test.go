package captcha

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	altcha "github.com/altcha-org/altcha-lib-go"
)

const testKey = "test-hmac-key-not-secret"

// solve does what the widget does: fetch a challenge, find the number, hand back the payload.
func solve(t *testing.T, v Verifier) string {
	t.Helper()
	ch, err := v.(Challenger).Challenge(context.Background())
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	c := ch.(altcha.Challenge)
	sol, err := altcha.SolveChallenge(c.Challenge, c.Salt, altcha.Algorithm(c.Algorithm), int(c.MaxNumber), 0, nil)
	if err != nil || sol == nil {
		t.Fatalf("solve: %v", err)
	}
	payload, _ := json.Marshal(altcha.Payload{
		Algorithm: c.Algorithm, Challenge: c.Challenge, Number: int64(sol.Number), Salt: c.Salt, Signature: c.Signature,
	})
	return base64.StdEncoding.EncodeToString(payload)
}

func TestAltchaAcceptsASolvedChallengeOnce(t *testing.T) {
	v := NewAltcha(testKey, nil)
	token := solve(t, v)
	if err := v.Verify(context.Background(), token, ""); err != nil {
		t.Fatalf("first submission: got %v, want nil", err)
	}
	// The whole point of the replay guard: the same solved payload must not buy a second
	// registration.
	if err := v.Verify(context.Background(), token, ""); err != ErrFailed {
		t.Fatalf("replayed submission: got %v, want ErrFailed", err)
	}
}

func TestAltchaRejectsWrongKeyAndTampering(t *testing.T) {
	issuer := NewAltcha(testKey, nil)
	token := solve(t, issuer)

	other := NewAltcha("a-different-key", nil)
	if err := other.Verify(context.Background(), token, ""); err != ErrFailed {
		t.Fatalf("challenge signed by another server: got %v, want ErrFailed", err)
	}

	raw, _ := base64.StdEncoding.DecodeString(token)
	var p altcha.Payload
	_ = json.Unmarshal(raw, &p)
	p.Number++ // a wrong answer to a genuine challenge
	tampered, _ := json.Marshal(p)
	if err := issuer.Verify(context.Background(), base64.StdEncoding.EncodeToString(tampered), ""); err != ErrFailed {
		t.Fatalf("wrong answer: got %v, want ErrFailed", err)
	}
}

func TestAltchaMissingAndGarbageTokens(t *testing.T) {
	v := NewAltcha(testKey, nil)
	if err := v.Verify(context.Background(), "   ", ""); err != ErrMissingToken {
		t.Fatalf("empty: got %v, want ErrMissingToken", err)
	}
	if err := v.Verify(context.Background(), "not base64!!", ""); err != ErrFailed {
		t.Fatalf("garbage: got %v, want ErrFailed", err)
	}
}
