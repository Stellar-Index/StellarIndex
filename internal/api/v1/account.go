package v1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// AccountStore is the v1 boundary against [auth.APIKeyStore], used by the account key create and list handlers.
type AccountStore interface {
	Create(ctx context.Context, req auth.CreateAPIKeyRequest) (auth.APIKeyRecord, string, error)
	// CreateCapped is Create with the identifier's active-key ceiling
	// enforced atomically with the write; refusals are
	// *auth.KeyQuotaExceededError or wrap auth.ErrKeyQuotaUnavailable.
	CreateCapped(ctx context.Context, req auth.CreateAPIKeyRequest, maxActive int) (auth.APIKeyRecord, string, error)
	ListKeysForIdentifier(ctx context.Context, identifier string) ([]auth.APIKeyRecord, error)
	RevokeKeyByID(ctx context.Context, identifier, keyID string) error
}

// Account is the wire shape for /v1/account/me: a union where API-key callers fill the key_* fields and magic-link callers fill nested `user` + `account`.
type Account struct {
	KeyID           string       `json:"key_id,omitempty"`
	Label           string       `json:"label,omitempty"`
	KeyPrefix       string       `json:"key_prefix,omitempty"`
	Tier            string       `json:"tier,omitempty"`
	RateLimitPerMin int          `json:"rate_limit_per_min,omitempty"`
	CreatedAt       WireTime     `json:"created_at,omitempty"`
	User            *AccountUser `json:"user,omitempty"`
	AccountInfo     *AccountInfo `json:"account,omitempty"`
}

// AccountUser is the magic-link-session caller's user info.
type AccountUser struct {
	ID              string    `json:"id"`
	Email           string    `json:"email"`
	DisplayName     string    `json:"display_name,omitempty"`
	Role            string    `json:"role,omitempty"`
	IsStaff         bool      `json:"is_staff"`
	EmailVerifiedAt *WireTime `json:"email_verified_at,omitempty"`
	LastLoginAt     *WireTime `json:"last_login_at,omitempty"`
}

// AccountInfo is the magic-link caller's parent account; rate-limit and quota are per-key defaults with account override, not the tier ceiling.
type AccountInfo struct {
	ID                  string `json:"id"`
	Name                string `json:"name,omitempty"`
	Slug                string `json:"slug,omitempty"`
	Tier                string `json:"tier,omitempty"`
	Status              string `json:"status,omitempty"`
	RateLimitPerMin     int    `json:"rate_limit_per_min,omitempty"`
	MonthlyRequestQuota int64  `json:"monthly_request_quota,omitempty"`
}

// SessionInfo is the wire projection of a magic-link session, defined here so v1 does not import dashboardauth.
type SessionInfo struct {
	UserID          string
	Email           string
	DisplayName     string
	Role            string
	IsStaff         bool
	EmailVerifiedAt time.Time
	LastLoginAt     time.Time

	AccountID     string
	AccountName   string
	AccountSlug   string
	AccountTier   string
	AccountStatus string
	// AccountRateLimitPerMin / AccountMonthlyRequestQuota are the
	// default-key enforced budgets — see [AccountInfo].
	AccountRateLimitPerMin     int
	AccountMonthlyRequestQuota int64
}

// SessionPeeker reads the magic-link session bound to the request context; main.go wires it from dashboardauth.
type SessionPeeker interface {
	SessionFromContext(ctx context.Context) (SessionInfo, bool)
}

// UsageRow is one /v1/account/usage entry; `billable` (ok + 4xx, never 429) is the quota-counted column and means the same on both rollup and fallback paths.
type UsageRow struct {
	Date      string `json:"date"`               // YYYY-MM-DD
	Endpoint  string `json:"endpoint,omitempty"` // route pattern; empty on the legacy fallback
	Requests  int    `json:"requests"`
	Billable  int    `json:"billable"`
	Errors    int    `json:"errors"`
	Throttled int    `json:"throttled"`
}

