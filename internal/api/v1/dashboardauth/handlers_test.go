package dashboardauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/notify"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// newTestRig wires the in-memory fakes + a noop sender into a
// NewHandlers. Returns the handlers + the underlying fakes so
// tests can poke + assert at internal state directly.
type testRig struct {
	h        *Handlers
	cfg      *Config
	accounts *fakeAccountStore
	users    *fakeUserStore
	tokens   *fakeTokenStore
	sender   *notify.NoopSender
	now      func() time.Time
}

func newTestRig(t *testing.T) *testRig {
	t.Helper()
	now := func() time.Time { return time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC) }
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
	h, err := NewHandlers(&cfg)
	if err != nil {
		t.Fatalf("NewHandlers: %v", err)
	}
	return &testRig{
		h: h, cfg: h.cfg,
		accounts: accounts, users: users, tokens: tokens,
		sender: sender, now: now,
	}
}

func (r *testRig) postLogin(t *testing.T, email string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(loginRequest{Email: email})
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", bytes.NewReader(body))
	req.RemoteAddr = "203.0.113.5:55123"
	w := httptest.NewRecorder()
	r.h.HandleLogin(w, req)
	return w
}

// attachCookies replays onto req the cookies a real browser would
// have stored from w. Load-bearing because `POST /v1/auth/login`
// sets the login-intent witness and `GET /v1/auth/callback` refuses to
// mint a session without it, so a callback request built without this
// step is a DIFFERENT browser — which is exactly the login-CSRF the
// binding rejects.
func attachCookies(req *http.Request, w *httptest.ResponseRecorder) {
	for _, c := range w.Result().Cookies() {
		req.AddCookie(c)
	}
}

// boundUnknownCallback builds a callback for a link correctly bound to
// the browser it carries the cookie of, but minted without /login — for
// the cases that need a token the store has never seen, where the
// binding must not be what decides the outcome.
func boundUnknownCallback(h *Handlers) *http.Request {
	browser := strings.Repeat("ab", MagicLinkPlaintextLen)
	nonce := strings.Repeat("cd", MagicLinkPlaintextLen/2)
	req := callbackFor(nonce + loginIntentTag(h.cfg.Generator.Secret, nonce, browser))
	req.AddCookie(&http.Cookie{Name: LoginIntentCookieName, Value: browser})
	return req
}

// extractTokenFromSentEmail pulls the magic-link plaintext out of
// the most-recently-sent NoopSender Message. The plaintext shows
// up in the rendered link URL — we parse it back out so tests can
// hit /v1/auth/callback without recomputing.
func (r *testRig) extractTokenFromSentEmail(t *testing.T) string {
	t.Helper()
	msg, ok := r.sender.Last()
	if !ok {
		t.Fatal("no email sent")
	}
	idx := strings.Index(msg.Text, "?token=")
	if idx < 0 {
		t.Fatalf("no ?token= in sent email: %s", msg.Text)
	}
	tok := msg.Text[idx+len("?token="):]
	if newline := strings.IndexAny(tok, "\n\r "); newline >= 0 {
		tok = tok[:newline]
	}
	return tok
}

func TestHandleLogin_HappyPath_NewEmail(t *testing.T) {
	r := newTestRig(t)
	w := r.postLogin(t, "alice@example.com")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if r.sender.SentCount() != 1 {
		t.Errorf("sent emails = %d, want 1", r.sender.SentCount())
	}
	last, _ := r.sender.Last()
	if got := last.To; len(got) != 1 || got[0] != "alice@example.com" {
		t.Errorf("recipient = %v", got)
	}
}

func TestHandleLogin_HappyPath_ExistingEmailDoesNotLeakEnumeration(t *testing.T) {
	r := newTestRig(t)
	// Pre-existing user.
	acct, _ := r.accounts.Create(context.Background(), platform.Account{
		Name: "x", Slug: "x", BillingEmail: "alice@example.com",
		Tier: platform.TierFree, Status: platform.AccountActive,
	})
	_, err := r.users.CreateUser(context.Background(), platform.User{
		AccountID: acct.ID, Email: "alice@example.com", Role: platform.RoleOwner,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	wExisting := r.postLogin(t, "alice@example.com")
	wMissing := r.postLogin(t, "noone@example.com")

	if wExisting.Code != wMissing.Code {
		t.Errorf("status differs: existing=%d missing=%d (enumeration leak)", wExisting.Code, wMissing.Code)
	}
	if got, want := wExisting.Body.String(), wMissing.Body.String(); got != want {
		t.Errorf("response body differs:\n  existing: %s\n  missing:  %s", got, want)
	}
}

func TestHandleLogin_RejectsMalformedEmail(t *testing.T) {
	r := newTestRig(t)
	for _, email := range []string{"", "no-at-sign", "@", "a@b"} {
		w := r.postLogin(t, email)
		if w.Code != http.StatusBadRequest {
			t.Errorf("email=%q got %d, want 400", email, w.Code)
		}
	}
}

func TestHandleLogin_RejectsMalformedJSON(t *testing.T) {
	r := newTestRig(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(`{bad`))
	req.RemoteAddr = "203.0.113.5:55123"
	w := httptest.NewRecorder()
	r.h.HandleLogin(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400", w.Code)
	}
}

func TestHandleCallback_HappyPath_FirstTimeSignupCreatesAccount(t *testing.T) {
	r := newTestRig(t)
	// Mint a token via /login so we exercise the same plumbing.
	lw := r.postLogin(t, "newuser@example.com")
	if lw.Code != http.StatusOK {
		t.Fatalf("login: %d", lw.Code)
	}
	plaintext := r.extractTokenFromSentEmail(t)

	cb := httptest.NewRequest(http.MethodGet, "/v1/auth/callback?token="+url.QueryEscape(plaintext), nil)
	cb.RemoteAddr = "203.0.113.5:55123"
	attachCookies(cb, lw)
	w := httptest.NewRecorder()
	r.h.HandleCallback(w, cb)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "https://app.stellarindex.io/") {
		t.Errorf("Location = %q", loc)
	}
	cookies := w.Result().Cookies()
	var session *http.Cookie
	for _, c := range cookies {
		if c.Name == SessionCookieName {
			session = c
			break
		}
	}
	if session == nil {
		t.Fatal("session cookie not set")
	}
	if !session.HttpOnly {
		t.Error("session cookie not HttpOnly")
	}
	if session.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v", session.SameSite)
	}

	// Verify the user + account were created with sensible defaults.
	user, err := r.users.GetUserByEmail(context.Background(), "newuser@example.com")
	if err != nil {
		t.Fatalf("user not created: %v", err)
	}
	if user.Role != platform.RoleOwner {
		t.Errorf("role = %v, want owner", user.Role)
	}
	if user.EmailVerifiedAt.IsZero() {
		t.Error("email_verified_at not set after callback")
	}
	acct, err := r.accounts.Get(context.Background(), user.AccountID)
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	if acct.Tier != platform.TierFree {
		t.Errorf("tier = %v, want free", acct.Tier)
	}
}

