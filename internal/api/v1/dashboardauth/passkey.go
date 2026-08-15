package dashboardauth

// Passkey (WebAuthn) sign-in for the customer dashboard.
//
// Two ceremonies, four endpoints, plus management:
//
//   - begin-register / finish-register: a SIGNED-IN user adds a
//     passkey. The browser calls navigator.credentials.create() with
//     the options begin returns; finish verifies the attestation and
//     stores the public key (webauthn_credentials, migration 0140).
//   - begin-login / finish-login: an anonymous visitor signs in with
//     a discoverable credential (usernameless — the authenticator
//     itself knows which account). finish verifies the assertion and
//     mints the SAME session cookie the email-code flow issues, via
//     the shared mintSession path.
//   - credentials (GET/DELETE): management for the settings page.
//
// The ceremony state (challenge) between begin and finish lives in a
// short-lived HMAC-signed cookie rather than server-side storage:
// the challenge is not a secret (the browser sees it), but it must
// be integrity-bound so a caller can't substitute their own — the
// HMAC (keyed by the same server secret that keys the 6-digit code
// derivation, domain-separated) provides exactly that.
//
// Integrity alone is not enough, and audit-2026-08-13 found both
// gaps live:
//
//   - The ceremony must EXPIRE server-side. The cookie's MaxAge is a
//     client-side hint; an attacker's HTTP client simply ignores it.
//     See [Handlers.webAuthn] for why the library needs telling.
//   - The ceremony must be SINGLE-USE. See passkey_ceremony_guard.go.
//
// Without both, a captured finish-login request (cookie + body) is a
// session-mint oracle for that account: unlimited uses, no expiry.
//
// RP identity derives from DashboardBaseURL — the origin the browser
// performs the ceremony on (https://stellarindex.io): RP ID is its
// hostname, allowed origin is its scheme://host. That mirrors how the
// magic-link callback URL is already built from the same config.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// PasskeyCeremonyCookieName carries the signed WebAuthn ceremony
// state between begin and finish. Distinct name from the session +
// login-intent cookies for the same cross-surface-confusion reason.
const PasskeyCeremonyCookieName = "stellarindex_passkey_ceremony"

// passkeyCeremonyTTL bounds how long a begin's challenge stays
// redeemable. Authenticator prompts resolve in seconds; 5 minutes
// tolerates a slow security-key fumble.
const passkeyCeremonyTTL = 5 * time.Minute

// passkeyCeremonyReserveSlack pads the begin-time reservation's TTL
// past the ceremony's own validity (see [passkeyCeremonyReserveGuard])
// so the reservation can only ever disappear through an allkeys-lru
// EVICTION — the condition we must fail closed on — and never through
// TTL expiry under a still-valid challenge, which would refuse a
// legitimate sign-in for no security gain.
const passkeyCeremonyReserveSlack = time.Minute

// passkeyCeremonyDomain domain-separates the ceremony-cookie HMAC
// from the login-code HMAC that shares the server secret.
const passkeyCeremonyDomain = "stellarindex/passkey-ceremony/v1|"

// maxPasskeyBodyBytes bounds finish-ceremony request bodies. Real
// attestation/assertion payloads are a few KB; 64 KiB is generous.
const maxPasskeyBodyBytes = 64 << 10

// maxPasskeyNameLen matches the storage CHECK (length(name) <= 100).
const maxPasskeyNameLen = 100

// passkeyCeremony is the signed cookie payload. Purpose prevents a
// registration challenge being replayed against finish-login (and
// vice versa).
type passkeyCeremony struct {
	Purpose string               `json:"purpose"` // "register" | "login"
	Session webauthn.SessionData `json:"session"`
}