// UsageReader is the storage seam for /v1/account/usage, bridged in main.go so v1 stays free of the usage import.
type UsageReader interface {
	Read(ctx context.Context, subject string, days int) ([]UsageDay, error)
}

// UsageDay mirrors usage.Day: Date is YYYY-MM-DD UTC and Requests is the day's billable total (429 and 5xx never reach it).
type UsageDay struct {
	Date     string
	Requests int64
}

// UsageRollupReader is the storage seam for the per-endpoint `usage_daily` rollups, bridged in main.go.
type UsageRollupReader interface {
	ReadRollup(ctx context.Context, subject string, days int) ([]UsageEndpointDay, error)
}

// UsageEndpointDay is one (day, endpoint) aggregate: Requests = non-429, Billable = ok + 4xx, Errors = 4xx (excl. 429) + 5xx, Throttled = 429s.
type UsageEndpointDay struct {
	Date      string // YYYY-MM-DD UTC
	Endpoint  string // route pattern
	Requests  int64
	Billable  int64
	Errors    int64
	Throttled int64
}

// KeyCreated is the /v1/account/keys POST reply; the plaintext key appears exactly once and cannot be recovered.
type KeyCreated struct {
	KeyID     string   `json:"key_id"`
	Plaintext string   `json:"plaintext"`
	KeyPrefix string   `json:"key_prefix,omitempty"`
	Label     string   `json:"label"`
	Scopes    []string `json:"scopes,omitempty"`
}

// createKeyRequest is the POST body. Identifier and Tier come from the caller, never the body; a SCOPED caller can only NARROW
// (empty Scopes inherits, an unowned scope is rejected, see middleware.ClampMintScopes) and the child inherits expiry, quota
// and rate limit (auth.ChildKeyRequest). Operator self-mints need X-Reason + a persisted key.mint row.
type createKeyRequest struct {
	Label  string   `json:"label"`
	Scopes []string `json:"scopes,omitempty"`
}

// validateScopes dedupes and checks a mint request's scopes against the platform vocabulary; returns a problem detail and false
// on failure.
func validateScopes(raw []string) ([]string, string) {
	if len(raw) == 0 {
		return nil, ""
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		if !platform.ValidKeyScope(s) {
			return nil, fmt.Sprintf("unknown scope %q — valid scopes: %s",
				s, strings.Join(platform.KnownKeyScopes(), ", "))
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out, ""
}

// handleAccountMe serves GET /v1/account/me. A session wins over an API key when both are present (it identifies a user, a key
// only a credential); anonymous gets 401.
func (s *Server) handleAccountMe(w http.ResponseWriter, r *http.Request) {
	// Magic-link session takes precedence when both are present.
	if s.SessionPeeker != nil {
		if sess, ok := s.SessionPeeker.SessionFromContext(r.Context()); ok {
			out := Account{
				User: &AccountUser{
					ID:              sess.UserID,
					Email:           sess.Email,
					DisplayName:     sess.DisplayName,
					Role:            sess.Role,
					IsStaff:         sess.IsStaff,
					EmailVerifiedAt: wireTimeOrNil(sess.EmailVerifiedAt),
					LastLoginAt:     wireTimeOrNil(sess.LastLoginAt),
				},
				AccountInfo: &AccountInfo{
					ID:                  sess.AccountID,
					Name:                sess.AccountName,
					Slug:                sess.AccountSlug,
					Tier:                sess.AccountTier,
					Status:              sess.AccountStatus,
					RateLimitPerMin:     sess.AccountRateLimitPerMin,
					MonthlyRequestQuota: sess.AccountMonthlyRequestQuota,
				},
			}
			writeJSON(w, out, Flags{})
			return
		}
	}

	subject, ok := auth.SubjectFrom(r.Context())
	if !ok || subject.Tier == auth.TierAnonymous || subject.Tier == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/unauthorized",
			"Authentication required", http.StatusUnauthorized,
			"/v1/account/me requires a magic-link session, API key, or SEP-10 token")
		return
	}

	out := Account{
		KeyID:           subject.KeyID,
		Label:           subject.Label,
		KeyPrefix:       subject.KeyPrefix,
		Tier:            string(subject.Tier),
		RateLimitPerMin: subject.RateLimitPerMin,
		CreatedAt:       WireTime(subject.CreatedAt),
	}
	writeJSON(w, out, Flags{})
}