func TestHandleCallback_HappyPath_ExistingUserNoDuplicateAccount(t *testing.T) {
	r := newTestRig(t)
	// Pre-seed.
	acct, _ := r.accounts.Create(context.Background(), platform.Account{
		Name: "Acme", Slug: "acme", BillingEmail: "ash@acme.com",
		Tier: platform.TierStarter, Status: platform.AccountActive,
	})
	original, _ := r.users.CreateUser(context.Background(), platform.User{
		AccountID: acct.ID, Email: "ash@acme.com", Role: platform.RoleOwner,
	})

	lw := r.postLogin(t, "ash@acme.com")
	if lw.Code != http.StatusOK {
		t.Fatalf("login: %d", lw.Code)
	}
	plaintext := r.extractTokenFromSentEmail(t)

	cb := httptest.NewRequest(http.MethodGet, "/v1/auth/callback?token="+url.QueryEscape(plaintext), nil)
	cb.RemoteAddr = "203.0.113.5:55123"
	attachCookies(cb, lw)
	w := httptest.NewRecorder()
	r.h.HandleCallback(w, cb)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", w.Code)
	}
	post, err := r.users.GetUserByEmail(context.Background(), "ash@acme.com")
	if err != nil {
		t.Fatalf("user lookup: %v", err)
	}
	if post.ID != original.ID {
		t.Errorf("user.ID changed: existing=%v post-callback=%v (duplicate user created)", original.ID, post.ID)
	}
	if post.AccountID != acct.ID {
		t.Errorf("AccountID changed: existing=%v post-callback=%v", acct.ID, post.AccountID)
	}
}

func TestHandleCallback_ExpiredTokenReturns410(t *testing.T) {
	r := newTestRig(t)
	lw := r.postLogin(t, "alice@example.com")
	if lw.Code != http.StatusOK {
		t.Fatalf("login: %d", lw.Code)
	}
	plaintext := r.extractTokenFromSentEmail(t)

	// Fast-forward clock past the 15-minute TTL.
	r.tokens.now = func() time.Time { return r.now().Add(20 * time.Minute) }

	cb := httptest.NewRequest(http.MethodGet, "/v1/auth/callback?token="+url.QueryEscape(plaintext), nil)
	cb.RemoteAddr = "203.0.113.5:55123"
	attachCookies(cb, lw)
	w := httptest.NewRecorder()
	r.h.HandleCallback(w, cb)

	if w.Code != http.StatusGone {
		t.Errorf("expired token: status = %d, want 410", w.Code)
	}
}

// The callback URL carries the magic-link plaintext, so no response on
// it — the 303 into the dashboard or any refusal — may leak a Referer.
func TestHandleCallback_SetsNoReferrerOnEveryExit(t *testing.T) {
	r := newTestRig(t)
	lw := r.postLogin(t, "referrer@example.com")
	if lw.Code != http.StatusOK {
		t.Fatalf("login: %d", lw.Code)
	}
	plaintext := r.extractTokenFromSentEmail(t)

	ok := httptest.NewRequest(http.MethodGet, "/v1/auth/callback?token="+url.QueryEscape(plaintext), nil)
	ok.RemoteAddr = "203.0.113.5:55123"
	attachCookies(ok, lw)
	noIntent := httptest.NewRequest(http.MethodGet, "/v1/auth/callback?token=deadbeef", nil)
	missing := httptest.NewRequest(http.MethodGet, "/v1/auth/callback", nil)

	for name, req := range map[string]*http.Request{"success": ok, "no-intent": noIntent, "missing-token": missing} {
		w := httptest.NewRecorder()
		r.h.HandleCallback(w, req)
		if name == "success" && w.Code != http.StatusSeeOther {
			t.Fatalf("success: status = %d, want 303", w.Code)
		}
		if got := w.Header().Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("%s (status %d): Referrer-Policy = %q, want no-referrer", name, w.Code, got)
		}
	}
}

func TestHandleCallback_InvalidTokenReturns400(t *testing.T) {
	r := newTestRig(t)
	// Binding valid, token unknown to the store: the 400 must come from
	// the token lookup, not from the login-intent binding.
	cb := boundUnknownCallback(r.h)
	w := httptest.NewRecorder()
	r.h.HandleCallback(w, cb)
	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid token: status = %d, want 400", w.Code)
	}
}

func TestHandleCallback_TokenSingleUse(t *testing.T) {
	r := newTestRig(t)
	lw := r.postLogin(t, "alice@example.com")
	if lw.Code != http.StatusOK {
		t.Fatalf("login: %d", lw.Code)
	}
	plaintext := r.extractTokenFromSentEmail(t)

	// Consume once.
	cb1 := httptest.NewRequest(http.MethodGet, "/v1/auth/callback?token="+url.QueryEscape(plaintext), nil)
	cb1.RemoteAddr = "203.0.113.5:55123"
	attachCookies(cb1, lw)
	w1 := httptest.NewRecorder()
	r.h.HandleCallback(w1, cb1)
	if w1.Code != http.StatusSeeOther {
		t.Fatalf("first callback: %d", w1.Code)
	}

	// Replay must fail. The browser still holds the login-intent
	// witness it was issued at /login, so the rejection has to come
	// from the token's single-use consumption, not the binding.
	cb2 := httptest.NewRequest(http.MethodGet, "/v1/auth/callback?token="+url.QueryEscape(plaintext), nil)
	cb2.RemoteAddr = "203.0.113.5:55123"
	attachCookies(cb2, lw)
	w2 := httptest.NewRecorder()
	r.h.HandleCallback(w2, cb2)
	if w2.Code != http.StatusBadRequest {
		t.Errorf("replay: status = %d, want 400 (single-use token)", w2.Code)
	}
}

func TestHandleCallback_NextParamPathOnly_RejectsOpenRedirect(t *testing.T) {
	r := newTestRig(t)
	lw := r.postLogin(t, "alice@example.com")
	if lw.Code != http.StatusOK {
		t.Fatalf("login: %d", lw.Code)
	}
	plaintext := r.extractTokenFromSentEmail(t)

	// Try to redirect to evil.com via //evil.com (protocol-relative).
	target := "/v1/auth/callback?token=" + url.QueryEscape(plaintext) + "&next=" + url.QueryEscape("//evil.com/x")
	cb := httptest.NewRequest(http.MethodGet, target, nil)
	cb.RemoteAddr = "203.0.113.5:55123"
	attachCookies(cb, lw)
	w := httptest.NewRecorder()
	r.h.HandleCallback(w, cb)
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://app.stellarindex.io/") {
		t.Errorf("open-redirect through //evil.com bypassed; Location = %q", loc)
	}
	if strings.Contains(loc, "evil.com") {
		t.Errorf("Location leaked attacker host: %q", loc)
	}
}

func TestSlugFromEmail(t *testing.T) {
	cases := map[string]string{
		"alice@example.com":                  "alice",
		"ash.francis@x.com":                  "ash-francis",
		"BIG.CAPS@y.com":                     "big-caps",
		"under_score@y.com":                  "under-score",
		"plus+tag@y.com":                     "plustag",
		"only-symbols@y.com":                 "only-symbols",
		"":                                   "user",
		"@nothing":                           "user",
		strings.Repeat("a", 64) + "@x.com":   strings.Repeat("a", 63),
		strings.Repeat("b", 62) + ".c@x.com": strings.Repeat("b", 62),
		"--" + strings.Repeat("d", 70) + "@x.com": strings.Repeat("d", 63),
		strings.Repeat("_", 70) + "@x.com":        "user",
	}
	for in, want := range cases {
		if got := slugFromEmail(in); got != want {
			t.Errorf("slugFromEmail(%q) = %q, want %q", in, got, want)
		}
		if got := slugFromEmail(in); !fakeAccountSlugRE.MatchString(got) {
			t.Errorf("slugFromEmail(%q) = %q violates the accounts slug CHECK", in, got)
		}
	}
}

