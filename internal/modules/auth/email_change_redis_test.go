package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"

	"github.com/StrafeChat/equinox/internal/config"
)

// The email-change flow against a real Redis. These run when AUTH_TEST_REDIS names one,
// e.g. AUTH_TEST_REDIS=redis://127.0.0.1:6379/15. Every key lives under a random
// "test:emailchange:<rand>:" prefix and is deleted afterwards, so a shared database is safe;
// the users, 2FA and sessions are in-memory fakes (fakes_test.go).

const ecPassword = "correcthorsebatterystaple"

type emailEvent struct {
	userID   int64
	email    string
	verified bool
}

type ecEnv struct {
	t      *testing.T
	ctx    context.Context
	rdb    *redis.Client
	prefix string
	svc    *service
	users  *memUsers
	tf     *memTwoFactor
	mailer *memMailer

	mu     sync.Mutex
	events []emailEvent
}

func newECEnv(t *testing.T, mailOn bool) *ecEnv {
	t.Helper()
	raw := os.Getenv("AUTH_TEST_REDIS")
	if raw == "" {
		t.Skip("set AUTH_TEST_REDIS=redis://host:port/db to run the Redis-backed email-change tests")
	}
	opts, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opts)
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatalf("redis: %v", err)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	prefix := "test:emailchange:" + hex.EncodeToString(b) + ":"
	t.Cleanup(func() {
		keys, _ := rdb.Keys(context.Background(), prefix+"*").Result()
		if len(keys) > 0 {
			_ = rdb.Del(context.Background(), keys...).Err()
		}
		_ = rdb.Close()
	})

	cfg := &config.Config{}
	cfg.Database.Redis.CachePrefix = prefix
	cfg.Mail.InstanceName = "x.test"
	cfg.Mail.LinkBase = "https://x.test"
	cfg.Flags.BlockDisposableEmail = true

	e := &ecEnv{t: t, ctx: ctx, rdb: rdb, prefix: prefix, users: newMemUsers(), tf: newMemTwoFactor(), mailer: &memMailer{}}
	e.svc = NewService(cfg, e.users, &memSessions{}, e.tf, rdb).(*service)
	if e.svc.redisPrefix != prefix {
		t.Fatalf("service prefix %q, want %q", e.svc.redisPrefix, prefix)
	}
	if mailOn {
		e.svc.SetMailer(e.mailer)
	}
	e.svc.SetEmailUpdateNotifier(func(_ context.Context, id int64, email string, verified bool) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.events = append(e.events, emailEvent{id, email, verified})
	})
	return e
}

var ecHash = func() string {
	h, err := bcrypt.GenerateFromPassword([]byte(ecPassword), bcrypt.MinCost)
	if err != nil {
		panic(err)
	}
	return string(h)
}()

func (e *ecEnv) addUser(id int64, email string) {
	e.users.add(&User{ID: id, Email: email, Username: "user" + email[:1], PasswordHash: ecHash})
}

// session is the account as the session middleware would hand it over.
func (e *ecEnv) session(id int64) *User {
	u := e.users.row(id)
	u.PasswordHash = "" // the cache never carries it; nothing may rely on it
	return u
}

func (e *ecEnv) eventCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.events)
}

func (e *ecEnv) clearCooldown(id int64) {
	_ = e.rdb.Del(e.ctx, e.svc.emailCooldownKey("change", strconv.FormatInt(id, 10))).Err()
}

func (e *ecEnv) exists(key string) bool {
	n, err := e.rdb.Exists(e.ctx, key).Result()
	if err != nil {
		e.t.Fatal(err)
	}
	return n > 0
}

func (e *ecEnv) request(id int64, email string) (*EmailState, bool, error) {
	return e.svc.RequestEmailChange(e.ctx, e.session(id), EmailChangeInput{Email: email, Password: ecPassword})
}

var tokenRe = regexp.MustCompile(`token=([0-9a-f]{64})`)

// tokenIn is the token in the n-th (1-based) mail to addr.
func (e *ecEnv) tokenIn(addr string, n int) string {
	e.t.Helper()
	msgs := e.mailer.waitFor(e.t, addr, n)
	m := tokenRe.FindStringSubmatch(msgs[n-1].Text)
	if m == nil {
		e.t.Fatalf("mail %d to %s carries no token:\n%s", n, addr, msgs[n-1].Text)
	}
	return m[1]
}