// handleAccountUsage serves GET /v1/account/usage from the [UsageRollupReader], falling back to the legacy [UsageReader] when the rollup is
// unwired, errors or is empty; days missing from the rollup (worker-outage gap, not zero traffic) are backfilled from legacy. Keys go
// through [middleware.UsageKeyForSubject] so writer and readers stay in lock-step. Anonymous gets 401; no backend gives 200 with empty data.
func (s *Server) handleAccountUsage(w http.ResponseWriter, r *http.Request) {
	key := ""
	if s.SessionPeeker != nil {
		if sess, ok := s.SessionPeeker.SessionFromContext(r.Context()); ok {
			key = usageKeyForSession(sess.AccountSlug)
		}
	}
	if key == "" {
		subject, ok := auth.SubjectFrom(r.Context())
		if !ok || subject.Tier == auth.TierAnonymous || subject.Tier == "" {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/unauthorized",
				"Authentication required", http.StatusUnauthorized,
				"/v1/account/usage requires a magic-link session, an API key, or a SEP-10 token (on a deployment running auth_mode=sep10)")
			return
		}
		// The single UsageTracker-shared derivation (id:<Identifier>, the
		// owner account, or key:<KeyID> as fallback) — calling it directly
		// instead of reimplementing it here means the writer and this reader
		// can never drift apart.
		key = middleware.UsageKeyForSubject(subject)
	}
	if key == "" {
		writeJSON(w, []UsageRow{}, Flags{})
		return
	}
	if rows, ok := s.readUsageRollup(r, key); ok {
		writeJSON(w, rows, Flags{})
		return
	}
	writeJSON(w, s.readUsageLegacy(r, key), Flags{})
}

// usageKeyForSession derives the usage key a session reads under, via [middleware.UsageKeyForSubject] so it cannot drift from
// what the account's API keys write under.
func usageKeyForSession(accountSlug string) string {
	if accountSlug == "" {
		return ""
	}
	return middleware.UsageKeyForSubject(auth.Subject{
		Identifier: auth.AccountIdentifier(accountSlug),
		Tier:       auth.TierAPIKey,
	})
}

// readUsageRollup reads per-endpoint rollups and backfills days the worker produced no row for, so an outage gap does not
// vanish; ok=false means nothing usable, fall back to legacy entirely.
func (s *Server) readUsageRollup(r *http.Request, key string) ([]UsageRow, bool) {
	if s.UsageRollupReader == nil {
		return nil, false
	}
	days, err := s.UsageRollupReader.ReadRollup(r.Context(), key, 30)
	if err != nil {
		s.logger.Warn("usage rollup read", "err", err, "subject", key)
		return nil, false
	}
	if len(days) == 0 {
		return nil, false
	}
	out := make([]UsageRow, len(days))
	present := make(map[string]struct{}, len(days))
	for i, d := range days {
		out[i] = UsageRow{
			Date:      d.Date,
			Endpoint:  d.Endpoint,
			Requests:  int(d.Requests),
			Billable:  int(d.Billable),
			Errors:    int(d.Errors),
			Throttled: int(d.Throttled),
		}
		present[d.Date] = struct{}{}
	}
	out = append(out, s.backfillMissingUsageDays(r, key, present)...)
	return out, true
}

// backfillMissingUsageDays fills days absent from present from the legacy reader (a worker gap, not a zero-traffic day); a
// legacy read failure means no backfill.
func (s *Server) backfillMissingUsageDays(r *http.Request, key string, present map[string]struct{}) []UsageRow {
	if s.UsageReader == nil {
		return nil
	}
	legacyDays, err := s.UsageReader.Read(r.Context(), key, 30)
	if err != nil {
		s.logger.Warn("usage rollup backfill read", "err", err, "subject", key)
		return nil
	}
	var backfilled []UsageRow
	for _, d := range legacyDays {
		if _, ok := present[d.Date]; ok {
			continue
		}
		backfilled = append(backfilled, legacyUsageRow(d))
	}
	return backfilled
}

