// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// PlatformAccountStore is the narrow platform-account boundary the
// operator tier-override endpoints need. Backed in production by
// postgresstore.AccountStore — the same store the Postgres API-key
// validator reads the account row (and its overrides) from at Lookup
// time. Distinct from [AccountStore] (the auth API-key store the
// self-service /v1/account/keys path uses): this one carries the
// account-level tier + rate-limit / monthly-quota overrides.
type PlatformAccountStore interface {
	Get(ctx context.Context, id uuid.UUID) (platform.Account, error)
	Update(ctx context.Context, a platform.Account) error
}

// AdminAccountView is the wire shape the operator account endpoints
// return — the account-level knobs an operator inspects / edits, no
// customer PII beyond the billing email. Mirrors the dashboardauth
// staff-lookup AdminAccountView projection so the two admin surfaces
// render consistently.
type AdminAccountView struct {
	ID                          string `json:"id"`
	Name                        string `json:"name"`
	Slug                        string `json:"slug"`
	Tier                        string `json:"tier"`
	Status                      string `json:"status"`
	BillingEmail                string `json:"billing_email,omitempty"`
	CreatedAt                   string `json:"created_at,omitempty"`
	SuspendedReason             string `json:"suspended_reason,omitempty"`
	RateLimitPerMinOverride     int    `json:"rate_limit_per_min_override"`
	MonthlyRequestQuotaOverride int64  `json:"monthly_request_quota_override"`
}

func adminAccountView(a platform.Account) AdminAccountView {
	v := AdminAccountView{
		ID:                          a.ID.String(),
		Name:                        a.Name,
		Slug:                        a.Slug,
		Tier:                        string(a.Tier),
		Status:                      string(a.Status),
		BillingEmail:                a.BillingEmail,
		SuspendedReason:             a.SuspendedReason,
		RateLimitPerMinOverride:     a.RateLimitPerMinOverride,
		MonthlyRequestQuotaOverride: a.MonthlyRequestQuotaOverride,
	}
	if !a.CreatedAt.IsZero() {
		v.CreatedAt = a.CreatedAt.UTC().Format(time.RFC3339)
	}
	return v
}

// adminAccountOverrideRequest is the PATCH /v1/admin/accounts/{id}
// body. All three fields are pointers so the handler can distinguish
// "field absent → leave unchanged" from "field present with value 0 →
// clear the override / set the value". Every field is optional but at
// least one must be present.
type adminAccountOverrideRequest struct {
	// Tier, when set, overwrites the account's plan tier — the operator
	// comp / enterprise path (Stripe normally drives tier; this is the
	// manual override for accounts not on self-service billing).
	Tier *string `json:"tier,omitempty"`
	// Status, when set, moves the account's lifecycle state:
	// active | suspended | closed. This is the operator KILL SWITCH
	// (C3-010, audit-2026-07-23) — the whole suspension machinery
	// already existed and was already enforced on the read paths
	// (postgresstore.AccountStore.Suspend, the Postgres API-key
	// validator's `acct.Status != AccountActive` rejection, the
	// dashboard session middleware's suspended/closed denial), but
	// nothing in the HTTP surface could TRIGGER it: this PATCH covered
	// tier + the two overrides and never touched Status, and Suspend()'s
	// only caller was an internal signup-race recovery path.
	Status *string `json:"status,omitempty"`
	// SuspendedReason is the customer-visible-ish note stored alongside
	// a suspension (distinct from the mandatory X-Reason audit header,
	// which records why the OPERATOR acted). Ignored when Status is
	// absent; cleared on a move back to active.
	SuspendedReason *string `json:"suspended_reason,omitempty"`
	// RateLimitPerMinOverride: 0 clears the override (inherit tier
	// default); a positive value sets an account-wide per-key floor.
	RateLimitPerMinOverride *int `json:"rate_limit_per_min_override,omitempty"`
	// MonthlyRequestQuotaOverride: 0 clears; positive sets the metered
	// cap the validator applies when a key has no per-key quota.
	MonthlyRequestQuotaOverride *int64 `json:"monthly_request_quota_override,omitempty"`
}

