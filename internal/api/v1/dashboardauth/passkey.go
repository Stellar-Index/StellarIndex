package dashboardauth

// Passkey (WebAuthn) sign-in for the customer dashboard.
//
//   - begin/finish-register: a signed-in user adds a passkey.
//   - begin/finish-login: an anonymous visitor signs in with a discoverable credential and gets
//     the same session cookie as the email-code flow, via mintSession.
//   - credentials (GET/DELETE): management for the settings page.
//
// Ceremony state lives in a short-lived HMAC-signed cookie, not server storage. The challenge is
// not secret but must be integrity-bound so a caller cannot substitute their own.
//
// Integrity alone is not enough: the ceremony must also expire server-side (the cookie's MaxAge
// is a hint an attacker ignores; see [Handlers.webAuthn]) and be single-use
// (passkey_ceremony_guard.go), else a captured finish-login request is a session-mint oracle.

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
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/notify"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// PasskeyCeremonyCookieName carries the signed WebAuthn ceremony state between begin and finish; named apart from the session and login-
// intent cookies to avoid cross-surface confusion.
const PasskeyCeremonyCookieName = "__Host-stellarindex_passkey_ceremony"

// passkeyCeremonyTTL bounds how long a begin's challenge stays redeemable; 5 minutes tolerates a slow security-key fumble.
const passkeyCeremonyTTL = 5 * time.Minute

// passkeyCeremonyReserveSlack pads the reservation TTL past the ceremony's validity so a reservation vanishes only by eviction
// (which must fail closed), never by TTL expiry under a still-valid challenge.
const passkeyCeremonyReserveSlack = time.Minute

// passkeyBeginLoginMaxPerIP per passkeyBeginLoginWindow caps anonymous begin-login calls per client IP (/64 for IPv6): each begin holds
// a reservation in the shared allkeys-lru Redis, so one IP could otherwise fill it.
const (
	passkeyBeginLoginMaxPerIP = 20
	passkeyBeginLoginWindow   = time.Minute
)

// passkeyCeremonyDomain domain-separates the ceremony-cookie HMAC
// from the login-code HMAC that shares the server secret.
const passkeyCeremonyDomain = "stellarindex/passkey-ceremony/v1|"

// maxPasskeyBodyBytes bounds finish-ceremony request bodies. Real
// attestation/assertion payloads are a few KB; 64 KiB is generous.
const maxPasskeyBodyBytes = 64 << 10

// maxPasskeyNameLen matches the storage CHECK (length(name) <= 100).
const maxPasskeyNameLen = 100

// passkeyCeremony is the signed cookie payload. Purpose prevents a registration challenge being replayed against finish-login.
type passkeyCeremony struct {
	Purpose string               `json:"purpose"` // "register" | "login"
	Session webauthn.SessionData `json:"session"`
}

