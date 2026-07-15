// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// AuditSink is the narrow subset of [platform.AuditStore] the admin
// handlers need. Same shape as [StripeAuditSink] — kept as its own
// type so the two surfaces can be wired (or left nil) independently.
// Appends are best-effort: a sink failure is logged, never blocks
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
}

// handleAdminKeysCreate serves POST /v1/admin/keys — the operator
// key-mint path. Only TierOperator subjects may call it (the tier
// exists only on staff-issued credentials seeded via
// stellarindex-ops; it is never granted to public callers).
//
// Every successful mint is audit-logged: a structured log line
// unconditionally, plus a persisted audit_log row ("key.mint",
// ActorStaff) when the deployment wired an [AuditSink] — the same
// best-effort posture as recordStripeUpgradeAudit.
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
	if s.accounts == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-store-unavailable",
			"Account store not configured", http.StatusServiceUnavailable,
			"this deployment has no AccountStore wired — typically because Redis is unavailable")
		return
	}

	req, ok := parseAdminCreateKeyRequest(w, r)
	if !ok {
		return
	}

	rec, plaintext, err := s.accounts.Create(r.Context(), auth.CreateAPIKeyRequest{
		Identifier:      req.Identifier,
		Label:           req.Label,
		Tier:            auth.Tier(req.Tier),
		Scopes:          req.Scopes,
		RateLimitPerMin: req.RateLimitPerMin,
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
		"minted_key_id", rec.KeyID,
		"tier", req.Tier,
		"scopes", req.Scopes)
	s.recordAdminKeyMintAudit(r, subject, req, rec.KeyID)

	writeEnvelopeStatus(w, http.StatusCreated, Envelope{
		Data: KeyCreated{
			KeyID:     rec.KeyID,
			Plaintext: plaintext,
			KeyPrefix: rec.KeyPrefix,
			Label:     rec.Label,
			Scopes:    rec.Scopes,
		},
		AsOf:  rec.CreatedAt,
		Flags: Flags{},
	})
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
	if req.Label == "" || len(req.Label) > 128 {
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
	if req.RateLimitPerMin < 0 || req.RateLimitPerMin > 100000 {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-rate-limit",
			"Invalid rate_limit_per_min", http.StatusBadRequest,
			"rate_limit_per_min must be in [0, 100000]; 0 inherits the deployment default")
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
	return req, true
}

// recordAdminKeyMintAudit persists the "key.mint" audit row.
// Best-effort — a sink failure logs at WARN and never blocks the
// mint (audit-log unavailability must not break staff workflows,
// same contract as platform.AuditStore.Append documents).
func (s *Server) recordAdminKeyMintAudit(
	r *http.Request, actor auth.Subject, req adminCreateKeyRequest, mintedKeyID string,
) {
	if s.audit == nil {
		return
	}
	meta, err := json.Marshal(map[string]any{
		"actor_key_id":       actor.KeyID,
		"actor_identifier":   actor.Identifier,
		"target_identifier":  req.Identifier,
		"tier":               req.Tier,
		"label":              req.Label,
		"scopes":             req.Scopes,
		"rate_limit_per_min": req.RateLimitPerMin,
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
	if err := s.audit.Append(r.Context(), entry); err != nil {
		s.logger.Warn("admin key mint: audit append failed (best-effort)",
			"err", err, "minted_key_id", mintedKeyID, "target_identifier", req.Identifier)
	}
}