// requireOperator gates a handler on an operator-tier credential,
// mirroring handleAdminKeysCreate exactly. Returns the subject +
// ok=true when the caller may proceed; on ok=false a problem+json has
// already been written. instance names the endpoint for the problem
// detail text.
func (s *Server) requireOperator(w http.ResponseWriter, r *http.Request, instance string) (auth.Subject, bool) {
	subject, ok := auth.SubjectFrom(r.Context())
	if !ok || subject.Tier == auth.TierAnonymous || subject.Tier == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/unauthorized",
			"Authentication required", http.StatusUnauthorized,
			instance+" requires an operator credential")
		return auth.Subject{}, false
	}
	if subject.Tier != auth.TierOperator {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/operator-required",
			"Operator credential required", http.StatusForbidden,
			instance+" is restricted to operator-tier credentials")
		return auth.Subject{}, false
	}
	return subject, true
}

// handleAdminAccountGet serves GET /v1/admin/accounts/{id} — read the
// account-level tier + overrides so an operator can inspect current
// state before patching. Operator-tier only; read-only (no audit row —
// the audit log records mutations, not reads, matching the staff
// lookup's structured-log-only posture).
func (s *Server) handleAdminAccountGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireOperator(w, r, "/v1/admin/accounts/{id}"); !ok {
		return
	}
	if s.platformAccounts == nil {
		writeAccountStoreUnavailable(w, r)
		return
	}
	id, ok := parseAccountID(w, r)
	if !ok {
		return
	}
	acct, err := s.platformAccounts.Get(r.Context(), id)
	if errors.Is(err, platform.ErrNotFound) {
		writeAccountNotFound(w, r)
		return
	}
	if err != nil {
		s.logger.Error("admin account get failed", "err", err, "account_id", id)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-get-failed",
			"Could not load account", http.StatusInternalServerError,
			"see X-Request-ID in server logs")
		return
	}
	writeJSON(w, adminAccountView(acct), Flags{})
}

// handleAdminAccountOverrides serves PATCH /v1/admin/accounts/{id} —
// set the account tier and/or the rate-limit / monthly-quota overrides.
// Operator-tier only; requires an `X-Reason` header (platform-spec
// §7.2: every write endpoint captures a reason into the audit log).
// Every successful mutation lands an "account.override.set" audit row.
func (s *Server) handleAdminAccountOverrides(w http.ResponseWriter, r *http.Request) {
	subject, ok := s.requireOperator(w, r, "/v1/admin/accounts/{id}")
	if !ok {
		return
	}
	if s.platformAccounts == nil {
		writeAccountStoreUnavailable(w, r)
		return
	}
	id, ok := parseAccountID(w, r)
	if !ok {
		return
	}
	reason := r.Header.Get("X-Reason")
	if reason == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-reason",
			"X-Reason header required", http.StatusBadRequest,
			"every admin write captures an X-Reason header into the audit log")
		return
	}
	req, ok := parseAccountOverrideRequest(w, r)
	if !ok {
		return
	}

	acct, err := s.platformAccounts.Get(r.Context(), id)
	if errors.Is(err, platform.ErrNotFound) {
		writeAccountNotFound(w, r)
		return
	}
	if err != nil {
		s.logger.Error("admin account overrides: load failed", "err", err, "account_id", id)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-get-failed",
			"Could not load account", http.StatusInternalServerError,
			"see X-Request-ID in server logs")
		return
	}

	before := adminAccountView(acct)
	priorTier := acct.Tier
	applyAccountOverrides(&acct, req, time.Now().UTC())

	if err := s.platformAccounts.Update(r.Context(), acct); err != nil {
		if errors.Is(err, platform.ErrNotFound) {
			writeAccountNotFound(w, r)
			return
		}
		s.logger.Error("admin account overrides: update failed", "err", err, "account_id", id)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-update-failed",
			"Could not update account", http.StatusInternalServerError,
			"see X-Request-ID in server logs")
		return
	}

	clamped, clampFailed := s.clampKeysAfterTierChange(r.Context(), subject, priorTier, acct)

	after := adminAccountView(acct)
	s.logger.Info("admin account override",
		"actor_key_id", subject.KeyID,
		"actor_identifier", subject.Identifier,
		"account_id", id,
		"reason", reason,
		"keys_clamped", clamped,
		"key_clamp_failures", clampFailed)
	s.recordAdminAccountAudit(r, subject, id.String(), reason, before, after, clamped, clampFailed)

	writeJSON(w, after, Flags{})
}

