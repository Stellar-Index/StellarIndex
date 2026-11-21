package dashboardauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// beginSignup runs begin-signup and returns the ceremony cookie, the
// challenge and the user handle the options carry.
func beginSignup(t *testing.T, rig *passkeyRig) (*http.Cookie, string, uuid.UUID) {
	t.Helper()
	w := httptest.NewRecorder()
	rig.h.HandlePasskeyBeginSignup(w, httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/begin-signup", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("begin-signup status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var opts struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			User      struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"user"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &opts); err != nil {
		t.Fatalf("unmarshal options: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(opts.PublicKey.User.ID)
	if err != nil {
		t.Fatalf("decode user handle: %v", err)
	}
	id, err := uuid.FromBytes(raw)
	if err != nil {
		t.Fatalf("user handle is not a UUID: %v", err)
	}
	if strings.Contains(opts.PublicKey.User.Name, "@") {
		t.Fatalf("user.name %q leaks the placeholder address", opts.PublicKey.User.Name)
	}
	c := ceremonyCookie(t, w)
	if c == nil {
		t.Fatal("begin-signup set no ceremony cookie")
	}
	return c, opts.PublicKey.Challenge, id
}

func finishSignup(t *testing.T, rig *passkeyRig, cookie *http.Cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/finish-signup", strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: PasskeyCeremonyCookieName, Value: cookie.Value})
	w := httptest.NewRecorder()
	rig.h.HandlePasskeyFinishSignup(w, req)
	return w
}

// A visitor with no session and no email ends up with an account, a
// session, and a passkey that signs them back in — and no mail is sent.
func TestPasskeySignup_EmailLessAccountThenLogin(t *testing.T) {
	rig, _, _ := newLiveClockPasskeyRig(t)
	cookie, challenge, userID := beginSignup(t, rig)
	auth := newSoftAuthenticator(t, userID)
	auth.credID = []byte("EXAMPLE-SIGNUP-CRED-PLACEHOLDER!")

	w := finishSignup(t, rig, cookie, auth.attestationBody(t, challenge, "Laptop"))
	if w.Code != http.StatusOK {
		t.Fatalf("finish-signup status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if !sessionCookieSet(w) {
		t.Fatal("finish-signup minted no session cookie")
	}
	user, err := rig.users.GetUserByID(t.Context(), userID)
	if err != nil {
		t.Fatalf("user not created under the credential's handle: %v", err)
	}
	if !platform.IsPlaceholderEmail(user.Email) || user.Role != platform.RoleOwner {
		t.Fatalf("user = %+v, want owner with a placeholder email", user)
	}
	if n := rig.sender.SentCount(); n != 0 {
		t.Fatalf("mails sent = %d, want 0 for an email-less account", n)
	}

	loginCookie, loginChallenge := beginLogin(t, rig)
	lw := finishLogin(t, rig, loginCookie, auth.assertionBody(t, loginChallenge, flagUserPresent|flagUserVerified, 0))
	if lw.Code != http.StatusOK || !sessionCookieSet(lw) {
		t.Fatalf("passkey login after signup: status = %d (%s)", lw.Code, lw.Body.String())
	}
}

// A captured finish-signup request must not mint a second session.
func TestPasskeySignup_ReplayMintsNothing(t *testing.T) {
	rig, _, _ := newLiveClockPasskeyRig(t)
	cookie, challenge, userID := beginSignup(t, rig)
	auth := newSoftAuthenticator(t, userID)
	auth.credID = []byte("EXAMPLE-SIGNUP-CRED-PLACEHOLDER!")
	body := auth.attestationBody(t, challenge, "Laptop")

	if w := finishSignup(t, rig, cookie, body); w.Code != http.StatusOK {
		t.Fatalf("first finish-signup status = %d (%s)", w.Code, w.Body.String())
	}
	w := finishSignup(t, rig, cookie, body)
	if w.Code != http.StatusBadRequest || sessionCookieSet(w) {
		t.Fatalf("replayed finish-signup: status = %d, session = %v; want 400 and none", w.Code, sessionCookieSet(w))
	}
}

// Ceremonies of the other purposes cannot be redeemed as a signup.
func TestPasskeySignup_RefusesOtherPurposeCeremony(t *testing.T) {
	rig, auth, _ := newLiveClockPasskeyRig(t)
	loginCookie, challenge := beginLogin(t, rig)
	w := finishSignup(t, rig, loginCookie, auth.attestationBody(t, challenge, "x"))
	if w.Code != http.StatusBadRequest || sessionCookieSet(w) {
		t.Fatalf("status = %d, want 400 with no session", w.Code)
	}
}

func TestPasskeySignup_BeginCappedPerIP(t *testing.T) {
	rig := newPasskeyRig(t)
	begin := func(addr string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/begin-signup", nil)
		req.RemoteAddr = addr
		w := httptest.NewRecorder()
		rig.h.HandlePasskeyBeginSignup(w, req)
		return w.Code
	}
	for i := range passkeySignupMaxPerIP {
		if c := begin("203.0.113.7:1"); c != http.StatusOK {
			t.Fatalf("begin %d: status = %d, want 200", i+1, c)
		}
	}
	if c := begin("203.0.113.7:2"); c != http.StatusTooManyRequests {
		t.Fatalf("begin past cap: status = %d, want 429", c)
	}
	if c := begin("198.51.100.9:1"); c != http.StatusOK {
		t.Fatalf("other IP: status = %d, want 200", c)
	}
}

type fakeSignupBudget struct {
	calls int
	err   error
}

func (f *fakeSignupBudget) CheckIP(_ context.Context, _ string) error {
	f.calls++
	return f.err
}

// passkeySignupOnce runs begin and finish for a fresh authenticator; credID must be unique within the rig.
func passkeySignupOnce(t *testing.T, rig *passkeyRig, credID string) *httptest.ResponseRecorder {
	t.Helper()
	cookie, challenge, userID := beginSignup(t, rig)
	a := newSoftAuthenticator(t, userID)
	a.credID = []byte(credID)
	return finishSignup(t, rig, cookie, a.attestationBody(t, challenge, "Laptop"))
}

func (r *testRig) accountCount() int {
	r.accounts.mu.Lock()
	defer r.accounts.mu.Unlock()
	return len(r.accounts.byID)
}

func (r *passkeyRig) credentialCount() int {
	r.passkeys.mu.Lock()
	defer r.passkeys.mu.Unlock()
	return len(r.passkeys.rows)
}

// The budget is spent at finish, after the attestation verifies and the ceremony is consumed, immediately before the
// account it pays for; it fails like /v1/register's: open on a transient error, closed on a sustained outage.
func TestPasskeySignup_FinishSpendsTheBudget(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{"budget open", nil, http.StatusOK},
		{"budget spent", auth.ErrSignupRateLimited, http.StatusServiceUnavailable},
		{"sustained outage fails closed", auth.ErrThrottleUnavailable, http.StatusServiceUnavailable},
		{"transient error fails open", errors.New("redis blip"), http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig, _, _ := newLiveClockPasskeyRig(t)
			budget := &fakeSignupBudget{err: tc.err}
			rig.h.cfg.SignupBudget = auth.NewRegistrationBudget(budget)
			cookie, challenge, userID := beginSignup(t, rig)
			if budget.calls != 0 {
				t.Fatalf("begin spent %d budget unit(s); it creates nothing", budget.calls)
			}
			accounts, creds := rig.accountCount(), rig.credentialCount()
			a := newSoftAuthenticator(t, userID)
			a.credID = []byte("EXAMPLE-SIGNUP-CRED-PLACEHOLDER!")
			body := a.attestationBody(t, challenge, "Laptop")

			w := finishSignup(t, rig, cookie, body)
			if w.Code != tc.wantStatus || budget.calls != 1 {
				t.Fatalf("finish: status = %d after %d budget spend(s); want %d after 1 (%s)", w.Code, budget.calls, tc.wantStatus, w.Body.String())
			}
			created := 0
			if tc.wantStatus == http.StatusOK {
				created = 1
			}
			if got := rig.accountCount() - accounts; got != created || rig.credentialCount()-creds != created || sessionCookieSet(w) != (created == 1) {
				t.Fatalf("finish created %d account(s), %d credential(s), session %v; want %d of each", got, rig.credentialCount()-creds, sessionCookieSet(w), created)
			}
			if errors.Is(tc.err, auth.ErrSignupRateLimited) && w.Header().Get("Retry-After") == "" {
				t.Error("a spent budget answered without Retry-After")
			}
			// The refused ceremony is consumed: retrying it cannot slip past the budget later; the visitor begins again.
			if tc.wantStatus != http.StatusOK {
				budget.err = nil
				if w := finishSignup(t, rig, cookie, body); w.Code != http.StatusBadRequest || rig.accountCount() != accounts {
					t.Fatalf("retried refused ceremony: status = %d, accounts +%d; want 400 and none", w.Code, rig.accountCount()-accounts)
				}
			}
		})
	}
}

// A begin the per-IP cap refuses spends nothing, and no begin spends the budget.
func TestPasskeySignup_PerIPRefusalSpendsNoBudget(t *testing.T) {
	rig := newPasskeyRig(t)
	budget := &fakeSignupBudget{}
	rig.h.cfg.SignupBudget = auth.NewRegistrationBudget(budget)
	codes := map[int]int{}
	for range passkeySignupMaxPerIP + 1 {
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/begin-signup", nil)
		req.RemoteAddr = "203.0.113.7:1"
		w := httptest.NewRecorder()
		rig.h.HandlePasskeyBeginSignup(w, req)
		codes[w.Code]++
	}
	if codes[http.StatusTooManyRequests] != 1 || budget.calls != 0 {
		t.Fatalf("statuses %v with %d budget spend(s); want one 429 and no spend", codes, budget.calls)
	}
}

// The registration switch refuses both legs, including the finish of a ceremony begun before it was thrown.
func TestPasskeySignup_HonoursRegistrationSwitch(t *testing.T) {
	begin := func(rig *passkeyRig) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/begin-signup", nil)
		req.RemoteAddr = "203.0.113.7:1"
		w := httptest.NewRecorder()
		rig.h.HandlePasskeyBeginSignup(w, req)
		return w
	}

	rig, _, _ := newLiveClockPasskeyRig(t)
	cookie, challenge, userID := beginSignup(t, rig)
	a := newSoftAuthenticator(t, userID)
	a.credID = []byte("EXAMPLE-SIGNUP-CRED-PLACEHOLDER!")
	accounts := rig.accountCount()
	rig.h.cfg.SignupDisabled = true
	if w := begin(rig); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("begin: status = %d, want 503", w.Code)
	}
	if w := finishSignup(t, rig, cookie, a.attestationBody(t, challenge, "Laptop")); w.Code != http.StatusServiceUnavailable ||
		rig.accountCount() != accounts {
		t.Fatalf("finish of a ceremony begun before the switch: status = %d, accounts +%d; want 503 and none", w.Code, rig.accountCount()-accounts)
	}
}

func TestPlaceholderEmail(t *testing.T) {
	id := uuid.New()
	if e := platform.PlaceholderEmail(id); !platform.IsPlaceholderEmail(e) || !strings.Contains(e, id.String()) {
		t.Fatalf("PlaceholderEmail = %q", e)
	}
	if platform.IsPlaceholderEmail("a@example.com") {
		t.Fatal("a real address was classified as a placeholder")
	}
}