func TestEmailChangeRedisHappyPath(t *testing.T) {
	e := newECEnv(t, true)
	e.addUser(1, "a@example.com")

	st, applied, err := e.request(1, " B@Example.com ")
	if err != nil || applied {
		t.Fatalf("request: %v applied=%v", err, applied)
	}
	if st.Email != "a@example.com" || st.Pending == nil || st.Pending.Email != "b@example.com" {
		t.Fatalf("state = %+v", st)
	}
	if left := time.Until(st.Pending.ExpiresAt); left < 59*time.Minute || left > time.Hour {
		t.Errorf("pending expires in %v, want ~1h", left)
	}
	tok := e.tokenIn("b@example.com", 1)
	notice := e.mailer.waitFor(t, "a@example.com", 1)[0]
	if !strings.Contains(notice.Text, "b***@e***.com") || strings.Contains(notice.Text, "b@example.com") || strings.Contains(notice.HTML, "b@example.com") {
		t.Errorf("the old address must see only the masked new one:\n%s", notice.Text)
	}
	if e.eventCount() != 0 {
		t.Error("a request publishes no event")
	}
	if got, _ := e.svc.GetEmailState(e.ctx, e.session(1)); got.Pending == nil || got.Pending.Email != "b@example.com" {
		t.Errorf("GET shows %+v", got)
	}

	stale := e.session(1) // the session's copy, from before the change
	st, err = e.svc.ConfirmEmailChange(e.ctx, stale, tok)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if st.Email != "b@example.com" || !st.Verified || st.Pending != nil {
		t.Errorf("confirmed state = %+v", st)
	}
	if row := e.users.row(1); row.Email != "b@example.com" || !row.VerifiedEmail {
		t.Errorf("row = %q verified=%v", row.Email, row.VerifiedEmail)
	}
	if e.eventCount() != 1 || e.events[0] != (emailEvent{1, "b@example.com", true}) {
		t.Errorf("events = %+v, want one USER_EMAIL_UPDATE {b, true}", e.events)
	}
	if e.exists(emailChangePendingKey(e.prefix, 1)) {
		t.Error("the pending record must be gone")
	}
	if !e.exists(emailChangedKey(e.prefix, 1)) {
		t.Error("the revival marker must be set")
	}
	if ttl := e.rdb.TTL(e.ctx, emailChangedKey(e.prefix, 1)).Val(); ttl < 24*time.Hour {
		t.Errorf("marker TTL %v, want 25h", ttl)
	}
	done := e.mailer.waitFor(t, "a@example.com", 2)[1]
	if done.Subject != "Your x.test email was changed" || !strings.Contains(done.Text, "b***@e***.com") {
		t.Errorf("done notice = %q\n%s", done.Subject, done.Text)
	}
	if u, _ := e.users.GetByEmail(e.ctx, "a@example.com"); u != nil {
		t.Error("the old address must be released")
	}

	// Double confirm (a refresh, a second click), with the stale session copy: 200 again.
	st, err = e.svc.ConfirmEmailChange(e.ctx, stale, tok)
	if err != nil || st.Email != "b@example.com" || !st.Verified {
		t.Errorf("second confirm: %+v %v", st, err)
	}
	if e.eventCount() != 1 || e.users.changeCount() != 1 {
		t.Errorf("a re-confirm changes nothing: events=%d changes=%d", e.eventCount(), e.users.changeCount())
	}
	if len(e.mailer.to("a@example.com")) != 2 {
		t.Error("a re-confirm mails nobody")
	}
}