// readUsageLegacy reads the per-day Redis totals; every failure degrades to the empty-list wire shape, never a 5xx, since usage
// is a dashboard nicety.
func (s *Server) readUsageLegacy(r *http.Request, key string) []UsageRow {
	if s.UsageReader == nil {
		return []UsageRow{}
	}
	days, err := s.UsageReader.Read(r.Context(), key, 30)
	if err != nil {
		s.logger.Warn("usage read", "err", err, "subject", key)
		return []UsageRow{}
	}
	out := make([]UsageRow, len(days))
	for i, d := range days {
		out[i] = legacyUsageRow(d)
	}
	return out
}

// legacyUsageRow renders one legacy per-day total. That counter is the
// billable one, so it is both `requests` and `billable` on this path.
func legacyUsageRow(d UsageDay) UsageRow {
	return UsageRow{
		Date:     d.Date,
		Requests: int(d.Requests),
		Billable: int(d.Requests),
	}
}

// handleAccountKeysCreate serves POST /v1/account/keys, inheriting the caller's Identifier and Tier. Anonymous 401; sep10 tier
// 403; bad body 400; no store 503. Operator callers must send X-Reason (400 without) and land a key.mint audit row, else an
// operator credential could spawn more operators unaudited.
func (s *Server) handleAccountKeysCreate(w http.ResponseWriter, r *http.Request) {
	subject, ok := auth.SubjectFrom(r.Context())
	if !ok || subject.Tier == auth.TierAnonymous || subject.Tier == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/unauthorized",
			"Authentication required", http.StatusUnauthorized,
			"/v1/account/keys requires an API key")
		return
	}
	// sep10 mode honours only JWTs, so a key minted here has no consumer;
	// and a SEP-10 identifier is a free keypair, so a per-identifier cap bounds nothing.
	if subject.Tier == auth.TierSEP10 {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/key-mint-not-available",
			"API keys not issued to SEP-10 subjects", http.StatusForbidden,
			"a deployment running auth_mode=sep10 accepts only SEP-10 tokens; authenticate with a fresh SEP-10 challenge instead of minting an API key")
		return
	}
	// Only account tiers may mint: any other credential has no key quota to charge.
	if subject.Tier != auth.TierAPIKey && subject.Tier != auth.TierOperator {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-tier-required",
			"Account credential required", http.StatusForbidden,
			"POST /v1/account/keys mints keys for API-key accounts only")
		return
	}
	if s.Accounts == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-store-unavailable",
			"Account store not configured", http.StatusServiceUnavailable,
			"this deployment has no AccountStore wired — typically because Redis is unavailable")
		return
	}
	reason, ok := s.operatorReasonOK(w, r, subject)
	if !ok {
		return
	}

	req, ok := parseCreateKeyRequest(w, r)
	if !ok {
		return
	}

	scopes, problem := validateScopes(req.Scopes)
	if problem != "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-scope",
			"Invalid scope", http.StatusBadRequest, problem)
		return
	}

	// Delegation clamp: a scoped caller may only mint a child whose scopes are a subset of its
	// own, and an empty request inherits them rather than defaulting to full access. Otherwise a
	// narrowed key could mint an unrestricted sibling and escape its confinement.
	scopes, ok = s.clampMintToCaller(w, r, subject, scopes, subject.RateLimitPerMin)
	if !ok {
		return
	}

	// Every delegated dimension (tier, rate limit, monthly quota, expiry,
	// email verification) is inherited from the caller, never defaulted:
	// a child from a metered or time-boxed key is metered and time-boxed.
	rec, plaintext, ok := s.mintAccountKey(w, r, auth.ChildKeyRequest(subject, req.Label, scopes))
	if !ok {
		return
	}

	if subject.Tier == auth.TierOperator {
		s.recordAccountKeyMintAudit(r, subject, req.Label, scopes, rec.KeyID, reason)
	}

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

