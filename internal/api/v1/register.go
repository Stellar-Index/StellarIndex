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
	"mime"
	"net/http"
	"net/mail"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// RegisterAccountCreator is the narrow platform-account boundary
// POST /v1/register needs: create-only. Backed in production by
// postgresstore.AccountStore (the same concrete store that serves
// [PlatformAccountStore]); narrowed here so the register path can't
// grow admin affordances by accident, mirroring how
// PlatformAccountStore stays Get/Update-only.
type RegisterAccountCreator interface {
	Create(ctx context.Context, a platform.Account) (platform.Account, error)
}

// registerRequest is the inbound POST /v1/register body. Both fields
// are optional — a bare `{}` (or an empty body) registers fine. The
// email, when present, is CONTACT-ONLY: no verification loop, no
// duplicate gate, no credential delivery rides on it.
type registerRequest struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

// RegisterLimits is the free-tier limits block on the /v1/register
// reply — the numbers the fresh account is subject to, so an agent
// can self-configure its client without a second request.
type RegisterLimits struct {
	RateLimitPerMin int   `json:"rate_limit_per_min"`
	MonthlyQuota    int64 `json:"monthly_quota"`
	MaxActiveKeys   int   `json:"max_active_keys"`
	MaxWebhooks     int   `json:"max_webhooks"`
	MaxPriceAlerts  int   `json:"max_price_alerts"`
}

// RegisterResult is the wire shape for /v1/register replies. The
// plaintext key appears here exactly once — callers that drop the
// response can simply register again (registration is free and not
// email-keyed).
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

// handleRegister serves POST /v1/register.
//
// Public, anonymous-tier, and deliberately friction-free: the
// canonical AGENT onboarding path. One curl —
//
//	curl -X POST https://api.stellarindex.io/v1/register
//
// — creates a free-tier platform account and mints its first API
// key. Name and email are both optional (email is contact-only; no
// verification email is sent and nothing is keyed on it). This is
// the platform-account sibling of the older email-keyed /v1/signup
// flow: signup mints a bare Redis credential against an email hash,
// register creates a real accounts row (the thing PATCH
// /v1/admin/accounts/{id} can later promote to partner limits) plus
// a Postgres-backed key attached to it.
//
// Abuse posture:
//   - Per-IP volume: the SAME per-IP signup throttle /v1/signup uses
//     ([SignupIPThrottle], default 5/hour/IP, F-1232) gates every
//     mint, sharing one budget across both endpoints so an abuser
//     can't double-dip. The global anonymous rate limit applies
//     upstream of that.
//   - CSRF/browser relay: when a Content-Type header is present it
//     must be application/json (a non-simple CORS type, so cross-site
//     browser POSTs get preflighted and refused — same reasoning as
//     /v1/signup). A body-less POST is allowed for curl ergonomics;
//     unlike signup there is no email side-effect to relay, and the
//     response is unreadable cross-origin.
//   - No enumeration surface: there is no per-email uniqueness, so
//     there is nothing to probe — every accepted call mints a fresh
//     account.
//
// Stores nil → 503 (deployment without Postgres), same posture as
// the admin account endpoints.
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
		if errors.Is(err, context.Canceled) {
			return
		}
		// The account row exists but its first key doesn't — an
		// orphan. Logged with the account_id so an operator can
		// reconcile (or the caller just registers again); mirroring
		// the signup-race posture of never failing the durable part
		// silently.
		s.logger.Error("register: key mint failed after account create (orphan account)",
			"err", err, "account_id", acct.ID)
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

// createRegisteredAccount inserts the free-tier account row, retrying
// once on a slug collision (12 hex chars of entropy makes that
// astronomically rare; the retry mirrors dashboardauth.signupNewUser).
// ok=false means the response has already been written.
func (s *Server) createRegisteredAccount(
	w http.ResponseWriter, r *http.Request, req registerRequest,
) (platform.Account, bool) {
	for attempt := 0; attempt < 2; attempt++ {
		slug, err := registerSlug()
		if err != nil {
			s.logger.Error("register: slug entropy read failed", "err", err)
			break
		}
		acct, err := s.registerAccounts.Create(r.Context(), platform.Account{
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

// mintRegisterKey mints the account's first API key at the free-tier
// budgets: rate limit at the tier ceiling, monthly quota persisted
// EXPLICITLY at the tier ceiling (not the 0 inherit sentinel) so the
// key is metered from birth — the same posture the dashboard mint
// path takes since "a key minted with no monthly_quota meters at the
// tier ceiling" (clampMonthlyQuotaToAccount).
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
	out, err := s.apiKeyBudgets.Platform.Create(ctx, rec, tier.MaxActiveKeys())
	if err != nil {
		return "", platform.APIKey{}, fmt.Errorf("create api key: %w", err)
	}
	return plaintext, out, nil
}

// registerSlug returns `reg-<12hex>` — a URL-safe handle with 48 bits
// of entropy. Registered-account slugs are machine-generated (there
// is no vanity-handle affordance on this path; the operator can
// rename later through the platform surfaces).
func registerSlug() (string, error) {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("read entropy: %w", err)
	}
	return "reg-" + hex.EncodeToString(buf[:]), nil
}

// parseAndValidateRegister runs the auth/store/body gates. Returns
// the normalised request and ok=true on success; on failure the
// response has already been written. Mirrors parseAndValidateSignup,
// minus the email-required leg (both fields here are optional).
func (s *Server) parseAndValidateRegister(w http.ResponseWriter, r *http.Request) (registerRequest, bool) {
	if subject, ok := auth.SubjectFrom(r.Context()); ok &&
		subject.Tier != auth.TierAnonymous && subject.Tier != "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/already-authenticated",
			"Already authenticated", http.StatusBadRequest,
			"this endpoint is for first-time registration; authenticated callers should mint additional keys via POST /v1/account/keys")
		return registerRequest{}, false
	}

	if s.registerAccounts == nil || s.apiKeyBudgets.Platform == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-store-unavailable",
			"Account store not configured", http.StatusServiceUnavailable,
			"this deployment has no platform AccountStore wired — typically because Postgres is unavailable")
		return registerRequest{}, false
	}

	// CSRF gate, same reasoning as /v1/signup: application/json is not
	// a CORS "simple request" type, so any cross-site browser POST
	// carrying it must be preflighted (and refused). A missing
	// Content-Type is tolerated ONLY alongside an empty body — the
	// bare-curl onboarding case.
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt, _, mtErr := mime.ParseMediaType(ct); mtErr != nil || mt != "application/json" {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/unsupported-media-type",
				"Unsupported media type", http.StatusUnsupportedMediaType,
				"/v1/register requires Content-Type: application/json when a body is sent")
			return registerRequest{}, false
		}
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
	if len(req.Name) > 128 {
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
		addr, err := mail.ParseAddress(req.Email)
		if err != nil {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-email",
				"Invalid email", http.StatusBadRequest,
				"the email field could not be parsed as a valid address (omit it entirely if you don't want a contact address on file)")
			return registerRequest{}, false
		}
		// Keep the parsed addr-spec, not the raw input — same
		// display-name-form normalisation as /v1/signup (cold audit
		// 2026-08-03), even though nothing here is keyed on it.
		req.Email = strings.ToLower(strings.TrimSpace(addr.Address))
	}

	return req, true
}
