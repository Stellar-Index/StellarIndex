// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/notify"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/signupreaper"
)

// RegisterAccountCreator is the narrow create-only account boundary POST /v1/register needs, so the register path cannot grow admin affordances.
type RegisterAccountCreator interface {
	Create(ctx context.Context, a platform.Account) (platform.Account, error)
	// Suspend quarantines an account behind a machine-matchable suspended_reason. Register marks an
	// orphan (first credential never reached the validator store) as a `signup-race:` suspension
	// so the signupreaper reclaims it instead of leaving an active account that can never
	// authenticate.
	Suspend(ctx context.Context, id uuid.UUID, reason string) error
}

// registerRequest is the POST /v1/register body; both fields are optional (a bare `{}` or empty body registers). Email is CONTACT-ONLY:
// no verification, duplicate gate or credential delivery.
type registerRequest struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

// RegisterLimits is the free-tier limits block on the /v1/register reply, so an agent can self-configure its client.
type RegisterLimits struct {
	RateLimitPerMin int   `json:"rate_limit_per_min"`
	MonthlyQuota    int64 `json:"monthly_quota"`
	MaxActiveKeys   int   `json:"max_active_keys"`
	MaxWebhooks     int   `json:"max_webhooks"`
	MaxPriceAlerts  int   `json:"max_price_alerts"`
}

// RegisterResult is the /v1/register reply; the plaintext key appears exactly once, and callers that drop it can simply register again.
type RegisterResult struct {
	AccountID string         `json:"account_id"`
	APIKey    string         `json:"api_key"` // plaintext bearer token; shown ONCE
	KeyID     string         `json:"key_id"`
	KeyPrefix string         `json:"key_prefix"`
	Tier      string         `json:"tier"`
	Limits    RegisterLimits `json:"limits"`
}

// registerBodyMaxBytes caps the request body at 4 KiB, matching
// /v1/signup — name + email + JSON wrapper fits in a fraction of it.
const registerBodyMaxBytes = 4 * 1024

// handleRegister serves POST /v1/register: public, anonymous-tier agent onboarding (one curl, no body needed) that creates a free-tier
// accounts row, promotable via
// PATCH /v1/admin/accounts/{id}, plus a Postgres-backed first API key. Unlike email-keyed /v1/signup (a bare Redis credential).
// Abuse posture: per-IP volume shares the [SignupIPThrottle] budget with /v1/signup; Content-Type application/json is REQUIRED
// (requireJSONContentType) because a header-less
// POST is a CORS simple request no preflight catches (a body-less POST is fine); an account whose key is never used is erased once its
// validator record expires ([accounterasure.AbandonedRegistrationRetention]).
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	req, ok := s.parseAndValidateRegister(w, r)
	if !ok {
		return
	}

	if !s.signupIPThrottleOK(w, r) {
		return
	}

	acct, ok := s.createRegisteredAccount(w, r, req)
	if !ok {
		return
	}

	plaintext, rec, err := s.mintRegisterKey(r.Context(), acct)
	if err != nil {
		// The account row exists but its first credential never reached the validator store, so
		// quarantine the orphan as `signup-race:` for the signupreaper to reclaim. This runs for
		// EVERY mint failure, including a client disconnect: the row is already durable, so a
		// canceled request context must not skip cleanup of state that outlives it.
		s.logger.Error("register: key mint failed after account create (orphan account)",
			"err", err, "account_id", acct.ID)
		s.suspendRegisterOrphan(r.Context(), acct.ID, err)
		if clientAborted(r, err) {
			return
		}
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError,
			"registration failed while minting the API key; try again in a moment")
		return
	}

	tier := acct.Tier.Canonical()
	writeJSON(w, RegisterResult{
		AccountID: acct.ID.String(),
		APIKey:    plaintext,
		KeyID:     rec.ID,
		KeyPrefix: rec.KeyPrefix,
		Tier:      string(tier),
		Limits: RegisterLimits{
			RateLimitPerMin: rec.RateLimitPerMin,
			MonthlyQuota:    rec.MonthlyQuota,
			MaxActiveKeys:   tier.MaxActiveKeys(),
			MaxWebhooks:     tier.MaxWebhooks(),
			MaxPriceAlerts:  tier.MaxPriceAlerts(),
		},
	}, Flags{})
}