// TestSignupNewUser_LongEmailFitsAccountConstraints: any address
// notify.CanonicalRecipient admits must provision an account; a derived slug or
// name past the accounts CHECK bounds was a permanent 500.
func TestSignupNewUser_LongEmailFitsAccountConstraints(t *testing.T) {
	cases := map[string]string{
		"64-char local part":  strings.Repeat("a", 64) + "@example.com",
		"254-char address":    strings.Repeat("b", 60) + "@" + strings.Repeat("c", 189) + ".com",
		"hyphen at slug edge": strings.Repeat("d", 62) + ".e" + strings.Repeat("f", 10) + "@example.com",
	}
	for name, email := range cases {
		t.Run(name, func(t *testing.T) {
			if canon, err := notify.CanonicalRecipient(email); err != nil || canon != email {
				t.Fatalf("fixture %q not admitted unchanged by CanonicalRecipient: %q, %v", email, canon, err)
			}
			r := newTestRig(t)
			user, err := r.h.signupNewUser(context.Background(), email)
			if err != nil {
				t.Fatalf("signupNewUser(%d-byte email): %v", len(email), err)
			}
			acct := r.accounts.byID[user.AccountID]
			if acct.BillingEmail != email {
				t.Errorf("BillingEmail = %q, want the full address", acct.BillingEmail)
			}
			if want := email[:min(len(email), platform.MaxAccountNameLen)]; acct.Name != want {
				t.Errorf("Name = %q, want %q", acct.Name, want)
			}
		})
	}
}

// TestSignupNewUser_SlugCollisionRetryFitsConstraint: the collision
// retry appends "-xxxx"; on a maximum-length slug that suffix must not
// push it past the CHECK bound.
func TestSignupNewUser_SlugCollisionRetryFitsConstraint(t *testing.T) {
	r := newTestRig(t)
	local := strings.Repeat("g", platform.MaxAccountSlugLen)
	taken := strings.Repeat("g", platform.MaxAccountSlugLen)
	if _, err := r.accounts.Create(context.Background(), platform.Account{
		Name: "squatter", Slug: taken, Tier: platform.TierFree, Status: platform.AccountActive,
	}); err != nil {
		t.Fatalf("seed colliding account: %v", err)
	}
	user, err := r.h.signupNewUser(context.Background(), local+"@example.com")
	if err != nil {
		t.Fatalf("signupNewUser after slug collision: %v", err)
	}
	slug := r.accounts.byID[user.AccountID].Slug
	if len(slug) != platform.MaxAccountSlugLen || !strings.HasPrefix(slug, strings.Repeat("g", 58)+"-") {
		t.Errorf("retry slug = %q, want 58 g's + \"-\" + 4 hex", slug)
	}
}

func TestAccountNameFromEmail_RuneBoundary(t *testing.T) {
	email := strings.Repeat("é", 150) + "@" + strings.Repeat("h", 100) + ".com"
	got := accountNameFromEmail(email)
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != platform.MaxAccountNameLen {
		t.Errorf("accountNameFromEmail: valid=%v runes=%d, want valid and %d",
			utf8.ValidString(got), utf8.RuneCountInString(got), platform.MaxAccountNameLen)
	}
	if !strings.HasPrefix(email, got) {
		t.Errorf("accountNameFromEmail is not a prefix of the address")
	}
}

// A display-name spelling of an inbox must be stored, mailed and later
// keyed as the bare address, exactly as /v1/signup canonicalises it —
// otherwise one inbox becomes two accounts.
func TestHandleLogin_CanonicalisesDisplayNameSpelling(t *testing.T) {
	r := newTestRig(t)
	w := r.postLogin(t, `"Support" <Victim@Example.com>`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	last, ok := r.sender.Last()
	if !ok {
		t.Fatal("no email sent")
	}
	if got := last.To; len(got) != 1 || got[0] != "victim@example.com" {
		t.Errorf("recipient = %q, want [victim@example.com]", got)
	}
	r.tokens.mu.Lock()
	defer r.tokens.mu.Unlock()
	if len(r.tokens.tokens) != 1 {
		t.Fatalf("stored tokens = %d, want 1", len(r.tokens.tokens))
	}
	for _, tok := range r.tokens.tokens {
		if tok.Email != "victim@example.com" {
			t.Errorf("token email = %q, want victim@example.com", tok.Email)
		}
	}
}

func TestHandleLogin_RejectsNonSingleMailbox(t *testing.T) {
	r := newTestRig(t)
	for _, email := range []string{
		"a@b.com,c@d.com",
		`"a b"@example.com`,
		strings.Repeat("a", 256) + "@b.co",
	} {
		if w := r.postLogin(t, email); w.Code != http.StatusBadRequest {
			t.Errorf("email=%q got %d, want 400", email, w.Code)
		}
	}
	if n := r.sender.SentCount(); n != 0 {
		t.Errorf("sent = %d, want 0", n)
	}
}

func TestRequireSession_AnonRequest401(t *testing.T) {
	guarded := RequireSession()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/dashboard/me", nil)
	w := httptest.NewRecorder()
	guarded.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("anon: status = %d, want 401", w.Code)
	}
}

