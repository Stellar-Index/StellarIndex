package dashboardauth

// Email-less account creation: a visitor with no session registers a passkey and gets an account
// whose only credential is that passkey.
//
// The user ID is minted at begin and bound into the credential as its WebAuthn user handle, so the
// row is created at finish under that same ID (finish-login matches the handle against users.id).
// Nothing is written before the attestation verifies and the ceremony is spent.
//
// Accepted trade-off: with no email there is no recovery path; losing every copy of the passkey
// loses the account.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// passkeySignupMaxPerIP per passkeySignupWindow caps account-creating calls per client IP (/64 for IPv6) per instance on each
// sign-up route separately (see [Handlers.signupIPAllowed]).
const (
	passkeySignupMaxPerIP = 10
	passkeySignupWindow   = time.Hour
)

const (
	passkeySignupPurpose     = "signup"
	passkeySignupDisplayName = "Passkey account"
)

// HandlePasskeyBeginSignup returns creation options for a brand-new, email-less user; the ceremony cookie carries the user ID chosen here.
func (h *Handlers) HandlePasskeyBeginSignup(w http.ResponseWriter, r *http.Request) {
	if !h.allowPasskeySignup(w, r) {
		return
	}
	wa, err := h.webAuthn()
	if err != nil {
		h.cfg.Logger.Error("webauthn config", "err", err)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	id := uuid.New()
	user := webauthnUser{user: platform.User{
		ID: id, Email: platform.PlaceholderEmail(id), DisplayName: passkeySignupDisplayName,
	}}
	creation, session, err := wa.BeginRegistration(user, passkeyRegistrationOptions(nil)...)
	if err != nil {
		h.cfg.Logger.Error("begin passkey signup", "err", err)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	ceremony := passkeyCeremony{Purpose: passkeySignupPurpose, Session: *session}
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
	_ = json.NewEncoder(w).Encode(creation)
}

func (h *Handlers) allowPasskeySignup(w http.ResponseWriter, r *http.Request) bool {
	if !h.signupEnabled(w, r) || !h.signupIPAllowed(w, r, "passkey", "too many passkey sign-ups; retry later") {
		return false
	}
	// Begin creates nothing, so it spends no budget unit: finish spends one, immediately before the account it creates.
	return true
}

// signupIPAllowed charges one account-creating attempt to the client IP on route's own per-IP cap, so no single address can
// drain an all-caller budget and free passkey begins cannot refuse an email-proven sign-up; false means the 429 is written.
// Call it before spending a budget unit.
func (h *Handlers) signupIPAllowed(w http.ResponseWriter, r *http.Request, route, detail string) bool {
	limiter := h.cfg.passkeySignupLimiter
	if limiter == nil {
		// Only reachable by bypassing NewHandlers; refuse rather than run uncapped.
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return false
	}
	if !limiter.Allow(route+":"+middleware.RemoteIPThrottleKey(r), passkeySignupMaxPerIP) {
		h.cfg.Logger.Warn("sign-up throttled", "route", route, "ip", clientIP(r).String())
		w.Header().Set("Retry-After", strconv.Itoa(int(passkeySignupWindow/time.Second)))
		writeProblem(w, http.StatusTooManyRequests, detail, r.URL.Path)
		return false
	}
	return true
}

// signupEnabled answers 503 when the deployment has switched anonymous registration off; false means it is written.
func (h *Handlers) signupEnabled(w http.ResponseWriter, r *http.Request) bool {
	if !h.cfg.SignupDisabled {
		return true
	}
	obs.RegistrationsTotal.WithLabelValues(obs.RegistrationDisabled, obs.RegistrationPoolAll).Inc()
	writeProblem(w, http.StatusServiceUnavailable, "sign-up is switched off on this deployment", r.URL.Path)
	return false
}

// signupBudgetOK spends one unit of the all-caller hourly budget for an account about to be created, failing as /v1/register's
// does: closed once the store has been down past its dwell time, open on a transient error. A spent unit is not refunded when
// creation then fails, as /v1/register does not.
func (h *Handlers) signupBudgetOK(w http.ResponseWriter, r *http.Request, budget *auth.RegistrationBudget, pool string) bool {
	err := budget.Spend(r.Context())
	switch {
	case err == nil:
		return true
	case errors.Is(err, auth.ErrSignupRateLimited):
		obs.RegistrationsTotal.WithLabelValues(obs.RegistrationBudgetExhausted, pool).Inc()
		w.Header().Set("Retry-After", strconv.FormatInt(3600-h.cfg.Now().Unix()%3600, 10))
		writeProblem(w, http.StatusServiceUnavailable, "sign-up is at its hourly capacity on this deployment; retry later", r.URL.Path)
		return false
	case errors.Is(err, auth.ErrThrottleUnavailable):
		h.cfg.Logger.Warn("registration budget unavailable; failing closed (sustained Redis errors)", "err", err)
		obs.RateLimitFailClosedTotal.WithLabelValues(obs.RateLimiterRegisterBudget).Inc()
		w.Header().Set("Retry-After", "30")
		writeProblem(w, http.StatusServiceUnavailable, "sign-up is temporarily unavailable; retry in a moment", r.URL.Path)
		return false
	}
	h.cfg.Logger.Warn("registration budget check failed; falling open", "err", err)
	return true
}

// HandlePasskeyFinishSignup verifies the attestation, creates the account, owner user and credential and mints a session; every
// verification failure is one generic 400.
func (h *Handlers) HandlePasskeyFinishSignup(w http.ResponseWriter, r *http.Request) {
	if !h.signupEnabled(w, r) {
		return
	}
	fail := func() {
		writeProblem(w, http.StatusBadRequest, "passkey sign-up failed — start again", r.URL.Path)
	}
	ceremony, err := h.readPasskeyCeremonyCookie(r, passkeySignupPurpose)
	if err != nil {
		fail()
		return
	}
	userID, err := uuid.FromBytes(ceremony.Session.UserID)
	if err != nil {
		fail()
		return
	}
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
	parsed, err := protocol.ParseCredentialCreationResponseBytes(req.Credential)
	if err != nil {
		fail()
		return
	}
	wa, err := h.webAuthn()
	if err != nil {
		h.cfg.Logger.Error("webauthn config", "err", err)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	cred, err := wa.CreateCredential(webauthnUser{user: platform.User{ID: userID}}, ceremony.Session, parsed)
	if err != nil {
		h.cfg.Logger.Warn("passkey signup attestation rejected", "err", err, "ip", clientIP(r).String())
		fail()
		return
	}
	// Spend the challenge before anything is created, so a replayed
	// request cannot mint a second account.
	if err := h.consumeCeremony(r.Context(), ceremony); err != nil {
		if errors.Is(err, errPasskeyCeremonyReplayed) {
			h.cfg.Logger.Warn("passkey signup ceremony replay refused", "ip", clientIP(r).String())
			fail()
			return
		}
		h.cfg.Logger.Error("consume passkey ceremony", "err", err)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	if !h.signupBudgetOK(w, r, h.cfg.SignupBudget, obs.RegistrationPoolProofFree) {
		return
	}
	user, row, err := h.createPasskeyAccount(r, userID, req.Name, cred)
	if err != nil {
		if errors.Is(err, platform.ErrConflict) {
			fail()
			return
		}
		h.cfg.Logger.Error("passkey signup provisioning", "err", err, "user_id", userID)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	if err := h.mintSession(w, r, user, SignInMethodPasskeySignup); err != nil {
		h.cfg.Logger.Error("start session (passkey signup)", "err", err, "user_id", user.ID)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	h.recordPasskeySignup(r, user, row)
	h.clearPasskeyCeremonyCookie(w)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(verifyCodeResponse{Status: "ok"})
}

// createPasskeyAccount inserts the account, its owner user (under the ID the credential was bound to) and the credential.
// A failure after the account exists suspends it, so no half-built account stays active.
func (h *Handlers) createPasskeyAccount(
	r *http.Request, userID uuid.UUID, name string, cred *webauthn.Credential,
) (platform.User, platform.WebAuthnCredential, error) {
	ctx := r.Context()
	short := userID.String()[:8]
	acct, err := h.cfg.Accounts.Create(ctx, platform.Account{
		Name:   passkeySignupDisplayName + " " + short,
		Slug:   "pk-" + short + "-" + userID.String()[9:13],
		Tier:   platform.TierFree,
		Status: platform.AccountActive,
	})
	if err != nil {
		return platform.User{}, platform.WebAuthnCredential{}, fmt.Errorf("create account: %w", err)
	}
	abandon := func(cause error) (platform.User, platform.WebAuthnCredential, error) {
		if sErr := h.cfg.Accounts.Suspend(ctx, acct.ID, "signup-failed: passkey provisioning incomplete"); sErr != nil {
			h.cfg.Logger.Warn("passkey signup: failed to suspend partial account", "err", sErr, "account_id", acct.ID)
		}
		return platform.User{}, platform.WebAuthnCredential{}, cause
	}
	user, err := h.cfg.Users.CreateUser(ctx, platform.User{
		ID:          userID,
		AccountID:   acct.ID,
		Email:       platform.PlaceholderEmail(userID),
		DisplayName: passkeySignupDisplayName,
		Role:        platform.RoleOwner,
	})
	if err != nil {
		return abandon(fmt.Errorf("create user: %w", err))
	}
	transports := make([]string, 0, len(cred.Transport))
	for _, t := range cred.Transport {
		transports = append(transports, string(t))
	}
	row, err := h.cfg.Passkeys.CreateWebAuthnCredential(ctx, platform.WebAuthnCredential{
		UserID:          user.ID,
		Name:            passkeyDisplayName(name),
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
		return abandon(fmt.Errorf("store passkey: %w", err))
	}
	obs.RegistrationsTotal.WithLabelValues(obs.RegistrationCreated, obs.RegistrationPoolProofFree).Inc()
	return user, row, nil
}

func (h *Handlers) recordPasskeySignup(r *http.Request, user platform.User, row platform.WebAuthnCredential) {
	appendCredentialAudit(r, h.cfg.Audit, h.cfg.Logger, platform.AuditEntry{
		AccountID:   user.AccountID,
		ActorUserID: user.ID,
		ActorKind:   platform.ActorUser,
		Action:      AuditActionPasskeyRegister,
		TargetKind:  auditTargetPasskey,
		TargetID:    row.ID.String(),
		Timestamp:   h.cfg.Now(),
	}, map[string]any{"signup": true, "backup_eligible": row.BackupEligible})
}
