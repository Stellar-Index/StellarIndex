// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// AuditSink is the narrow subset of [platform.AuditStore] the admin
// handlers need. Appends are best-effort: a sink failure is logged, never blocks
// the admin action, and the action is ALWAYS also structured-logged
// (the handlers_admin.go "record who did what" pattern), so an
// unwired sink still leaves an operator-greppable trail.
type AuditSink interface {
	Append(ctx context.Context, e platform.AuditEntry) error
}

// adminCreateKeyRequest is the POST /v1/admin/keys body. Unlike the
// self-service /v1/account/keys, the operator names the TARGET
// identifier and may pin tier / rate limit / scopes explicitly —
// this is the "separate admin path" the self-service handler's doc
// has always pointed at.
type adminCreateKeyRequest struct {
	// Identifier is the owner reference the new key authenticates
	// as (e.g. "acct:<slug>" or a signup email identifier).
	// Required.
	Identifier string `json:"identifier"`
	// Label is the human-readable key name. Required, ≤128 chars.
	Label string `json:"label"`
	// Tier is optional: "apikey" (default) or "operator". Anything
	// else is rejected — minting anonymous/sep10-tier keys makes no
	// sense.
	Tier string `json:"tier,omitempty"`
	// RateLimitPerMin optionally overrides the per-tier default
	// budget. Zero inherits the deployment default.
	RateLimitPerMin int `json:"rate_limit_per_min,omitempty"`
	// Scopes optionally confines the key to route families
	// (platform.KnownKeyScopes). Empty mints full access.
	Scopes []string `json:"scopes,omitempty"`
	// Account confirms a mint onto a platform account: required iff
	// Identifier is "acct:<slug>", and must equal <slug>. The identifier
	// is the metering subject, so such a key draws down that account's
	// monthly quota.
	Account string `json:"account,omitempty"`
}