func TestMiddleware_CookieToContext(t *testing.T) {
	r := newTestRig(t)
	// Mint a session directly.
	acct, _ := r.accounts.Create(context.Background(), platform.Account{
		Name: "x", Slug: "x", Tier: platform.TierFree, Status: platform.AccountActive,
	})
	user, _ := r.users.CreateUser(context.Background(), platform.User{
		AccountID: acct.ID, Email: "owner@example.com", Role: platform.RoleOwner,
	})
	_, token := mintTestSession(t, r.users, platform.Session{
		UserID: user.ID, ExpiresAt: r.now().Add(24 * time.Hour),
	})

	var got SessionContext
	var ok bool
	stack := Middleware(r.cfg)(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got, ok = SessionFromContext(req.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/dashboard/me", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	stack.ServeHTTP(httptest.NewRecorder(), req)

	if !ok {
		t.Fatal("session not planted on context")
	}
	if got.User.ID != user.ID {
		t.Errorf("user.ID = %v, want %v", got.User.ID, user.ID)
	}
	if got.Account.ID != acct.ID {
		t.Errorf("account.ID = %v, want %v", got.Account.ID, acct.ID)
	}
}

func TestMiddleware_RevokedSessionDropsContext(t *testing.T) {
	r := newTestRig(t)
	acct, _ := r.accounts.Create(context.Background(), platform.Account{
		Name: "x", Slug: "x", Tier: platform.TierFree, Status: platform.AccountActive,
	})
	user, _ := r.users.CreateUser(context.Background(), platform.User{
		AccountID: acct.ID, Email: "owner@example.com", Role: platform.RoleOwner,
	})
	sess, token := mintTestSession(t, r.users, platform.Session{
		UserID: user.ID, ExpiresAt: r.now().Add(24 * time.Hour),
	})
	// Revoke it.
	_ = r.users.RevokeSession(context.Background(), sess.ID)

	var ok bool
	stack := Middleware(r.cfg)(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		_, ok = SessionFromContext(req.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/dashboard/me", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	stack.ServeHTTP(httptest.NewRecorder(), req)
	if ok {
		t.Error("revoked session leaked into context")
	}
}

func TestMiddleware_SuspendedAccountRevokesAndDrops(t *testing.T) {
	r := newTestRig(t)
	acct, _ := r.accounts.Create(context.Background(), platform.Account{
		Name: "x", Slug: "x", Tier: platform.TierFree, Status: platform.AccountActive,
	})
	user, _ := r.users.CreateUser(context.Background(), platform.User{
		AccountID: acct.ID, Email: "owner@example.com", Role: platform.RoleOwner,
	})
	sess, token := mintTestSession(t, r.users, platform.Session{
		UserID: user.ID, ExpiresAt: r.now().Add(24 * time.Hour),
	})
	// Suspend the account.
	if err := r.accounts.Suspend(context.Background(), acct.ID, "test"); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	var ok bool
	stack := Middleware(r.cfg)(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		_, ok = SessionFromContext(req.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/dashboard/me", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	stack.ServeHTTP(httptest.NewRecorder(), req)

	if ok {
		t.Error("suspended-account session leaked into context")
	}
	// Side effect: revoke must have been called.
	if _, err := r.users.GetSession(context.Background(), sess.ID); !errors.Is(err, platform.ErrNotFound) {
		t.Error("middleware did not revoke session for suspended account")
	}
}

func TestTouchTracker_Debounces(t *testing.T) {
	tt := newTouchTracker(time.Minute)
	id := uuid.New()
	t0 := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	if !tt.shouldTouch(id, t0) {
		t.Fatal("first call must touch")
	}
	if tt.shouldTouch(id, t0.Add(30*time.Second)) {
		t.Error("inside-interval call must not touch")
	}
	if !tt.shouldTouch(id, t0.Add(2*time.Minute)) {
		t.Error("post-interval call must touch")
	}
}

// stubLoginThrottle lets a test force the magic-link throttle decision.
type stubLoginThrottle struct {
	allow bool
	err   error
	calls int
}

func (s *stubLoginThrottle) Allow(_ context.Context, _, _ string) (bool, error) {
	s.calls++
	return s.allow, s.err
}

// TestHandleLogin_ThrottleDenies_SkipsEmailButReturnsGeneric200 — when the
// throttle denies, the handler MUST NOT send the email yet MUST still return
// the same generic 200 {status:"sent"} as the happy path (no inbox bomb, no
// signal that a throttle fired).
func TestHandleLogin_ThrottleDenies_SkipsEmailButReturnsGeneric200(t *testing.T) {
	r := newTestRig(t)
	thr := &stubLoginThrottle{allow: false}
	r.cfg.LoginThrottle = thr

	// Baseline: an un-throttled login returns this exact body.
	wantBody := strings.TrimSpace(func() string {
		clean := newTestRig(t)
		return clean.postLogin(t, "x@example.com").Body.String()
	}())

	w := r.postLogin(t, "victim@example.com")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (must not leak the throttle)", w.Code)
	}
	if r.sender.SentCount() != 0 {
		t.Errorf("sent emails = %d, want 0 (throttled send must be skipped)", r.sender.SentCount())
	}
	if thr.calls != 1 {
		t.Errorf("throttle calls = %d, want 1", thr.calls)
	}
	if got := strings.TrimSpace(w.Body.String()); got != wantBody {
		t.Errorf("throttled body = %q, want generic %q (enumeration/throttle signal)", got, wantBody)
	}
}

// TestHandleLogin_ThrottleErrors_FailsOpen — a throttle backing-store error
// must fall OPEN (send the email): login availability beats a brief abuse
// window, and the global rate-limit still bounds per-IP volume.
func TestHandleLogin_ThrottleErrors_FailsOpen(t *testing.T) {
	r := newTestRig(t)
	r.cfg.LoginThrottle = &stubLoginThrottle{allow: false, err: errors.New("redis down")}
	w := r.postLogin(t, "alice@example.com")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if r.sender.SentCount() != 1 {
		t.Errorf("sent emails = %d, want 1 (must fall open on throttle error)", r.sender.SentCount())
	}
}

// TestHandleLogin_EmailDescribesUAFromClosedVocabulary: the login email
// describes the requesting client only in terms from a fixed browser/OS
// vocabulary. Whatever else the User-Agent header carries must not reach
// either rendered body.
func TestHandleLogin_EmailDescribesUAFromClosedVocabulary(t *testing.T) {
	const prose = "URGENT call +1-555-0100 to secure your account"
	cases := []struct {
		name, ua, want string
	}{
		{"known browser with trailing prose", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:128.0) Gecko/20100101 Firefox/128.0 " + prose, "(Firefox on Windows)"},
		{"prose only", prose, "(an unrecognised browser)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestRig(t)
			body, _ := json.Marshal(loginRequest{Email: "alice@example.com"})
			req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", bytes.NewReader(body))
			req.RemoteAddr = "203.0.113.5:55123"
			req.Header.Set("User-Agent", tc.ua)
			w := httptest.NewRecorder()
			r.h.HandleLogin(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			msg, ok := r.sender.Last()
			if !ok {
				t.Fatal("no email sent")
			}
			for part, rendered := range map[string]string{"text": msg.Text, "html": msg.HTML} {
				for _, leaked := range []string{"URGENT", "555-0100", "secure your account", "Mozilla", "Gecko"} {
					if strings.Contains(rendered, leaked) {
						t.Errorf("%s body carries UA text %q", part, leaked)
					}
				}
				if !strings.Contains(rendered, tc.want) {
					t.Errorf("%s body missing client description %q", part, tc.want)
				}
			}
		})
	}
}

// TestTruncateUA_RuneSafe — a multi-byte rune straddling the
// byte-256 truncation boundary must not be split. A byte-slice
// truncation (a naive implementation) cuts the leading bytes of "€"
// (E2 82 AC) off mid-sequence and hands Postgres invalid UTF-8, which
// `user_agent text NOT NULL` (migration 0027) refuses — after the
// login token has already been consumed, burning the attempt.
func TestTruncateUA_RuneSafe(t *testing.T) {
	prefix := strings.Repeat("a", 255)
	ua := prefix + "€" + "xyz" // rune 256 is the 3-byte "€", straddling byte offset 256

	got := truncateUA(ua)

	if !utf8.ValidString(got) {
		t.Fatalf("truncateUA(%d-byte UA) = %q, not valid UTF-8", len(ua), got)
	}
	want := prefix + "€"
	if got != want {
		t.Errorf("truncateUA(%d-byte UA) = %q, want %q (256 runes, not 256 bytes)", len(ua), got, want)
	}
}

// TestTruncateUA_BreaksOutOfTemplateDelimiter — the
// plaintext magic-link template renders the UA inside a literal
// "({{.UserAgent}})" with no escaping. A UA that closes that paren
// early and adds prose must not survive truncateUA, or the rendered
// email reads as a trusted, DKIM-signed alert authored by the account
// owner's own browser.
func TestTruncateUA_BreaksOutOfTemplateDelimiter(t *testing.T) {
	ua := `Mozilla/5.0) URGENT: call +1-555-0100 to secure your account (`

	got := truncateUA(ua)

	for _, bad := range []string{"(", ")", ":", "!"} {
		if strings.Contains(got, bad) {
			t.Errorf("truncateUA(%q) = %q, still contains delimiter-breaking char %q", ua, got, bad)
		}
	}
	want := "Mozilla/5.0 URGENT call +1-555-0100 to secure your account "
	if got != want {
		t.Errorf("truncateUA(%q) = %q, want %q", ua, got, want)
	}
}

// TestMaskEmail moved to internal/pii with the implementation.

// TestHandleCallback_MailedLinkDoesNotSignInAForeignBrowser is the
// headline login-CSRF regression, written against nothing but the public
// behaviour so it stands on its own: the ONLY thing that changes
// between failing and passing is whether a magic link mailed to a
// third party signs that third party into the requester's account.
//
// Attack: the attacker requests a link for their OWN address, then
// mails the link to a victim. The victim's browser has never been
// through /v1/auth/login, so it holds nothing that ties it to the
// link. Without the binding, the callback mints the attacker's session there
// anyway (`sessionSameSite()` is Lax, which permits top-level
// cross-site GET navigation), and every later action the victim took
// — minting an API key, attaching a payment method — landed in the
// attacker's dashboard.
func TestHandleCallback_MailedLinkDoesNotSignInAForeignBrowser(t *testing.T) {
	r := newTestRig(t)

	if w := r.postLogin(t, "attacker@evil.example"); w.Code != http.StatusOK {
		t.Fatalf("attacker login: %d", w.Code)
	}
	attackerToken := r.extractTokenFromSentEmail(t)

	// A browser that never asked for this link.
	req := httptest.NewRequest(http.MethodGet,
		"/v1/auth/callback?token="+url.QueryEscape(attackerToken), nil)
	req.RemoteAddr = "198.51.100.7:44001"
	w := httptest.NewRecorder()
	r.h.HandleCallback(w, req)

	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName && c.Value != "" {
			t.Fatalf("login CSRF: a mailed link signed a foreign browser in "+
				"(session cookie %q, status %d)", c.Value, w.Code)
		}
	}
	if w.Code == http.StatusSeeOther {
		t.Fatalf("status = 303: the foreign browser was redirected into the dashboard as the link's owner")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

// The link's browser binding must be keyed: a tag anyone can compute
// would let a third party re-bind a link to a browser id of their choice.
func TestHandleCallback_RefusesUnkeyedLoginIntent(t *testing.T) {
	r := newTestRig(t)
	lw := r.postLogin(t, "binding@example.com")
	if lw.Code != http.StatusOK {
		t.Fatalf("login: %d", lw.Code)
	}
	plaintext := r.extractTokenFromSentEmail(t)

	browser := strings.Repeat("ef", MagicLinkPlaintextLen)
	nonce := plaintext[:loginIntentBrowserLen/2]
	sum := sha256.New()
	sum.Write([]byte(loginIntentDomain + nonce + "|" + browser))
	unkeyed := nonce + hex.EncodeToString(sum.Sum(nil)[:MagicLinkPlaintextLen/2])

	cb := httptest.NewRequest(http.MethodGet, "/v1/auth/callback?token="+url.QueryEscape(unkeyed), nil)
	cb.RemoteAddr = "203.0.113.5:55123"
	cb.AddCookie(&http.Cookie{Name: LoginIntentCookieName, Value: browser})
	w := httptest.NewRecorder()
	r.h.HandleCallback(w, cb)
	if w.Code != http.StatusForbidden {
		t.Fatalf("callback with an unkeyed binding tag: status = %d, want 403", w.Code)
	}
	if c := cookieNamed(w, SessionCookieName); c != nil && c.Value != "" {
		t.Fatal("session minted from an unkeyed binding tag")
	}

	// Positive control: the cookie the server itself set still binds.
	ok := httptest.NewRequest(http.MethodGet, "/v1/auth/callback?token="+url.QueryEscape(plaintext), nil)
	ok.RemoteAddr = "203.0.113.5:55123"
	attachCookies(ok, lw)
	w2 := httptest.NewRecorder()
	r.h.HandleCallback(w2, ok)
	if w2.Code != http.StatusSeeOther {
		t.Fatalf("callback with the server-set intent: status = %d, want 303", w2.Code)
	}
}

// TestHandleCallback_LoginCSRF_AttackerLinkInVictimBrowserRejected is
// the login-CSRF regression: the attacker requests a magic link for their
// OWN account and mails that link to the victim. Without the
// login-intent binding the victim's browser follows it, takes the
// attacker's session cookie, and every later action the victim
// performs lands in the attacker's dashboard.
//
// The victim's browser must not be signed in, and — because the check
// runs before consumption — the attacker's own token must survive so
// this can never be used to burn someone's link.
func TestHandleCallback_LoginCSRF_AttackerLinkInVictimBrowserRejected(t *testing.T) {
	r := newTestRig(t)

	// Attacker's browser requests a link for the attacker's own email.
	attackerLogin := r.postLogin(t, "attacker@evil.example")
	if attackerLogin.Code != http.StatusOK {
		t.Fatalf("attacker login: %d", attackerLogin.Code)
	}
	attackerToken := r.extractTokenFromSentEmail(t)

	// Victim's browser follows the mailed link. It never asked for a
	// link, so it holds no login-intent witness.
	victim := callbackFor(attackerToken)
	w := httptest.NewRecorder()
	r.h.HandleCallback(w, victim)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (login CSRF: victim signed into the attacker's account)", w.Code)
	}
	if c := cookieNamed(w, SessionCookieName); c != nil {
		t.Fatalf("session cookie minted for the victim's browser: %q", c.Value)
	}
	if loc := w.Header().Get("Location"); loc != "" {
		t.Errorf("Location = %q, want no redirect into the dashboard", loc)
	}

	// The rejection must not have spent the token: the attacker's own
	// browser can still complete its own login.
	own := callbackFor(attackerToken)
	attachCookies(own, attackerLogin)
	w2 := httptest.NewRecorder()
	r.h.HandleCallback(w2, own)
	if w2.Code != http.StatusSeeOther {
		t.Fatalf("originating browser status = %d, want 303 (binding checked before consumption)", w2.Code)
	}
	if c := cookieNamed(w2, SessionCookieName); c == nil {
		t.Error("originating browser got no session cookie")
	}
}

// TestHandleCallback_LoginIntentIsPerToken proves the binding is
// per-token, not merely "this browser has logged in at some point".
// The victim here is mid-login with a link of their own, so a cookie
// IS present — a naive presence check would pass the attacker's link.
func TestHandleCallback_LoginIntentIsPerToken(t *testing.T) {
	r := newTestRig(t)

	attackerLogin := r.postLogin(t, "attacker@evil.example")
	if attackerLogin.Code != http.StatusOK {
		t.Fatalf("attacker login: %d", attackerLogin.Code)
	}
	attackerToken := r.extractTokenFromSentEmail(t)

	victimLogin := r.postLogin(t, "victim@example.com")
	if victimLogin.Code != http.StatusOK {
		t.Fatalf("victim login: %d", victimLogin.Code)
	}
	if cookieNamed(victimLogin, LoginIntentCookieName) == nil {
		t.Fatal("victim browser holds no login-intent cookie; test would be vacuous")
	}

	req := callbackFor(attackerToken)
	attachCookies(req, victimLogin) // victim's own witness, attacker's token
	w := httptest.NewRecorder()
	r.h.HandleCallback(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (witness must bind the specific token)", w.Code)
	}
	if c := cookieNamed(w, SessionCookieName); c != nil {
		t.Fatalf("session cookie minted from another browser's token: %q", c.Value)
	}
}

// TestHandleCallback_ClearsLoginIntentOnSuccess — a spent witness
// shouldn't linger in the browser for the rest of the link's TTL.
func TestHandleCallback_ClearsLoginIntentOnSuccess(t *testing.T) {
	r := newTestRig(t)
	lw := r.postLogin(t, "alice@example.com")
	if lw.Code != http.StatusOK {
		t.Fatalf("login: %d", lw.Code)
	}
	plaintext := r.extractTokenFromSentEmail(t)

	cb := callbackFor(plaintext)
	attachCookies(cb, lw)
	w := httptest.NewRecorder()
	r.h.HandleCallback(w, cb)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	c := cookieNamed(w, LoginIntentCookieName)
	if c == nil {
		t.Fatal("login-intent cookie not cleared after a successful sign-in")
	}
	if c.MaxAge >= 0 || c.Value != "" {
		t.Errorf("login-intent cookie not expired: value=%q MaxAge=%d", c.Value, c.MaxAge)
	}
}

// A deployment with no Resend credential wires a transport that
// declares it cannot deliver (notify.UnconfiguredSender). HandleLogin must
// refuse up front: 503, a counted failed send, and NO side effect — no
// magic-link row, no login-intent cookie — for a link nobody can receive.
//
// Reverting only the handler guard leaves the send-error branch to absorb
// ErrNotConfigured: 200 {"status":"sent"} and a live token row. That is the
// shape this test goes red on.
func TestHandleLogin_MailUnconfigured_Refuses503WithNoSideEffects(t *testing.T) {
	r := newTestRig(t)
	r.cfg.Sender = notify.UnconfiguredSender{Reason: "env X is unset/empty"}
	beforeSent := notifyCount(t, obs.NotifyTemplateMagicLink, obs.NotifySendResultSent)
	beforeFailed := notifyCount(t, obs.NotifyTemplateMagicLink, obs.NotifySendResultFailed)

	w := r.postLogin(t, "alice@example.com")

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("login status = %d, want 503 when the mail transport has no credential", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	if strings.Contains(w.Body.String(), `"sent"`) {
		t.Errorf("body claims sent: %s", w.Body.String())
	}
	if got := notifyCount(t, obs.NotifyTemplateMagicLink, obs.NotifySendResultSent) - beforeSent; got != 0 {
		t.Errorf("result=sent delta = %v, want 0", got)
	}
	if got := notifyCount(t, obs.NotifyTemplateMagicLink, obs.NotifySendResultFailed) - beforeFailed; got != 1 {
		t.Errorf("result=failed delta = %v, want 1 (the failure-ratio alert reads this counter)", got)
	}
	r.tokens.mu.Lock()
	rows := len(r.tokens.tokens)
	r.tokens.mu.Unlock()
	if rows != 0 {
		t.Errorf("magic-link rows = %d, want 0", rows)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == LoginIntentCookieName {
			t.Errorf("login-intent cookie set although no link was minted")
		}
	}
}

// The refusal must be the same for every well-formed request. The throttled
// branch answers a decoy 200 {"status":"sent"}; if the mail guard ran after
// it, a 200 among 503s would tell a caller "a throttle fired for this
// address" — the oracle [LoginThrottle]'s contract forbids — and would claim
// an email on a deployment that cannot send one.
func TestHandleLogin_MailUnconfigured_RefusesBeforeTheThrottle(t *testing.T) {
	r := newTestRig(t)
	r.cfg.Sender = notify.UnconfiguredSender{}
	thr := &stubLoginThrottle{allow: false}
	r.cfg.LoginThrottle = thr

	w := r.postLogin(t, "alice@example.com")

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("throttled login status = %d, want the same 503 as an unthrottled one", w.Code)
	}
	if thr.calls != 0 {
		t.Errorf("throttle consulted %d time(s); the mail guard must run first so a refused "+
			"request cannot burn a victim's per-email quota", thr.calls)
	}
}

// A malformed request is still the caller's error: the guard sits behind
// request validation, so it never masks a 400.
func TestHandleLogin_MailUnconfigured_InvalidEmailIsStill400(t *testing.T) {
	r := newTestRig(t)
	r.cfg.Sender = notify.UnconfiguredSender{}
	if w := r.postLogin(t, "not-an-email"); w.Code != http.StatusBadRequest {
		t.Errorf("invalid email status = %d, want 400", w.Code)
	}
}

// The recording test double stays a working transport: the guard keys on a
// transport DECLARING it has no credential, not on "is not Resend", so the
// happy path every other test in this package drives is untouched.
func TestHandleLogin_RecordingSender_StillSends(t *testing.T) {
	r := newTestRig(t)
	if w := r.postLogin(t, "alice@example.com"); w.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200", w.Code)
	}
	if r.sender.SentCount() != 1 {
		t.Errorf("SentCount = %d, want 1", r.sender.SentCount())
	}
}

// TestHandleLogin_RecordsNotifySendMetric pins the send metric for the
// magic-link path: internal/notify had zero prometheus visibility, and
// HandleLogin swallows the send error (returns 200 either way to avoid an
// enumeration oracle), so a mail outage that silently kills login was invisible.
// The send call site must now bump stellarindex_notify_sends_total{template=
// "magic-link"} with result=sent on success and result=failed on error.
func TestHandleLogin_RecordsNotifySendMetric(t *testing.T) {
	// Success path: the default rig wires a NoopSender (accepts the message).
	r := newTestRig(t)
	beforeSent := notifyCount(t, obs.NotifyTemplateMagicLink, obs.NotifySendResultSent)

	if w := r.postLogin(t, "alice@example.com"); w.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200", w.Code)
	}
	if got := notifyCount(t, obs.NotifyTemplateMagicLink, obs.NotifySendResultSent) - beforeSent; got != 1 {
		t.Errorf("notify_sends_total{template=magic-link,result=sent} delta = %v, want 1", got)
	}

	// Failure path: swap in a sender that always errors (a Resend outage).
	// The response is still 200 (enumeration-safe), so the counter is the
	// ONLY signal the mail never went out.
	r.cfg.Sender = stubFailSender{err: errors.New("resend: 503 service unavailable")}
	beforeFailed := notifyCount(t, obs.NotifyTemplateMagicLink, obs.NotifySendResultFailed)

	if w := r.postLogin(t, "bob@example.com"); w.Code != http.StatusOK {
		t.Fatalf("login status on send failure = %d, want 200 (enumeration-safe)", w.Code)
	}
	if got := notifyCount(t, obs.NotifyTemplateMagicLink, obs.NotifySendResultFailed) - beforeFailed; got != 1 {
		t.Errorf("notify_sends_total{template=magic-link,result=failed} delta = %v, want 1", got)
	}
}

// A suppressed recipient is answered exactly like a sent one (200, same
// body), counted as suppressed, and never as failed.
func TestHandleLogin_SuppressedIsCountedSuppressedNotFailed(t *testing.T) {
	r := newTestRig(t)
	r.cfg.Sender = stubFailSender{err: notify.ErrSuppressed}
	sup := func() float64 { return notifyCount(t, obs.NotifyTemplateMagicLink, obs.NotifySendResultSuppressed) }
	failed := func() float64 { return notifyCount(t, obs.NotifyTemplateMagicLink, obs.NotifySendResultFailed) }
	s0, f0 := sup(), failed()

	w := r.postLogin(t, "bounced@example.com")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"sent"`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := sup() - s0; got != 1 {
		t.Errorf("suppressed delta = %v, want 1", got)
	}
	if got := failed() - f0; got != 0 {
		t.Errorf("failed delta = %v, want 0", got)
	}
}

// TestHandleLogin_SetsLoginIntentCookieForTheMintedToken pins the
// witness's shape: the token just minted must be bound to its id,
// HttpOnly, and expire with the link it witnesses.
func TestHandleLogin_SetsLoginIntentCookieForTheMintedToken(t *testing.T) {
	r := newTestRig(t)
	w := r.postLogin(t, "alice@example.com")
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d", w.Code)
	}
	plaintext := r.extractTokenFromSentEmail(t)

	c := cookieNamed(w, LoginIntentCookieName)
	if c == nil {
		t.Fatal("no login-intent cookie set by /v1/auth/login")
	}
	half := loginIntentBrowserLen / 2
	if !isLoginIntentHex(c.Value) ||
		plaintext[half:] != loginIntentTag(r.h.cfg.Generator.Secret, plaintext[:half], c.Value) {
		t.Errorf("emailed token %q is not bound to the cookie's browser id %q", plaintext, c.Value)
	}
	if !c.HttpOnly {
		t.Error("login-intent cookie is not HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}
	if got, wantAge := c.MaxAge, int(r.cfg.MagicLinkTTL/time.Second); got != wantAge {
		t.Errorf("MaxAge = %d, want %d (the link's own TTL)", got, wantAge)
	}
}

// TestHandleLogin_KeepsPriorIntentSoAnEarlierLinkStillWorks — a user
// who taps "email me a link" twice holds two live tokens and may click
// either. A single-slot witness would break the older link.
func TestHandleLogin_KeepsPriorIntentSoAnEarlierLinkStillWorks(t *testing.T) {
	r := newTestRig(t)

	first := r.postLogin(t, "alice@example.com")
	if first.Code != http.StatusOK {
		t.Fatalf("first login: %d", first.Code)
	}
	firstToken := r.extractTokenFromSentEmail(t)

	// Second request from the SAME browser: it replays the cookie it
	// already holds, exactly as a browser would.
	body, _ := json.Marshal(loginRequest{Email: "alice@example.com"})
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", bytes.NewReader(body))
	req.RemoteAddr = "203.0.113.5:55123"
	attachCookies(req, first)
	second := httptest.NewRecorder()
	r.h.HandleLogin(second, req)
	if second.Code != http.StatusOK {
		t.Fatalf("second login: %d", second.Code)
	}
	secondToken := r.extractTokenFromSentEmail(t)
	if secondToken == firstToken {
		t.Fatal("second login re-issued the same token; test would be vacuous")
	}

	// Clicking the OLDER link must still work.
	cb := callbackFor(firstToken)
	attachCookies(cb, second)
	w := httptest.NewRecorder()
	r.h.HandleCallback(w, cb)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("older link status = %d, want 303", w.Code)
	}
}

// TestHandleLogin_ThrottledResponseStillCarriesAnIntentCookie — the
// login-intent cookie must not become a side channel for the
// magic-link throttle. [LoginThrottle]'s contract is that a throttled
// send is byte-indistinguishable from a real one; if only the
// un-throttled path emitted a Set-Cookie, the header's presence would
// answer "does a throttle currently apply to this address?" for free.
func TestHandleLogin_ThrottledResponseStillCarriesAnIntentCookie(t *testing.T) {
	sent := newTestRig(t)
	sentW := sent.postLogin(t, "alice@example.com")
	sentCookie := cookieNamed(sentW, LoginIntentCookieName)
	if sentCookie == nil {
		t.Fatal("un-throttled login set no login-intent cookie; test would be vacuous")
	}

	throttled := newTestRig(t)
	throttled.cfg.LoginThrottle = &stubLoginThrottle{allow: false}
	throttledW := throttled.postLogin(t, "victim@example.com")

	if throttled.sender.SentCount() != 0 {
		t.Fatalf("throttled send count = %d, want 0", throttled.sender.SentCount())
	}
	throttledCookie := cookieNamed(throttledW, LoginIntentCookieName)
	if throttledCookie == nil {
		t.Fatal("throttled login set no login-intent cookie — the header's absence " +
			"tells an attacker a throttle fired for this address")
	}
	if !isLoginIntentHex(throttledCookie.Value) {
		t.Errorf("throttled cookie value = %q, want an id of the same shape as the real one (%q)",
			throttledCookie.Value, sentCookie.Value)
	}
	if throttledCookie.MaxAge != sentCookie.MaxAge ||
		throttledCookie.HttpOnly != sentCookie.HttpOnly ||
		throttledCookie.SameSite != sentCookie.SameSite {
		t.Errorf("throttled cookie attributes differ from the real one: %+v vs %+v",
			throttledCookie, sentCookie)
	}
	// And the decoy must be inert: it witnesses a token no store saw.
	if _, err := throttled.tokens.ConsumeMagicLinkToken(t.Context(), []byte("unused")); err == nil {
		t.Error("expected the throttled path to have persisted no token")
	}
}

// TestHandleLogin_ThrottledTapsKeepEveryMailedLinkRedeemable — a
// user who taps "email me a link" past the send throttle must still be
// able to open every link that WAS mailed, in the browser that asked for
// it. Without this, each throttled tap writes a decoy into a 3-slot
// cookie, so three taps left every mailed link 403ing.
func TestHandleLogin_ThrottledTapsKeepEveryMailedLinkRedeemable(t *testing.T) {
	r := newTestRig(t)
	throttle := &stubLoginThrottle{allow: true}
	r.cfg.LoginThrottle = throttle

	const sends = 5
	var prev *httptest.ResponseRecorder
	var tokens []string
	for range sends {
		prev = r.loginFrom(t, "alice@example.com", prev)
		tokens = append(tokens, r.extractTokenFromSentEmail(t))
	}
	throttle.allow = false
	for range 3 {
		prev = r.loginFrom(t, "alice@example.com", prev)
	}
	if got := r.sender.SentCount(); got != sends {
		t.Fatalf("sent %d mails, want %d; throttle not exercised", got, sends)
	}
	for i, tok := range tokens {
		if !r.redeems(t, tok, prev) {
			t.Errorf("mailed link %d refused after throttled taps", i)
		}
	}
}

// TestHandleLogin_ThrottleUnobservableAcrossFollowUps — [LoginThrottle]'s
// contract across a request SEQUENCE, not one response: whether a probe
// was throttled must change neither the probe's cookie, nor the cookie a
// follow-up request (real or throttled) gets back, nor which earlier
// links still redeem. Every observable is compared against a reference
// fixed before the probe, so the two branches are pinned to each other.
func TestHandleLogin_ThrottleUnobservableAcrossFollowUps(t *testing.T) {
	for _, priorSends := range []int{0, 1, 2, 3} {
		for _, priorThrottled := range []bool{false, true} {
			for _, probeThrottled := range []bool{false, true} {
				for _, followThrottled := range []bool{false, true} {
					r := newTestRig(t)
					throttle := &stubLoginThrottle{allow: true}
					r.cfg.LoginThrottle = throttle

					var prev *httptest.ResponseRecorder
					var links []string
					for range priorSends {
						prev = r.loginFrom(t, "attacker@evil.example", prev)
						links = append(links, r.extractTokenFromSentEmail(t))
					}
					if priorThrottled {
						throttle.allow = false
						prev = r.loginFrom(t, "attacker@evil.example", prev)
					}

					throttle.allow = !probeThrottled
					probe := r.loginFrom(t, "victim@example.com", prev)
					throttle.allow = !followThrottled
					follow := r.loginFrom(t, "attacker@evil.example", probe)

					name := fmt.Sprintf("prior=%d priorThrottled=%v probeThrottled=%v followThrottled=%v",
						priorSends, priorThrottled, probeThrottled, followThrottled)
					want := intentValue(t, probe)
					if prev != nil && want != intentValue(t, prev) {
						t.Errorf("%s: probe changed the browser's intent cookie", name)
					}
					if got := intentValue(t, follow); got != want || !isLoginIntentHex(got) {
						t.Errorf("%s: follow-up cookie %q, want the probe's %q", name, got, want)
					}
					for i, l := range links {
						if !r.redeems(t, l, follow) {
							t.Errorf("%s: earlier link %d no longer redeems", name, i)
						}
					}
				}
			}
		}
	}
}

// TestMiddleware_NilNowDoesNotPanic is a regression for the live
// production bug where main.go built the auth Config without a Now
// func and passed it raw to Middleware. NewHandlers defaulted Now on
// its own copy, but the resolver Middleware kept the nil — so
// resolveSession's cfg.Now() nil-derefed on every authenticated
// request. The magic-link cookie resolved fine; /v1/account/me then
// 500'd, making login look broken. Middleware must default Now (and
// Logger) so a valid session resolves without panicking.
func TestMiddleware_NilNowDoesNotPanic(t *testing.T) {
	accounts := newFakeAccountStore()
	users := newFakeUserStore()

	acct, err := accounts.Create(context.Background(), platform.Account{
		Name:   "tester",
		Slug:   "tester",
		Status: platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	user, err := users.CreateUser(context.Background(), platform.User{
		AccountID: acct.ID,
		Email:     "tester@example.com",
		Role:      platform.RoleOwner,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, token := mintTestSession(t, users, platform.Session{
		UserID:    user.ID,
		ExpiresAt: time.Now().Add(24 * time.Hour),
	})

	// Config with Now AND Logger left nil — the exact shape that
	// panicked in production.
	cfg := &Config{
		Accounts: accounts,
		Users:    users,
		Tokens:   newFakeTokenStore(nil),
	}

	var resolved bool
	h := Middleware(cfg)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, resolved = SessionFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/account/me", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	rec := httptest.NewRecorder()

	// The bug manifested as a panic recovered upstream into a 500;
	// here an unrecovered panic fails the test directly.
	h.ServeHTTP(rec, req)

	if !resolved {
		t.Fatal("expected the valid session cookie to resolve, got anonymous")
	}
	if cfg.Now == nil {
		t.Fatal("Middleware should have defaulted a nil cfg.Now")
	}
}

func TestMiddleware_IdleSessionRejectedAndRevoked(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name        string
		lastSeen    time.Time
		created     time.Time
		wantResolve bool
	}{
		{"idle 8 days", now.Add(-8 * 24 * time.Hour), now.Add(-10 * 24 * time.Hour), false},
		{"active 1 hour ago", now.Add(-time.Hour), now.Add(-10 * 24 * time.Hour), true},
		{"zero last-seen, fresh create", time.Time{}, now.Add(-time.Minute), true},
		{"zero last-seen, stale create", time.Time{}, now.Add(-8 * 24 * time.Hour), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			accounts := newFakeAccountStore()
			users := newFakeUserStore()
			acct, err := accounts.Create(context.Background(), platform.Account{
				Name: "tester", Slug: "tester", Status: platform.AccountActive,
			})
			if err != nil {
				t.Fatalf("create account: %v", err)
			}
			user, err := users.CreateUser(context.Background(), platform.User{
				AccountID: acct.ID, Email: "tester@example.com", Role: platform.RoleOwner,
			})
			if err != nil {
				t.Fatalf("create user: %v", err)
			}
			sess, token := mintTestSession(t, users, platform.Session{
				UserID:    user.ID,
				ExpiresAt: now.Add(20 * 24 * time.Hour),
			})
			users.mu.Lock()
			s := users.sessions[sess.ID]
			s.LastSeenAt, s.CreatedAt = tc.lastSeen, tc.created
			users.sessions[sess.ID] = s
			users.mu.Unlock()

			cfg := &Config{Accounts: accounts, Users: users, Now: func() time.Time { return now }}
			var resolved bool
			h := Middleware(cfg)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				_, resolved = SessionFromContext(r.Context())
			}))
			req := httptest.NewRequest(http.MethodGet, "/v1/account/me", nil)
			req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
			h.ServeHTTP(httptest.NewRecorder(), req)

			if resolved != tc.wantResolve {
				t.Fatalf("resolved = %v, want %v", resolved, tc.wantResolve)
			}
			users.mu.Lock()
			revoked := !users.sessions[sess.ID].RevokedAt.IsZero()
			users.mu.Unlock()
			if revoked == tc.wantResolve {
				t.Fatalf("revoked = %v, want %v", revoked, !tc.wantResolve)
			}
		})
	}
}

// TestSignupNewUser_EmailLocker_PreemptsLoser — when the locker is
// already held for an email (a concurrent winner is provisioning),
// the loser must wait + return the winner's user WITHOUT creating
// a speculative Account row.
//
// Proves the full-fix path. The
// fallback Suspend-on-conflict recovery still serves as defence
// in depth, but the lock path should never trigger it.
func TestSignupNewUser_EmailLocker_PreemptsLoser(t *testing.T) {
	r := newTestRig(t)
	locker := newFakeEmailLocker()
	r.cfg.EmailLocker = locker

	// Simulate the winner: pre-hold the lock + insert a User row
	// behind the winner's Account so the loser's poll converges.
	emailHash := hashEmailForLocker("owner@example.com")
	if ok, _, err := locker.Acquire(context.Background(), emailHash, time.Second); !ok || err != nil {
		t.Fatalf("pre-acquire: ok=%v err=%v", ok, err)
	}

	winnerAcct, err := r.accounts.Create(context.Background(), platform.Account{
		Name: "winner", Slug: "winner", BillingEmail: "owner@example.com",
		Tier: platform.TierFree, Status: platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("seed winner account: %v", err)
	}
	winner, err := r.users.CreateUser(context.Background(), platform.User{
		AccountID: winnerAcct.ID, Email: "owner@example.com", Role: platform.RoleOwner,
	})
	if err != nil {
		t.Fatalf("seed winner user: %v", err)
	}

	before := len(r.accounts.byID)

	// Loser comes through signupNewUser. Lock acquire fails ->
	// waitForWinnerUser converges to the winner row -> return.
	got, err := r.h.signupNewUser(context.Background(), "owner@example.com")
	if err != nil {
		t.Fatalf("signupNewUser as loser: %v", err)
	}
	if got.ID != winner.ID {
		t.Errorf("loser got user %v, want winner %v", got.ID, winner.ID)
	}
	if got.AccountID != winnerAcct.ID {
		t.Errorf("loser got AccountID %v, want winner's %v", got.AccountID, winnerAcct.ID)
	}
	if delta := len(r.accounts.byID) - before; delta != 0 {
		t.Errorf("speculative-account rows created by loser = %d, want 0 (lock should pre-empt before Account.Create)", delta)
	}
}

// TestSignupNewUser_EmailLocker_WinnerSucceeds — happy path with
// the lock available. Winner acquires, provisions, releases. The
// loser case is covered above; this case just proves the lock
// doesn't break the normal flow.
func TestSignupNewUser_EmailLocker_WinnerSucceeds(t *testing.T) {
	r := newTestRig(t)
	r.cfg.EmailLocker = newFakeEmailLocker()

	got, err := r.h.signupNewUser(context.Background(), "fresh@example.com")
	if err != nil {
		t.Fatalf("signupNewUser as winner: %v", err)
	}
	if got.Email != "fresh@example.com" {
		t.Errorf("winner.Email = %q", got.Email)
	}
	if got.AccountID == [16]byte{} {
		t.Errorf("winner.AccountID is zero — Account.Create should have run")
	}
}

// TestTouchTracker_EvictsAgedEntries guards that
// touchTracker.last must not grow without bound. Without an
// eviction sweep, every distinct session ID ever seen stays in the
// map for the process lifetime — one permanent entry per session.
// Here 50 sessions are each touched once, then time is advanced past
// the debounce interval and a single FRESH session is touched (which
// triggers the opportunistic sweep): all 50 aged-out entries must be
// gone, leaving only the fresh one.
func TestTouchTracker_EvictsAgedEntries(t *testing.T) {
	tr := newTouchTracker(time.Minute)
	base := time.Unix(1_750_000_000, 0)

	const n = 50
	for i := 0; i < n; i++ {
		id := uuid.New()
		if !tr.shouldTouch(id, base) {
			t.Fatalf("session %d: first touch should always be due", i)
		}
	}
	if got := len(tr.last); got != n {
		t.Fatalf("after seeding: touchTracker.last size = %d, want %d", got, n)
	}

	later := base.Add(2 * time.Minute)
	fresh := uuid.New()
	if !tr.shouldTouch(fresh, later) {
		t.Fatal("fresh session's first touch should be due")
	}

	if got := len(tr.last); got != 1 {
		t.Errorf("after sweep: touchTracker.last size = %d, want 1 — "+
			"aged-out entries were not evicted (REL-05: unbounded growth)", got)
	}
}
