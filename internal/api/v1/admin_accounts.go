// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// PlatformAccountStore is the narrow platform-account boundary for operator tier-override endpoints, carrying account-level tier and limit overrides.
type PlatformAccountStore interface {
	Get(ctx context.Context, id uuid.UUID) (platform.Account, error)
	GetBySlug(ctx context.Context, slug string) (platform.Account, error)
	Update(ctx context.Context, a platform.Account) error
	// UpdateAtomic loads the row, applies mutate and writes it back in one transaction holding a row
	// lock (postgresstore.AccountStore.UpdateAtomic). PATCH uses it instead of Get+Update so two
	// concurrent operator PATCHes cannot lose an update on the kill-switch (status) field.
	UpdateAtomic(ctx context.Context, id uuid.UUID, mutate func(*platform.Account) error) (before, after platform.Account, err error)
}

// AccountSessionRevoker is the [platform.UserStore] slice account closure needs to log every member out.
type AccountSessionRevoker interface {
	ListUsersForAccount(ctx context.Context, accountID uuid.UUID) ([]platform.User, error)
	RevokeAllUserSessions(ctx context.Context, userID uuid.UUID) error
}

// AdminAccountView is the operator account wire shape: account-level knobs only, no customer PII beyond the billing email.
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
	// EffectiveRateLimitPerMin / EffectiveMonthlyQuota are what auth
	// enforces on a key minted at the dashboard defaults, override
	// included ([platform.Account.EffectiveRateLimitPerMin]); kept in
	// step with the dashboardauth staff-lookup projection this mirrors.
	EffectiveRateLimitPerMin int   `json:"effective_rate_limit_per_min"`
	EffectiveMonthlyQuota    int64 `json:"effective_monthly_quota"`
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
		EffectiveRateLimitPerMin:    a.EffectiveRateLimitPerMin(),
		EffectiveMonthlyQuota:       a.EffectiveMonthlyQuota(),
	}
	if !a.CreatedAt.IsZero() {
		v.CreatedAt = a.CreatedAt.UTC().Format(time.RFC3339)
	}
	return v
}

// adminAccountOverrideRequest is the PATCH /v1/admin/accounts/{id} body. Fields are pointers so
// the handler can tell "absent, leave unchanged" from "present with value 0, clear the override".
// At least one must be present.
type adminAccountOverrideRequest struct {
	// Tier, when set, overwrites the account's tier — the operator
	// comp / partner path (staff-set limits; there is no self-service
	// billing).
	Tier *string `json:"tier,omitempty"`
	// Status, when set, moves the account's lifecycle state: active | suspended | closed. This is
	// the operator KILL SWITCH, the HTTP trigger for the suspension enforced on the read paths (the
	// Postgres API-key validator's non-active rejection, the dashboard session middleware's denial).
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

// requireOperator gates a handler on an operator-tier credential, mirroring handleAdminKeysCreate.
// On ok=false a problem+json has already been written; instance names the endpoint in its detail.
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

// requireReason enforces the X-Reason contract shared by every unconditional operator-tier write
// (admin keys, PATCH accounts, status-notice create/resolve). On ok=false a problem+json has
// already been written. Operator-conditional routes (e.g. /v1/account/keys) use account.go's
// operatorReasonOK instead, whose message names that route.
func (s *Server) requireReason(w http.ResponseWriter, r *http.Request) (string, bool) {
	reason := r.Header.Get("X-Reason")
	if reason == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-reason",
			"X-Reason header required", http.StatusBadRequest,
			"every admin write captures an X-Reason header into the audit log")
		return "", false
	}
	return reason, true
}

// handleAdminAccountGet serves GET /v1/admin/accounts/{id}: the tier + overrides an operator
// inspects before patching. The view carries the billing email, so every successful read lands an
// "admin.account.read" audit row, as the dashboard staff lookup does for the same PII.
func (s *Server) handleAdminAccountGet(w http.ResponseWriter, r *http.Request) {
	subject, ok := s.requireOperator(w, r, "/v1/admin/accounts/{id}")
	if !ok {
		return
	}
	if s.PlatformAccounts == nil {
		writeAccountStoreUnavailable(w, r)
		return
	}
	id, ok := parseAccountID(w, r)
	if !ok {
		return
	}
	acct, err := s.PlatformAccounts.Get(r.Context(), id)
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
	s.recordAdminAccountReadAudit(r, subject, acct)
	writeJSON(w, adminAccountView(acct), Flags{})
}

