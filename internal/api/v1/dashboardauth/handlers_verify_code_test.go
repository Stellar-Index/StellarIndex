package dashboardauth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// loginAndCode runs the login send for email and returns the 6-digit
// code the user would have received — derived from the same hash the
// store persisted, exactly as HandleVerifyCode recomputes it.
func (r *testRig) loginAndCode(t *testing.T, email string) string {
	t.Helper()
	r.postLogin(t, email)
	plaintext := r.extractTokenFromSentEmail(t)
	return CodeFromHash(HashMagicLinkPlaintext(plaintext))
}

func (r *testRig) postVerifyCode(t *testing.T, email, code string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(verifyCodeRequest{Email: email, Code: code})
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/verify-code", bytes.NewReader(body))
	req.RemoteAddr = "203.0.113.9:44001"
	w := httptest.NewRecorder()
	r.h.HandleVerifyCode(w, req)
	return w
}

// wrongCode returns a 6-digit code guaranteed to differ from correct.
func wrongCode(correct string) string {
	b := []byte(correct)
	b[0] = '0' + ((b[0]-'0')+1)%10
	return string(b)
}

func sessionCookieSet(w *httptest.ResponseRecorder) bool {
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName && c.Value != "" {
			return true
		}
	}
	return false
}

func TestHandleVerifyCode_HappyPath_FirstTimeSignup(t *testing.T) {
	r := newTestRig(t)
	code := r.loginAndCode(t, "newcomer@example.com")

	w := r.postVerifyCode(t, "newcomer@example.com", code)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp verifyCodeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Status != "ok" {
		t.Fatalf("status field = %q, want ok", resp.Status)
	}
	if !sessionCookieSet(w) {
		t.Fatal("expected a session cookie to be set")
	}
	if _, err := r.users.GetUserByEmail(t.Context(), "newcomer@example.com"); err != nil {
		t.Fatalf("expected user provisioned on first verify: %v", err)
	}
}

// Case-insensitive email: the user types mixed case but the token was
// stored lowercased. The handler lowercases before lookup.
func TestHandleVerifyCode_NormalisesEmailCase(t *testing.T) {
	r := newTestRig(t)
	code := r.loginAndCode(t, "mixed@example.com")

	w := r.postVerifyCode(t, "Mixed@Example.com", code)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleVerifyCode_WrongCodeBurnsAttempt(t *testing.T) {
	r := newTestRig(t)
	code := r.loginAndCode(t, "user@example.com")

	w := r.postVerifyCode(t, "user@example.com", wrongCode(code))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if sessionCookieSet(w) {
		t.Fatal("no session cookie should be set on a wrong code")
	}

	cands, err := r.tokens.ConsumableLoginCandidates(t.Context(), "user@example.com", maxCodeAttempts)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if len(cands) != 1 || cands[0].Attempts != 1 {
		t.Fatalf("expected one candidate with attempts=1, got %+v", cands)
	}

	// The correct code still works while under the cap.
	if w := r.postVerifyCode(t, "user@example.com", code); w.Code != http.StatusOK {
		t.Fatalf("correct code after one miss: status = %d, want 200", w.Code)
	}
}

func TestHandleVerifyCode_AttemptCapRetiresToken(t *testing.T) {
	r := newTestRig(t)
	code := r.loginAndCode(t, "victim@example.com")

	for i := 0; i < maxCodeAttempts; i++ {
		if w := r.postVerifyCode(t, "victim@example.com", wrongCode(code)); w.Code != http.StatusBadRequest {
			t.Fatalf("miss %d: status = %d, want 400", i, w.Code)
		}
	}
	// Token is now at the cap → no longer a candidate, so even the
	// correct code is rejected.
	w := r.postVerifyCode(t, "victim@example.com", code)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("correct code after cap: status = %d, want 400 (token retired)", w.Code)
	}
	if sessionCookieSet(w) {
		t.Fatal("no session after the token was retired")
	}
}

func TestHandleVerifyCode_SingleUse(t *testing.T) {
	r := newTestRig(t)
	code := r.loginAndCode(t, "once@example.com")

	if w := r.postVerifyCode(t, "once@example.com", code); w.Code != http.StatusOK {
		t.Fatalf("first verify: status = %d, want 200", w.Code)
	}
	// Same code again — token consumed, so 400.
	if w := r.postVerifyCode(t, "once@example.com", code); w.Code != http.StatusBadRequest {
		t.Fatalf("replay: status = %d, want 400", w.Code)
	}
}

func TestHandleVerifyCode_NoOutstandingToken(t *testing.T) {
	r := newTestRig(t)
	// Never logged in → no candidate. Any well-formed code is a 400,
	// not a 500.
	w := r.postVerifyCode(t, "stranger@example.com", "123456")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleVerifyCode_RejectsMalformedInput(t *testing.T) {
	r := newTestRig(t)
	cases := []struct{ email, code string }{
		{"not-an-email", "123456"},
		{"ok@example.com", "12345"},   // too short
		{"ok@example.com", "1234567"}, // too long
		{"ok@example.com", "12345x"},  // non-numeric
		{"ok@example.com", ""},        // empty
	}
	for _, c := range cases {
		if w := r.postVerifyCode(t, c.email, c.code); w.Code != http.StatusBadRequest {
			t.Fatalf("email=%q code=%q: status = %d, want 400", c.email, c.code, w.Code)
		}
	}
}
