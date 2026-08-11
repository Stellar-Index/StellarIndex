package dashboardauth

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/notify"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// C3-032 (audit-2026-07-23) — durable per-email code-guess lockout.
//
// The pre-fix bound on guessing the 6-digit email code was:
//
//	maxCodeAttempts = 5      per TOKEN — and IncrementLoginCodeAttempts
//	                         burns one on every live token for the
//	                         address, so 5 wrong guesses retire the
//	                         whole candidate set …
//	                         … but a NEW /v1/auth/login mint starts at 0.
//	RedisLoginThrottle       5 mints/hour/email — in REDIS, with an
//	                         in-process fallback. A FLUSHALL, a
//	                         fail-over or a restart clears it.
//
// So the standing budget was ~25 guesses/hour/email forever: ≈ 0.22
// probability of landing a code over a year of patient grinding on one
// targeted address, and unbounded if the attacker can wait out (or
// cause) a Redis reset. Nothing durable counted failures per ADDRESS.
//
// These tests pin the durable counter. The load-bearing one is
// LockoutSurvivesTokenReMint: it drives the attack — mint, guess, mint,
// guess — and then presents the CORRECT code for a fresh token, which
// pre-fix signs the attacker in.

// lockoutRig is newTestRig with a movable clock, so the 24-hour window
// and lockout can be crossed without sleeping.
type lockoutRig struct {
	*testRig
	mu  sync.Mutex
	clk time.Time
}

func newLockoutRig(t *testing.T) *lockoutRig {
	t.Helper()
	lr := &lockoutRig{clk: time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)}
	now := func() time.Time {
		lr.mu.Lock()
		defer lr.mu.Unlock()
		return lr.clk
	}
	accounts := newFakeAccountStore()
	users := newFakeUserStore()
	tokens := newFakeTokenStore(now)
	sender := &notify.NoopSender{}
	cfg := Config{
		Accounts:         accounts,
		Users:            users,
		Tokens:           tokens,
		Sender:           sender,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:              now,
		DashboardBaseURL: "https://app.stellarindex.io",
		EmailFrom:        "Stellar Index <hello@stellarindex.io>",
		MagicLinkTTL:     15 * time.Minute,
		SessionTTL:       30 * 24 * time.Hour,
	}
	h, err := NewHandlers(cfg)
	if err != nil {
		t.Fatalf("NewHandlers: %v", err)
	}
	lr.testRig = &testRig{
		h: h, cfg: h.cfg,
		accounts: accounts, users: users, tokens: tokens,
		sender: sender, now: now,
	}
	return lr
}

func (lr *lockoutRig) advance(d time.Duration) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.clk = lr.clk.Add(d)
}

// grind performs n wrong-code guesses, re-minting a fresh token whenever
// the current one has burned through maxCodeAttempts — i.e. exactly what
// an attacker does, and exactly what the per-token cap does not stop.
func (lr *lockoutRig) grind(t *testing.T, email string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if i%maxCodeAttempts == 0 {
			lr.postLogin(t, email)
		}
		plaintext := lr.extractTokenFromSentEmail(t)
		code := lr.h.cfg.Generator.CodeForHash(HashMagicLinkPlaintext(plaintext))
		if w := lr.postVerifyCode(t, email, wrongCode(code)); w.Code != http.StatusBadRequest {
			t.Fatalf("guess %d: status = %d, want 400", i+1, w.Code)
		}
	}
}

// TestLockout_SurvivesTokenReMint — the finding, executed.
//
// Ten wrong guesses spread across three separate mints (so the per-token
// cap is reset twice along the way), then the CORRECT code for a
// brand-new token. Pre-fix that final request mints a session: the fresh
// token's attempts is 0 and nothing durable remembers the grinding.
func TestLockout_SurvivesTokenReMint(t *testing.T) {
	const email = "target@example.com"
	lr := newLockoutRig(t)

	lr.grind(t, email, maxDurableCodeFailures)

	// A fresh mint — the attacker's reset button — and the RIGHT code.
	code := lr.loginAndCode(t, email)
	w := lr.postVerifyCode(t, email, code)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: after %d durable failures a re-mint must NOT hand out a fresh guess budget",
			w.Code, maxDurableCodeFailures)
	}
	if sessionCookieSet(w) {
		t.Fatal("a session was minted for a locked-out address")
	}
	// Generic error only — a distinguishable "locked out" response would
	// be an account-enumeration oracle. It must be byte-identical to the
	// wrong-code response, which the grind above already produced.
	if body := w.Body.String(); !strings.Contains(body, "invalid or expired code") ||
		strings.Contains(strings.ToLower(body), "lock") {
		t.Errorf("response body leaks the lockout state: %s", body)
	}
}