// handleAdminAccountOverrides serves PATCH /v1/admin/accounts/{id}: set the account tier and/or the
// rate-limit / monthly-quota overrides. Operator-tier only; requires an `X-Reason` header, and
// every successful mutation lands an "account.override.set" audit row.
func (s *Server) handleAdminAccountOverrides(w http.ResponseWriter, r *http.Request) {
	subject, ok := s.requireOperator(w, r, "/v1/admin/accounts/{id}")
	if !ok {
		return
	}
	if s.PlatformAccounts == nil {
		writeAccountStoreUnavailable(w, r)
		return
	}
	id, ok := parseAccountID(w, r)
	if !ok {
		return
	}
	reason, ok := s.requireReason(w, r)
	if !ok {
		return
	}
	req, ok := parseAccountOverrideRequest(w, r)
	if !ok {
		return
	}

	// UpdateAtomic loads the row under a row lock and writes the mutated
	// result back inside the same transaction, so a concurrent PATCH on
	// this account can't read the same stale snapshot we did.
	beforeAcct, acct, err := s.PlatformAccounts.UpdateAtomic(r.Context(), id, func(a *platform.Account) error {
		if a.Status == platform.AccountClosed && req.Status != nil &&
			platform.AccountStatus(*req.Status) != platform.AccountClosed {
			return errAccountClosed
		}
		applyAccountOverrides(a, req, time.Now().UTC())
		return nil
	})
	if err != nil {
		s.writeAccountUpdateError(w, r, id, err)
		return
	}

	priorTier := beforeAcct.Tier
	priorStatus := beforeAcct.Status
	priorRateOverride := beforeAcct.RateLimitPerMinOverride
	priorQuotaOverride := beforeAcct.MonthlyRequestQuotaOverride

	clamped, clampFailed := s.clampKeysAfterTierChange(r.Context(), subject, priorTier, acct)
	evicted, evictFailed := s.evictKeyCacheOnEnforcementChange(
		r.Context(), subject, priorStatus, priorRateOverride, priorQuotaOverride, acct)
	revoked, revokeFailed := s.revokeKeysOnClosure(r.Context(), subject, acct)
	sessUsers, sessFailed := s.revokeSessionsOnClosure(r.Context(), acct)

	before := adminAccountView(beforeAcct)
	after := adminAccountView(acct)
	s.logger.Info("admin account override",
		"actor_key_id", subject.KeyID,
		"actor_identifier", subject.Identifier,
		"account_id", id,
		"reason", reason,
		"keys_clamped", clamped,
		"key_clamp_failures", clampFailed,
		"keys_evicted", evicted,
		"key_evict_failures", evictFailed,
		"keys_revoked", revoked,
		"key_revoke_failures", revokeFailed,
		"session_users_revoked", sessUsers,
		"session_revoke_failures", sessFailed)
	s.recordAdminAccountAudit(r, subject, id.String(), reason, before, after, adminAccountKeyOutcome{
		clamped: clamped, clampFailed: clampFailed, revoked: revoked, revokeFailed: revokeFailed,
		sessionUsers: sessUsers, sessionFailed: sessFailed,
	})

	writeJSON(w, after, Flags{})
}

// errAccountClosed refuses any status change out of `closed`. Closure is
// terminal: reopening would bring back every passkey, webhook and key the
// closure did not revoke, so a returning customer registers afresh.
var errAccountClosed = errors.New("account is closed")

// accountClosedRevokeReason is stamped on every key revoked by a closure.
const accountClosedRevokeReason = "account closed"

// adminAccountKeyOutcome is what a PATCH did to the account's credentials,
// recorded durably in its audit row.
type adminAccountKeyOutcome struct {
	clamped, clampFailed, revoked, revokeFailed int
	sessionUsers, sessionFailed                 int
}

// writeAccountUpdateError maps an UpdateAtomic failure to its problem response.
func (s *Server) writeAccountUpdateError(w http.ResponseWriter, r *http.Request, id uuid.UUID, err error) {
	switch {
	case errors.Is(err, platform.ErrNotFound):
		writeAccountNotFound(w, r)
	case errors.Is(err, errAccountClosed):
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-closed",
			"Account is closed", http.StatusConflict,
			"a closed account cannot change status; closure is terminal")
	default:
		s.logger.Error("admin account overrides: update failed", "err", err, "account_id", id)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-update-failed",
			"Could not update account", http.StatusInternalServerError,
			"see X-Request-ID in server logs")
	}
}