// createRegisteredAccount inserts the free-tier account row, retrying once on a slug collision (mirrors dashboardauth.signupNewUser).
// ok=false means the response has been written.
func (s *Server) createRegisteredAccount(
	w http.ResponseWriter, r *http.Request, req registerRequest,
) (platform.Account, bool) {
	for attempt := 0; attempt < 2; attempt++ {
		slug, err := registerSlug()
		if err != nil {
			s.logger.Error("register: slug entropy read failed", "err", err)
			break
		}
		acct, err := s.RegisterAccounts.Create(r.Context(), platform.Account{
			Name:         req.Name,
			Slug:         slug,
			BillingEmail: req.Email,
			Tier:         platform.TierFree,
			Status:       platform.AccountActive,
		})
		if err == nil {
			return acct, true
		}
		if errors.Is(err, context.Canceled) {
			return platform.Account{}, false
		}
		if errors.Is(err, platform.ErrConflict) {
			continue
		}
		s.logger.Error("register: account create failed", "err", err)
		break
	}
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/internal",
		"Internal error", http.StatusInternalServerError,
		"registration failed; try again in a moment")
	return platform.Account{}, false
}

// mintRegisterKey mints the account's first API key at the free-tier budgets. The monthly quota is persisted EXPLICITLY at the tier
// ceiling, not the 0 inherit sentinel,
// so the key is metered from birth (as in clampMonthlyQuotaToAccount).
func (s *Server) mintRegisterKey(ctx context.Context, acct platform.Account) (string, platform.APIKey, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", platform.APIKey{}, fmt.Errorf("read key entropy: %w", err)
	}
	// `sip_<64hex>` — same shape the dashboard mints; auth hashes the
	// full plaintext, the prefix is display/identification only.
	plaintext := "sip_" + hex.EncodeToString(buf[:])
	hash := sha256.Sum256([]byte(plaintext))

	var idBuf [12]byte
	if _, err := rand.Read(idBuf[:]); err != nil {
		return "", platform.APIKey{}, fmt.Errorf("read key-id entropy: %w", err)
	}

	tier := acct.Tier.Canonical()
	rec := platform.APIKey{
		ID:              "kid_" + hex.EncodeToString(idBuf[:]),
		AccountID:       acct.ID,
		Name:            "registration key",
		KeyHash:         hash[:],
		KeyPrefix:       plaintext[:12],
		Tier:            platform.APIKeyTierAPIKey,
		RateLimitPerMin: tier.MaxRateLimitPerMin(),
		MonthlyQuota:    tier.MaxMonthlyQuota(),
		Permissions:     platform.KeyPermissions{All: true},
	}
	// Mirror the credential into the Redis validator store FIRST, before the durable Postgres row.
	// r1 runs the Redis validator, so the mirror IS the working credential and the Postgres
	// api_keys row is only the management record (listing, tier clamp, revocation); a Postgres-only
	// key 401s on first use.
	//
	// Mirror-first matters for orphan reaping: a mirror failure leaves no api_keys row, only the
	// account row that handleRegister suspends for the signupreaper, rather than a permanent pair
	// no reaper matches. The mirror uses the same plaintext so one secret validates on either
	// backend, and its idle TTL re-warms on use (CreateWithSecret) so it cannot grow the
	// allkeys-lru keyspace without bound.
	if s.APIKeyBudgets.RedisMirror != nil {
		mirrored, err := auth.APIKeyRecordFromPlatform(rec, auth.AccountIdentifier(acct.Slug))
		if err != nil {
			return "", platform.APIKey{}, fmt.Errorf("mirror api key to validator store: %w", err)
		}
		if err := s.APIKeyBudgets.RedisMirror.CreateWithSecret(ctx, auth.MirroredKey{
			Plaintext: plaintext,
			Record:    mirrored,
		}); err != nil {
			// No management row committed yet — refuse rather than hand back
			// a key that silently fails, and leave no durable orphan.
			return "", platform.APIKey{}, fmt.Errorf("mirror api key to validator store: %w", err)
		}
	}

	out, err := s.APIKeyBudgets.Platform.Create(ctx, rec, tier.MaxActiveKeys())
	if err != nil {
		// The credential is live in the validator store but its management row failed to commit.
		// Roll the mirror back so we never emit an unlistable, unrevokable key. Best-effort: the
		// account is suspended and reaped and the mirror has an idle TTL, so a failed rollback
		// self-heals.
		//
		// Detached from ctx: a canceled or expired request context is the common trigger (the
		// Create above just failed on it), so rolling back on the same dead context would also
		// fail and leave a live credential for the mirror's full idle TTL.
		if s.APIKeyBudgets.RedisMirror != nil {
			rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if rbErr := s.APIKeyBudgets.RedisMirror.RevokeKeyByID(rbCtx, auth.AccountIdentifier(acct.Slug), rec.ID); rbErr != nil && !errors.Is(rbErr, auth.ErrKeyNotFound) {
				s.logger.Error("register: mirror rollback after management-row create failed",
					"err", rbErr, "account_id", acct.ID, "key_id", rec.ID)
			}
			cancel()
		}
		return "", platform.APIKey{}, fmt.Errorf("create api key: %w", err)
	}
	return plaintext, out, nil
}