// clampKeysAfterTierChange lowers every credential the account can still
// authenticate with to the new tier's budget, whenever the PATCH LOWERED
// the tier ceiling. Returns (keys lowered, per-key failures) so the
// caller can record both in the audit row.
//
// 52105fdb residual (audit-2026-07-23). The kill-switch/downgrade work
// gave the Stripe path a full clamp — `applyPlanDowngrade` lowers the
// account override AND both key stores — but PATCH
// /v1/admin/accounts/{id} only wrote `accounts.tier`. The enforced
// per-minute budget is read straight off the key record
// (auth/apikey_postgres.go `rateLimit := pgKey.RateLimitPerMin`;
// auth/apikey_redis.go returns the stored value), so an operator
// demoting an abusive or non-paying account Pro→Free left every existing
// key serving 10_000/min indefinitely — the same defect C3-014 fixed for
// Stripe, still live on the operator surface. Same helper, same ladder
// (`platform.Tier.MaxRateLimitPerMin`), so the two paths cannot drift.
//
// Only runs when the ceiling DROPS. A raise is intentionally not
// mirrored: budgets are raised at mint time by the dashboard's
// `clampRateLimitToTier`, and silently lifting existing keys on a comp
// would grant throughput the operator did not ask for.
//
// The account-level `rate_limit_per_min_override` is deliberately NOT
// clamped here, unlike the Stripe path: on the operator surface the
// override is an explicit field of the SAME request. An operator who
// PATCHes tier=free while leaving a 10_000 override in place is making a
// deliberate, audited comp decision; overwriting it would silently
// discard the very affordance this endpoint exists to provide. (Stripe
// clamps it because no human is in that loop.)
func (s *Server) clampKeysAfterTierChange(
	ctx context.Context, subject auth.Subject, priorTier platform.Tier, acct platform.Account,
) (lowered, failed int) {
	ceiling := acct.Tier.MaxRateLimitPerMin()
	if ceiling >= priorTier.MaxRateLimitPerMin() {
		return 0, 0
	}
	cause := "admin PATCH /v1/admin/accounts/" + acct.ID.String() +
		" by " + subject.KeyID + " (" + string(priorTier) + "→" + string(acct.Tier) + ")"
	return s.clampKeyBudgetsToTier(ctx, cause, s.apiKeyBudgets, acct, ceiling)
}

// applyAccountOverrides mutates acct in place from the request's set
// (non-nil) fields. Validation has already run in
// parseAccountOverrideRequest, so this is a pure field copy apart from
// the suspension bookkeeping.
func applyAccountOverrides(acct *platform.Account, req adminAccountOverrideRequest, now time.Time) {
	if req.Tier != nil {
		acct.Tier = platform.Tier(*req.Tier)
	}
	if req.Status != nil {
		applyAccountStatus(acct, platform.AccountStatus(*req.Status), req.SuspendedReason, now)
	}
	if req.RateLimitPerMinOverride != nil {
		acct.RateLimitPerMinOverride = *req.RateLimitPerMinOverride
	}
	if req.MonthlyRequestQuotaOverride != nil {
		acct.MonthlyRequestQuotaOverride = *req.MonthlyRequestQuotaOverride
	}
}