// TestLockout_BelowThresholdStillLetsAValidCodeThrough — the control.
// One short of the cap must behave exactly as before, or the fix has
// simply broken login.
func TestLockout_BelowThresholdStillLetsAValidCodeThrough(t *testing.T) {
	const email = "clumsy@example.com"
	lr := newLockoutRig(t)

	lr.grind(t, email, maxDurableCodeFailures-1)

	code := lr.loginAndCode(t, email)
	w := lr.postVerifyCode(t, email, code)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 at %d failures (cap is %d); body=%s",
			w.Code, maxDurableCodeFailures-1, maxDurableCodeFailures, w.Body.String())
	}
	if !sessionCookieSet(w) {
		t.Fatal("no session cookie on a valid code below the cap")
	}
}

// TestLockout_SuccessfulLoginClearsTheCounter — proof of ownership
// retires the suspicion. Without this a legitimate user's typos would
// accumulate across months until they locked themselves out.
func TestLockout_SuccessfulLoginClearsTheCounter(t *testing.T) {
	const email = "typo@example.com"
	lr := newLockoutRig(t)

	lr.grind(t, email, maxDurableCodeFailures-1)
	code := lr.loginAndCode(t, email)
	if w := lr.postVerifyCode(t, email, code); w.Code != http.StatusOK {
		t.Fatalf("valid code: status = %d, want 200", w.Code)
	}

	state, err := lr.tokens.LoginCodeLockoutStatus(t.Context(), email)
	if err != nil {
		t.Fatalf("lockout status: %v", err)
	}
	if state.FailedCount != 0 {
		t.Errorf("failed_count = %d after a successful sign-in, want 0", state.FailedCount)
	}

	// And the budget is genuinely back: another near-cap grind does not
	// trip the lock.
	lr.grind(t, email, maxDurableCodeFailures-1)
	code2 := lr.loginAndCode(t, email)
	if w := lr.postVerifyCode(t, email, code2); w.Code != http.StatusOK {
		t.Fatalf("post-clear grind: status = %d, want 200 — the counter did not actually reset", w.Code)
	}
}

// TestLockout_MagicLinkStillWorksWhileLocked — the availability property
// that makes a 24-hour lockout defensible, and the reason this cannot be
// used to lock a victim out of their own account: the lockout gates the
// CODE door only, exactly as the pre-existing per-token cap does.
func TestLockout_MagicLinkStillWorksWhileLocked(t *testing.T) {
	const email = "locked@example.com"
	lr := newLockoutRig(t)

	lr.grind(t, email, maxDurableCodeFailures)

	// Confirm the code door is shut.
	code := lr.loginAndCode(t, email)
	if w := lr.postVerifyCode(t, email, code); w.Code != http.StatusBadRequest {
		t.Fatalf("pre-condition: code path status = %d, want 400", w.Code)
	}

	// The link from the same email must still sign the owner in.
	lr.postLogin(t, email)
	plaintext := lr.extractTokenFromSentEmail(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/auth/callback?token="+url.QueryEscape(plaintext), nil)
	req.RemoteAddr = "203.0.113.5:55123"
	attachLoginIntent(req, plaintext)
	w := httptest.NewRecorder()
	lr.h.HandleCallback(w, req)
	if !sessionCookieSet(w) {
		t.Fatalf("magic link refused while the CODE path is locked (status %d, body %s) — "+
			"the lockout must not lock a customer out of their own account", w.Code, w.Body.String())
	}
}

// TestLockout_ExpiresAfterTheLockoutWindow — the lock is a delay, not a
// permanent ban; and the count restarts, so the sustained budget really
// is ~maxDurableCodeFailures per lockout period.
func TestLockout_ExpiresAfterTheLockoutWindow(t *testing.T) {
	const email = "patient@example.com"
	lr := newLockoutRig(t)

	lr.grind(t, email, maxDurableCodeFailures)
	code := lr.loginAndCode(t, email)
	if w := lr.postVerifyCode(t, email, code); w.Code != http.StatusBadRequest {
		t.Fatalf("pre-condition: status = %d, want 400 (locked)", w.Code)
	}

	lr.advance(durableCodeLockout + time.Minute)

	code2 := lr.loginAndCode(t, email)
	w := lr.postVerifyCode(t, email, code2)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 once the lockout elapsed; body=%s", w.Code, w.Body.String())
	}
}

