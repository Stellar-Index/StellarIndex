package dashboardauth

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

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

func TestPlaceholderEmail(t *testing.T) {
	id := uuid.New()
	if e := platform.PlaceholderEmail(id); !platform.IsPlaceholderEmail(e) || !strings.Contains(e, id.String()) {
		t.Fatalf("PlaceholderEmail = %q", e)
	}
	if platform.IsPlaceholderEmail("a@example.com") {
		t.Fatal("a real address was classified as a placeholder")
	}
}