// revokeKeysOnClosure revokes every live Postgres key of a closed account and evicts each from the
// auth cache, so closure is recorded on the credential rows rather than resting on the
// account-status gate alone. It runs on every PATCH that leaves the account closed, so a re-PATCH
// retries a key a previous sweep failed on. Redis self-service keys are gated by the Redis
// validator's account-status check, which a terminal closure keeps shut.
func (s *Server) revokeKeysOnClosure(
	ctx context.Context, subject auth.Subject, acct platform.Account,
) (revoked, failed int) {
	st := s.APIKeyBudgets
	if acct.Status != platform.AccountClosed || st.Platform == nil {
		return 0, 0
	}
	cause := "admin PATCH /v1/admin/accounts/" + acct.ID.String() + " by " + subject.KeyID + " (account closed)"
	keys, err := st.Platform.ListActiveForAccount(ctx, acct.ID)
	if err != nil {
		st.note("list_keys")
		s.logger.Error("account closure: ListActiveForAccount failed; keys stay unrevoked "+
			"(the account-status gate still denies them); re-PATCH status=closed to retry",
			"cause", cause, "account_id", acct.ID, "err", err)
		return 0, 1
	}
	for i := range keys {
		k := keys[i]
		if err := st.Platform.Revoke(ctx, acct.ID, k.ID, uuid.Nil, accountClosedRevokeReason); err != nil {
			st.note("key_revoke")
			s.logger.Error("account closure: key revoke failed; re-PATCH status=closed to retry",
				"cause", cause, "account_id", acct.ID, "key_id", k.ID, "err", err)
			failed++
			continue
		}
		revoked++
		s.invalidatePlatformKeyCache(ctx, cause, st, acct, k)
	}
	return revoked, failed
}

// revokeSessionsOnClosure revokes every dashboard session of each member of a closed account, so
// closure is recorded on the session rows rather than resting on the middleware's status gate
// alone. Like revokeKeysOnClosure it re-runs on every PATCH that leaves the account closed.
func (s *Server) revokeSessionsOnClosure(ctx context.Context, acct platform.Account) (users, failed int) {
	if acct.Status != platform.AccountClosed || s.PlatformUsers == nil {
		return 0, 0
	}
	members, err := s.PlatformUsers.ListUsersForAccount(ctx, acct.ID)
	if err != nil {
		s.logger.Error("account closure: ListUsersForAccount failed; sessions stay unrevoked "+
			"(the account-status gate still denies them); re-PATCH status=closed to retry",
			"account_id", acct.ID, "err", err)
		return 0, 1
	}
	for i := range members {
		if err := s.PlatformUsers.RevokeAllUserSessions(ctx, members[i].ID); err != nil {
			s.logger.Error("account closure: session revoke failed; re-PATCH status=closed to retry",
				"account_id", acct.ID, "user_id", members[i].ID, "err", err)
			failed++
			continue
		}
		users++
	}
	return users, failed
}

// clampKeysAfterTierChange lowers every credential the account can still authenticate with to the
// new tier's budget when the PATCH LOWERED the tier ceiling. Returns (keys lowered, per-key
// failures) for the audit row.
//
// Writing `accounts.tier` alone is not enough: the enforced per-minute budget is read off the key
// record, so demoting Pro to Free would leave every existing key at 10_000/min indefinitely.
//
// A raise is not mirrored, since silently lifting keys would grant throughput the operator did not
// ask for. The account-level `rate_limit_per_min_override` is NOT clamped: it is an explicit field
// of the same request, so tier=free with a 10_000 override is a deliberate, audited decision.
func (s *Server) clampKeysAfterTierChange(
	ctx context.Context, subject auth.Subject, priorTier platform.Tier, acct platform.Account,
) (lowered, failed int) {
	ceiling := acct.Tier.MaxRateLimitPerMin()
	if ceiling >= priorTier.MaxRateLimitPerMin() {
		return 0, 0
	}
	cause := "admin PATCH /v1/admin/accounts/" + acct.ID.String() +
		" by " + subject.KeyID + " (" + string(priorTier) + "→" + string(acct.Tier) + ")"
	return s.clampKeyBudgetsToTier(ctx, cause, s.APIKeyBudgets, acct, ceiling)
}