// TestLockout_StoreFailureFailsOpen — pins the deliberate posture (and
// its reason, so a future reader does not "fix" it into a fail-closed
// that takes dashboard login offline whenever migration 0122 lags a
// deploy). The per-token maxCodeAttempts cap still applies underneath.
func TestLockout_StoreFailureFailsOpen(t *testing.T) {
	const email = "degraded@example.com"
	lr := newLockoutRig(t)
	code := lr.loginAndCode(t, email)

	lr.tokens.mu.Lock()
	lr.tokens.lockoutErr = errors.New("relation \"login_code_lockouts\" does not exist")
	lr.tokens.mu.Unlock()

	w := lr.postVerifyCode(t, email, code)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a lockout-store failure must not break a VALID sign-in", w.Code)
	}
}

func lockoutErrors(t *testing.T, op string) float64 {
	t.Helper()
	return testutil.ToFloat64(obs.LoginCodeLockoutErrorsTotal.WithLabelValues(op))
}

// TestLockout_StatusCheckFailureIsCounted — the fail-open above is the
// right trade, but it means the control is OFF and the HTTP response is
// byte-identical to the healthy path. A `Logger.Error` is not an
// observable: nothing pages, nothing graphs, and the exact scenario the
// fail-open rationale names (migration 0122 lagging a node) would
// silently disable the lockout fleet-wide for as long as it lasted.
//
// So the counter is the fix's own accountability, exactly as
// AdminAuditWriteFailuresTotal is for the audit path in this wave.
func TestLockout_StatusCheckFailureIsCounted(t *testing.T) {
	const email = "status-blip@example.com"
	lr := newLockoutRig(t)
	code := lr.loginAndCode(t, email)

	lr.tokens.mu.Lock()
	lr.tokens.lockoutErr = errors.New("relation \"login_code_lockouts\" does not exist")
	lr.tokens.mu.Unlock()

	before := lockoutErrors(t, obs.LoginCodeLockoutOpStatusCheck)
	if w := lr.postVerifyCode(t, email, code); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got, want := lockoutErrors(t, obs.LoginCodeLockoutOpStatusCheck), before+1; got != want {
		t.Errorf("login_code_lockout_errors_total{op=%q} = %v, want %v — "+
			"a silently-disabled control with no metric is the defect this wave keeps finding",
			obs.LoginCodeLockoutOpStatusCheck, got, want)
	}
}

// TestLockout_RegisterFailureIsCounted — the other silent half: the
// wrong guess happened, nothing durable recorded it, and the response is
// the same generic 400. Without a counter the budget leaks invisibly.
func TestLockout_RegisterFailureIsCounted(t *testing.T) {
	const email = "register-blip@example.com"
	lr := newLockoutRig(t)
	code := lr.loginAndCode(t, email)

	lr.tokens.mu.Lock()
	lr.tokens.lockoutErr = errors.New("statement timeout")
	lr.tokens.mu.Unlock()

	before := lockoutErrors(t, obs.LoginCodeLockoutOpRegister)
	if w := lr.postVerifyCode(t, email, wrongCode(code)); w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if got, want := lockoutErrors(t, obs.LoginCodeLockoutOpRegister), before+1; got != want {
		t.Errorf("login_code_lockout_errors_total{op=%q} = %v, want %v",
			obs.LoginCodeLockoutOpRegister, got, want)
	}
}

// TestLockout_HealthyPathLeavesTheErrorCounterAlone — or the metric is
// permanently climbing and means nothing.
func TestLockout_HealthyPathLeavesTheErrorCounterAlone(t *testing.T) {
	const email = "healthy@example.com"
	lr := newLockoutRig(t)
	code := lr.loginAndCode(t, email)

	before := map[string]float64{}
	for _, op := range []string{obs.LoginCodeLockoutOpStatusCheck, obs.LoginCodeLockoutOpRegister} {
		before[op] = lockoutErrors(t, op)
	}
	if w := lr.postVerifyCode(t, email, wrongCode(code)); w.Code != http.StatusBadRequest {
		t.Fatalf("wrong code: status = %d, want 400", w.Code)
	}
	if w := lr.postVerifyCode(t, email, code); w.Code != http.StatusOK {
		t.Fatalf("valid code: status = %d, want 200", w.Code)
	}
	for op, want := range before {
		if got := lockoutErrors(t, op); got != want {
			t.Errorf("errors_total{op=%q} moved on the healthy path: %v → %v", op, want, got)
		}
	}
}