// webAuthn lazily builds the library handle from DashboardBaseURL.
// Called per-request (cheap: parses one URL) so a Handlers
// constructed without Passkeys never pays for it.
func (h *Handlers) webAuthn() (*webauthn.WebAuthn, error) {
	u, err := url.Parse(h.cfg.DashboardBaseURL)
	if err != nil || u.Hostname() == "" {
		return nil, errors.New("dashboardauth: DashboardBaseURL is not an absolute URL")
	}
	// Timeouts is load-bearing, not cosmetic: go-webauthn stamps
	// SessionData.Expires ONLY when the matching Enforce is true
	// (webauthn/login.go, webauthn/registration.go) and Enforce
	// defaults FALSE. Ship the zero value and Expires stays the zero
	// time, which makes every `!Expires.IsZero()` guard — ours in
	// [Handlers.readPasskeyCeremonyCookie] AND the library's own in
	// ValidateLogin / CreateCredential — dead code. The ceremony then
	// has no server-side lifetime at all: the cookie's MaxAge=300 is
	// a browser hint, and a captured cookie replayed by a plain HTTP
	// client is honoured forever. audit-2026-08-13 (HIGH).
	//
	// TimeoutUVD (the "user verification discouraged" variant) is set
	// to the same value for completeness; we require UV everywhere
	// (see the Begin* handlers) so it should never be selected.
	timeouts := webauthn.TimeoutConfig{
		Enforce:    true,
		Timeout:    passkeyCeremonyTTL,
		TimeoutUVD: passkeyCeremonyTTL,
	}
	return webauthn.New(&webauthn.Config{
		RPID:          u.Hostname(),
		RPDisplayName: "Stellar Index",
		RPOrigins:     []string{u.Scheme + "://" + u.Host},
		Timeouts: webauthn.TimeoutsConfig{
			Login:        timeouts,
			Registration: timeouts,
		},
	})
}

// webauthnUser adapts a platform.User (+ its stored credentials) to
// the library's User interface. The user handle is the user's UUID
// bytes — stable, opaque, and 16 bytes.
type webauthnUser struct {
	user  platform.User
	creds []webauthn.Credential
}

func (u webauthnUser) WebAuthnID() []byte { return u.user.ID[:] }

func (u webauthnUser) WebAuthnName() string { return u.user.Email }

func (u webauthnUser) WebAuthnDisplayName() string {
	if u.user.DisplayName != "" {
		return u.user.DisplayName
	}
	return u.user.Email
}

func (u webauthnUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// toLibCredential converts a stored row into the library's shape —
// the fields assertion verification consumes.
func toLibCredential(c platform.WebAuthnCredential) webauthn.Credential {
	transports := make([]protocol.AuthenticatorTransport, 0, len(c.Transports))
	for _, t := range c.Transports {
		transports = append(transports, protocol.AuthenticatorTransport(t))
	}
	return webauthn.Credential{
		ID:              c.CredentialID,
		PublicKey:       c.PublicKey,
		AttestationType: c.AttestationType,
		Transport:       transports,
		Flags: webauthn.CredentialFlags{
			BackupEligible: c.BackupEligible,
			BackupState:    c.BackupState,
		},
		Authenticator: webauthn.Authenticator{
			AAGUID:    c.AAGUID,
			SignCount: uint32(c.SignCount), //nolint:gosec // stored from a uint32; see fromLibCredential
		},
	}
}

// ─── Ceremony cookie (HMAC-signed, short TTL) ─────────────────────

func passkeyCeremonyMAC(secret, payload []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(passkeyCeremonyDomain))
	mac.Write(payload)
	return mac.Sum(nil)
}

// setPasskeyCeremonyCookie serialises + signs the ceremony state.
func (h *Handlers) setPasskeyCeremonyCookie(w http.ResponseWriter, c passkeyCeremony) error {
	payload, err := json.Marshal(c)
	if err != nil {
		return err
	}
	enc := base64.RawURLEncoding
	value := enc.EncodeToString(payload) + "." +
		enc.EncodeToString(passkeyCeremonyMAC(h.cfg.Generator.Secret, payload))
	http.SetCookie(w, &http.Cookie{
		Name:     PasskeyCeremonyCookieName,
		Value:    value,
		Path:     "/",
		Domain:   h.cfg.CookieDomain,
		MaxAge:   int(passkeyCeremonyTTL / time.Second),
		HttpOnly: true,
		Secure:   h.cfg.CookieSecure,
		SameSite: sessionSameSite(),
	})
	return nil
}

