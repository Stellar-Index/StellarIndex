package dashboardauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// TestMintSession_RevokesTheSessionItReplaces pins #1322: logging in again
// is the victim's instinctive remedy for a stolen cookie, so the session the
// browser presents at login must be revoked, not left alive beside the new
// one. Both login paths (magic link / code and passkey) go through
// mintSession.
func TestMintSession_RevokesTheSessionItReplaces(t *testing.T) {
	rig := newPasskeyRig(t)
	const oldToken = "old-session-cookie-value"
	old, err := rig.users.CreateSession(context.Background(), platform.Session{
		UserID:    rig.user.ID,
		TokenHash: HashSessionToken(oldToken),
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/auth/verify-code", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: oldToken})
	w := httptest.NewRecorder()
	if err := rig.h.mintSession(w, req, rig.user); err != nil {
		t.Fatalf("mintSession: %v", err)
	}

	var minted string
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName {
			minted = c.Value
		}
	}
	if minted == "" || minted == oldToken {
		t.Fatalf("new session cookie = %q, want a fresh token", minted)
	}

	rig.users.mu.Lock()
	revokedAt := rig.users.sessions[old.ID].RevokedAt
	rig.users.mu.Unlock()
	if revokedAt.IsZero() {
		t.Error("the session presented at login is still live after a fresh login; " +
			"a stolen copy of that cookie survives the victim logging in again")
	}
}

// TestMintSession_CapsLiveSessionsPerUser pins that a login beyond the
// per-user cap revokes the oldest live session, so the row count stays
// bounded however often a door mints.
func TestMintSession_CapsLiveSessionsPerUser(t *testing.T) {
	rig := newPasskeyRig(t)
	base := time.Now().Add(-time.Hour)
	var oldest platform.Session
	for i := range maxLiveSessionsPerUser {
		s, err := rig.users.CreateSession(context.Background(), platform.Session{
			UserID:    rig.user.ID,
			TokenHash: HashSessionToken("prior-" + strconv.Itoa(i)),
			ExpiresAt: time.Now().Add(time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		rig.users.mu.Lock()
		s.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		rig.users.sessions[s.ID] = s
		rig.users.mu.Unlock()
		if i == 0 {
			oldest = s
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/finish-login", nil)
	req.RemoteAddr = "203.0.113.10:44100"
	w := httptest.NewRecorder()
	if err := rig.h.mintSession(w, req, rig.user); err != nil {
		t.Fatalf("mintSession: %v", err)
	}
	var minted string
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName {
			minted = c.Value
		}
	}
	if _, err := rig.users.GetSessionByTokenHash(context.Background(), HashSessionToken(minted)); err != nil {
		t.Fatalf("freshly minted session must be live: %v", err)
	}

	rig.users.mu.Lock()
	defer rig.users.mu.Unlock()
	live := 0
	for _, s := range rig.users.sessions {
		if s.UserID == rig.user.ID && s.RevokedAt.IsZero() {
			live++
		}
	}
	if live != maxLiveSessionsPerUser {
		t.Errorf("live sessions = %d, want %d", live, maxLiveSessionsPerUser)
	}
	if rig.users.sessions[oldest.ID].RevokedAt.IsZero() {
		t.Error("oldest session still live after a login beyond the cap")
	}
}
