package dashboardauth

// Ceremony-lifecycle tests: expiry, single-use, and user verification.
//
// These drive the REAL flow — HandlePasskeyBeginLogin, then
// HandlePasskeyFinishLogin with an assertion that actually verifies —
// rather than hand-building a webauthn.SessionData and asserting on
// the helper that consumes it. That distinction is the whole reason
// this file exists: the shipped suite covered the ceremony cookie by
// constructing SessionData with an Expires field the production path
// never populated, so it passed while every expiry guard in the
// package was dead code (audit-2026-08-13).
//
// The missing piece was an authenticator. [softAuthenticator] below is
// one: a P-256 key plus the ~40 bytes of CBOR/flags the library
// verifies against. It is not a mock of our code — the library's real
// signature verification runs — so a test that reaches "200 + session
// cookie" has proven the whole chain.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
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

// ─── A software authenticator ─────────────────────────────────────

const testRPID = "app.stellarindex.io"

// authenticator flag bits (W3C §6.1).
const (
	flagUserPresent  = 0x01
	flagUserVerified = 0x04
)

type softAuthenticator struct {
	key    *ecdsa.PrivateKey
	credID []byte
	userID []byte
}

func newSoftAuthenticator(t *testing.T, userID uuid.UUID) *softAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return &softAuthenticator{
		key:    key,
		credID: []byte("EXAMPLE-CRED-ID-PLACEHOLDER-SOFT"),
		userID: userID[:],
	}
}

// cosePublicKey encodes the public key the way an authenticator would
// hand it to us at registration: a COSE_Key CBOR map
// {1: 2 (EC2), 3: -7 (ES256), -1: 1 (P-256), -2: x, -3: y}.
//
// Hand-rolled rather than pulled from a CBOR library: it is six fixed
// header bytes around two 32-byte coordinates, and a test-only
// dependency on the encoder the library happens to use would be a
// worse trade than this comment.
func (a *softAuthenticator) cosePublicKey() []byte {
	x := a.key.PublicKey.X.FillBytes(make([]byte, 32))
	y := a.key.PublicKey.Y.FillBytes(make([]byte, 32))
	out := []byte{
		0xa5,       // map(5)
		0x01, 0x02, // 1 (kty): 2 (EC2)
		0x03, 0x26, // 3 (alg): -7 (ES256)
		0x20, 0x01, // -1 (crv): 1 (P-256)
		0x21, 0x58, 0x20, // -2 (x): bytes(32)
	}
	out = append(out, x...)
	out = append(out, 0x22, 0x58, 0x20) // -3 (y): bytes(32)
	return append(out, y...)
}

// authData is the 37-byte authenticator data: rpIdHash ‖ flags ‖
// counter. No attested-credential-data (that is a registration-only
// tail), which keeps this to the assertion shape.
func (a *softAuthenticator) authData(flags byte, counter uint32) []byte {
	h := sha256.Sum256([]byte(testRPID))
	out := append([]byte{}, h[:]...)
	out = append(out, flags)
	return binary.BigEndian.AppendUint32(out, counter)
}

// assertionBody produces the JSON body the browser would POST to
// finish-login for the given challenge, signed for real.
func (a *softAuthenticator) assertionBody(t *testing.T, challenge string, flags byte, counter uint32) string {
	t.Helper()
	clientData, err := json.Marshal(map[string]any{
		"type":      "webauthn.get",
		"challenge": challenge,
		"origin":    "https://" + testRPID,
	})
	if err != nil {
		t.Fatalf("marshal client data: %v", err)
	}
	authData := a.authData(flags, counter)
	clientDataHash := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(append([]byte{}, authData...), clientDataHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		t.Fatalf("sign assertion: %v", err)
	}
	enc := base64.RawURLEncoding
	body, err := json.Marshal(map[string]any{
		"id":    enc.EncodeToString(a.credID),
		"rawId": enc.EncodeToString(a.credID),
		"type":  "public-key",
		"response": map[string]any{
			"clientDataJSON":    enc.EncodeToString(clientData),
			"authenticatorData": enc.EncodeToString(authData),
			"signature":         enc.EncodeToString(sig),
			"userHandle":        enc.EncodeToString(a.userID),
		},
	})
	if err != nil {
		t.Fatalf("marshal assertion: %v", err)
	}
	return string(body)
}