// handleAdminKeysCreate serves POST /v1/admin/keys — the operator
// key-mint path. Only TierOperator subjects may call it (the tier
// exists only on staff-issued credentials seeded via
// stellarindex-ops; it is never granted to public callers).
//
// Requires an `X-Reason` header, the same contract
// as PATCH /v1/admin/accounts and DELETE /v1/admin/keys/{keyID}. Minting
// a privileged credential is at least as consequential as setting a
// per-account override or killing a key.
//
// Every successful mint is audit-logged: a structured log line
// unconditionally, plus a persisted audit_log row ("key.mint",
// ActorStaff) when the deployment wired an [AuditSink] —
// best-effort, never blocking the mint.
func (s *Server) handleAdminKeysCreate(w http.ResponseWriter, r *http.Request) {
	subject, ok := auth.SubjectFrom(r.Context())
	if !ok || subject.Tier == auth.TierAnonymous || subject.Tier == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/unauthorized",
			"Authentication required", http.StatusUnauthorized,
			"/v1/admin/keys requires an operator credential")
		return
	}
	if subject.Tier != auth.TierOperator {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/operator-required",
			"Operator credential required", http.StatusForbidden,
			"/v1/admin/keys is restricted to operator-tier credentials; customer keys mint their own via POST /v1/account/keys")
		return
	}
	if s.Accounts == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-store-unavailable",
			"Account store not configured", http.StatusServiceUnavailable,
			"this deployment has no AccountStore wired — typically because Redis is unavailable")
		return
	}
	reason, ok := s.requireReason(w, r)
	if !ok {
		return
	}

	req, ok := parseAdminCreateKeyRequest(w, r)
	if !ok {
		return
	}
	if !s.requireMintAccountExists(w, r, req.Account) {
		return
	}

	// Delegation clamp — the SAME chokepoint the self-service path
	// runs (handleAccountKeysCreate). The tier check above is not the
	// whole authority question: this very handler mints `tier:
	// operator` keys with an explicit `scopes` list, so a
	// scope-narrowed operator credential exists by construction, and
	// an empty scope list means EVERY capability (auth.Subject.HasScope
	// / checkScopes short-circuit on len(Scopes)==0). Without the clamp
	// a staff key minted as `tier: operator, scopes: ["admin"]` —
	// deliberately unable to read customer data — could POST
	// `scopes: []` here and mint itself a full-access operator key,
	// with the audit row as the only signal. ClampMintScopes' own doc
	// calls itself "the single chokepoint every mint path funnels
	// through"; this call is what makes that true for the admin path.
	// The same clamp bounds rate_limit_per_min, so a scope-narrowed
	// operator cannot mint 100k/min keys.
	scopes, ok := s.clampMintToCaller(w, r, subject, req.Scopes, req.RateLimitPerMin)
	if !ok {
		return
	}
	// Persist, audit-log and echo the CLAMPED set, never the request's:
	// the audit trail has to record what was actually issued.
	req.Scopes = scopes
	// A request that leaves rate_limit_per_min unset (0) never reaches
	// ClampToMinter's own-ceiling check (it only fires for
	// rateLimitPerMin > 0), so an unset request would otherwise persist
	// as 0 — the deployment/tier default, which can exceed a
	// rate-limited caller's own ceiling. Inherit the caller's ceiling
	// explicitly, mirroring ChildKeyRequest's unconditional copy.
	if req.RateLimitPerMin == 0 && subject.RateLimitPerMin > 0 {
		req.RateLimitPerMin = subject.RateLimitPerMin
	}

	rec, plaintext, err := s.Accounts.Create(r.Context(), auth.CreateAPIKeyRequest{
		MintedBy:        &subject,
		Identifier:      req.Identifier,
		Label:           req.Label,
		Tier:            auth.Tier(req.Tier),
		Scopes:          req.Scopes,
		RateLimitPerMin: req.RateLimitPerMin,
		// Operator-minted keys stay unmetered (0) by design; the admin
		// request carries no monthly cap to persist.
		MonthlyQuota: 0,
	})
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("admin key create failed", "err", err,
			"actor_key_id", subject.KeyID, "target_identifier", req.Identifier)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-create-failed",
			"Could not issue key", http.StatusInternalServerError,
			"see X-Request-ID in server logs")
		return
	}

	// Audit trail — unconditional structured log (mirrors the staff
	// customer-lookup pattern), plus the persisted row when wired.
	s.logger.Info("admin key mint",
		"actor_key_id", subject.KeyID,
		"actor_identifier", subject.Identifier,
		"target_identifier", req.Identifier,
		"target_account", req.Account,
		"minted_key_id", rec.KeyID,
		"tier", req.Tier,
		"scopes", req.Scopes,
		"monthly_quota", rec.MonthlyQuota, "reason", reason)
	s.recordAdminKeyMintAudit(r, subject, req, rec, reason)

	writeEnvelopeStatus(w, http.StatusCreated, Envelope{
		Data: KeyCreated{
			KeyID:     rec.KeyID,
			Plaintext: plaintext,
			KeyPrefix: rec.KeyPrefix,
			Label:     rec.Label,
			Scopes:    rec.Scopes,
		},
		AsOf:  WireTime(rec.CreatedAt),
		Flags: Flags{},
	})
}

// clampMintToCaller applies [auth.ClampToMinter] ahead of the store's own
// re-check so a refused escalation is a 403 naming the reason, logged at
// WARN with the actor and counted in [obs.MintScopeClampRefusedTotal]
// — otherwise a repeated escalation probe leaves no telemetry
// an alert could fire on.
func (s *Server) clampMintToCaller(
	w http.ResponseWriter, r *http.Request, caller auth.Subject, scopes []string, rateLimitPerMin int,
) ([]string, bool) {
	clamped, err := auth.ClampToMinter(caller, scopes, rateLimitPerMin)
	if err == nil {
		return clamped, true
	}
	s.logger.Warn("key mint refused: request exceeds the minting credential",
		"err", err, "actor_key_id", caller.KeyID, "actor_identifier", caller.Identifier,
		"path", r.URL.Path, "request_id", middleware.RequestIDFrom(r))
	obs.MintScopeClampRefusedTotal.WithLabelValues(r.URL.Path).Inc()
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/scope-exceeds-caller",
		"Scope exceeds caller", http.StatusForbidden,
		strings.TrimPrefix(err.Error(), auth.ErrMintExceedsCaller.Error()+": "))
	return nil, false
}