// webAuthn lazily builds the library handle from DashboardBaseURL, per request (cheap), so a Handlers without Passkeys never pays for it.
func (h *Handlers) webAuthn() (*webauthn.WebAuthn, error) {
	u, err := url.Parse(h.cfg.DashboardBaseURL)
	if err != nil || u.Hostname() == "" {
		return nil, errors.New("dashboardauth: DashboardBaseURL is not an absolute URL")
	}
	// Timeouts is load-bearing: go-webauthn stamps SessionData.Expires only when Enforce is true,
	// and Enforce defaults false. With the zero value every Expires guard (ours in
	// [Handlers.readPasskeyCeremonyCookie] and the library's own) is dead code, and a captured
	// cookie replayed by a plain HTTP client is honoured forever.
	// TimeoutUVD is set too for completeness; UV is required everywhere so it is never selected.
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

// webauthnUser adapts a platform.User and its credentials to the library's User. The handle is the user's 16 UUID bytes: stable and opaque.
type webauthnUser struct {
	user  platform.User
	creds []webauthn.Credential
}

func (u webauthnUser) WebAuthnID() []byte { return u.user.ID[:] }

func (u webauthnUser) WebAuthnName() string {
	if platform.IsPlaceholderEmail(u.user.Email) {
		return u.WebAuthnDisplayName()
	}
	return u.user.Email
}

func (u webauthnUser) WebAuthnDisplayName() string {
	if u.user.DisplayName != "" || platform.IsPlaceholderEmail(u.user.Email) {
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
	mac := hmac.New(sha256.New, mustPurposeKey(secret, passkeyCeremonyDomain))
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
	cookie := credentialCookie(PasskeyCeremonyCookieName, value) //nolint:gosec // G124: credentialCookie sets Secure, HttpOnly and SameSite=Lax
	cookie.MaxAge = int(passkeyCeremonyTTL / time.Second)
	http.SetCookie(w, cookie)
	return nil
}

// readPasskeyCeremonyCookie verifies signature and expiry and returns the ceremony for wantPurpose. Any failure is one generic error
// so a tamperer learns nothing about which check tripped.
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
	// A missing expiry is refused, not waved through: a config regression (see
	// [Handlers.webAuthn]) must lock passkeys out rather than quietly issue eternal challenges.
	if ceremony.Session.Expires.IsZero() || !ceremony.Session.Expires.After(h.cfg.Now()) {
		return passkeyCeremony{}, errInvalid
	}
	return ceremony, nil
}

// errPasskeyCeremonyReplayed marks an already-spent ceremony, distinct from a store outage so callers log them differently
// (replay is an attack signal, outage an ops signal) while returning the same generic body.
var errPasskeyCeremonyReplayed = errors.New("dashboardauth: passkey ceremony already used")

// passkeyCeremonyDigest is the single-use guard's key: purpose + challenge, plain SHA-256. Not the cookie's HMAC, because keying it
// would tie the spent-set to a secret that can rotate independently of the challenges it must outlive.
func passkeyCeremonyDigest(c passkeyCeremony) string {
	sum := sha256.Sum256([]byte(passkeyCeremonyDomain + c.Purpose + "|" + c.Session.Challenge))
	return hex.EncodeToString(sum[:])
}

// passkeyCeremonyReserveGuard is the optional capability of a guard whose spent-set can evict (Redis allkeys-lru), where "marker absent
// == fresh" would re-open replay. It reserves at begin and claims at finish only if reserved, so eviction fails closed.
type passkeyCeremonyReserveGuard interface {
	Reserve(ctx context.Context, digest string, ttl time.Duration) error
	ClaimReserved(ctx context.Context, digest string) (bool, error)
}

// reserveCeremony records a begin ceremony as live when the guard's spent-set is evictable (see [passkeyCeremonyReserveGuard]); no-op
// otherwise. Runs before the challenge reaches the browser, so no challenge is issued without a reservation.
func (h *Handlers) reserveCeremony(ctx context.Context, c passkeyCeremony) error {
	guard, ok := h.cfg.PasskeyCeremonyGuard.(passkeyCeremonyReserveGuard)
	if !ok {
		return nil
	}
	return guard.Reserve(ctx, passkeyCeremonyDigest(c), passkeyCeremonyTTL+passkeyCeremonyReserveSlack)
}

// consumeCeremony spends a ceremony so it can never be presented again; returns [errPasskeyCeremonyReplayed] on replay and a wrapped error
// if the store is unreachable (callers refuse both). Called after verification, so unauthenticated callers cannot spend store slots.
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
	c := credentialCookie(PasskeyCeremonyCookieName, "") //nolint:gosec // G124: credentialCookie sets Secure, HttpOnly and SameSite=Lax
	c.MaxAge = -1
	http.SetCookie(w, c)
}

// ─── Registration (session-gated) ─────────────────────────────────