// accountKeyIdempotencySubject scopes an Idempotency-Key to the authenticating CREDENTIAL: a replay returns minted plaintext,
// so a narrower sibling must never replay a wider one's mint.
func accountKeyIdempotencySubject(r *http.Request) string {
	subject, ok := auth.SubjectFrom(r.Context())
	if !ok || subject.Tier == auth.TierAnonymous || subject.Tier == "" {
		return ""
	}
	if subject.KeyID != "" {
		return "key:" + subject.KeyID
	}
	if subject.Identifier == "" {
		return ""
	}
	return "sub:" + subject.Identifier
}

// parseCreateKeyRequest reads + validates the POST /v1/account/keys
// body. ok=false means the 400 has already been written.
func parseCreateKeyRequest(w http.ResponseWriter, r *http.Request) (createKeyRequest, bool) {
	var req createKeyRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4*1024))
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/body-too-large",
			"Request body too large", http.StatusBadRequest,
			"/v1/account/keys body must be under 4 KiB")
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
	if req.Label == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-label",
			"Label is required", http.StatusBadRequest,
			"the new key needs a label so the customer can identify it later")
		return req, false
	}
	if utf8.RuneCountInString(req.Label) > 128 {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/label-too-long",
			"Label too long", http.StatusBadRequest,
			"label must be 128 characters or fewer")
		return req, false
	}
	return req, true
}

// operatorReasonOK enforces the X-Reason contract on self-service key routes for operator-tier callers only; returns false
// after writing the 400.
func (s *Server) operatorReasonOK(w http.ResponseWriter, r *http.Request, subject auth.Subject) (string, bool) {
	if subject.Tier != auth.TierOperator {
		return "", true
	}
	reason := r.Header.Get("X-Reason")
	if reason == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-reason",
			"X-Reason header required", http.StatusBadRequest,
			"operator-tier callers of /v1/account/keys capture an X-Reason header into the audit log, "+
				"same as /v1/admin/keys")
		return "", false
	}
	return reason, true
}

// recordAccountKeyMintAudit persists the key.mint row for an operator self-service mint, shaped like the admin mint (the route
// field tells them apart). Best-effort: a sink failure logs at WARN and never blocks the mint.
func (s *Server) recordAccountKeyMintAudit(
	r *http.Request, actor auth.Subject, label string, scopes []string, mintedKeyID, reason string,
) {
	s.logger.Info("account key mint (operator)",
		"actor_key_id", actor.KeyID,
		"actor_identifier", actor.Identifier,
		"minted_key_id", mintedKeyID,
		"tier", actor.Tier,
		"scopes", scopes,
		"reason", reason)
	if s.Audit == nil {
		return
	}
	meta, err := json.Marshal(map[string]any{
		"route":              "/v1/account/keys",
		"actor_key_id":       actor.KeyID,
		"actor_identifier":   actor.Identifier,
		"target_identifier":  actor.Identifier,
		"tier":               string(actor.Tier),
		"label":              label,
		"scopes":             scopes,
		"rate_limit_per_min": actor.RateLimitPerMin,
		"reason":             reason,
	})
	if err != nil {
		s.logger.Warn("account key mint: audit metadata marshal failed (skipping audit row)",
			"err", err, "minted_key_id", mintedKeyID)
		return
	}
	s.appendKeyAudit(r, platform.AuditEntry{
		ActorKind:  platform.ActorStaff,
		Action:     "key.mint",
		TargetKind: "api_key",
		TargetID:   mintedKeyID,
		Metadata:   meta,
	}, "key_mint", "account key mint")
}

