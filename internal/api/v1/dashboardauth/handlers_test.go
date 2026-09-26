package dashboardauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
// have stored from w. Load-bearing since C3-030: `POST /v1/auth/login`
// sets the login-intent witness and `GET /v1/auth/callback` refuses to
// mint a session without it, so a callback request built without this
// step is a DIFFERENT browser — which is exactly the login-CSRF the
// binding rejects.
func attachCookies(req *http.Request, w *httptest.ResponseRecorder) {
	for _, c := range w.Result().Cookies() {
		req.AddCookie(c)
	}
}

// attachLoginIntent stamps the witness a browser would hold for
// `plaintext` without going through /login — for the cases that need a
// token the store has never seen (or has since dropped), where the
// binding must not be what decides the outcome.
func attachLoginIntent(req *http.Request, plaintext string) {
	req.AddCookie(&http.Cookie{
		Name:  LoginIntentCookieName,
		Value: loginIntentDigest(HashMagicLinkPlaintext(plaintext)),
	})
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

func TestHandleCallback_InvalidTokenReturns400(t *testing.T) {
	r := newTestRig(t)
	cb := httptest.NewRequest(http.MethodGet, "/v1/auth/callback?token=deadbeef", nil)
	cb.RemoteAddr = "203.0.113.5:55123"
	// Intent witness present, token unknown to the store: the 400 must
	// come from the token lookup, not from the C3-030 binding.
	attachLoginIntent(cb, "deadbeef")
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
	attachLoginIntent(cb2, plaintext)
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

func TestHandleLogout_IdempotentWithoutCookie(t *testing.T) {
	r := newTestRig(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/logout", nil)
	w := httptest.NewRecorder()
	r.h.HandleLogout(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	// Cookie should be cleared anyway.
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName && c.MaxAge >= 0 {
			t.Errorf("logout did not clear cookie: %+v", c)
		}
	}
}

func TestHandleLogout_RevokesActiveSession(t *testing.T) {
	r := newTestRig(t)
	// Mint a session directly.
	acct, _ := r.accounts.Create(context.Background(), platform.Account{
		Name: "x", Slug: "x", Tier: platform.TierFree, Status: platform.AccountActive,
	})
	user, _ := r.users.CreateUser(context.Background(), platform.User{
		AccountID: acct.ID, Email: "owner@example.com", Role: platform.RoleOwner,
	})
	sess, token := mintTestSession(t, r.users, platform.Session{
		UserID: user.ID, ExpiresAt: r.now().Add(24 * time.Hour),
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	w := httptest.NewRecorder()
	r.h.HandleLogout(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d", w.Code)
	}
	// Subsequent GetSession must return ErrNotFound.
	if _, err := r.users.GetSession(context.Background(), sess.ID); !errors.Is(err, platform.ErrNotFound) {
		t.Errorf("session not revoked after logout: err=%v", err)
	}
}

func TestHandleLogout_TolersInvalidCookieValue(t *testing.T) {
	r := newTestRig(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "not-a-uuid"})
	w := httptest.NewRecorder()
	r.h.HandleLogout(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (idempotent)", w.Code)
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
	r := newTestRig(t)
	guarded := RequireSession(r.cfg)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

// stubLoginThrottle lets a test force the magic-link throttle decision
// (audit-2026-06-14 A12).
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

// TestTruncateUA_RuneSafe — GH-1303: a multi-byte rune straddling the
// byte-256 truncation boundary must not be split. A byte-slice
// truncation (the pre-fix behaviour) cuts the leading bytes of "€"
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

// TestTruncateUA_BreaksOutOfTemplateDelimiter — RLT-320/RSEC-N1: the
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

// TestMaskEmail moved to internal/pii with the implementation (#346 F8).