// passkeyRegistrationOptions are the creation options every passkey
// enrolment uses, signed-in or signup.
func passkeyRegistrationOptions(exclusions []protocol.CredentialDescriptor) []webauthn.RegistrationOption {
	return []webauthn.RegistrationOption{
		// User verification REQUIRED: this is a passwordless first-factor credential, so without
		// UV possession of the authenticator is the account. Accepted trade-off: a security key
		// with no PIN cannot be enrolled. Login requires the UV bit on every assertion too.
		//
		// ORDER MATTERS. WithAuthenticatorSelection replaces the whole struct while
		// WithResidentKeyRequirement sets only two fields, so this must come first or UV is
		// silently dropped. The options-shape assertions in passkey_test.go pin both fields.
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			UserVerification: protocol.VerificationRequired,
		}),
		// Discoverable (resident) so the login side can be
		// usernameless.
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithExclusions(exclusions),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation),
	}
}

// HandlePasskeyBeginRegister returns credential-creation options for navigator.credentials.create(); session-gated, since adding a
// passkey needs an authenticated user.
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
	creation, session, err := wa.BeginRegistration(user, passkeyRegistrationOptions(exclusions)...)
	if err != nil {
		h.cfg.Logger.Error("begin passkey registration", "err", err, "user_id", sc.User.ID)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	ceremony := passkeyCeremony{Purpose: "register", Session: *session}
	// Reserve the ceremony BEFORE handing the challenge to the browser
	// so its single-use claim survives an allkeys-lru eviction of the
	// spent-set. A store outage here fails closed:
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

// passkeyDisplayName normalises the label to what the storage CHECK (`length(name) <= 100`, migration 0140) accepts. Truncates by runes:
// a mid-rune byte cut is invalid UTF-8 Postgres refuses, a 500 after the authenticator already burned a credential slot.
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

// HandlePasskeyFinishRegister verifies the attestation against the ceremony cookie's challenge and stores the credential.
func (h *Handlers) HandlePasskeyFinishRegister(w http.ResponseWriter, r *http.Request) {
	sc, ok := SessionFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication required", r.URL.Path)
		return
	}
	// MaxBytesReader, not LimitReader: LimitReader truncates silently at its cap, which would make
	// the "body too large" branch unreachable.
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
	// A new sign-in method invalidates the existing sessions: any other
	// holder of this user's cookie must re-authenticate. The credential is
	// already committed, so a failed revoke is logged, not a 500.
	if err := h.cfg.Users.RevokeOtherUserSessions(r.Context(), sc.User.ID, sc.Session.ID); err != nil {
		h.cfg.Logger.Error("revoke other sessions after passkey registration", "err", err, "user_id", sc.User.ID)
	}
	h.recordPasskeyRegistered(r, sc, row)
	h.notifyPasskeyChanged(r, sc, notify.PasskeyAdded)
	h.clearPasskeyCeremonyCookie(w)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(passkeyDTOFrom(row))
}

// ─── Login (anonymous) ────────────────────────────────────────────

