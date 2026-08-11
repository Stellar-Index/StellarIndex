package dashboardauth

// Passkey (WebAuthn) handler tests.
//
// Coverage note: a FULL WebAuthn ceremony (real attestation +
// assertion signatures) needs an authenticator; go-webauthn ships no
// public helper for forging one. These tests therefore cover every
// layer AROUND the library's signature check — endpoint gating,
// ceremony-cookie integrity (HMAC, purpose binding, expiry, user
// binding), option shapes, storage semantics, and the session-mint
// path — and trust the pinned library for the cryptographic core.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// ─── Fake store ───────────────────────────────────────────────────

type fakeWebAuthnStore struct {
	mu   sync.Mutex
	rows map[uuid.UUID]platform.WebAuthnCredential
}

func newFakeWebAuthnStore() *fakeWebAuthnStore {
	return &fakeWebAuthnStore{rows: map[uuid.UUID]platform.WebAuthnCredential{}}
}

func (f *fakeWebAuthnStore) CreateWebAuthnCredential(_ context.Context, c platform.WebAuthnCredential) (platform.WebAuthnCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.rows {
		if bytes.Equal(existing.CredentialID, c.CredentialID) {
			return platform.WebAuthnCredential{}, platform.ErrConflict
		}
	}
	c.ID = uuid.New()
	c.CreatedAt = time.Now().UTC()
	f.rows[c.ID] = c
	return c, nil
}