// recordAccountKeyRevokeAudit persists the key.revoke row for an operator self-service revoke; best-effort, the revoke must not
// be undone by a sink outage.
func (s *Server) recordAccountKeyRevokeAudit(
	r *http.Request, actor auth.Subject, keyID, reason string,
) {
	s.logger.Info("account key revoke (operator)",
		"actor_key_id", actor.KeyID,
		"actor_identifier", actor.Identifier,
		"key_id", keyID,
		"reason", reason)
	if s.Audit == nil {
		return
	}
	meta, err := json.Marshal(map[string]any{
		"route":             "/v1/account/keys",
		"actor_key_id":      actor.KeyID,
		"actor_identifier":  actor.Identifier,
		"target_identifier": actor.Identifier,
		"reason":            reason,
	})
	if err != nil {
		s.logger.Warn("account key revoke: audit metadata marshal failed (skipping audit row)",
			"err", err, "key_id", keyID)
		return
	}
	s.appendKeyAudit(r, platform.AuditEntry{
		ActorKind:  platform.ActorStaff,
		Action:     "key.revoke",
		TargetKind: "api_key",
		TargetID:   keyID,
		Metadata:   meta,
	}, "key_revoke", "account key revoke")
}

// appendKeyAudit stamps UA, IP and time onto entry and appends best-effort, counting a sink failure under the given surface
// label.
func (s *Server) appendKeyAudit(r *http.Request, entry platform.AuditEntry, surface, what string) {
	entry.UserAgent = r.UserAgent()
	entry.Timestamp = time.Now().UTC()
	if ip := middleware.RemoteIP(r); ip != "" {
		entry.IP = net.ParseIP(ip)
	}
	if err := s.Audit.Append(r.Context(), entry); err != nil {
		obs.AdminAuditWriteFailuresTotal.WithLabelValues(surface).Inc()
		s.logger.Warn(what+": audit append failed (best-effort)",
			"err", err, "key_id", entry.TargetID)
	}
}

// defaultAccountKeyQuota is the self-service active-key ceiling per identifier when [Options.AccountKeyQuota] is unset; 25
// matches the free tier's [platform.Tier.MaxActiveKeys] and turns an unbounded mint loop into a 409.
const defaultAccountKeyQuota = 25

// mintAccountKey issues a self-service key under the per-identifier cap, enforced inside the store's critical section
// ([auth.RedisAPIKeyStore.CreateCapped]) so a concurrent burst cannot all read a count below the cap. An unverifiable count
// fails CLOSED (503); returns false after writing the response.
func (s *Server) mintAccountKey(w http.ResponseWriter, r *http.Request, req auth.CreateAPIKeyRequest) (auth.APIKeyRecord, string, bool) {
	quota := s.AccountKeyQuota
	if quota == 0 {
		quota = defaultAccountKeyQuota
	}
	var (
		rec       auth.APIKeyRecord
		plaintext string
		err       error
	)
	if quota < 0 { // explicitly disabled by the operator
		rec, plaintext, err = s.Accounts.Create(r.Context(), req)
	} else {
		rec, plaintext, err = s.Accounts.CreateCapped(r.Context(), req, quota)
	}
	if err == nil {
		return rec, plaintext, true
	}
	if clientAborted(r, err) {
		return auth.APIKeyRecord{}, "", false
	}
	var over *auth.KeyQuotaExceededError
	switch {
	case errors.As(err, &over):
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/key-quota-exceeded",
			"API key quota reached", http.StatusConflict,
			fmt.Sprintf("this account already holds %d active API keys (max %d) — revoke one via DELETE /v1/account/keys/{keyID} first",
				over.Active, over.Max))
	case errors.Is(err, auth.ErrKeyQuotaUnavailable):
		s.logger.Error("account key quota check failed", "err", err, "identifier", req.Identifier)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-store-unavailable",
			"Could not verify key quota", http.StatusServiceUnavailable,
			"the key store could not be read to count existing keys; retry shortly")
	default:
		s.logger.Error("account key create failed", "err", err, "identifier", req.Identifier)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-create-failed",
			"Could not issue key", http.StatusInternalServerError,
			"see X-Request-ID in server logs")
	}
	return auth.APIKeyRecord{}, "", false
}