// revokeKeyEverywhere revokes a credential in every store that might
// hold a live record for it. Redis is the working
// credential under the default auth_backend=redis; Postgres's
// api_keys row is the durable management record that
// mintRegisterKey ALSO writes for the same KeyID. Revoking only
// Redis stops authentication but leaves api_keys.revoked_at NULL —
// the row keeps counting toward the active-key ceiling and lists as
// live to every Postgres reader. Both legs are scoped to identifier
// and treat "no such key" and "another owner's key" identically, so
// neither can be used to enumerate or revoke another account's keys.
//
// Returns [auth.ErrKeyNotFound] when NEITHER leg revoked anything, so
// a caller never reports a typo'd identifier or key id as a revoke.
func (s *Server) revokeKeyEverywhere(ctx context.Context, identifier, keyID, reason string) error {
	err := s.Accounts.RevokeKeyByID(ctx, identifier, keyID)
	revoked := err == nil
	if errors.Is(err, auth.ErrKeyNotFound) {
		err = nil
	}
	pgErr := s.revokeOwnedPlatformKey(ctx, identifier, keyID, reason)
	switch {
	case pgErr == nil:
		revoked = true
	case errors.Is(pgErr, platform.ErrNotFound):
	case err == nil:
		err = pgErr
	default:
		s.logger.Error("revoke: postgres management row also failed",
			"err", pgErr, "identifier", identifier, "key_id", keyID)
	}
	if err == nil && !revoked {
		return auth.ErrKeyNotFound
	}
	return err
}

// revokeOwnedPlatformKey revokes keyID's Postgres api_keys row only
// when that row's account is the one identifier names. The store's
// Revoke is scoped by account UUID, but the operator names the owner by
// identifier, so resolving one to the other has to happen here. A missing row, a
// row owned by another account, an already-revoked row, or no store to
// prove ownership all return [platform.ErrNotFound] without revoking
// (fail closed, no enumeration oracle); nil means a row was revoked.
func (s *Server) revokeOwnedPlatformKey(ctx context.Context, identifier, keyID, reason string) error {
	keys := s.APIKeyBudgets.Platform
	if keys == nil {
		return platform.ErrNotFound
	}
	if s.PlatformAccounts == nil {
		s.logger.Warn("revoke: postgres key store wired without an account store; management row left untouched",
			"identifier", identifier, "key_id", keyID)
		return platform.ErrNotFound
	}
	k, err := keys.Get(ctx, keyID)
	if err != nil {
		return err
	}
	owner, err := s.PlatformAccounts.Get(ctx, k.AccountID)
	if err != nil {
		return err
	}
	if auth.AccountIdentifier(owner.Slug) != identifier {
		return platform.ErrNotFound
	}
	return keys.Revoke(ctx, k.AccountID, keyID, uuid.Nil, reason)
}

// handleAdminKeysRevoke serves DELETE
// /v1/admin/keys/{keyID}?identifier=<owner> — the operator kill switch
// for a leaked or abused credential.
//
// It is the only way for staff to kill an arbitrary key: self-service
// revoke (DELETE /v1/account/keys/{keyID}) is scoped to the caller's OWN
// identifier, so it requires the compromised customer's credential, and
// the dashboard staff surface is explicitly read-only.
//
// `identifier` is required rather than inferred: the key store is
// keyed by secret hash and indexed by (identifier, key_id), and scoping
// the delete to a named owner is what stops a mistyped key id from
// reaching into a different customer's account. Operators read both off
// GET /v1/account/keys or the audit row for the mint.
//
// Operator-tier only; requires an `X-Reason` header (same contract as
// PATCH /v1/admin/accounts). 204 only when a key was
// actually revoked; 404, with no audit row, when no key matches
// (identifier, keyID). This is the emergency containment path, so a
// typo'd identifier must read as a failure, never as a contained leak.
// "No such key" and "another owner's key" share the 404, so it is still
// no cross-account enumeration oracle.
func (s *Server) handleAdminKeysRevoke(w http.ResponseWriter, r *http.Request) {
	subject, ok := s.requireOperator(w, r, "/v1/admin/keys/{keyID}")
	if !ok {
		return
	}
	if s.Accounts == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-store-unavailable",
			"Account store not configured", http.StatusServiceUnavailable,
			"this deployment has no AccountStore wired — typically because Redis is unavailable")
		return
	}
	reason, ok := s.requireReason(w, r)
	if !ok {
		return
	}
	keyID := r.PathValue("keyID")
	if keyID == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-key-id",
			"Missing key id", http.StatusBadRequest,
			"path must be /v1/admin/keys/{keyID}")
		return
	}
	identifier := r.URL.Query().Get("identifier")
	if identifier == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-identifier",
			"Missing identifier", http.StatusBadRequest,
			"?identifier= names the key's owner (e.g. acct:<slug> or signup-<hash>); "+
				"the revoke is scoped to it so a mistyped key id can't reach another customer's account")
		return
	}

	if err := s.revokeKeyEverywhere(r.Context(), identifier, keyID, reason); err != nil {
		if clientAborted(r, err) {
			return
		}
		if errors.Is(err, auth.ErrKeyNotFound) {
			s.logger.Warn("admin key revoke matched no key",
				"actor_key_id", subject.KeyID, "target_identifier", identifier, "key_id", keyID)
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/key-not-found",
				"Key not found", http.StatusNotFound,
				"no live key "+keyID+" is owned by identifier "+identifier+"; nothing was revoked")
			return
		}
		s.logger.Error("admin key revoke failed", "err", err,
			"actor_key_id", subject.KeyID, "target_identifier", identifier, "key_id", keyID)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-revoke-failed",
			"Could not revoke key", http.StatusInternalServerError,
			"see X-Request-ID in server logs")
		return
	}

	s.logger.Info("admin key revoke",
		"actor_key_id", subject.KeyID,
		"actor_identifier", subject.Identifier,
		"target_identifier", identifier,
		"key_id", keyID,
		"reason", reason)
	s.recordAdminKeyRevokeAudit(r, subject, identifier, keyID, reason)

	w.WriteHeader(http.StatusNoContent)
}