// readPasskeyCeremonyCookie verifies the signature + expiry and
// returns the ceremony for the expected purpose. Any failure is a
// single generic error — callers map it to one 400 so a tamperer
// learns nothing about which check tripped.
func (h *Handlers) readPasskeyCeremonyCookie(r *http.Request, wantPurpose string) (passkeyCeremony, error) {
	errInvalid := errors.New("dashboardauth: invalid passkey ceremony")
	c, err := r.Cookie(PasskeyCeremonyCookieName)
	if err != nil {
		return passkeyCeremony{}, errInvalid
	}
	dot := strings.IndexByte(c.Value, '.')
	if dot < 0 {
		return passkeyCeremony{}, errInvalid
	}
	enc := base64.RawURLEncoding
	payload, err := enc.DecodeString(c.Value[:dot])
	if err != nil {
		return passkeyCeremony{}, errInvalid
	}
	gotMAC, err := enc.DecodeString(c.Value[dot+1:])
	if err != nil {
		return passkeyCeremony{}, errInvalid
	}
	if !hmac.Equal(gotMAC, passkeyCeremonyMAC(h.cfg.Generator.Secret, payload)) {
		return passkeyCeremony{}, errInvalid
	}
	var ceremony passkeyCeremony
	if err := json.Unmarshal(payload, &ceremony); err != nil {
		return passkeyCeremony{}, errInvalid
	}
	if ceremony.Purpose != wantPurpose {
		return passkeyCeremony{}, errInvalid
	}
	// A MISSING expiry is refused, not waved through. The previous
	// `!IsZero() && …` spelling meant an unstamped ceremony lived
	// forever — which is exactly what shipped, because the library
	// never stamped one (see [Handlers.webAuthn]). Requiring the
	// stamp makes that failure mode impossible to reintroduce
	// silently: a config regression locks passkeys out rather than
	// quietly issuing eternal challenges. The one visible cost is at
	// deploy time — ceremonies begun by the previous binary carry no
	// expiry and are refused, so a sign-in in flight across the
	// restart must be retried. audit-2026-08-13.
	if ceremony.Session.Expires.IsZero() || !ceremony.Session.Expires.After(h.cfg.Now()) {
		return passkeyCeremony{}, errInvalid
	}
	return ceremony, nil
}

// errPasskeyCeremonyReplayed is the sentinel for "this ceremony has
// already been spent" — distinct from a store outage so the caller
// can log the two differently (a replay is an attack signal; an
// outage is an ops signal) while returning the same generic body.
var errPasskeyCeremonyReplayed = errors.New("dashboardauth: passkey ceremony already used")

// passkeyCeremonyDigest is the single-use guard's key for one
// ceremony: purpose + challenge, hashed.
//
// The challenge is the per-ceremony unique value (32 random bytes
// from the library), so it alone identifies the ceremony; purpose is
// mixed in for the same reason the cookie carries it, and the domain
// string keeps this digest from colliding with any other use of the
// same inputs. Plain SHA-256 rather than the cookie's HMAC: the
// challenge is not a secret (the browser and the authenticator both
// see it), and keying the digest would tie the spent-set to a secret
// that can rotate independently of the challenges it must outlive.
func passkeyCeremonyDigest(c passkeyCeremony) string {
	sum := sha256.Sum256([]byte(passkeyCeremonyDomain + c.Purpose + "|" + c.Session.Challenge))
	return hex.EncodeToString(sum[:])
}

// passkeyCeremonyReserveGuard is the optional capability a ceremony
// guard exposes when its spent-set lives in a store that can EVICT
// under memory pressure — Redis under R1's allkeys-lru. Such a guard
// cannot treat "marker absent == fresh": an evicted spent-marker would
// re-open the replay window (W1-auth-passkey-1). Instead it RESERVEs
// the ceremony at begin and, at finish, claims it only if the
// reservation still exists, so an evicted reservation fails CLOSED (no
// session) rather than freeing the slot for a captured request. The
// in-process default guard doesn't evict and doesn't implement this —
// [consumeCeremony] falls back to its plain spent-set.
//
// This is an optional-interface upgrade in the style of http.Flusher:
// the base [PasskeyCeremonyGuard] contract is unchanged, and a guard
// opts into the stronger protocol by implementing these two methods.
type passkeyCeremonyReserveGuard interface {
	Reserve(ctx context.Context, digest string, ttl time.Duration) error
	ClaimReserved(ctx context.Context, digest string) (bool, error)
}