// handleAccountKeysList serves GET /v1/account/keys: public-safe records for the caller's Identifier, oldest first. Anonymous
// 401; no store 503.
func (s *Server) handleAccountKeysList(w http.ResponseWriter, r *http.Request) {
	subject, ok := auth.SubjectFrom(r.Context())
	if !ok || subject.Tier == auth.TierAnonymous || subject.Tier == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/unauthorized",
			"Authentication required", http.StatusUnauthorized,
			"/v1/account/keys requires an API key (or a SEP-10 token, on a deployment running auth_mode=sep10 — a deployment accepts one or the other, never both)")
		return
	}
	if s.Accounts == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-store-unavailable",
			"Account store not configured", http.StatusServiceUnavailable,
			"this deployment has no AccountStore wired — typically because Redis is unavailable")
		return
	}

	keys, err := s.Accounts.ListKeysForIdentifier(r.Context(), subject.Identifier)
	if err != nil {
		s.logger.Error("account keys list failed", "err", err,
			"identifier", subject.Identifier)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-list-failed",
			"Could not list keys", http.StatusInternalServerError,
			"see X-Request-ID in server logs")
		return
	}

	// Sort by CreatedAt ascending — oldest first, so a customer sees
	// their original signup key before any rotations.
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].CreatedAt.Before(keys[j].CreatedAt)
	})

	out := make([]Account, 0, len(keys))
	for _, k := range keys {
		out = append(out, Account{
			KeyID:           k.KeyID,
			Label:           k.Label,
			KeyPrefix:       k.KeyPrefix,
			Tier:            string(k.Tier),
			RateLimitPerMin: k.RateLimitPerMin,
			CreatedAt:       WireTime(k.CreatedAt),
		})
	}
	writeJSON(w, out, Flags{})
}

// handleAccountKeysRevoke serves DELETE /v1/account/keys/{keyID}, scoped to the caller's Identifier. Missing key returns 204
// too, so another account's keyIDs do not leak; revoking the key the request used is 409. Operator audit row only when a key
// was actually revoked.
func (s *Server) handleAccountKeysRevoke(w http.ResponseWriter, r *http.Request) {
	subject, ok := auth.SubjectFrom(r.Context())
	if !ok || subject.Tier == auth.TierAnonymous || subject.Tier == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/unauthorized",
			"Authentication required", http.StatusUnauthorized,
			"/v1/account/keys requires an API key (or a SEP-10 token, on a deployment running auth_mode=sep10 — a deployment accepts one or the other, never both)")
		return
	}
	keyID := r.PathValue("keyID")
	if keyID == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-key-id",
			"Missing key id", http.StatusBadRequest,
			"path must be /v1/account/keys/{keyID}")
		return
	}
	if subject.KeyID == keyID {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/cannot-revoke-self",
			"Can't revoke the key you're using", http.StatusConflict,
			"authenticate with a different key (or SEP-10 token) and retry")
		return
	}
	if s.Accounts == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-store-unavailable",
			"Account store not configured", http.StatusServiceUnavailable,
			"this deployment has no AccountStore wired — typically because Redis is unavailable")
		return
	}
	reason, ok := s.operatorReasonOK(w, r, subject)
	if !ok {
		return
	}
	// revokeKeyEverywhere (admin_keys.go) also clears the
	// Postgres management row for a key that has one — a self-service
	// caller can hold a /v1/register-minted key, which mirrors to both
	// stores, alongside Redis-only keys minted via this same endpoint.
	err := s.revokeKeyEverywhere(r.Context(), subject.Identifier, keyID, reason)
	if errors.Is(err, auth.ErrKeyNotFound) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		s.logger.Error("account keys revoke failed", "err", err,
			"identifier", subject.Identifier, "key_id", keyID)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-revoke-failed",
			"Could not revoke key", http.StatusInternalServerError,
			"see X-Request-ID in server logs")
		return
	}
	if subject.Tier == auth.TierOperator {
		s.recordAccountKeyRevokeAudit(r, subject, keyID, reason)
	}
	w.WriteHeader(http.StatusNoContent)
}