// HandlePasskeyBeginLogin returns discoverable-assertion options; usernameless, so it reveals nothing an anonymous caller lacked.
func (h *Handlers) HandlePasskeyBeginLogin(w http.ResponseWriter, r *http.Request) {
	if !h.allowPasskeyBeginLogin(w, r) {
		return
	}
	wa, err := h.webAuthn()
	if err != nil {
		h.cfg.Logger.Error("webauthn config", "err", err)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	// User verification REQUIRED here is what makes the assertion side check the UV bit: the
	// library derives that from these options. Unset, the browser defaults to "preferred" and UV
	// is never verified, degrading sign-in to possession of the authenticator alone.
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
	// session for a captured request.
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

// allowPasskeyBeginLogin enforces the per-IP begin-login cap and writes the refusal itself. Runs before any ceremony is minted,
// so a throttled call writes nothing to the guard's store.
func (h *Handlers) allowPasskeyBeginLogin(w http.ResponseWriter, r *http.Request) bool {
	limiter := h.cfg.passkeyBeginLimiter
	if limiter == nil {
		// Only reachable by bypassing NewHandlers; refuse rather than run uncapped.
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return false
	}
	if limiter.Allow(middleware.RemoteIPThrottleKey(r), passkeyBeginLoginMaxPerIP) {
		return true
	}
	h.cfg.Logger.Warn("passkey begin-login throttled", "ip", clientIP(r).String())
	w.Header().Set("Retry-After", strconv.Itoa(int(passkeyBeginLoginWindow/time.Second)))
	writeProblem(w, http.StatusTooManyRequests, "too many passkey sign-in attempts; retry later", r.URL.Path)
	return false
}

// HandlePasskeyFinishLogin verifies the assertion and mints a session cookie; every verification failure is one generic 400 so callers
// cannot probe which credentials exist.
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

	// Resolves rawID to stored credential and owner; ValidateDiscoverableLogin then verifies the
	// signature against that key. The library's handler has no ctx parameter, so capture it here.
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
		h.refusePasskeyCloneWarning(w, r, matchedUser, matchedRow, parsed.Response.AuthenticatorData.Counter)
		return
	}
	// Spend the challenge: a captured finish-login request still verifies byte-for-byte, so only
	// the spent ceremony makes it useless a second time. Everything below mints or mutates state.
	// A store outage refuses too (500): no session we cannot prove fresh, and passkey sign-in
	// degrading while email-code sign-in keeps working is the right way to fail.
	if err := h.consumeCeremony(r.Context(), ceremony); err != nil {
		if errors.Is(err, errPasskeyCeremonyReplayed) {
			h.refusePasskeyReplay(w, r, matchedUser, matchedRow, parsed.Response.AuthenticatorData.Counter)
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
	// email doors do. Best-effort.
	// The lockouts are keyed by the canonical address; users.email may not be.
	if email, err := notify.CanonicalRecipient(matchedUser.Email); err == nil {
		h.clearLoginCodeLockouts(r, email)
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

// refusePasskeyCloneWarning refuses a login whose authenticator reported a sign-count regression (the key may be copied).
// Split out of [Handlers.HandlePasskeyFinishLogin] for funlen.
func (h *Handlers) refusePasskeyCloneWarning(
	w http.ResponseWriter, r *http.Request, user platform.User, cred platform.WebAuthnCredential, signCount uint32,
) {
	h.cfg.Logger.Error("passkey clone warning — refusing login",
		"user_id", user.ID, "credential_id", cred.ID, "ip", clientIP(r).String())
	h.recordPasskeyLoginRefusal(r, AuditActionPasskeyCloneWarning, obs.PasskeyRefusalCloneWarning,
		user, cred, signCount)
	writeProblem(w, http.StatusBadRequest, "passkey sign-in failed — try again", r.URL.Path)
}

// refusePasskeyReplay refuses a finish-login whose ceremony was already consumed (a replayed capture).
// Split out of [Handlers.HandlePasskeyFinishLogin] for funlen.
func (h *Handlers) refusePasskeyReplay(
	w http.ResponseWriter, r *http.Request, user platform.User, cred platform.WebAuthnCredential, signCount uint32,
) {
	h.cfg.Logger.Warn("passkey ceremony replay refused",
		"user_id", user.ID, "credential_id", cred.ID, "ip", clientIP(r).String())
	h.recordPasskeyLoginRefusal(r, AuditActionPasskeyLoginReplay, obs.PasskeyRefusalCeremonyReplay,
		user, cred, signCount)
	writeProblem(w, http.StatusBadRequest, "passkey sign-in failed — try again", r.URL.Path)
}

// ─── Management (session-gated) ───────────────────────────────────

// passkeyDTO is the wire shape for one registered passkey. No key material: credential IDs and public keys stay server-side.
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

// HandlePasskeyDelete removes one of the signed-in user's passkeys; owner-scoped in the store query, so another user's credential ID is
// a 404 like a nonexistent one.
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
	RecordSessionCredentialEvent(r, h.cfg.Audit, h.cfg.Logger, h.cfg.Now(), sc, CredentialEvent{
		Action:     AuditActionPasskeyDelete,
		TargetKind: auditTargetPasskey,
		TargetID:   id.String(),
	})
	h.notifyPasskeyChanged(r, sc, notify.PasskeyRemoved)
	w.WriteHeader(http.StatusNoContent)
}