func (f *fakeWebAuthnStore) ListWebAuthnCredentialsForUser(_ context.Context, userID uuid.UUID) ([]platform.WebAuthnCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []platform.WebAuthnCredential{}
	for _, c := range f.rows {
		if c.UserID == userID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeWebAuthnStore) GetWebAuthnCredentialByCredentialID(_ context.Context, credentialID []byte) (platform.WebAuthnCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.rows {
		if bytes.Equal(c.CredentialID, credentialID) {
			return c, nil
		}
	}
	return platform.WebAuthnCredential{}, platform.ErrNotFound
}

func (f *fakeWebAuthnStore) UpdateWebAuthnCredentialSignCount(_ context.Context, id uuid.UUID, signCount int64, lastUsedAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.rows[id]
	if !ok {
		return platform.ErrNotFound
	}
	c.SignCount = signCount
	c.LastUsedAt = lastUsedAt
	f.rows[id] = c
	return nil
}

func (f *fakeWebAuthnStore) DeleteWebAuthnCredential(_ context.Context, id, userID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.rows[id]
	if !ok || c.UserID != userID {
		return platform.ErrNotFound
	}
	delete(f.rows, id)
	return nil
}

// ─── Rig ──────────────────────────────────────────────────────────

type passkeyRig struct {
	*testRig
	passkeys *fakeWebAuthnStore
	user     platform.User
	account  platform.Account
}

func newPasskeyRig(t *testing.T) *passkeyRig {
	t.Helper()
	rig := newTestRig(t)
	store := newFakeWebAuthnStore()
	rig.h.cfg.Passkeys = store

	acct, err := rig.accounts.Create(context.Background(), platform.Account{
		Name: "pk@example.com", Slug: "pk", BillingEmail: "pk@example.com",
		Tier: platform.TierFree, Status: platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	user, err := rig.users.CreateUser(context.Background(), platform.User{
		AccountID: acct.ID, Email: "pk@example.com", Role: platform.RoleOwner,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return &passkeyRig{testRig: rig, passkeys: store, user: user, account: acct}
}

// withSession plants an authenticated SessionContext, as the resolver
// middleware would after validating a cookie.
func (r *passkeyRig) withSession(req *http.Request) *http.Request {
	return req.WithContext(WithSession(req.Context(), SessionContext{
		Session: platform.Session{ID: uuid.New(), UserID: r.user.ID},
		User:    r.user,
		Account: r.account,
	}))
}

func ceremonyCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == PasskeyCeremonyCookieName {
			return c
		}
	}
	return nil
}

// ─── Begin (options + cookie) ─────────────────────────────────────

func TestPasskeyBeginLogin_OptionsAndCeremonyCookie(t *testing.T) {
	rig := newPasskeyRig(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/begin-login", nil)
	w := httptest.NewRecorder()
	rig.h.HandlePasskeyBeginLogin(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var opts struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			RPID      string `json:"rpId"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &opts); err != nil {
		t.Fatalf("unmarshal options: %v", err)
	}
	if opts.PublicKey.Challenge == "" {
		t.Fatal("options carry no challenge")
	}
	// RP ID must be the DashboardBaseURL host — the origin the browser
	// performs the ceremony on (testRig uses https://app.stellarindex.io).
	if opts.PublicKey.RPID != "app.stellarindex.io" {
		t.Fatalf("rpId = %q, want app.stellarindex.io", opts.PublicKey.RPID)
	}

	c := ceremonyCookie(t, w)
	if c == nil {
		t.Fatal("no ceremony cookie set")
	}
	if !c.HttpOnly {
		t.Fatal("ceremony cookie must be HttpOnly")
	}
	if c.MaxAge != int(passkeyCeremonyTTL/time.Second) {
		t.Fatalf("ceremony cookie MaxAge = %d, want %d", c.MaxAge, int(passkeyCeremonyTTL/time.Second))
	}
}

func TestPasskeyBeginRegister_RequiresSessionAndExcludesExisting(t *testing.T) {
	rig := newPasskeyRig(t)

	// No session on the context → 401 (the handler's own gate; the
	// mux additionally wraps the route in RequireSession).
	w := httptest.NewRecorder()
	rig.h.HandlePasskeyBeginRegister(w, httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/begin-register", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous begin-register: status = %d, want 401", w.Code)
	}

	// Register an existing credential — begin must exclude it so the
	// authenticator refuses to double-register.
	existing, err := rig.passkeys.CreateWebAuthnCredential(context.Background(), platform.WebAuthnCredential{
		UserID:       rig.user.ID,
		CredentialID: []byte("EXAMPLE-CRED-ID-PLACEHOLDER-01"),
		PublicKey:    []byte("EXAMPLE-PUBKEY-PLACEHOLDER"),
	})
	if err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	w = httptest.NewRecorder()
	rig.h.HandlePasskeyBeginRegister(w, rig.withSession(httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/begin-register", nil)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var opts struct {
		PublicKey struct {
			User struct {
				Name string `json:"name"`
			} `json:"user"`
			ExcludeCredentials []struct {
				ID string `json:"id"`
			} `json:"excludeCredentials"`
			AuthenticatorSelection struct {
				ResidentKey string `json:"residentKey"`
			} `json:"authenticatorSelection"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &opts); err != nil {
		t.Fatalf("unmarshal options: %v", err)
	}
	if opts.PublicKey.User.Name != rig.user.Email {
		t.Fatalf("user.name = %q, want %q", opts.PublicKey.User.Name, rig.user.Email)
	}
	if len(opts.PublicKey.ExcludeCredentials) != 1 {
		t.Fatalf("excludeCredentials len = %d, want 1", len(opts.PublicKey.ExcludeCredentials))
	}
	wantID := base64.RawURLEncoding.EncodeToString(existing.CredentialID)
	if opts.PublicKey.ExcludeCredentials[0].ID != wantID {
		t.Fatalf("excluded id = %q, want %q", opts.PublicKey.ExcludeCredentials[0].ID, wantID)
	}
	if opts.PublicKey.AuthenticatorSelection.ResidentKey != "required" {
		t.Fatalf("residentKey = %q, want required (discoverable login depends on it)", opts.PublicKey.AuthenticatorSelection.ResidentKey)
	}
	if ceremonyCookie(t, w) == nil {
		t.Fatal("no ceremony cookie set")
	}
}

// ─── Ceremony cookie integrity ────────────────────────────────────

func TestPasskeyCeremonyCookie_RoundTripAndTamper(t *testing.T) {
	rig := newPasskeyRig(t)
	sess := webauthn.SessionData{
		Challenge: "example-challenge-placeholder",
		UserID:    rig.user.ID[:],
		Expires:   rig.now().Add(passkeyCeremonyTTL),
	}

	w := httptest.NewRecorder()
	if err := rig.h.setPasskeyCeremonyCookie(w, passkeyCeremony{Purpose: "login", Session: sess}); err != nil {
		t.Fatalf("set cookie: %v", err)
	}
	c := ceremonyCookie(t, w)
	if c == nil {
		t.Fatal("cookie not set")
	}

	replay := func(value string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/finish-login", nil)
		req.AddCookie(&http.Cookie{Name: PasskeyCeremonyCookieName, Value: value})
		return req
	}

	// Round-trips under the right purpose.
	got, err := rig.h.readPasskeyCeremonyCookie(replay(c.Value), "login")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Session.Challenge != sess.Challenge {
		t.Fatalf("challenge = %q, want %q", got.Session.Challenge, sess.Challenge)
	}

	// Purpose binding: a login ceremony can't finish a registration.
	if _, err := rig.h.readPasskeyCeremonyCookie(replay(c.Value), "register"); err == nil {
		t.Fatal("login ceremony accepted for register purpose")
	}

	// Integrity: flip one payload byte → refused.
	dot := strings.IndexByte(c.Value, '.')
	payload, _ := base64.RawURLEncoding.DecodeString(c.Value[:dot])
	payload[0] ^= 0x01
	tampered := base64.RawURLEncoding.EncodeToString(payload) + c.Value[dot:]
	if _, err := rig.h.readPasskeyCeremonyCookie(replay(tampered), "login"); err == nil {
		t.Fatal("tampered payload accepted")
	}

	// Signature from a DIFFERENT secret → refused.
	other := &Handlers{cfg: &Config{Generator: &Generator{Secret: []byte("another-secret-placeholder")}, Now: rig.now}}
	w2 := httptest.NewRecorder()
	if err := other.setPasskeyCeremonyCookie(w2, passkeyCeremony{Purpose: "login", Session: sess}); err != nil {
		t.Fatalf("set cookie (other): %v", err)
	}
	c2 := ceremonyCookie(t, w2)
	if _, err := rig.h.readPasskeyCeremonyCookie(replay(c2.Value), "login"); err == nil {
		t.Fatal("foreign-signed ceremony accepted")
	}

	// Expired session data → refused.
	w3 := httptest.NewRecorder()
	expired := sess
	expired.Expires = rig.now().Add(-time.Minute)
	if err := rig.h.setPasskeyCeremonyCookie(w3, passkeyCeremony{Purpose: "login", Session: expired}); err != nil {
		t.Fatalf("set cookie (expired): %v", err)
	}
	c3 := ceremonyCookie(t, w3)
	if _, err := rig.h.readPasskeyCeremonyCookie(replay(c3.Value), "login"); err == nil {
		t.Fatal("expired ceremony accepted")
	}
}

// ─── Finish (validation layers) ───────────────────────────────────

func TestPasskeyFinishLogin_RejectsWithoutCeremony(t *testing.T) {
	rig := newPasskeyRig(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/finish-login", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	rig.h.HandlePasskeyFinishLogin(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if sessionCookieSet(w) {
		t.Fatal("a session cookie was set on a failed login")
	}
}

func TestPasskeyFinishLogin_RejectsGarbageAssertion(t *testing.T) {
	rig := newPasskeyRig(t)

	// Real begin → valid ceremony cookie, then a garbage body.
	begin := httptest.NewRecorder()
	rig.h.HandlePasskeyBeginLogin(begin, httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/begin-login", nil))
	c := ceremonyCookie(t, begin)

	req := httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/finish-login", strings.NewReader(`{"not":"an assertion"}`))
	req.AddCookie(&http.Cookie{Name: PasskeyCeremonyCookieName, Value: c.Value})
	w := httptest.NewRecorder()
	rig.h.HandlePasskeyFinishLogin(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if sessionCookieSet(w) {
		t.Fatal("a session cookie was set on a failed login")
	}
}

func TestPasskeyFinishRegister_RejectsForeignUserCeremony(t *testing.T) {
	rig := newPasskeyRig(t)

	// Ceremony minted for ANOTHER user must not register onto ours.
	otherID := uuid.New()
	w0 := httptest.NewRecorder()
	err := rig.h.setPasskeyCeremonyCookie(w0, passkeyCeremony{
		Purpose: "register",
		Session: webauthn.SessionData{
			Challenge: "example-challenge-placeholder",
			UserID:    otherID[:],
			Expires:   rig.now().Add(passkeyCeremonyTTL),
		},
	})
	if err != nil {
		t.Fatalf("set cookie: %v", err)
	}
	c := ceremonyCookie(t, w0)

	body := `{"name":"Example key","credential":{"id":"x"}}`
	req := rig.withSession(httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/finish-register", strings.NewReader(body)))
	req.AddCookie(&http.Cookie{Name: PasskeyCeremonyCookieName, Value: c.Value})
	w := httptest.NewRecorder()
	rig.h.HandlePasskeyFinishRegister(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if rows, _ := rig.passkeys.ListWebAuthnCredentialsForUser(context.Background(), rig.user.ID); len(rows) != 0 {
		t.Fatalf("credential stored despite user mismatch (%d rows)", len(rows))
	}
}

// ─── Session mint (shared path) ───────────────────────────────────

// TestMintSession_SetsSameCookieAsEmailFlow pins that the passkey
// login's terminal step issues the identical credential the email
// flows do: a DB session row + the stellarindex_session cookie with
// the same attributes.
func TestMintSession_SetsSameCookieAsEmailFlow(t *testing.T) {
	rig := newPasskeyRig(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/finish-login", nil)
	req.RemoteAddr = "203.0.113.10:44100"
	w := httptest.NewRecorder()
	if err := rig.h.mintSession(w, req, rig.user); err != nil {
		t.Fatalf("mintSession: %v", err)
	}

	var got *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName {
			got = c
		}
	}
	if got == nil || got.Value == "" {
		t.Fatal("no session cookie set")
	}
	if !got.HttpOnly {
		t.Fatal("session cookie must be HttpOnly")
	}
	id, err := uuid.Parse(got.Value)
	if err != nil {
		t.Fatalf("session cookie value is not a session id: %v", err)
	}
	sess, err := rig.users.GetSession(context.Background(), id)
	if err != nil {
		t.Fatalf("session row not created: %v", err)
	}
	if sess.UserID != rig.user.ID {
		t.Fatalf("session user = %s, want %s", sess.UserID, rig.user.ID)
	}
	wantExpiry := rig.now().Add(rig.h.cfg.SessionTTL)
	if !sess.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("session expiry = %v, want %v", sess.ExpiresAt, wantExpiry)
	}
}

// ─── Management ───────────────────────────────────────────────────

func TestPasskeyListAndDelete_OwnerScoped(t *testing.T) {
	rig := newPasskeyRig(t)
	mine, err := rig.passkeys.CreateWebAuthnCredential(context.Background(), platform.WebAuthnCredential{
		UserID:       rig.user.ID,
		Name:         "My key",
		CredentialID: []byte("EXAMPLE-CRED-ID-PLACEHOLDER-02"),
		PublicKey:    []byte("EXAMPLE-PUBKEY-PLACEHOLDER"),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	theirs, err := rig.passkeys.CreateWebAuthnCredential(context.Background(), platform.WebAuthnCredential{
		UserID:       uuid.New(), // someone else's
		Name:         "Not my key",
		CredentialID: []byte("EXAMPLE-CRED-ID-PLACEHOLDER-03"),
		PublicKey:    []byte("EXAMPLE-PUBKEY-PLACEHOLDER"),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// List shows only mine, and never leaks key material.
	w := httptest.NewRecorder()
	rig.h.HandlePasskeyList(w, rig.withSession(httptest.NewRequest(http.MethodGet, "/v1/auth/passkey/credentials", nil)))
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d", w.Code)
	}
	var list passkeyListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(list.Credentials) != 1 || list.Credentials[0].Name != "My key" {
		t.Fatalf("list = %+v, want exactly my key", list.Credentials)
	}
	if strings.Contains(w.Body.String(), "PUBKEY") || strings.Contains(w.Body.String(), "CRED-ID") {
		t.Fatal("list response leaks credential material")
	}

	// Deleting someone else's credential id → 404, row survives.
	req := rig.withSession(httptest.NewRequest(http.MethodDelete, "/v1/auth/passkey/credentials/"+theirs.ID.String(), nil))
	req.SetPathValue("id", theirs.ID.String())
	w = httptest.NewRecorder()
	rig.h.HandlePasskeyDelete(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-user delete status = %d, want 404", w.Code)
	}

	// Deleting mine → 204, gone.
	req = rig.withSession(httptest.NewRequest(http.MethodDelete, "/v1/auth/passkey/credentials/"+mine.ID.String(), nil))
	req.SetPathValue("id", mine.ID.String())
	w = httptest.NewRecorder()
	rig.h.HandlePasskeyDelete(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", w.Code)
	}
	if rows, _ := rig.passkeys.ListWebAuthnCredentialsForUser(context.Background(), rig.user.ID); len(rows) != 0 {
		t.Fatalf("credential not deleted (%d rows)", len(rows))
	}
}

// TestMount_PasskeyRoutesGated — without a Passkeys store the routes
// must not exist; with one they must respond.
func TestMount_PasskeyRoutesGated(t *testing.T) {
	plain := newTestRig(t) // no Passkeys store
	mux := http.NewServeMux()
	plain.h.Mount(mux)
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/begin-login", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unwired passkey route: status = %d, want 404", w.Code)
	}

	rig := newPasskeyRig(t)
	mux = http.NewServeMux()
	rig.h.Mount(mux)
	req = httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/begin-login", nil)
	// Same-origin write: the RequireSameSiteWrite gate compares the
	// Origin header against the request's own scheme://host.
	req.Header.Set("Origin", "http://"+req.Host)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("wired passkey route: status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
}