// suspendRegisterOrphan quarantines the account left when key minting failed after the account row committed. The `signup-race:`
// suspended_reason is the exact prefix
// signupreaper.ReapSuspendedOrphans matches; a register orphan has no child user or api_keys child, so it satisfies the reaper's
// predicate.
// Best-effort: it runs on a detached short-deadline context so a request whose deadline the failing write consumed can still quarantine,
// and a failure never changes the 500.
func (s *Server) suspendRegisterOrphan(ctx context.Context, accountID uuid.UUID, cause error) {
	if s.RegisterAccounts == nil {
		return
	}
	reason := signupreaper.SignupRaceReasonPrefix + " register orphan: first credential never reached the validator store"
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.RegisterAccounts.Suspend(sctx, accountID, reason); err != nil {
		s.logger.Error("register: failed to suspend orphan account for reaper reclaim",
			"err", err, "account_id", accountID, "orphan_cause", cause)
		return
	}
	s.logger.Warn("register: suspended orphan account for signup-reaper reclaim",
		"account_id", accountID, "orphan_cause", cause)
}

// registerSlug returns `reg-<12hex>`, a URL-safe machine-generated handle with 48 bits of entropy (no vanity handles on this path).
func registerSlug() (string, error) {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("read entropy: %w", err)
	}
	return "reg-" + hex.EncodeToString(buf[:]), nil
}

// parseAndValidateRegister runs the auth/store/body gates, returning the normalised request; on failure the response is already written.
// Mirrors parseAndValidateSignup minus the email-required leg.
func (s *Server) parseAndValidateRegister(w http.ResponseWriter, r *http.Request) (registerRequest, bool) {
	if subject, ok := auth.SubjectFrom(r.Context()); ok &&
		subject.Tier != auth.TierAnonymous && subject.Tier != "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/already-authenticated",
			"Already authenticated", http.StatusBadRequest,
			"this endpoint is for first-time registration; authenticated callers should mint additional keys via POST /v1/account/keys")
		return registerRequest{}, false
	}

	if s.RegisterAccounts == nil || s.APIKeyBudgets.Platform == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-store-unavailable",
			"Account store not configured", http.StatusServiceUnavailable,
			"this deployment has no platform AccountStore wired — typically because Postgres is unavailable")
		return registerRequest{}, false
	}

	// CSRF gate, as in /v1/signup: application/json is not a CORS simple-request type, so a
	// cross-site POST carrying it must be preflighted and refused. The header is REQUIRED;
	// requireJSONContentType is shared so the two anonymous write gates cannot drift apart.
	if !requireJSONContentType(w, r, "/v1/register") {
		return registerRequest{}, false
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, registerBodyMaxBytes))
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/body-too-large",
			"Request body too large", http.StatusBadRequest,
			"/v1/register body must be under 4 KiB")
		return registerRequest{}, false
	}

	var req registerRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-body",
				"Malformed JSON body", http.StatusBadRequest,
				"could not parse request body as JSON")
			return registerRequest{}, false
		}
	}

	req.Name = strings.TrimSpace(req.Name)
	if utf8.RuneCountInString(req.Name) > 128 {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/name-too-long",
			"Name too long", http.StatusBadRequest,
			"name must be 128 characters or fewer")
		return registerRequest{}, false
	}
	if req.Name == "" {
		req.Name = "api-registered account"
	}

	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if req.Email != "" {
		canon, err := notify.CanonicalRecipient(req.Email)
		if err != nil {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-email",
				"Invalid email", http.StatusBadRequest,
				"the email field could not be parsed as a valid address (omit it entirely if you don't want a contact address on file)")
			return registerRequest{}, false
		}
		req.Email = canon
	}

	return req, true
}