// evictKeyCacheOnEnforcementChange evicts the account's cached API-key records when a PATCH changed
// a field feeding a key's RESOLVED enforcement state, so it applies on the next request rather
// than after the ~1h TTL. Two transitions:
//   - The account left active (suspended/closed), the operator kill switch. The cache-HIT path
//     checks only the KEY's own revoked/expired fields, never account status, while the MISS
//     path rejects non-active accounts before cacheStore, so one eviction is durable.
//   - The rate-limit or monthly-quota OVERRIDE changed (any delta): the validator caches the
//     resolved floor and ceiling, so a tightened override would stay unenforced on warm keys.
//
// POSTGRES BACKEND ONLY. Under auth_backend=redis the key a DEL would hit is the canonical
// credential, so an "eviction" would destroy the customer's key; [NewAPIKeyBudgetStores] leaves
// CacheInvalidator nil there and this returns (0, 0). Best-effort and idempotent: a failed key
// just waits out the TTL.
func (s *Server) evictKeyCacheOnEnforcementChange(
	ctx context.Context, subject auth.Subject, priorStatus platform.AccountStatus,
	priorRateOverride int, priorQuotaOverride int64, acct platform.Account,
) (evicted, failed int) {
	statusLeftActive := priorStatus == platform.AccountActive && acct.Status != platform.AccountActive
	overrideChanged := acct.RateLimitPerMinOverride != priorRateOverride ||
		acct.MonthlyRequestQuotaOverride != priorQuotaOverride
	if !statusLeftActive && !overrideChanged {
		return 0, 0
	}
	st := s.APIKeyBudgets
	if st.Platform == nil || st.CacheInvalidator == nil {
		return 0, 0
	}
	cause := "admin PATCH /v1/admin/accounts/" + acct.ID.String() +
		" by " + subject.KeyID + " (" + enforcementChangeCause(statusLeftActive, overrideChanged, priorStatus, acct.Status) + ")"
	keys, err := st.Platform.ListActiveForAccount(ctx, acct.ID)
	if err != nil {
		st.note("list_keys")
		s.logger.Error("enforcement-change cache evict: ListActiveForAccount failed; cached keys keep "+
			"the stale resolved budget until the validator TTL rolls them off",
			"cause", cause, "account_id", acct.ID, "err", err)
		return 0, 1
	}
	for i := range keys {
		k := keys[i]
		if len(k.KeyHash) == 0 {
			continue
		}
		if err := st.CacheInvalidator.InvalidateCachedKey(ctx, hex.EncodeToString(k.KeyHash)); err != nil {
			st.note("key_cache_invalidate")
			s.logger.Warn("enforcement-change cache evict: InvalidateCachedKey failed",
				"cause", cause, "account_id", acct.ID, "key_id", k.ID, "err", err)
			failed++
			continue
		}
		evicted++
	}
	if evicted > 0 {
		s.logger.Info("enforcement-change cache evict: evicted cached API keys so the persisted "+
			"account state is enforced immediately",
			"cause", cause, "account_id", acct.ID, "status", string(acct.Status),
			"keys_evicted", evicted)
	}
	return evicted, failed
}

// enforcementChangeCause renders the human-readable trigger stamped on the
// eviction's log lines: the status transition, the override delta, or both.
func enforcementChangeCause(
	statusLeftActive, overrideChanged bool,
	priorStatus, status platform.AccountStatus,
) string {
	switch {
	case statusLeftActive && overrideChanged:
		return string(priorStatus) + "→" + string(status) + " + override change"
	case statusLeftActive:
		return string(priorStatus) + "→" + string(status)
	default:
		return "override change"
	}
}