// applyAccountStatus moves the account's lifecycle state and keeps the
// SuspendedAt / SuspendedReason bookkeeping consistent with it —
// mirroring what postgresstore.AccountStore's Suspend / Unsuspend do, so
// a status set through this endpoint is indistinguishable from one set
// through those. A re-suspension keeps the ORIGINAL SuspendedAt: the age
// of the suspension is the age of the incident, not of the last edit.
func applyAccountStatus(acct *platform.Account, status platform.AccountStatus, reason *string, now time.Time) {
	acct.Status = status
	switch status {
	case platform.AccountActive:
		acct.SuspendedAt = time.Time{}
		acct.SuspendedReason = ""
	case platform.AccountSuspended, platform.AccountClosed:
		if acct.SuspendedAt.IsZero() {
			acct.SuspendedAt = now
		}
		if reason != nil {
			acct.SuspendedReason = *reason
		}
	}
}

// parseAccountOverrideRequest reads + validates the PATCH body. Returns
// ok=false with a problem+json already written on any failure. Rejects
// an empty patch (no recognised field present) so a no-op PATCH doesn't
// silently write an audit row.
func parseAccountOverrideRequest(w http.ResponseWriter, r *http.Request) (adminAccountOverrideRequest, bool) {
	var req adminAccountOverrideRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4*1024))
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/body-too-large",
			"Request body too large", http.StatusBadRequest,
			"/v1/admin/accounts body must be under 4 KiB")
		return req, false
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-body",
				"Malformed JSON body", http.StatusBadRequest,
				"could not parse request body as JSON")
			return req, false
		}
	}
	if req.Tier == nil && req.Status == nil &&
		req.RateLimitPerMinOverride == nil && req.MonthlyRequestQuotaOverride == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/empty-patch",
			"No fields to update", http.StatusBadRequest,
			"set at least one of tier, status, rate_limit_per_min_override, monthly_request_quota_override")
		return req, false
	}
	if req.Tier != nil && !validAccountTier(*req.Tier) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-tier",
			"Invalid tier", http.StatusBadRequest,
			"tier must be one of free, starter, pro, business, enterprise")
		return req, false
	}
	if req.Status != nil && !validAccountStatus(*req.Status) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-status",
			"Invalid status", http.StatusBadRequest,
			"status must be one of active, suspended, closed")
		return req, false
	}
	if req.SuspendedReason != nil && len(*req.SuspendedReason) > 500 {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-suspended-reason",
			"suspended_reason too long", http.StatusBadRequest,
			"suspended_reason must be 500 characters or fewer")
		return req, false
	}
	// Override columns are `> 0` in the schema; 0 is the "clear /
	// inherit tier default" sentinel (the store NULLIFs it). Reject
	// negatives and an absurd rate-limit ceiling (matches the admin
	// key-mint bound).
	if req.RateLimitPerMinOverride != nil {
		if v := *req.RateLimitPerMinOverride; v < 0 || v > 100000 {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-rate-limit",
				"Invalid rate_limit_per_min_override", http.StatusBadRequest,
				"must be in [0, 100000]; 0 clears the override (inherit tier default)")
			return req, false
		}
	}
	if req.MonthlyRequestQuotaOverride != nil {
		if v := *req.MonthlyRequestQuotaOverride; v < 0 {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-monthly-quota",
				"Invalid monthly_request_quota_override", http.StatusBadRequest,
				"must be >= 0; 0 clears the override (inherit tier default)")
			return req, false
		}
	}
	return req, true
}

func validAccountTier(t string) bool {
	switch platform.Tier(t) {
	case platform.TierFree, platform.TierStarter, platform.TierPro,
		platform.TierBusiness, platform.TierEnterprise:
		return true
	default:
		return false
	}
}