// ─── Rig helpers ──────────────────────────────────────────────────

// movableClock is a test clock that starts at the REAL current time
// and can be advanced. Anchoring on the real clock matters: the
// go-webauthn library stamps SessionData.Expires from time.Now()
// (there is no clock seam), so a rig frozen in the past would find
// every ceremony eternally fresh no matter how far its own clock
// moved.
type movableClock struct {
	mu     sync.Mutex
	base   time.Time
	offset time.Duration
}

func newMovableClock() *movableClock { return &movableClock{base: time.Now().UTC()} }

func (c *movableClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.base.Add(c.offset)
}

func (c *movableClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

// newLiveClockPasskeyRig is newPasskeyRig with a real-time-anchored,
// advanceable clock and one registered credential belonging to the
// rig's user.
func newLiveClockPasskeyRig(t *testing.T) (*passkeyRig, *softAuthenticator, *movableClock) {
	t.Helper()
	rig := newPasskeyRig(t)
	clock := newMovableClock()
	rig.h.cfg.Now = clock.now

	auth := newSoftAuthenticator(t, rig.user.ID)
	if _, err := rig.passkeys.CreateWebAuthnCredential(context.Background(), platform.WebAuthnCredential{
		UserID:       rig.user.ID,
		Name:         "Soft key",
		CredentialID: auth.credID,
		PublicKey:    auth.cosePublicKey(),
		SignCount:    0,
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	return rig, auth, clock
}

// beginLogin runs the real begin handler and returns the ceremony
// cookie plus the challenge the options carried.
func beginLogin(t *testing.T, rig *passkeyRig) (*http.Cookie, string) {
	t.Helper()
	w := httptest.NewRecorder()
	rig.h.HandlePasskeyBeginLogin(w, httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/begin-login", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("begin-login status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var opts struct {
		PublicKey struct {
			Challenge        string `json:"challenge"`
			UserVerification string `json:"userVerification"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &opts); err != nil {
		t.Fatalf("unmarshal options: %v", err)
	}
	c := ceremonyCookie(t, w)
	if c == nil {
		t.Fatal("begin-login set no ceremony cookie")
	}
	return c, opts.PublicKey.Challenge
}

// finishLogin replays one captured request: same ceremony cookie,
// same body — exactly what an attacker who intercepted the POST holds.
func finishLogin(t *testing.T, rig *passkeyRig, cookie *http.Cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/finish-login", strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: PasskeyCeremonyCookieName, Value: cookie.Value})
	w := httptest.NewRecorder()
	rig.h.HandlePasskeyFinishLogin(w, req)
	return w
}

// ─── The audit-2026-08-13 regressions ─────────────────────────────

// TestPasskeyFinishLogin_ChallengeIsSingleUse — the HIGH finding. A
// captured finish-login request (ceremony cookie + assertion body) was
// an unlimited-use session mint: the assertion still verifies on the
// second POST because it is byte-identical, and nothing marked the
// challenge spent. Replaying it must now fail.
//
// The signature COUNTER here is 0, deliberately, and it is what makes
// this test test the right thing. A counter that advances would make
// the library's clone-warning refuse the replay for us
// (UpdateCounter: `authDataCount <= SignCount` → CloneWarning), and
// the test would pass with the consumption step deleted. But
// go-webauthn exempts 0 from that check — `(authDataCount != 0 ||
// a.SignCount != 0)` — because plenty of authenticators don't
// implement counters at all: Apple/iCloud passkeys, the single most
// common passkey on the internet, report 0 forever. For those
// credentials the sign counter is no replay defence whatsoever, so
// single-use ceremonies are the ONLY thing standing between a
// captured request and an endless session supply. Model the
// zero-counter authenticator or you are testing the wrong guard.
func TestPasskeyFinishLogin_ChallengeIsSingleUse(t *testing.T) {
	rig, auth, _ := newLiveClockPasskeyRig(t)
	cookie, challenge := beginLogin(t, rig)
	body := auth.assertionBody(t, challenge, flagUserPresent|flagUserVerified, 0)

	first := finishLogin(t, rig, cookie, body)
	if first.Code != http.StatusOK {
		t.Fatalf("first finish-login status = %d, want 200 (%s)", first.Code, first.Body.String())
	}
	if !sessionCookieSet(first) {
		t.Fatal("first finish-login minted no session cookie")
	}

	// Byte-for-byte the same request again.
	second := finishLogin(t, rig, cookie, body)
	if second.Code != http.StatusBadRequest {
		t.Fatalf("replayed finish-login status = %d, want 400 (%s)", second.Code, second.Body.String())
	}
	if sessionCookieSet(second) {
		t.Fatal("replayed finish-login minted a SECOND session — the challenge was not consumed")
	}
}

// TestPasskeyFinishLogin_CeremonyExpires — a captured ceremony must
// stop working once its TTL elapses. The assertion here is otherwise
// perfectly valid (the same one that mints a session in the test
// above), so expiry is the only thing that can refuse it.
func TestPasskeyFinishLogin_CeremonyExpires(t *testing.T) {
	rig, auth, clock := newLiveClockPasskeyRig(t)
	cookie, challenge := beginLogin(t, rig)
	body := auth.assertionBody(t, challenge, flagUserPresent|flagUserVerified, 1)

	// Still inside the window: the same request would succeed (proven
	// by TestPasskeyFinishLogin_ChallengeIsSingleUse). Move past it.
	clock.advance(passkeyCeremonyTTL + time.Minute)

	w := finishLogin(t, rig, cookie, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expired finish-login status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if sessionCookieSet(w) {
		t.Fatal("an expired ceremony minted a session")
	}
}

// TestPasskeyBeginLogin_StampsServerSideExpiry pins the library
// configuration the expiry check depends on. go-webauthn only sets
// SessionData.Expires when Config.Timeouts.<ceremony>.Enforce is true,
// and that field defaults FALSE — so this asserts on the produced
// cookie rather than on our intent.
func TestPasskeyBeginLogin_StampsServerSideExpiry(t *testing.T) {
	rig := newPasskeyRig(t)
	before := time.Now().UTC()
	w := httptest.NewRecorder()
	rig.h.HandlePasskeyBeginLogin(w, httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/begin-login", nil))
	c := ceremonyCookie(t, w)
	if c == nil {
		t.Fatal("no ceremony cookie")
	}

	dot := strings.IndexByte(c.Value, '.')
	payload, err := base64.RawURLEncoding.DecodeString(c.Value[:dot])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var got passkeyCeremony
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal ceremony: %v", err)
	}
	if got.Session.Expires.IsZero() {
		t.Fatal("ceremony carries no expiry — Config.Timeouts.Login.Enforce is off and every expiry guard is dead code")
	}
	if want := before.Add(passkeyCeremonyTTL); got.Session.Expires.Before(before) || got.Session.Expires.After(want.Add(time.Minute)) {
		t.Fatalf("expiry = %v, want ≈ %v", got.Session.Expires, want)
	}
}

// TestPasskeyCeremony_ZeroExpiryRefused — a ceremony with no stamped
// expiry (what the pre-fix binary minted) is refused rather than
// treated as eternal.
func TestPasskeyCeremony_ZeroExpiryRefused(t *testing.T) {
	rig := newPasskeyRig(t)
	w := httptest.NewRecorder()
	if err := rig.h.setPasskeyCeremonyCookie(w, passkeyCeremony{Purpose: "login"}); err != nil {
		t.Fatalf("set cookie: %v", err)
	}
	c := ceremonyCookie(t, w)
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/finish-login", nil)
	req.AddCookie(&http.Cookie{Name: PasskeyCeremonyCookieName, Value: c.Value})
	if _, err := rig.h.readPasskeyCeremonyCookie(req, "login"); err == nil {
		t.Fatal("ceremony with zero Expires accepted")
	}
}

// TestPasskeyFinishLogin_RequiresUserVerification — the MEDIUM
// finding. With UV neither requested nor required, a passkey sign-in
// was possession-only: whoever holds the authenticator is the account,
// no biometric or PIN involved. An assertion whose UV bit is clear
// must now be refused.
func TestPasskeyFinishLogin_RequiresUserVerification(t *testing.T) {
	rig, auth, _ := newLiveClockPasskeyRig(t)
	cookie, challenge := beginLogin(t, rig)

	// User present, but NOT verified.
	body := auth.assertionBody(t, challenge, flagUserPresent, 1)
	w := finishLogin(t, rig, cookie, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("UV-less finish-login status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if sessionCookieSet(w) {
		t.Fatal("an assertion without user verification minted a session")
	}
}

// TestPasskeyBeginLogin_AsksForUserVerification — the options the
// browser receives must SAY required, or the browser applies its own
// default and the authenticator never sets the bit we check for.
func TestPasskeyBeginLogin_AsksForUserVerification(t *testing.T) {
	rig := newPasskeyRig(t)
	w := httptest.NewRecorder()
	rig.h.HandlePasskeyBeginLogin(w, httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/begin-login", nil))
	var opts struct {
		PublicKey struct {
			UserVerification string `json:"userVerification"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &opts); err != nil {
		t.Fatalf("unmarshal options: %v", err)
	}
	if opts.PublicKey.UserVerification != "required" {
		t.Fatalf("userVerification = %q, want required", opts.PublicKey.UserVerification)
	}
}

// TestPasskeyFinishLogin_OversizeBodyRejected — the "body too large"
// branch used to be unreachable (io.LimitReader returns nil error at
// its cap, silently truncating), so an oversize assertion surfaced as
// a confusing parse failure.
func TestPasskeyFinishLogin_OversizeBodyRejected(t *testing.T) {
	rig := newPasskeyRig(t)
	cookie, _ := beginLogin(t, rig)
	w := finishLogin(t, rig, cookie, strings.Repeat("x", maxPasskeyBodyBytes+1))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "too large") {
		t.Fatalf("body = %s, want the too-large detail", w.Body.String())
	}
}

// ─── Name truncation ──────────────────────────────────────────────

// TestPasskeyDisplayName_TruncatesByRunes — the storage CHECK counts
// characters; slicing bytes clipped legal names AND could emit invalid
// UTF-8, which Postgres rejects with a 500 after the authenticator has
// already committed a resident-credential slot.
func TestPasskeyDisplayName_TruncatesByRunes(t *testing.T) {
	cjk := strings.Repeat("鍵", 150) // 3 bytes per rune
	got := passkeyDisplayName(cjk)
	if n := len([]rune(got)); n != maxPasskeyNameLen {
		t.Fatalf("rune length = %d, want %d", n, maxPasskeyNameLen)
	}
	if got != strings.Repeat("鍵", maxPasskeyNameLen) {
		t.Fatal("truncation split a rune — the result is not the first 100 characters")
	}

	if got := passkeyDisplayName("   "); got != "Passkey" {
		t.Fatalf("blank name = %q, want the Passkey default", got)
	}
	if got := passkeyDisplayName(" MacBook Touch ID "); got != "MacBook Touch ID" {
		t.Fatalf("name = %q, want it trimmed and preserved", got)
	}
	if n := len([]rune(passkeyDisplayName(strings.Repeat("a", 200)))); n != maxPasskeyNameLen {
		t.Fatalf("ascii rune length = %d, want %d", n, maxPasskeyNameLen)
	}
}

// ─── The in-process guard ─────────────────────────────────────────

// TestInProcessPasskeyCeremonyGuard — the default single-use store:
// first claim wins, second loses, and the record is dropped once its
// TTL elapses (bounding memory).
func TestInProcessPasskeyCeremonyGuard(t *testing.T) {
	clock := newMovableClock()
	g := newInProcessPasskeyCeremonyGuard(clock.now)
	ctx := context.Background()

	claimed, err := g.Consume(ctx, "digest-a", time.Minute)
	if err != nil || !claimed {
		t.Fatalf("first claim: claimed=%v err=%v", claimed, err)
	}
	claimed, err = g.Consume(ctx, "digest-a", time.Minute)
	if err != nil || claimed {
		t.Fatalf("replay claim: claimed=%v err=%v, want false", claimed, err)
	}

	// A different ceremony is unaffected.
	if claimed, _ := g.Consume(ctx, "digest-b", time.Minute); !claimed {
		t.Fatal("an unrelated ceremony was refused")
	}

	// Past the TTL the record is swept, so memory stays bounded.
	clock.advance(2 * time.Minute)
	if claimed, _ := g.Consume(ctx, "digest-a", time.Minute); !claimed {
		t.Fatal("record outlived its TTL")
	}
	if len(g.seen) != 1 {
		t.Fatalf("spent-set holds %d records after the sweep, want 1", len(g.seen))
	}
}

// TestInProcessPasskeyCeremonyGuard_ConcurrentClaimsHaveOneWinner —
// two requests presenting the same captured ceremony at once must not
// both be told they claimed it. Run under -race.
func TestInProcessPasskeyCeremonyGuard_ConcurrentClaimsHaveOneWinner(t *testing.T) {
	g := newInProcessPasskeyCeremonyGuard(func() time.Time { return time.Now().UTC() })
	const racers = 16
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		winld int
	)
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if claimed, _ := g.Consume(context.Background(), "contended", time.Minute); claimed {
				mu.Lock()
				winld++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if winld != 1 {
		t.Fatalf("%d racers claimed the same ceremony, want exactly 1", winld)
	}
}

// TestConsumeCeremony_SingleUsePerPurpose covers the registration
// half of the seam, which no forged-attestation test reaches: the
// same helper both finish handlers call is one-shot, and the two
// purposes keep independent slots so finishing a registration can
// never spend a login's challenge (or vice versa).
func TestConsumeCeremony_SingleUsePerPurpose(t *testing.T) {
	rig := newPasskeyRig(t)
	ctx := context.Background()
	const challenge = "example-challenge-placeholder"
	login := passkeyCeremony{Purpose: "login", Session: webauthn.SessionData{Challenge: challenge}}
	register := passkeyCeremony{Purpose: "register", Session: webauthn.SessionData{Challenge: challenge}}

	if err := rig.h.consumeCeremony(ctx, login); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if err := rig.h.consumeCeremony(ctx, login); !errors.Is(err, errPasskeyCeremonyReplayed) {
		t.Fatalf("second consume err = %v, want errPasskeyCeremonyReplayed", err)
	}
	if err := rig.h.consumeCeremony(ctx, register); err != nil {
		t.Fatalf("register ceremony with the same challenge was refused: %v", err)
	}
}

// TestConsumeCeremony_NilGuardFailsClosed — a Handlers assembled
// without going through NewHandlers must refuse rather than silently
// skip replay protection.
func TestConsumeCeremony_NilGuardFailsClosed(t *testing.T) {
	h := &Handlers{cfg: &Config{}}
	err := h.consumeCeremony(context.Background(), passkeyCeremony{Purpose: "login"})
	if err == nil {
		t.Fatal("a nil guard silently allowed the ceremony")
	}
}

// stubCeremonyGuard fails every claim, standing in for an unreachable
// Redis.
type stubCeremonyGuard struct{ err error }

func (s stubCeremonyGuard) Consume(context.Context, string, time.Duration) (bool, error) {
	return false, s.err
}

// TestPasskeyFinishLogin_FailsClosedWhenGuardUnavailable — if we
// cannot prove the ceremony is fresh we do not mint a session, even
// though the assertion itself verified. Passkey sign-in degrades;
// email-code sign-in is unaffected.
func TestPasskeyFinishLogin_FailsClosedWhenGuardUnavailable(t *testing.T) {
	rig, auth, _ := newLiveClockPasskeyRig(t)
	rig.h.cfg.PasskeyCeremonyGuard = stubCeremonyGuard{err: context.DeadlineExceeded}

	cookie, challenge := beginLogin(t, rig)
	w := finishLogin(t, rig, cookie, auth.assertionBody(t, challenge, flagUserPresent|flagUserVerified, 1))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", w.Code, w.Body.String())
	}
	if sessionCookieSet(w) {
		t.Fatal("a session was minted while the spent-set was unreachable")
	}
}

// evictableCeremonyGuard models a Redis ceremony guard whose markers
// live in an allkeys-lru keyspace: both the legacy spent-set (Consume)
// and the begin-time reservation (Reserve/ClaimReserved) sit in maps
// that evict() empties, exactly as an LRU pass under memory pressure
// would. Implementing BOTH the base contract and the reservation
// upgrade lets the same fake drive the pre-fix (Consume) and post-fix
// (ClaimReserved) finish paths through the real handlers — so the test
// below is red against the bare spent-set and green against the fix.
type evictableCeremonyGuard struct {
	mu    sync.Mutex
	spent map[string]bool // Consume's spent-marker (the vulnerable path)
	live  map[string]bool // Reserve's reservation marker (the fixed path)
}

func newEvictableCeremonyGuard() *evictableCeremonyGuard {
	return &evictableCeremonyGuard{spent: map[string]bool{}, live: map[string]bool{}}
}

// Consume is the vulnerable spent-set: an ABSENT marker reads as fresh,
// so an eviction between two presentations re-opens the claim.
func (g *evictableCeremonyGuard) Consume(_ context.Context, digest string, _ time.Duration) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.spent[digest] {
		return false, nil
	}
	g.spent[digest] = true
	return true, nil
}

func (g *evictableCeremonyGuard) Reserve(_ context.Context, digest string, _ time.Duration) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.live[digest] = true
	return nil
}

// ClaimReserved requires the reservation to still be present: an
// evicted (absent) marker refuses rather than re-claiming.
func (g *evictableCeremonyGuard) ClaimReserved(_ context.Context, digest string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.live[digest] {
		return false, nil
	}
	delete(g.live, digest)
	return true, nil
}

// evict empties both marker sets, as an allkeys-lru pass under memory
// pressure would.
func (g *evictableCeremonyGuard) evict() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.spent = map[string]bool{}
	g.live = map[string]bool{}
}

// TestPasskeyFinishLogin_SurvivesSpentMarkerEviction is
// W1-auth-passkey-1. R1's Redis runs allkeys-lru, so the ceremony
// spent-marker can be EVICTED before its TTL under memory pressure — an
// attacker floods the shared instance via open registration's
// no-expiry apikey mirror. With the bare SETNX spent-set, a captured
// finish-login replayed after that eviction found the slot free and
// minted a SECOND live session for the victim: account takeover from a
// single captured request, defeating the very guard built to stop it.
// The begin-time reservation must make that replay fail CLOSED.
//
// Counter is 0 (as in the single-use test) so the library's clone-
// warning can't refuse the replay for us — the ceremony guard is the
// only thing under test.
func TestPasskeyFinishLogin_SurvivesSpentMarkerEviction(t *testing.T) {
	rig, auth, _ := newLiveClockPasskeyRig(t)
	guard := newEvictableCeremonyGuard()
	rig.h.cfg.PasskeyCeremonyGuard = guard

	cookie, challenge := beginLogin(t, rig)
	body := auth.assertionBody(t, challenge, flagUserPresent|flagUserVerified, 0)

	// Legitimate sign-in consumes the ceremony.
	first := finishLogin(t, rig, cookie, body)
	if first.Code != http.StatusOK {
		t.Fatalf("first finish-login status = %d, want 200 (%s)", first.Code, first.Body.String())
	}
	if !sessionCookieSet(first) {
		t.Fatal("first finish-login minted no session cookie")
	}

	// Attacker drives Redis to maxmemory; allkeys-lru evicts the
	// ceremony markers before their TTL.
	guard.evict()

	// The captured request, replayed inside the ceremony's own 5-minute
	// validity, must NOT mint a second session.
	second := finishLogin(t, rig, cookie, body)
	if second.Code != http.StatusBadRequest {
		t.Fatalf("replayed finish-login after eviction status = %d, want 400 (%s)", second.Code, second.Body.String())
	}
	if sessionCookieSet(second) {
		t.Fatal("a replay after spent-marker eviction minted a SECOND session — the single-use guard was defeated by allkeys-lru")
	}
}
