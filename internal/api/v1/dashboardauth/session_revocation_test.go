package dashboardauth

// A user must be able to end every one of their sessions themselves, and adding a sign-in method must end every session but the
// one that added it. Each test names the invariant it pins.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

type logoutRig struct {
	*testRig
	user, other platform.User
}

func newLogoutRig(t *testing.T) *logoutRig {
	t.Helper()
	rig := newTestRig(t)
	ctx := context.Background()
	acct, err := rig.accounts.Create(ctx, platform.Account{
		Name: "x", Slug: "x", Tier: platform.TierFree, Status: platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	user, err := rig.users.CreateUser(ctx, platform.User{AccountID: acct.ID, Email: "owner@example.com", Role: platform.RoleOwner})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	other, err := rig.users.CreateUser(ctx, platform.User{AccountID: acct.ID, Email: "member@example.com", Role: platform.RoleMember})
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}
	return &logoutRig{testRig: rig, user: user, other: other}
}

func (r *logoutRig) mint(t *testing.T, u platform.User) (platform.Session, string) {
	t.Helper()
	return mintTestSession(t, r.users, platform.Session{UserID: u.ID, ExpiresAt: r.now().Add(24 * time.Hour)})
}

// authenticates reports whether token still resolves on the auth path.
func (r *logoutRig) authenticates(token string) bool {
	_, err := r.users.GetSessionByTokenHash(context.Background(), HashSessionToken(token))
	return err == nil
}

func (r *logoutRig) logout(t *testing.T, target, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, nil)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	}
	w := httptest.NewRecorder()
	r.h.HandleLogout(w, req)
	return w
}

func assertSessionCookieCleared(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName && c.MaxAge < 0 {
			return
		}
	}
	t.Errorf("logout did not expire %s: %v", SessionCookieName, w.Result().Cookies())
}

func TestHandleLogout_WithoutAll_RevokesOnlyPresentedSession(t *testing.T) {
	r := newLogoutRig(t)
	_, presented := r.mint(t, r.user)
	_, second := r.mint(t, r.user)

	w := r.logout(t, "/v1/auth/logout", presented)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	assertSessionCookieCleared(t, w)
	if r.authenticates(presented) {
		t.Error("presented session still authenticates after logout")
	}
	if !r.authenticates(second) {
		t.Error("a plain logout revoked the user's other session")
	}
}

func TestHandleLogout_All_RevokesEveryUserSessionAndNoOtherUsers(t *testing.T) {
	r := newLogoutRig(t)
	_, presented := r.mint(t, r.user)
	_, second := r.mint(t, r.user)
	_, foreign := r.mint(t, r.other)

	w := r.logout(t, "/v1/auth/logout?all=true", presented)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	assertSessionCookieCleared(t, w)
	if r.authenticates(presented) {
		t.Error("presented session still authenticates after logout ?all=true")
	}
	if r.authenticates(second) {
		t.Error("the user's other session survived logout ?all=true")
	}
	if !r.authenticates(foreign) {
		t.Error("logout ?all=true revoked another user's session")
	}
}

func TestHandleLogout_All_WithoutSessionBehavesAsPlainLogout(t *testing.T) {
	r := newLogoutRig(t)
	_, bystander := r.mint(t, r.user)

	for _, token := range []string{"", "not-a-live-session-token"} {
		w := r.logout(t, "/v1/auth/logout?all=1", token)
		if w.Code != http.StatusOK {
			t.Errorf("token %q: status = %d, want 200", token, w.Code)
		}
		assertSessionCookieCleared(t, w)
	}
	if !r.authenticates(bystander) {
		t.Error("an unauthenticated logout ?all=1 revoked a live session")
	}
}

func TestHandleLogout_All_RejectsUnparseableValueWithoutRevoking(t *testing.T) {
	r := newLogoutRig(t)
	_, presented := r.mint(t, r.user)

	w := r.logout(t, "/v1/auth/logout?all=everything", presented)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !r.authenticates(presented) {
		t.Error("a rejected logout still revoked the presented session")
	}
}

type failingRevokeAllStore struct{ *fakeUserStore }

func (failingRevokeAllStore) RevokeAllUserSessions(context.Context, uuid.UUID) error {
	return errors.New("db down")
}

type failingLookupStore struct{ *fakeUserStore }

func (failingLookupStore) GetSessionByTokenHash(context.Context, []byte) (platform.Session, error) {
	return platform.Session{}, errors.New("db down")
}

// A 200 from ?all=true claims every session is dead, so a failed bulk
// revoke must not be reported as success.
func TestHandleLogout_All_StoreFailureIsNotReportedAsSuccess(t *testing.T) {
	r := newLogoutRig(t)
	_, presented := r.mint(t, r.user)
	r.h.cfg.Users = failingRevokeAllStore{r.users}

	w := r.logout(t, "/v1/auth/logout?all=true", presented)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when the bulk revoke fails", w.Code)
	}
}

// The same claim is made when the presented session cannot be resolved at
// all: without a store answer nothing was revoked, so the cookie must stay
// for a retry instead of being cleared under a 200.
func TestHandleLogout_All_LookupFailureKeepsCookieAndIsNotSuccess(t *testing.T) {
	r := newLogoutRig(t)
	_, presented := r.mint(t, r.user)
	r.h.cfg.Users = failingLookupStore{r.users}

	w := r.logout(t, "/v1/auth/logout?all=true", presented)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when the session lookup fails", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName && c.MaxAge < 0 {
			t.Fatal("cookie cleared although nothing was revoked")
		}
	}
	if !r.authenticates(presented) {
		t.Error("presented session revoked despite the failed lookup")
	}
}

func TestPasskeyRegister_RevokesOtherSessionsKeepsPresented(t *testing.T) {
	rig := newPasskeyRig(t)
	mint := func() (platform.Session, string) {
		return mintTestSession(t, rig.users, platform.Session{UserID: rig.user.ID, ExpiresAt: rig.now().Add(24 * time.Hour)})
	}
	presented, presentedToken := mint()
	_, otherToken := mint()
	stranger, err := rig.users.CreateUser(context.Background(), platform.User{
		AccountID: rig.account.ID, Email: "stranger@example.com", Role: platform.RoleMember,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, strangerToken := mintTestSession(t, rig.users, platform.Session{UserID: stranger.ID, ExpiresAt: rig.now().Add(24 * time.Hour)})
	rig.sessionID = presented.ID

	if w := registerPasskeyVia(t, rig); w.Code != http.StatusOK {
		t.Fatalf("finish-register status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	authenticates := func(token string) bool {
		_, err := rig.users.GetSessionByTokenHash(context.Background(), HashSessionToken(token))
		return err == nil
	}
	if !authenticates(presentedToken) {
		t.Error("passkey enrolment revoked the session that performed it")
	}
	if authenticates(otherToken) {
		t.Error("a pre-existing session of the user survived passkey enrolment")
	}
	if !authenticates(strangerToken) {
		t.Error("passkey enrolment revoked another user's session")
	}
}