// validAccountStatus gates the kill-switch vocabulary to the three
// states the schema's CHECK constraint (migration 0027) and
// [platform.AccountStatus] recognise.
func validAccountStatus(st string) bool {
	switch platform.AccountStatus(st) {
	case platform.AccountActive, platform.AccountSuspended, platform.AccountClosed:
		return true
	default:
		return false
	}
}

// parseAccountID reads the {id} path value as a UUID. ok=false means a
// problem+json was already written.
func parseAccountID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	raw := r.PathValue("id")
	id, err := uuid.Parse(raw)
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-account-id",
			"Invalid account id", http.StatusBadRequest,
			"path must be /v1/admin/accounts/{id} with id a UUID")
		return uuid.Nil, false
	}
	return id, true
}

func writeAccountStoreUnavailable(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/account-store-unavailable",
		"Account store not configured", http.StatusServiceUnavailable,
		"this deployment has no platform AccountStore wired — typically because Postgres is unavailable")
}

func writeAccountNotFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/account-not-found",
		"Account not found", http.StatusNotFound,
		"no account with that id")
}

// recordAdminAccountAudit persists the "account.override.set" audit
// row. Best-effort — a sink failure logs at WARN and never blocks the
// mutation (audit-log unavailability must not break staff workflows;
// same contract as recordAdminKeyMintAudit).
func (s *Server) recordAdminAccountAudit(
	r *http.Request, actor auth.Subject, accountID, reason string, before, after AdminAccountView,
	keysClamped, keyClampFailures int,
) {
	if s.audit == nil {
		return
	}
	meta, err := json.Marshal(map[string]any{
		"actor_key_id":     actor.KeyID,
		"actor_identifier": actor.Identifier,
		"reason":           reason,
		// The tier-lowering key clamp's OUTCOME, durably. There is no
		// dedicated metric for this path (the Stripe counter's name,
		// help text and runbook are all Stripe-specific, so reusing it
		// would mislabel), and a clamp that silently failed would leave
		// paid throughput live on a demoted account. The audit_log is
		// the surface staff already read for privileged mutations, so a
		// non-zero key_clamp_failures is queryable next to the tier
		// change that caused it.
		"keys_clamped":       keysClamped,
		"key_clamp_failures": keyClampFailures,
		"before": map[string]any{
			"tier":                           before.Tier,
			"status":                         before.Status,
			"suspended_reason":               before.SuspendedReason,
			"rate_limit_per_min_override":    before.RateLimitPerMinOverride,
			"monthly_request_quota_override": before.MonthlyRequestQuotaOverride,
		},
		"after": map[string]any{
			"tier":                           after.Tier,
			"status":                         after.Status,
			"suspended_reason":               after.SuspendedReason,
			"rate_limit_per_min_override":    after.RateLimitPerMinOverride,
			"monthly_request_quota_override": after.MonthlyRequestQuotaOverride,
		},
	})
	if err != nil {
		s.logger.Warn("admin account override: audit metadata marshal failed (skipping audit row)",
			"err", err, "account_id", accountID)
		return
	}
	acctUUID, _ := uuid.Parse(accountID)
	entry := platform.AuditEntry{
		AccountID:  acctUUID,
		ActorKind:  platform.ActorStaff,
		Action:     "account.override.set",
		TargetKind: "account",
		TargetID:   accountID,
		Metadata:   meta,
		UserAgent:  r.UserAgent(),
		Timestamp:  time.Now().UTC(),
	}
	if ip := middleware.RemoteIP(r); ip != "" {
		entry.IP = net.ParseIP(ip)
	}
	if err := s.audit.Append(r.Context(), entry); err != nil {
		// C3-067: a tier/quota override landed with no durable record of the
		// operator, the reason, or the previous values.
		obs.AdminAuditWriteFailuresTotal.WithLabelValues("account_override").Inc()
		s.logger.Warn("admin account override: audit append failed (best-effort)",
			"err", err, "account_id", accountID)
	}
}
