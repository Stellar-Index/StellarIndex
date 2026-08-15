package dashboardauth

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// TestSessionTokenIsHashedAtRest is the W1-auth-passkey-2 regression:
// the session cookie must carry a high-entropy random token whose
// sha256 (NOT the raw value, and NOT the row's primary key) is what the
// sessions table stores and what the auth path looks up by. This pins
// the security property three ways, each of which fails on the pre-fix
// code (cookie == sess.ID.String(); resolveSession GetSession(uuid)):
//
//	(1) the freshly-minted cookie resolves to its user (round trip);
//	(2) the stored value is sha256(cookie) — a hash, not the plaintext;
//	(3) the session's primary key, replayed as a cookie, does NOT
//	    resolve — so a leak of the sessions table (which holds only the
//	    hash + the PK) is not directly replayable.
//
// Proven red: reverting either the mintSession hunk (cookie=token,
// store TokenHash) or the resolveSession hunk (hash-lookup) fails
// assertions (2) and (3).
func TestSessionTokenIsHashedAtRest(t *testing.T) {
	r := newTestRig(t)
	acct, err := r.accounts.Create(context.Background(), platform.Account{
		Name: "x", Slug: "x", Tier: platform.TierFree, Status: platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	user, err := r.users.CreateUser(context.Background(), platform.User{
		AccountID: acct.ID, Email: "ash@example.com", Role: platform.RoleOwner,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Mint through the single issuance path (magic link, code, and
	// passkey all terminate here).
	w := httptest.NewRecorder()
	mintReq := httptest.NewRequest(http.MethodPost, "/v1/auth/finish", nil)
	if err := r.h.mintSession(w, mintReq, user); err != nil {
		t.Fatalf("mintSession: %v", err)
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value == "" {
		t.Fatal("mintSession set no session cookie")
	}

	// The single stored row.
	var stored platform.Session
	n := 0
	for _, s := range r.users.sessions {
		stored = s
		n++
	}
	if n != 1 {
		t.Fatalf("expected exactly one stored session, got %d", n)
	}

	// (1) Round trip: the cookie resolves to the minting user.
	roundTrip := httptest.NewRequest(http.MethodGet, "/v1/account/me", nil)
	roundTrip.AddCookie(&http.Cookie{Name: SessionCookieName, Value: cookie.Value})
	sc, ok := resolveSession(roundTrip, r.cfg, newTouchTracker(time.Minute))
	if !ok {
		t.Fatal("freshly-minted session cookie did not resolve")
	}
	if sc.User.ID != user.ID {
		t.Fatalf("resolved user = %v, want %v", sc.User.ID, user.ID)
	}

	// (2) The stored value is sha256(cookie) — a hash, never the plaintext.
	if !bytes.Equal(stored.TokenHash, HashSessionToken(cookie.Value)) {
		t.Fatalf("stored token_hash != sha256(cookie): the row does not hash the bearer token")
	}
	if bytes.Equal(stored.TokenHash, []byte(cookie.Value)) {
		t.Fatal("stored token_hash equals the raw cookie — the token is stored in the clear")
	}
	if cookie.Value == stored.ID.String() {
		t.Fatal("cookie carries the session PK verbatim — the raw-id bearer defect is live")
	}

	// (3) The primary key, replayed as a cookie, must NOT resolve. This
	// is the whole point: DB-read access yields the PK and the hash,
	// neither of which is a presentable credential.
	replay := httptest.NewRequest(http.MethodGet, "/v1/account/me", nil)
	replay.AddCookie(&http.Cookie{Name: SessionCookieName, Value: stored.ID.String()})
	if _, ok := resolveSession(replay, r.cfg, newTouchTracker(time.Minute)); ok {
		t.Fatal("session primary key replayed as a cookie resolved — " +
			"the unhashed-bearer defect (W1-auth-passkey-2) is live")
	}
}