// reserveCeremony records a begin ceremony as live and single-use when
// the guard's spent-set is evictable (see [passkeyCeremonyReserveGuard]).
// A no-op — nil — for a non-evicting guard. Called at BEGIN, before the
// challenge is handed to the browser, so a challenge is never issued
// without a reservation backing its later single-use claim. The
// reservation outlives the ceremony cookie (slack) so only a genuine
// eviction, never TTL expiry, can make a valid finish fail closed.
func (h *Handlers) reserveCeremony(ctx context.Context, c passkeyCeremony) error {
	guard, ok := h.cfg.PasskeyCeremonyGuard.(passkeyCeremonyReserveGuard)
	if !ok {
		return nil
	}
	return guard.Reserve(ctx, passkeyCeremonyDigest(c), passkeyCeremonyTTL+passkeyCeremonyReserveSlack)
}

// consumeCeremony spends a ceremony so it can never be presented
// again. Returns [errPasskeyCeremonyReplayed] for a second
// presentation, and a wrapped store error when the spent-set is
// unreachable — callers refuse in BOTH cases (fail closed: a session
// we can't prove is fresh is one we don't mint).
//
// Called AFTER the assertion/attestation verifies, mirroring the
// SEP-10 replay guard's placement (internal/auth/sep10/validator.go):
// verifying first keeps unauthenticated callers from spending slots
// in the shared store, and a submission that fails verification never
// minted anything worth replaying.
//
// An evictable guard (Redis/allkeys-lru) claims through its begin-time
// RESERVATION so an evicted marker fails closed instead of re-opening
// the replay window (W1-auth-passkey-1); a non-evicting guard uses its
// plain spent-set. Both paths report a replay as
// [errPasskeyCeremonyReplayed] and a store outage as a wrapped error.
func (h *Handlers) consumeCeremony(ctx context.Context, c passkeyCeremony) error {
	guard := h.cfg.PasskeyCeremonyGuard
	if guard == nil {
		// Unreachable via NewHandlers (validate installs the
		// in-process default). Refuse rather than skip the check —
		// a Handlers assembled by hand must not silently lose replay
		// protection.
		return errors.New("dashboardauth: no passkey ceremony guard configured")
	}
	digest := passkeyCeremonyDigest(c)
	if rg, ok := guard.(passkeyCeremonyReserveGuard); ok {
		claimed, err := rg.ClaimReserved(ctx, digest)
		if err != nil {
			return err
		}
		if !claimed {
			return errPasskeyCeremonyReplayed
		}
		return nil
	}
	claimed, err := guard.Consume(ctx, digest, passkeyCeremonyTTL)
	if err != nil {
		return err
	}
	if !claimed {
		return errPasskeyCeremonyReplayed
	}
	return nil
}

func (h *Handlers) clearPasskeyCeremonyCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     PasskeyCeremonyCookieName,
		Value:    "",
		Path:     "/",
		Domain:   h.cfg.CookieDomain,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.cfg.CookieSecure,
		SameSite: sessionSameSite(),
	})
}

// ─── Registration (session-gated) ─────────────────────────────────