func TestEmailChangeRedisSupersedeCancelResend(t *testing.T) {
	e := newECEnv(t, true)
	e.addUser(1, "a@example.com")

	if _, _, err := e.request(1, "c@example.com"); err != nil {
		t.Fatal(err)
	}
	first := e.tokenIn("c@example.com", 1)
	e.clearCooldown(1)
	if _, _, err := e.request(1, "d@example.com"); err != nil {
		t.Fatal(err)
	}
	second := e.tokenIn("d@example.com", 1)
	if _, err := e.svc.ConfirmEmailChange(e.ctx, e.session(1), first); !errors.Is(err, ErrEmailChangeTokenInvalid) {
		t.Errorf("superseded token: %v, want ErrEmailChangeTokenInvalid", err)
	}
	if e.exists(emailChangeTokenKey(e.prefix, mfaTokenHash(first))) {
		t.Error("the superseded token key must be deleted")
	}

	// Cancel kills the live one; cancelling again is fine.
	st, err := e.svc.CancelEmailChange(e.ctx, e.session(1))
	if err != nil || st.Pending != nil {
		t.Fatalf("cancel: %+v %v", st, err)
	}
	if _, err := e.svc.CancelEmailChange(e.ctx, e.session(1)); err != nil {
		t.Errorf("cancel is idempotent: %v", err)
	}
	if _, err := e.svc.ConfirmEmailChange(e.ctx, e.session(1), second); !errors.Is(err, ErrEmailChangeTokenInvalid) {
		t.Errorf("cancelled token: %v", err)
	}
	if _, err := e.svc.ResendEmailChange(e.ctx, e.session(1)); !errors.Is(err, ErrNoPendingEmailChange) {
		t.Errorf("resend with nothing pending: %v", err)
	}

	// Resend: a new token, the old one dead.
	e.clearCooldown(1)
	if _, _, err := e.request(1, "f@example.com"); err != nil {
		t.Fatal(err)
	}
	old := e.tokenIn("f@example.com", 1)
	if _, err := e.svc.ResendEmailChange(e.ctx, e.session(1)); !errors.Is(err, ErrEmailCooldown) {
		t.Errorf("resend within the minute: %v, want ErrEmailCooldown", err)
	}
	e.clearCooldown(1)
	st, err = e.svc.ResendEmailChange(e.ctx, e.session(1))
	if err != nil || st.Pending == nil || st.Pending.Email != "f@example.com" {
		t.Fatalf("resend: %+v %v", st, err)
	}
	fresh := e.tokenIn("f@example.com", 2)
	if fresh == old {
		t.Fatal("a resend issues a new token")
	}
	if n := len(e.mailer.to("a@example.com")); n != 3 {
		t.Errorf("the old address got %d notices, want 3 (one per request, none for the resend)", n)
	}
	if _, err := e.svc.ConfirmEmailChange(e.ctx, e.session(1), old); !errors.Is(err, ErrEmailChangeTokenInvalid) {
		t.Errorf("pre-resend token: %v", err)
	}
	if _, err := e.svc.ConfirmEmailChange(e.ctx, e.session(1), fresh); err != nil {
		t.Errorf("resent token: %v", err)
	}
}