// recordAdminKeyRevokeAudit persists the "key.revoke" audit row.
// Best-effort, same contract as recordAdminKeyMintAudit — a sink failure
// logs at WARN and never blocks the revoke, because failing to kill a
// leaked credential because the audit log is down is the worse outcome.
func (s *Server) recordAdminKeyRevokeAudit(
	r *http.Request, actor auth.Subject, identifier, keyID, reason string,
) {
	if s.Audit == nil {
		return
	}
	meta, err := json.Marshal(map[string]any{
		"actor_key_id":      actor.KeyID,
		"actor_identifier":  actor.Identifier,
		"target_identifier": identifier,
		"reason":            reason,
	})
	if err != nil {
		s.logger.Warn("admin key revoke: audit metadata marshal failed (skipping audit row)",
			"err", err, "key_id", keyID)
		return
	}
	entry := platform.AuditEntry{
		ActorKind:  platform.ActorStaff,
		Action:     "key.revoke",
		TargetKind: "api_key",
		TargetID:   keyID,
		Metadata:   meta,
		UserAgent:  r.UserAgent(),
		Timestamp:  time.Now().UTC(),
	}
	if ip := middleware.RemoteIP(r); ip != "" {
		entry.IP = net.ParseIP(ip)
	}
	if err := s.Audit.Append(r.Context(), entry); err != nil {
		// The revoke already happened; the audit row did not. Count
		// it so the hole in the trail is observable, not just logged.
		obs.AdminAuditWriteFailuresTotal.WithLabelValues("key_revoke").Inc()
		s.logger.Warn("admin key revoke: audit append failed (best-effort)",
			"err", err, "key_id", keyID, "target_identifier", identifier)
	}
}

// parseAdminCreateKeyRequest reads + validates the body. ok=false
// means a problem+json was already written.
func parseAdminCreateKeyRequest(w http.ResponseWriter, r *http.Request) (adminCreateKeyRequest, bool) {
	var req adminCreateKeyRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4*1024))
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/body-too-large",
			"Request body too large", http.StatusBadRequest,
			"/v1/admin/keys body must be under 4 KiB")
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
	if req.Identifier == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-identifier",
			"Identifier is required", http.StatusBadRequest,
			"identifier names the owner reference the minted key authenticates as")
		return req, false
	}
	if req.Label == "" || utf8.RuneCountInString(req.Label) > 128 {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-label",
			"Label is required", http.StatusBadRequest,
			"label must be 1–128 characters")
		return req, false
	}
	switch req.Tier {
	case "":
		req.Tier = string(auth.TierAPIKey)
	case string(auth.TierAPIKey), string(auth.TierOperator):
	default:
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-tier",
			"Invalid tier", http.StatusBadRequest,
			"tier must be \"apikey\" (default) or \"operator\"")
		return req, false
	}
	if req.RateLimitPerMin < 0 || req.RateLimitPerMin > auth.MaxKeyRateLimitPerMin {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-rate-limit",
			"Invalid rate_limit_per_min", http.StatusBadRequest,
			fmt.Sprintf("rate_limit_per_min must be in [0, %d]; 0 inherits the deployment default", auth.MaxKeyRateLimitPerMin))
		return req, false
	}
	scopes, problem := validateScopes(req.Scopes)
	if problem != "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-scope",
			"Invalid scope", http.StatusBadRequest, problem)
		return req, false
	}
	req.Scopes = scopes
	slug, isAccount := strings.CutPrefix(req.Identifier, auth.AccountIdentifierPrefix)
	switch {
	case isAccount && (slug == "" || req.Account != slug):
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-binding-unconfirmed",
			"Account binding unconfirmed", http.StatusBadRequest,
			"an acct:<slug> identifier bills the key's traffic to that account's monthly quota; "+
				"set \"account\" to the same <slug> to confirm")
		return req, false
	case !isAccount && req.Account != "":
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-binding-unconfirmed",
			"Account binding mismatch", http.StatusBadRequest,
			"\"account\" is only valid with an acct:<slug> identifier naming the same account")
		return req, false
	}
	return req, true
}