// applyAccountOverrides mutates acct in place from the request's set
// (non-nil) fields. Validation has already run in
// parseAccountOverrideRequest, so this is a pure field copy apart from
// the suspension bookkeeping.
func applyAccountOverrides(acct *platform.Account, req adminAccountOverrideRequest, now time.Time) {
	if req.Tier != nil {
		// Canonicalise so the clamp and audit rows downstream always speak the three-level model even
		// for a legacy paid-tier string; the store folds back to a CHECK-legal string at write time
		// (platform.Tier.StorageValue).
		acct.Tier = platform.Tier(*req.Tier).Canonical()
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

// applyAccountStatus moves the account's lifecycle state and keeps SuspendedAt / SuspendedReason
// consistent, as postgresstore.AccountStore's Suspend / Unsuspend do, so the two paths are
// indistinguishable. A re-suspension keeps the ORIGINAL SuspendedAt: the age of the suspension is
// the age of the incident, not of the last edit.
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
			"tier must be one of free, partner (legacy paid-tier names are accepted and mapped: starter→free; pro/business/enterprise→partner)")
		return req, false
	}
	if req.Status != nil && !validAccountStatus(*req.Status) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-status",
			"Invalid status", http.StatusBadRequest,
			"status must be one of active, suspended, closed")
		return req, false
	}
	if req.SuspendedReason != nil && utf8.RuneCountInString(*req.SuspendedReason) > 500 {
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

// validAccountTier gates the PATCH tier vocabulary: free / partner (anon is not an account tier)
// plus the legacy paid-tier strings, which applyAccountOverrides canonicalises
// (starter→free; pro/business/enterprise→partner) so existing operator tooling keeps working.
// The vocabulary lives in [platform.KnownTier].
func validAccountTier(t string) bool {
	return platform.KnownTier(t)
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

// auditSurfaceAdminAccountRead is the operator account read's label on
// [obs.AdminAuditWriteFailuresTotal]; internal/obs seeds the same value.
const auditSurfaceAdminAccountRead = "admin_account_read"

// recordAdminAccountReadAudit persists the "admin.account.read" audit row:
// which operator credential read which account's billing email. Best-effort
// like every sibling: the read has already happened, so a sink failure is
// counted and logged rather than failing the request.
func (s *Server) recordAdminAccountReadAudit(r *http.Request, actor auth.Subject, acct platform.Account) {
	if s.Audit == nil {
		return
	}
	meta, err := json.Marshal(map[string]any{
		"actor_key_id":     actor.KeyID,
		"actor_identifier": actor.Identifier,
	})
	if err != nil {
		obs.AdminAuditWriteFailuresTotal.WithLabelValues(auditSurfaceAdminAccountRead).Inc()
		s.logger.Warn("admin account read: audit metadata marshal failed (skipping audit row)",
			"err", err, "account_id", acct.ID)
		return
	}
	entry := platform.AuditEntry{
		AccountID:  acct.ID,
		ActorKind:  platform.ActorStaff,
		Action:     "admin.account.read",
		TargetKind: "account",
		TargetID:   acct.ID.String(),
		Metadata:   meta,
		UserAgent:  r.UserAgent(),
		Timestamp:  time.Now().UTC(),
	}
	if ip := middleware.RemoteIP(r); ip != "" {
		entry.IP = net.ParseIP(ip)
	}
	if err := s.Audit.Append(r.Context(), entry); err != nil {
		obs.AdminAuditWriteFailuresTotal.WithLabelValues(auditSurfaceAdminAccountRead).Inc()
		s.logger.Warn("admin account read: audit append failed (best-effort)",
			"err", err, "account_id", acct.ID, "actor_key_id", actor.KeyID)
	}
}

// recordAdminAccountAudit persists the "account.override.set" audit
// row. Best-effort — a sink failure logs at WARN and never blocks the
// mutation (audit-log unavailability must not break staff workflows;
// same contract as recordAdminKeyMintAudit).
func (s *Server) recordAdminAccountAudit(
	r *http.Request, actor auth.Subject, accountID, reason string, before, after AdminAccountView,
	keys adminAccountKeyOutcome,
) {
	if s.Audit == nil {
		return
	}
	meta, err := json.Marshal(map[string]any{
		"actor_key_id":     actor.KeyID,
		"actor_identifier": actor.Identifier,
		"reason":           reason,
		// Record the key clamp's OUTCOME durably: a silently failed clamp would leave over-tier
		// throughput live on a demoted account, and a non-zero key_clamp_failures is then queryable
		// in audit_log next to the tier change that caused it.
		"keys_clamped":       keys.clamped,
		"key_clamp_failures": keys.clampFailed,
		// Keys revoked because the account closed; failures are retried
		// by re-PATCHing status=closed.
		"keys_revoked":        keys.revoked,
		"key_revoke_failures": keys.revokeFailed,
		// Members whose dashboard sessions the closure revoked.
		"session_users_revoked":   keys.sessionUsers,
		"session_revoke_failures": keys.sessionFailed,
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
	if err := s.Audit.Append(r.Context(), entry); err != nil {
		// A failed append leaves a tier/quota override with no durable
		// record of the operator, the reason, or the previous values.
		obs.AdminAuditWriteFailuresTotal.WithLabelValues("account_override").Inc()
		s.logger.Warn("admin account override: audit append failed (best-effort)",
			"err", err, "account_id", accountID)
	}
}