// HandlePasskeyBeginRegister returns the credential-creation options
// for navigator.credentials.create(). Session-gated: adding a
// passkey is a capability of an authenticated user, mirroring how a
// password change would demand a live session.
func (h *Handlers) HandlePasskeyBeginRegister(w http.ResponseWriter, r *http.Request) {
	sc, ok := SessionFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication required", r.URL.Path)
		return
	}
	wa, err := h.webAuthn()
	if err != nil {
		h.cfg.Logger.Error("webauthn config", "err", err)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	existing, err := h.cfg.Passkeys.ListWebAuthnCredentialsForUser(r.Context(), sc.User.ID)
	if err != nil {
		h.cfg.Logger.Error("list passkeys for begin-register", "err", err, "user_id", sc.User.ID)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	exclusions := make([]protocol.CredentialDescriptor, 0, len(existing))
	libCreds := make([]webauthn.Credential, 0, len(existing))
	for _, row := range existing {
		lc := toLibCredential(row)
		libCreds = append(libCreds, lc)
		exclusions = append(exclusions, lc.Descriptor())
	}

	user := webauthnUser{user: sc.User, creds: libCreds}
	creation, session, err := wa.BeginRegistration(user,
		// User verification REQUIRED. The credential registered here
		// is a first-factor, passwordless sign-in credential (the
		// login side asks for nothing else), so it has to carry a
		// second factor of its own: without UV, possession of the
		// authenticator IS the account. Requiring it at registration
		// means the credential is created behind a biometric/PIN, and
		// the login side (HandlePasskeyBeginLogin) requires the UV bit
		// on every assertion. Trade-off, accepted deliberately: a
		// security key with no PIN configured cannot be enrolled.
		//
		// ORDER MATTERS. WithAuthenticatorSelection REPLACES the whole
		// AuthenticatorSelection struct, while WithResidentKeyRequirement
		// only sets its two resident-key fields — so this option must
		// come FIRST or the UV requirement is silently dropped. The
		// options-shape assertions in passkey_test.go pin both fields
		// so a reorder fails the build rather than weakening sign-in.
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			UserVerification: protocol.VerificationRequired,
		}),
		// Discoverable (resident) so the login side can be
		// usernameless.
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithExclusions(exclusions),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation),
	)
	if err != nil {
		h.cfg.Logger.Error("begin passkey registration", "err", err, "user_id", sc.User.ID)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	ceremony := passkeyCeremony{Purpose: "register", Session: *session}
	// Reserve the ceremony BEFORE handing the challenge to the browser
	// so its single-use claim survives an allkeys-lru eviction of the
	// spent-set (W1-auth-passkey-1). A store outage here fails closed:
	// no challenge is issued that finish couldn't safely consume.
	if err := h.reserveCeremony(r.Context(), ceremony); err != nil {
		h.cfg.Logger.Error("reserve passkey ceremony", "err", err, "user_id", sc.User.ID)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	if err := h.setPasskeyCeremonyCookie(w, ceremony); err != nil {
		h.cfg.Logger.Error("set passkey ceremony cookie", "err", err)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(creation)
}

// passkeyDisplayName normalises the user-chosen label to something
// the storage CHECK (`length(name) <= 100`, migration 0140) accepts.
//
// Truncation is by RUNES, not bytes. Postgres `length()` counts
// CHARACTERS, so a byte slice was wrong twice over: it clipped a
// perfectly legal 100-character CJK name to 33 characters, and — the
// real bug — it could cut mid-rune and hand Postgres invalid UTF-8,
// which Postgres refuses. That surfaced as a 500 AFTER the
// authenticator had already burned a resident-credential slot for a
// credential the server then never stored: the user is left with a
// dead passkey on their device and no row here. audit-2026-08-13.
func passkeyDisplayName(raw string) string {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "Passkey"
	}
	if utf8.RuneCountInString(name) > maxPasskeyNameLen {
		name = string([]rune(name)[:maxPasskeyNameLen])
	}
	return name
}

// finishRegisterRequest wraps the raw credential JSON the browser
// produced with the user-chosen label.
type finishRegisterRequest struct {
	Name       string          `json:"name"`
	Credential json.RawMessage `json:"credential"`
}

// HandlePasskeyFinishRegister verifies the attestation response
// against the ceremony cookie's challenge and stores the credential.
func (h *Handlers) HandlePasskeyFinishRegister(w http.ResponseWriter, r *http.Request) {
	sc, ok := SessionFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication required", r.URL.Path)
		return
	}
	// MaxBytesReader, not LimitReader: a LimitReader hitting its cap
	// returns (n, nil) — the read SUCCEEDS with a silently truncated
	// body, so the "body too large" branch below was unreachable and
	// an oversize attestation surfaced as "malformed JSON" instead.
	// MaxBytesReader is the repo-wide pattern and returns a real
	// error at the cap. audit-2026-08-13.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPasskeyBodyBytes))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "request body too large", r.URL.Path)
		return
	}
	var req finishRegisterRequest
	if err := json.Unmarshal(body, &req); err != nil || len(req.Credential) == 0 {
		writeProblem(w, http.StatusBadRequest, "malformed JSON", r.URL.Path)
		return
	}
	ceremony, err := h.readPasskeyCeremonyCookie(r, "register")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "passkey registration failed — start again", r.URL.Path)
		return
	}
	// The ceremony must belong to THIS session's user: a challenge
	// minted for user A must not register a credential onto user B.
	if !bytes.Equal(ceremony.Session.UserID, sc.User.ID[:]) {
		writeProblem(w, http.StatusBadRequest, "passkey registration failed — start again", r.URL.Path)
		return
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(req.Credential)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "passkey registration failed — start again", r.URL.Path)
		return
	}
	wa, err := h.webAuthn()
	if err != nil {
		h.cfg.Logger.Error("webauthn config", "err", err)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	cred, err := wa.CreateCredential(webauthnUser{user: sc.User}, ceremony.Session, parsed)
	if err != nil {
		h.cfg.Logger.Warn("passkey attestation rejected", "err", err, "user_id", sc.User.ID)
		writeProblem(w, http.StatusBadRequest, "passkey registration failed — start again", r.URL.Path)
		return
	}
	// Spend the challenge now that the attestation verified — before
	// the credential is stored, so a replayed registration can't take
	// a second bite even if the store's uniqueness check changes.
	if err := h.consumeCeremony(r.Context(), ceremony); err != nil {
		if errors.Is(err, errPasskeyCeremonyReplayed) {
			h.cfg.Logger.Warn("passkey registration ceremony replay refused", "user_id", sc.User.ID)
			writeProblem(w, http.StatusBadRequest, "passkey registration failed — start again", r.URL.Path)
			return
		}
		h.cfg.Logger.Error("consume passkey ceremony", "err", err, "user_id", sc.User.ID)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}

	transports := make([]string, 0, len(cred.Transport))
	for _, t := range cred.Transport {
		transports = append(transports, string(t))
	}
	row, err := h.cfg.Passkeys.CreateWebAuthnCredential(r.Context(), platform.WebAuthnCredential{
		UserID:          sc.User.ID,
		Name:            passkeyDisplayName(req.Name),
		CredentialID:    cred.ID,
		PublicKey:       cred.PublicKey,
		AttestationType: cred.AttestationType,
		Transports:      transports,
		SignCount:       int64(cred.Authenticator.SignCount),
		BackupEligible:  cred.Flags.BackupEligible,
		BackupState:     cred.Flags.BackupState,
		AAGUID:          cred.Authenticator.AAGUID,
	})
	if err != nil {
		if errors.Is(err, platform.ErrConflict) {
			writeProblem(w, http.StatusConflict, "this passkey is already registered", r.URL.Path)
			return
		}
		h.cfg.Logger.Error("store passkey", "err", err, "user_id", sc.User.ID)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	h.clearPasskeyCeremonyCookie(w)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(passkeyDTOFrom(row))
}

// ─── Login (anonymous) ────────────────────────────────────────────

// HandlePasskeyBeginLogin returns discoverable-assertion options for
// navigator.credentials.get(). Usernameless: no email is asked for or
// revealed — the authenticator picks the account, so this endpoint
// leaks nothing an anonymous caller didn't already have.
func (h *Handlers) HandlePasskeyBeginLogin(w http.ResponseWriter, r *http.Request) {
	wa, err := h.webAuthn()
	if err != nil {
		h.cfg.Logger.Error("webauthn config", "err", err)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	// User verification REQUIRED, and required HERE is what makes the
	// assertion side check it: the library derives shouldVerifyUser
	// from session.UserVerification == "required" (webauthn/login.go),
	// which is populated from these options. Unset, the field is
	// omitted from the options JSON entirely, the browser falls back
	// to its own default ("preferred"), and the UV bit is never
	// verified — passwordless sign-in degrades to possession of the
	// authenticator alone. audit-2026-08-13.
	assertion, session, err := wa.BeginDiscoverableLogin(
		webauthn.WithUserVerification(protocol.VerificationRequired),
	)
	if err != nil {
		h.cfg.Logger.Error("begin passkey login", "err", err)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	ceremony := passkeyCeremony{Purpose: "login", Session: *session}
	// Reserve BEFORE issuing the challenge — see the matching note in
	// HandlePasskeyBeginRegister. This is what lets finish-login fail
	// closed when the spent-set is evicted instead of re-minting a
	// session for a captured request (W1-auth-passkey-1).
	if err := h.reserveCeremony(r.Context(), ceremony); err != nil {
		h.cfg.Logger.Error("reserve passkey ceremony", "err", err)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	if err := h.setPasskeyCeremonyCookie(w, ceremony); err != nil {
		h.cfg.Logger.Error("set passkey ceremony cookie", "err", err)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(assertion)
}

// HandlePasskeyFinishLogin verifies the assertion and mints the same
// session cookie the email flows issue. Every verification failure —
// unknown credential, bad signature, stale challenge, already-spent
// ceremony, tampered cookie — returns one generic 400: an anonymous
// caller must not be able to probe which credentials exist. The one
// non-400 refusal is an unreachable spent-set (500): that is an ops
// condition, not a fact about the caller's credentials.
func (h *Handlers) HandlePasskeyFinishLogin(w http.ResponseWriter, r *http.Request) {
	ceremony, err := h.readPasskeyCeremonyCookie(r, "login")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "passkey sign-in failed — try again", r.URL.Path)
		return
	}
	// MaxBytesReader, not LimitReader — see the note in
	// HandlePasskeyFinishRegister.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPasskeyBodyBytes))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "request body too large", r.URL.Path)
		return
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(body)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "passkey sign-in failed — try again", r.URL.Path)
		return
	}
	wa, err := h.webAuthn()
	if err != nil {
		h.cfg.Logger.Error("webauthn config", "err", err)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}

	// The handler resolves rawID → stored credential → owning user.
	// It runs inside ValidateDiscoverableLogin, which then verifies
	// the assertion signature against the returned public key. The
	// request context is captured explicitly — the library's handler
	// signature has no ctx parameter.
	ctx := r.Context()
	var (
		matchedUser platform.User
		matchedRow  platform.WebAuthnCredential
	)
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		row, err := h.cfg.Passkeys.GetWebAuthnCredentialByCredentialID(ctx, rawID)
		if err != nil {
			return nil, err
		}
		user, err := h.cfg.Users.GetUserByID(ctx, row.UserID)
		if err != nil {
			return nil, err
		}
		// The authenticator-reported user handle must be the stored
		// owner — a mismatch means a credential re-bound to another
		// handle, which we refuse.
		if !bytes.Equal(userHandle, user.ID[:]) {
			return nil, errors.New("user handle mismatch")
		}
		matchedUser = user
		matchedRow = row
		return webauthnUser{user: user, creds: []webauthn.Credential{toLibCredential(row)}}, nil
	}

	cred, err := wa.ValidateDiscoverableLogin(handler, ceremony.Session, parsed)
	if err != nil {
		h.cfg.Logger.Warn("passkey assertion rejected", "err", err, "ip", clientIP(r).String())
		writeProblem(w, http.StatusBadRequest, "passkey sign-in failed — try again", r.URL.Path)
		return
	}
	if cred.Authenticator.CloneWarning {
		// Sign-count regression: at least two copies of the private
		// key may exist. Refuse the login and leave a loud trail —
		// this is the one WebAuthn signal of credential theft.
		h.cfg.Logger.Error("passkey clone warning — refusing login",
			"user_id", matchedUser.ID, "credential_id", matchedRow.ID, "ip", clientIP(r).String())
		writeProblem(w, http.StatusBadRequest, "passkey sign-in failed — try again", r.URL.Path)
		return
	}
	// Spend the challenge. This is the step that makes a CAPTURED
	// finish-login request (ceremony cookie + assertion body) useless
	// a second time: the assertion still verifies — it is byte-for-
	// byte the one that verified before — but the ceremony behind it
	// is gone. Everything below this line mints or mutates state, so
	// nothing has happened yet when we refuse. A store outage refuses
	// too (500, not the generic 400): we will not issue a session we
	// cannot prove is fresh, and passkey sign-in degrading while
	// email-code sign-in keeps working is the right way to fail.
	if err := h.consumeCeremony(r.Context(), ceremony); err != nil {
		if errors.Is(err, errPasskeyCeremonyReplayed) {
			h.cfg.Logger.Warn("passkey ceremony replay refused",
				"user_id", matchedUser.ID, "credential_id", matchedRow.ID, "ip", clientIP(r).String())
			writeProblem(w, http.StatusBadRequest, "passkey sign-in failed — try again", r.URL.Path)
			return
		}
		h.cfg.Logger.Error("consume passkey ceremony", "err", err, "user_id", matchedUser.ID)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	// Persist the advanced sign counter. Best-effort: the assertion
	// already verified; losing one counter write only weakens the
	// NEXT clone check, it doesn't invalidate this login.
	if err := h.cfg.Passkeys.UpdateWebAuthnCredentialSignCount(
		r.Context(), matchedRow.ID, int64(cred.Authenticator.SignCount), h.cfg.Now()); err != nil {
		h.cfg.Logger.Warn("update passkey sign count", "err", err, "credential_id", matchedRow.ID)
	}

	// A successful passkey login proves the account owner is present —
	// retire the durable email-code failure counter exactly as the
	// email doors do (C3-032). Best-effort.
	if err := h.cfg.Tokens.ClearLoginCodeLockout(r.Context(), matchedUser.Email); err != nil {
		h.cfg.Logger.Warn("clear login code lockout", "err", err, "user_id", matchedUser.ID)
	}

	if err := h.mintSession(w, r, matchedUser); err != nil {
		h.cfg.Logger.Error("start session (passkey)", "err", err, "user_id", matchedUser.ID)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	h.clearPasskeyCeremonyCookie(w)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(verifyCodeResponse{Status: "ok"})
}

// ─── Management (session-gated) ───────────────────────────────────

// passkeyDTO is the wire shape for one registered passkey. No key
// material: credential IDs and public keys stay server-side — the
// list is for display + deletion only.
type passkeyDTO struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Transports     []string `json:"transports,omitempty"`
	BackupEligible bool     `json:"backup_eligible"`
	CreatedAt      string   `json:"created_at"`
	LastUsedAt     string   `json:"last_used_at,omitempty"`
}

func passkeyDTOFrom(c platform.WebAuthnCredential) passkeyDTO {
	dto := passkeyDTO{
		ID:             c.ID.String(),
		Name:           c.Name,
		Transports:     c.Transports,
		BackupEligible: c.BackupEligible,
		CreatedAt:      c.CreatedAt.UTC().Format(time.RFC3339),
	}
	if !c.LastUsedAt.IsZero() {
		dto.LastUsedAt = c.LastUsedAt.UTC().Format(time.RFC3339)
	}
	return dto
}

type passkeyListResponse struct {
	Credentials []passkeyDTO `json:"credentials"`
}

// HandlePasskeyList returns the signed-in user's passkeys.
func (h *Handlers) HandlePasskeyList(w http.ResponseWriter, r *http.Request) {
	sc, ok := SessionFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication required", r.URL.Path)
		return
	}
	rows, err := h.cfg.Passkeys.ListWebAuthnCredentialsForUser(r.Context(), sc.User.ID)
	if err != nil {
		h.cfg.Logger.Error("list passkeys", "err", err, "user_id", sc.User.ID)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	out := passkeyListResponse{Credentials: make([]passkeyDTO, 0, len(rows))}
	for _, row := range rows {
		out.Credentials = append(out.Credentials, passkeyDTOFrom(row))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// HandlePasskeyDelete removes one of the signed-in user's passkeys.
// Owner-scoped in the store query, so another user's credential ID
// is indistinguishable from a nonexistent one (404 either way).
func (h *Handlers) HandlePasskeyDelete(w http.ResponseWriter, r *http.Request) {
	sc, ok := SessionFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication required", r.URL.Path)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid passkey id", r.URL.Path)
		return
	}
	if err := h.cfg.Passkeys.DeleteWebAuthnCredential(r.Context(), id, sc.User.ID); err != nil {
		if errors.Is(err, platform.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "passkey not found", r.URL.Path)
			return
		}
		h.cfg.Logger.Error("delete passkey", "err", err, "user_id", sc.User.ID)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