// requireMintAccountExists refuses an acct:<slug> mint whose account is
// not in the platform store, so a key cannot be pre-planted on a slug a
// later registration would inherit. An empty slug (non-account identifier) passes.
func (s *Server) requireMintAccountExists(w http.ResponseWriter, r *http.Request, slug string) bool {
	if slug == "" {
		return true
	}
	if s.PlatformAccounts == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-store-unavailable",
			"Account store not configured", http.StatusServiceUnavailable,
			"this deployment cannot verify platform accounts, so it cannot mint acct:<slug> keys")
		return false
	}
	_, err := s.PlatformAccounts.GetBySlug(r.Context(), slug)
	switch {
	case err == nil:
		return true
	case errors.Is(err, platform.ErrNotFound):
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-not-found",
			"Account not found", http.StatusNotFound,
			"no platform account has slug "+slug)
	case clientAborted(r, err):
	default:
		s.logger.Error("admin key mint: account lookup failed", "err", err, "account", slug)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-store-unavailable",
			"Account lookup failed", http.StatusServiceUnavailable,
			"see X-Request-ID in server logs")
	}
	return false
}

// recordAdminKeyMintAudit persists the "key.mint" audit row.
// Best-effort — a sink failure logs at WARN and never blocks the
// mint (audit-log unavailability must not break staff workflows,
// same contract as platform.AuditStore.Append documents).
func (s *Server) recordAdminKeyMintAudit(
	r *http.Request, actor auth.Subject, req adminCreateKeyRequest, minted auth.APIKeyRecord, reason string,
) {
	if s.Audit == nil {
		return
	}
	mintedKeyID := minted.KeyID
	// monthly_quota is the ceiling the store resolved, not a request
	// field: the admin body carries none, so record what was issued.
	meta, err := json.Marshal(map[string]any{
		"actor_key_id":       actor.KeyID,
		"actor_identifier":   actor.Identifier,
		"target_identifier":  req.Identifier,
		"target_account":     req.Account,
		"tier":               req.Tier,
		"label":              req.Label,
		"scopes":             req.Scopes,
		"rate_limit_per_min": req.RateLimitPerMin,
		"monthly_quota":      minted.MonthlyQuota,
		"reason":             reason,
	})
	if err != nil {
		s.logger.Warn("admin key mint: audit metadata marshal failed (skipping audit row)",
			"err", err, "minted_key_id", mintedKeyID)
		return
	}
	entry := platform.AuditEntry{
		ActorKind:  platform.ActorStaff,
		Action:     "key.mint",
		TargetKind: "api_key",
		TargetID:   mintedKeyID,
		Metadata:   meta,
		UserAgent:  r.UserAgent(),
		Timestamp:  time.Now().UTC(),
	}
	if ip := middleware.RemoteIP(r); ip != "" {
		entry.IP = net.ParseIP(ip)
	}
	if err := s.Audit.Append(r.Context(), entry); err != nil {
		// A live credential exists with no record of who minted it.
		obs.AdminAuditWriteFailuresTotal.WithLabelValues("key_mint").Inc()
		s.logger.Warn("admin key mint: audit append failed (best-effort)",
			"err", err, "minted_key_id", mintedKeyID, "target_identifier", req.Identifier)
	}
}