func TestEmailChangeRedisWrongAccountAndStale(t *testing.T) {
	e := newECEnv(t, true)
	e.addUser(1, "a@example.com")
	e.addUser(2, "z@example.com")

	if _, _, err := e.request(1, "b@example.com"); err != nil {
		t.Fatal(err)
	}
	tok := e.tokenIn("b@example.com", 1)
	if _, err := e.svc.ConfirmEmailChange(e.ctx, e.session(2), tok); !errors.Is(err, ErrEmailChangeWrongAccount) {
		t.Errorf("another account's token: %v, want ErrEmailChangeWrongAccount", err)
	}
	if !e.exists(emailChangeTokenKey(e.prefix, mfaTokenHash(tok))) {
		t.Error("a wrong-account confirm must leave the token alone")
	}

	// Stale: while this confirm waits for the per-user lock, the lock holder (an
	// administrator's ApplyEmailChange) moves the address. The confirm then answers 409.
	lockKey := emailChangeLockKey(e.prefix, 1)
	if err := e.rdb.Set(e.ctx, lockKey, "held-by-test", emailChangeLockTTL).Err(); err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() {
		_, err := e.svc.ConfirmEmailChange(e.ctx, e.session(1), tok)
		errc <- err
	}()
	time.Sleep(300 * time.Millisecond)
	if err := e.users.ChangeEmail(e.ctx, 1, "a@example.com", "admin@example.com", true); err != nil {
		t.Fatal(err)
	}
	_ = e.rdb.Del(e.ctx, lockKey).Err()
	if err := <-errc; !errors.Is(err, ErrEmailChangeConflict) {
		t.Errorf("confirm after the address moved: %v, want ErrEmailChangeConflict (email_change_stale)", err)
	}
	if e.exists(emailChangePendingKey(e.prefix, 1)) {
		t.Error("a stale confirm clears the pending change")
	}

	// Sequential: a change through ApplyEmailChange drops any pending change, so a link
	// opened afterwards is simply no longer valid.
	e.clearCooldown(1)
	if _, _, err := e.request(1, "c@example.com"); err != nil {
		t.Fatal(err)
	}
	tok = e.tokenIn("c@example.com", 1)
	if err := ApplyEmailChange(e.ctx, e.svc.changeDeps(), 1, "admin@example.com", "admin2@example.com", true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ConfirmEmailChange(e.ctx, e.session(1), tok); !errors.Is(err, ErrEmailChangeTokenInvalid) {
		t.Errorf("confirm after an admin change: %v, want ErrEmailChangeTokenInvalid", err)
	}

	// Busy: a lock nobody releases is ErrEmailChangeBusy after ~3 s.
	if err := e.rdb.Set(e.ctx, lockKey, "held-by-test", emailChangeLockTTL).Err(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := ApplyEmailChange(e.ctx, e.svc.changeDeps(), 1, "admin2@example.com", "admin3@example.com", true)
	if !errors.Is(err, ErrEmailChangeBusy) || time.Since(start) < 2500*time.Millisecond {
		t.Errorf("held lock: %v after %v, want ErrEmailChangeBusy after ~3s", err, time.Since(start))
	}
	_ = e.rdb.Del(e.ctx, lockKey).Err()
}

func TestEmailChangeRedisConcurrentConfirms(t *testing.T) {
	e := newECEnv(t, true)
	e.addUser(1, "a@example.com")
	if _, _, err := e.request(1, "b@example.com"); err != nil {
		t.Fatal(err)
	}
	tok := e.tokenIn("b@example.com", 1)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = e.svc.ConfirmEmailChange(e.ctx, e.session(1), tok)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("confirm %d: %v", i, err)
		}
	}
	if n := e.users.changeCount(); n != 1 {
		t.Errorf("ChangeEmail ran %d times, want 1", n)
	}
	if n := e.eventCount(); n != 1 {
		t.Errorf("%d events, want 1", n)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(e.mailer.to("a@example.com")); n != 2 {
		t.Errorf("old address got %d mails, want 2 (requested + one done)", n)
	}
}

func TestEmailChangeRedisTakenAddress(t *testing.T) {
	e := newECEnv(t, true)
	e.addUser(1, "a@example.com")
	e.addUser(2, "taken@example.com")
	e.addUser(3, "c@example.com")

	free, _, err := e.request(3, "free@example.com")
	if err != nil {
		t.Fatal(err)
	}
	st, applied, err := e.request(1, "Taken@example.com")
	if err != nil || applied {
		t.Fatalf("taken request: %v applied=%v", err, applied)
	}
	if st.Pending == nil || st.Pending.Email != "taken@example.com" || (st.Pending == nil) != (free.Pending == nil) {
		t.Errorf("the taken answer must look like the free one: %+v vs %+v", st, free)
	}
	msg := e.mailer.waitFor(t, "taken@example.com", 1)[0]
	if msg.Subject != "Email change request on x.test" || tokenRe.MatchString(msg.Text) || strings.Contains(msg.HTML, "href=") {
		t.Errorf("taken mail = %q\n%s", msg.Subject, msg.Text)
	}
	e.mailer.waitFor(t, "a@example.com", 1) // the old address is told either way
	rec, err := e.svc.readPendingEmailChange(e.ctx, 1)
	if err != nil || rec == nil || !rec.Taken || rec.TokenHash != "" {
		t.Fatalf("pending record = %+v %v", rec, err)
	}
	if keys, _ := e.rdb.Keys(e.ctx, e.prefix+"email:change:token:*").Result(); len(keys) != 1 {
		t.Errorf("token keys %v: only the free request (account 3) may have one", keys)
	}
	if got, _ := e.svc.GetEmailState(e.ctx, e.session(1)); got.Pending == nil || got.Pending.Email != "taken@example.com" {
		t.Errorf("GET shows %+v", got)
	}
	// No token was issued, so there is nothing to confirm; a resend mails the notice again.
	e.clearCooldown(1)
	if _, err := e.svc.ResendEmailChange(e.ctx, e.session(1)); err != nil {
		t.Fatal(err)
	}
	if got := e.mailer.waitFor(t, "taken@example.com", 2)[1]; got.Subject != "Email change request on x.test" {
		t.Errorf("resent taken mail = %q", got.Subject)
	}
}

func TestEmailChangeRedisBudgets(t *testing.T) {
	e := newECEnv(t, true)
	e.addUser(1, "a@example.com")

	if _, _, err := e.request(1, "target@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.request(1, "other@example.com"); !errors.Is(err, ErrEmailCooldown) {
		t.Errorf("second request within the minute: %v, want ErrEmailCooldown", err)
	}
	// Mails 2 and 3 to the target, then the 4th is refused.
	for i := 2; i <= 3; i++ {
		e.clearCooldown(1)
		if _, err := e.svc.ResendEmailChange(e.ctx, e.session(1)); err != nil {
			t.Fatalf("mail %d: %v", i, err)
		}
	}
	e.clearCooldown(1)
	if _, err := e.svc.ResendEmailChange(e.ctx, e.session(1)); !errors.Is(err, ErrEmailTargetLimited) {
		t.Errorf("4th mail to one address: %v, want ErrEmailTargetLimited", err)
	}
	if _, _, err := e.request(1, "TARGET@example.com"); !errors.Is(err, ErrEmailTargetLimited) {
		t.Errorf("a new request to the same target: %v, want ErrEmailTargetLimited", err)
	}
	e.mailer.waitFor(t, "target@example.com", 3)
	time.Sleep(50 * time.Millisecond)
	if n := len(e.mailer.to("target@example.com")); n != 3 {
		t.Errorf("target received %d mails, want 3", n)
	}
}

func TestEmailChangeRedisReauth(t *testing.T) {
	e := newECEnv(t, true)
	e.addUser(1, "a@example.com")
	e.addUser(2, "two@example.com")
	codes := []string{"AAAAA-11111", "BBBBB-22222"}
	e.tf.totp[2] = "not-a-real-ciphertext"
	_ = e.tf.CreateRecoveryCodes(e.ctx, 2, []string{hashRecoveryCode(codes[0]), hashRecoveryCode(codes[1])})

	// 2FA: no code is mfa_required and costs nothing; a recovery code passes and is spent.
	_, _, err := e.svc.RequestEmailChange(e.ctx, e.session(2), EmailChangeInput{Email: "n@example.com", Password: ecPassword})
	if !errors.Is(err, ErrSecondFactorRequired) {
		t.Fatalf("no code: %v", err)
	}
	if n, _ := e.rdb.Get(e.ctx, e.svc.emailChangeReauthKey(2)).Int(); n != 0 {
		t.Errorf("mfa_required must not count against the budget, have %d", n)
	}
	if _, _, err := e.svc.RequestEmailChange(e.ctx, e.session(2), EmailChangeInput{Email: "n@example.com", Password: ecPassword, RecoveryCode: "ZZZZZ-99999"}); !errors.Is(err, ErrInvalidRecoveryCode) {
		t.Errorf("bad recovery code: %v", err)
	}
	if _, _, err := e.svc.RequestEmailChange(e.ctx, e.session(2), EmailChangeInput{Email: "n@example.com", Password: ecPassword, RecoveryCode: codes[0]}); err != nil {
		t.Fatalf("recovery code: %v", err)
	}
	if n, _ := e.tf.CountUnusedRecoveryCodes(e.ctx, 2); n != 1 {
		t.Errorf("%d codes left, want 1", n)
	}
	if n, _ := e.rdb.Exists(e.ctx, e.svc.emailChangeReauthKey(2)).Result(); n != 0 {
		t.Error("a successful step-up clears the failure count")
	}
	// The cooldown is now held: a refusal must not spend the remaining code.
	if _, _, err := e.svc.RequestEmailChange(e.ctx, e.session(2), EmailChangeInput{Email: "m@example.com", Password: ecPassword, RecoveryCode: codes[1]}); !errors.Is(err, ErrEmailCooldown) {
		t.Errorf("within the cooldown: %v", err)
	}
	if n, _ := e.tf.CountUnusedRecoveryCodes(e.ctx, 2); n != 1 {
		t.Errorf("a cooldown refusal consumed a recovery code (%d left)", n)
	}

	// Ten wrong passwords lock the endpoint for the hour, even for the right one.
	for i := 0; i < emailChangeReauthMax; i++ {
		if _, _, err := e.svc.RequestEmailChange(e.ctx, e.session(1), EmailChangeInput{Email: "n@example.com", Password: "wrong-password"}); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	if _, _, err := e.request(1, "n@example.com"); !errors.Is(err, ErrReauthLocked) {
		t.Errorf("after %d failures: %v, want ErrReauthLocked", emailChangeReauthMax, err)
	}
	if ttl := e.rdb.TTL(e.ctx, e.svc.emailChangeReauthKey(1)).Val(); ttl <= 0 || ttl > time.Hour {
		t.Errorf("failure budget TTL %v, want <= 1h", ttl)
	}
}

func TestEmailChangeRedisTokenRevival(t *testing.T) {
	e := newECEnv(t, true)
	e.addUser(1, "a@example.com")

	if err := e.svc.RequestPasswordReset(e.ctx, "a@example.com"); err != nil {
		t.Fatal(err)
	}
	reset := e.tokenIn("a@example.com", 1)
	deps := e.svc.changeDeps()
	if err := ApplyEmailChange(e.ctx, deps, 1, "a@example.com", "b@example.com", true); err != nil {
		t.Fatal(err)
	}
	if err := ApplyEmailChange(e.ctx, deps, 1, "b@example.com", "a@example.com", true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ResetPassword(e.ctx, reset, "a-brand-new-password"); !errors.Is(err, ErrResetTokenInvalid) {
		t.Errorf("reset token from before A->B->A: %v, want ErrResetTokenInvalid", err)
	}
	// A token issued after the move works.
	time.Sleep(5 * time.Millisecond)
	_ = e.rdb.Del(e.ctx, e.svc.emailCooldownKey("reset", mfaTokenHash("a@example.com"))).Err()
	if err := e.svc.RequestPasswordReset(e.ctx, "a@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ResetPassword(e.ctx, e.tokenIn("a@example.com", 2), "a-brand-new-password"); err != nil {
		t.Errorf("fresh reset token: %v", err)
	}
}

func TestEmailChangeRedisVerifyTokenStaleCache(t *testing.T) {
	e := newECEnv(t, true)
	e.addUser(1, "a@example.com")
	if err := e.svc.SendVerificationEmail(e.ctx, e.session(1)); err != nil {
		t.Fatal(err)
	}
	verify := e.tokenIn("a@example.com", 1)
	before := e.users.row(1)
	if err := ApplyEmailChange(e.ctx, e.svc.changeDeps(), 1, "a@example.com", "b@example.com", false); err != nil {
		t.Fatal(err)
	}
	// Only the uncached read stands between the stale cache and a verified B: drop the
	// marker, and serve the pre-change row from GetByID.
	_ = e.rdb.Del(e.ctx, emailChangedKey(e.prefix, 1)).Err()
	e.users.mu.Lock()
	e.users.stale[1] = before
	e.users.mu.Unlock()
	if _, err := e.svc.VerifyEmail(e.ctx, verify); !errors.Is(err, ErrEmailTokenInvalid) {
		t.Errorf("verify link for the old address: %v, want ErrEmailTokenInvalid", err)
	}
	if e.users.row(1).VerifiedEmail {
		t.Error("the new address must not be marked verified by the old link")
	}
	// And a stale session asking for a link mid-change is told to wait, not mailed.
	if err := e.svc.SendVerificationEmail(e.ctx, before); !errors.Is(err, ErrEmailChangeBusy) {
		t.Errorf("a stale session's verification request: %v, want ErrEmailChangeBusy", err)
	}
}

func TestEmailChangeRedisVerifyPublishes(t *testing.T) {
	e := newECEnv(t, true)
	e.addUser(1, "a@example.com")
	if err := e.svc.SendVerificationEmail(e.ctx, e.session(1)); err != nil {
		t.Fatal(err)
	}
	tok := e.tokenIn("a@example.com", 1)
	if _, err := e.svc.VerifyEmail(e.ctx, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.VerifyEmail(e.ctx, tok); err != nil {
		t.Fatal(err)
	}
	if e.eventCount() != 1 || e.events[0] != (emailEvent{1, "a@example.com", true}) {
		t.Errorf("events = %+v, want one {a, true} for the false->true flip only", e.events)
	}
}

func TestEmailChangeRedisResetClearsPending(t *testing.T) {
	e := newECEnv(t, true)
	e.addUser(1, "a@example.com")
	if _, _, err := e.request(1, "b@example.com"); err != nil {
		t.Fatal(err)
	}
	tok := e.tokenIn("b@example.com", 1)
	if err := e.svc.RequestPasswordReset(e.ctx, "a@example.com"); err != nil {
		t.Fatal(err)
	}
	var reset string
	for _, m := range e.mailer.waitFor(t, "a@example.com", 2) {
		if strings.Contains(m.Text, "/reset-password?token=") {
			reset = tokenRe.FindStringSubmatch(m.Text)[1]
		}
	}
	if reset == "" {
		t.Fatal("no reset mail")
	}
	if _, err := e.svc.ResetPassword(e.ctx, reset, "a-brand-new-password"); err != nil {
		t.Fatal(err)
	}
	if e.exists(emailChangePendingKey(e.prefix, 1)) || e.exists(emailChangeTokenKey(e.prefix, mfaTokenHash(tok))) {
		t.Error("a password reset must cancel the pending change")
	}
	if _, err := e.svc.ConfirmEmailChange(e.ctx, e.session(1), tok); !errors.Is(err, ErrEmailChangeTokenInvalid) {
		t.Errorf("confirm after reset: %v", err)
	}
}

func TestEmailChangeRedisDirectMode(t *testing.T) {
	e := newECEnv(t, false)
	e.addUser(1, "a@example.com")
	e.addUser(2, "taken@example.com")

	st, applied, err := e.request(1, "B@example.com")
	if err != nil || !applied {
		t.Fatalf("direct: %v applied=%v", err, applied)
	}
	if st.Email != "b@example.com" || st.Verified || st.Pending != nil {
		t.Errorf("state = %+v", st)
	}
	if e.eventCount() != 1 || e.events[0] != (emailEvent{1, "b@example.com", false}) {
		t.Errorf("events = %+v", e.events)
	}
	if _, _, err := e.request(1, "taken@example.com"); !errors.Is(err, ErrEmailInUse) {
		t.Errorf("taken in direct mode: %v, want ErrEmailInUse", err)
	}
	if _, err := e.svc.ResendEmailChange(e.ctx, e.session(1)); !errors.Is(err, ErrEmailDisabled) {
		t.Errorf("resend without mail: %v, want ErrEmailDisabled", err)
	}
	if _, _, err := e.request(1, "b@example.com"); !errors.Is(err, ErrEmailUnchanged) {
		t.Errorf("same address: %v", err)
	}
	if _, _, err := e.request(1, "x@yopmail.com"); !errors.Is(err, ErrDisposableEmail) {
		t.Errorf("disposable: %v", err)
	}
	if _, _, err := e.request(1, "x@yopmail.com."); !errors.Is(err, ErrInvalidEmail) {
		t.Errorf("trailing dot: %v", err)
	}
}

// A cancel that lands between the resend's read of the pending record and its write must
// stay a cancel: the resend may not write the record back (with KEEPTTL on a key that no
// longer exists it would have no expiry at all) nor mail a fresh link for it.
func TestEmailChangeRedisResendRacingCancel(t *testing.T) {
	e := newECEnv(t, true)
	e.addUser(1, "a@example.com")
	if _, _, err := e.request(1, "b@example.com"); err != nil {
		t.Fatal(err)
	}
	e.tokenIn("b@example.com", 1)
	e.clearCooldown(1)

	testHookResendBeforeWrite = func() {
		if _, err := e.svc.CancelEmailChange(e.ctx, e.session(1)); err != nil {
			t.Errorf("cancel: %v", err)
		}
	}
	t.Cleanup(func() { testHookResendBeforeWrite = nil })

	if _, err := e.svc.ResendEmailChange(e.ctx, e.session(1)); !errors.Is(err, ErrNoPendingEmailChange) {
		t.Errorf("resend racing a cancel: %v, want ErrNoPendingEmailChange", err)
	}
	testHookResendBeforeWrite = nil
	key := emailChangePendingKey(e.prefix, 1)
	if e.exists(key) {
		t.Errorf("the cancelled change came back (TTL %v)", e.rdb.PTTL(e.ctx, key).Val())
	}
	if st, err := e.svc.GetEmailState(e.ctx, e.session(1)); err != nil || st.Pending != nil {
		t.Errorf("GET after the cancel: %+v %v", st, err)
	}
	time.Sleep(100 * time.Millisecond) // the mail goroutine, had one been started
	if n := len(e.mailer.to("b@example.com")); n != 1 {
		t.Errorf("the new address got %d mails, want only the original link", n)
	}
	// The minute was handed back: a fresh request goes through at once.
	if _, _, err := e.request(1, "c@example.com"); err != nil {
		t.Errorf("request after the refused resend: %v", err)
	}
}
