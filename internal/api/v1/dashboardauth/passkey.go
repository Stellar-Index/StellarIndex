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
// RP identity derives from DashboardBaseURL — the origin the browser
// performs the ceremony on (https://stellarindex.io): RP ID is its
// hostname, allowed origin is its scheme://host. That mirrors how the
// magic-link callback URL is already built from the same config.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

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
	return webauthn.New(&webauthn.Config{
		RPID:          u.Hostname(),
		RPDisplayName: "Stellar Index",
		RPOrigins:     []string{u.Scheme + "://" + u.Host},
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
	if !ceremony.Session.Expires.IsZero() && !ceremony.Session.Expires.After(h.cfg.Now()) {
		return passkeyCeremony{}, errInvalid
	}
	return ceremony, nil
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
		// Discoverable (resident) so the login side can be
		// usernameless; UV preferred so platform authenticators
		// prompt biometrics without hard-failing security keys.
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithExclusions(exclusions),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation),
	)
	if err != nil {
		h.cfg.Logger.Error("begin passkey registration", "err", err, "user_id", sc.User.ID)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	if err := h.setPasskeyCeremonyCookie(w, passkeyCeremony{Purpose: "register", Session: *session}); err != nil {
		h.cfg.Logger.Error("set passkey ceremony cookie", "err", err)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(creation)
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
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPasskeyBodyBytes))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "body too large", r.URL.Path)
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

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "Passkey"
	}
	if len(name) > maxPasskeyNameLen {
		name = name[:maxPasskeyNameLen]
	}
	transports := make([]string, 0, len(cred.Transport))
	for _, t := range cred.Transport {
		transports = append(transports, string(t))
	}
	row, err := h.cfg.Passkeys.CreateWebAuthnCredential(r.Context(), platform.WebAuthnCredential{
		UserID:          sc.User.ID,
		Name:            name,
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
	assertion, session, err := wa.BeginDiscoverableLogin()
	if err != nil {
		h.cfg.Logger.Error("begin passkey login", "err", err)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	if err := h.setPasskeyCeremonyCookie(w, passkeyCeremony{Purpose: "login", Session: *session}); err != nil {
		h.cfg.Logger.Error("set passkey ceremony cookie", "err", err)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(assertion)
}

// HandlePasskeyFinishLogin verifies the assertion and mints the same
// session cookie the email flows issue. Every verification failure —
// unknown credential, bad signature, stale challenge, tampered
// cookie — returns one generic 400: an anonymous caller must not be
// able to probe which credentials exist.
func (h *Handlers) HandlePasskeyFinishLogin(w http.ResponseWriter, r *http.Request) {
	ceremony, err := h.readPasskeyCeremonyCookie(r, "login")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "passkey sign-in failed — try again", r.URL.Path)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPasskeyBodyBytes))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "body too large", r.URL.Path)
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
