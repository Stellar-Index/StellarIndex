package v1

import (
	"context"
	"encoding/json"
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

// AccountStore is the v1 boundary against [auth.APIKeyStore].
// Two consumers today: [Server.handleAccountKeysCreate] (POST)
// and [Server.handleAccountKeysList] (GET). Production wiring is
// [auth.RedisAPIKeyStore] which provides both methods.
type AccountStore interface {
	Create(ctx context.Context, req auth.CreateAPIKeyRequest) (auth.APIKeyRecord, string, error)
	ListKeysForIdentifier(ctx context.Context, identifier string) ([]auth.APIKeyRecord, error)
	RevokeKeyByID(ctx context.Context, identifier, keyID string) error
}

// Account is the wire shape for /v1/account/me responses. Mirrors
// the OpenAPI Account schema; the field set is the public-safe
// projection of [auth.APIKeyRecord] (no expires_at / scopes
// surfaced — those are implementation detail until /v1/account/keys
// list returns them).
//
// The shape is a union: API-key callers populate the top-level
// key_* / tier / rate_limit_per_min / created_at fields and leave
// `user` + `account` null. Magic-link session callers populate the
// nested `user` + `account` objects (and leave the API-key fields
// empty). Clients can detect which mode by checking which slice is
// populated. Both shapes coexist forever — bumping a major version
// for an additive field would be silly.
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
	ID              string   `json:"id"`
	Email           string   `json:"email"`
	DisplayName     string   `json:"display_name,omitempty"`
	Role            string   `json:"role,omitempty"`
	IsStaff         bool     `json:"is_staff"`
	EmailVerifiedAt WireTime `json:"email_verified_at,omitempty"`
	LastLoginAt     WireTime `json:"last_login_at,omitempty"`
}

// AccountInfo is the magic-link-session caller's parent account.
//
// RateLimitPerMin / MonthlyRequestQuota are what auth enforces on a key
// minted at the dashboard defaults, account override included
// ([platform.Account.EffectiveRateLimitPerMin]) — not the tier ceiling.
// Limits are per key, so an explicitly budgeted key can differ.
type AccountInfo struct {
	ID                  string `json:"id"`
	Name                string `json:"name,omitempty"`
	Slug                string `json:"slug,omitempty"`
	Tier                string `json:"tier,omitempty"`
	Status              string `json:"status,omitempty"`
	RateLimitPerMin     int    `json:"rate_limit_per_min,omitempty"`
	MonthlyRequestQuota int64  `json:"monthly_request_quota,omitempty"`
}

// SessionInfo is the wire-shape projection of a magic-link
// session. Defined in v1 so this package doesn't import
// dashboardauth directly; the binary's wiring (main.go) converts
// dashboardauth's SessionContext into this shape.
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

// SessionPeeker reads the magic-link session bound to the request
// context. Implementations come from the dashboardauth bundle via
// main.go's wiring; v1 holds the interface so the dependency
// flows the right way.
type SessionPeeker interface {
	SessionFromContext(ctx context.Context) (SessionInfo, bool)
}

// UsageRow is the wire shape for /v1/account/usage entries.
//
// When the `usage_daily` rollups are wired (production), the list
// carries one row per (day, endpoint family) with Endpoint set to
// the route pattern (e.g. "/v1/assets/{asset_id}") and the
// errors / throttled columns filled: errors = 4xx (excluding 429)
// + 5xx responses, throttled = 429 rate-limit rejections, and
// `requests` = every non-429 outcome, 5xx included.
//
// On the fallback path (rollup reader unwired or not yet swept)
// rows degrade to the legacy shape: one row per day, Endpoint
// empty, errors/throttled zero. The legacy store only holds the
// billable total, so there `requests` excludes 5xx as well.
//
// `billable` is the one column with the same meaning on both paths:
// the request units the monthly quota counts (ok + 4xx; never 429 or
// platform-caused 5xx (COR-05), though not a timed-out read — see
// middleware.billableClass). Sum it by `date` to reconcile against a
// quota 429's `month_to_date`.
type UsageRow struct {
	Date      string `json:"date"`               // YYYY-MM-DD
	Endpoint  string `json:"endpoint,omitempty"` // route pattern; empty on the legacy fallback
	Requests  int    `json:"requests"`
	Billable  int    `json:"billable"`
	Errors    int    `json:"errors"`
	Throttled int    `json:"throttled"`
}

// UsageReader is the storage seam for /v1/account/usage. The
// internal/usage package's *Counter implements via its Read method;
// main.go's adapter bridges so this package stays free of the
// usage package import.
type UsageReader interface {
	Read(ctx context.Context, subject string, days int) ([]UsageDay, error)
}

// UsageDay mirrors usage.Day on the v1 boundary. Date is
// YYYY-MM-DD UTC; Requests is the day's BILLABLE request-unit total
// (the MonthlyQuota counter — 429 and 5xx never reach it).
type UsageDay struct {
	Date     string
	Requests int64
}

// UsageRollupReader is the storage seam for the per-endpoint usage
// rollups (`usage_daily` hypertable, maintained by the API binary's
// usage-rollup worker). main.go's adapter bridges
// *timescale.Store.ReadUsageDaily so this package stays free of the
// storage import.
type UsageRollupReader interface {
	ReadRollup(ctx context.Context, subject string, days int) ([]UsageEndpointDay, error)
}

// UsageEndpointDay is one (day, endpoint) aggregate on the v1
// boundary. Requests counts all non-429 outcomes, 5xx included;
// Billable is ok + 4xx — what the monthly quota counts; Errors is
// 4xx (excl. 429) + 5xx; Throttled is 429s.
type UsageEndpointDay struct {
	Date      string // YYYY-MM-DD UTC
	Endpoint  string // route pattern
	Requests  int64
	Billable  int64
	Errors    int64
	Throttled int64
}

// KeyCreated is the wire shape for /v1/account/keys (POST) replies.
// The plaintext appears here exactly once — clients that drop the
// response can never recover it.
type KeyCreated struct {
	KeyID     string   `json:"key_id"`
	Plaintext string   `json:"plaintext"`
	KeyPrefix string   `json:"key_prefix,omitempty"`
	Label     string   `json:"label"`
	Scopes    []string `json:"scopes,omitempty"`
}

// createKeyRequest is the inbound POST body. The server adopts the
// caller's Identifier (so callers can only mint keys that share
// their owner reference) and ignores Tier — the new key inherits
// the caller's tier verbatim. Operator callers mint for other
// identifiers/tiers via POST /v1/admin/keys; an operator rotating
// its OWN credential here is held to the same audit contract as
// that path (X-Reason header + persisted key.mint row).
//
// Scopes is optional: for a full-access caller (empty scope list),
// absent/empty mints a full-access key (the pre-scopes posture);
// non-empty confines the key to the listed route families
// (platform.KnownKeyScopes vocabulary). A SCOPED caller can only
// delegate a subset of its own scopes — an empty request inherits the
// caller's scopes rather than granting full access, and any scope the
// caller does not itself hold is rejected (see middleware.ClampMintScopes).
// A caller can only NARROW — scopes never grant anything the key's tier
// wouldn't already reach, and the child inherits the caller's expiry,
// quota and rate limit (see auth.ChildKeyRequest).
type createKeyRequest struct {
	Label  string   `json:"label"`
	Scopes []string `json:"scopes,omitempty"`
}

// validateScopes normalises + validates a mint request's scope
// list against the platform vocabulary. Returns ("", true) on
// success (with duplicates removed) or (problem detail, false).
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

// handleAccountMe serves GET /v1/account/me.
//
// Returns the authenticated caller's account info. Magic-link
// session callers populate the nested user/account objects;
// API-key callers populate the top-level key_* fields. Both
// flows can coexist on a request — session takes precedence
// because it identifies a real user, while a key only
// identifies a credential.
//
// Anonymous callers receive 401 — /me is meaningless without
// any credential.
func (s *Server) handleAccountMe(w http.ResponseWriter, r *http.Request) {
	// Magic-link session takes precedence when both are present.
	if s.sessionPeeker != nil {
		if sess, ok := s.sessionPeeker.SessionFromContext(r.Context()); ok {
			out := Account{
				User: &AccountUser{
					ID:              sess.UserID,
					Email:           sess.Email,
					DisplayName:     sess.DisplayName,
					Role:            sess.Role,
					IsStaff:         sess.IsStaff,
					EmailVerifiedAt: WireTime(sess.EmailVerifiedAt),
					LastLoginAt:     WireTime(sess.LastLoginAt),
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

// handleAccountUsage serves GET /v1/account/usage.
//
// Preferred path: the [UsageRollupReader] seam over the
// `usage_daily` Timescale hypertable (maintained by the API
// binary's usage-rollup worker) — one row per (day, endpoint
// family) over the trailing 30 days, with errors + throttled
// filled. Fallback path: the legacy [UsageReader] per-day Redis
// totals (one row per day, no endpoint) when the rollup reader is
// unwired, errors, or hasn't produced rows for this subject yet
// (fresh deployment / worker not yet swept). Any day the rollup
// reader is missing entirely (a worker-outage gap, not "zero
// traffic") is backfilled from the legacy reader rather than
// silently dropped (Q160).
//
// Subject keying calls [middleware.UsageKeyForSubject] directly (HLT-01:
// this used to reimplement the derivation inline, which could silently
// drift from the writer's copy) so the writer + both readers stay in
// lock-step (`id:<Identifier>` — the OWNER ACCOUNT — with `key:<KeyID>`
// only for credentials carrying no owner reference). The account key is
// what makes this endpoint's name true: the rows cover every key the
// account holds, not just the one that authenticated the call, and they
// survive a key rotation (RLT-404). The `?from=` / `?to=` query params
// are reserved in the OpenAPI spec but ignored — every successful
// response is the trailing 30-day window today; full from/to honouring
// lands when an operator surface needs it.
//
// A magic-link dashboard session authenticates this route too, same
// precedence as [Server.handleAccountMe]: a session identifies the
// account directly, so it reads under that account's key
// (`id:acct:<slug>`, via [usageKeyForSession]) without needing the caller
// to also hold an API key (GH #796 / RLT-415 — every signed-in dashboard
// user with only a session cookie used to 401 here, and the frontend
// silently swallowed it into an empty usage page). Anonymous callers
// (neither session nor API key) receive 401.
//
// Backend-absent posture: the handler returns `[]` in the
// wire-shape envelope (200 OK with an empty data array). Callers
// that distinguish "no usage reported" from "usage backend not
// wired" can probe `/v1/readyz` (NOT `/healthz` — the
// per-dependency `checks` field is `/readyz`-only).
func (s *Server) handleAccountUsage(w http.ResponseWriter, r *http.Request) {
	key := ""
	if s.sessionPeeker != nil {
		if sess, ok := s.sessionPeeker.SessionFromContext(r.Context()); ok {
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

// usageKeyForSession derives the account-scoped usage key a
// dashboard session reads under — the SAME key an API key minted on
// that account writes under ([middleware.UsageKeyForSubject]'s
// Identifier branch: `id:` + [auth.AccountIdentifier]). Routes
// through the shared derivation rather than duplicating the `id:`
// prefix so the two can never drift apart; the synthetic Subject's
// Tier only needs to clear UsageKeyForSubject's anonymous guard and
// never leaves this function.
func usageKeyForSession(accountSlug string) string {
	if accountSlug == "" {
		return ""
	}
	return middleware.UsageKeyForSubject(auth.Subject{
		Identifier: auth.AccountIdentifier(accountSlug),
		Tier:       auth.TierAPIKey,
	})
}

// readUsageRollup reads the per-endpoint rollups for the subject and
// backfills any day the rollup worker produced NO row for at all from
// the legacy per-day reader (Q160), so a worker-outage gap in the
// middle of the 30-day window doesn't silently vanish from the
// response — only "the rollup reader itself produced nothing usable"
// (unwired, read error, or zero rows) falls back to the legacy shape
// entirely; ok=false means that.
func (s *Server) readUsageRollup(r *http.Request, key string) ([]UsageRow, bool) {
	if s.usageRollupReader == nil {
		return nil, false
	}
	days, err := s.usageRollupReader.ReadRollup(r.Context(), key, 30)
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

// backfillMissingUsageDays fills in, from the legacy per-day reader,
// any day within the trailing 30-day window that `present` (the days
// the rollup reader actually produced a row for) has no entry for at
// all — a rollup-worker gap, not a legitimate zero-traffic day (which
// the rollup reader still emits a row for). Best-effort: a legacy
// read failure just means no backfill, same posture as
// [Server.readUsageLegacy].
func (s *Server) backfillMissingUsageDays(r *http.Request, key string, present map[string]struct{}) []UsageRow {
	if s.usageReader == nil {
		return nil
	}
	legacyDays, err := s.usageReader.Read(r.Context(), key, 30)
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

// readUsageLegacy reads the per-day Redis totals (no endpoint
// dimension). Every failure degrades to the locked empty-list wire
// shape rather than a 5xx — usage is a dashboard nicety, never
// worth failing a customer integration over.
func (s *Server) readUsageLegacy(r *http.Request, key string) []UsageRow {
	if s.usageReader == nil {
		return []UsageRow{}
	}
	days, err := s.usageReader.Read(r.Context(), key, 30)
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

// handleAccountKeysCreate serves POST /v1/account/keys.
//
// Issues a fresh API key for the authenticated caller. The new key
// inherits the caller's Identifier and Tier — a paid customer
// rotates their own credentials without escalating; an operator
// uses a separate admin path (not yet shipped) to mint keys for
// other identifiers.
//
// Anonymous → 401. Missing/empty body → 400. Store unavailable →
// 503 (the binary didn't wire one because Redis was missing).
//
// Operator-tier callers keep tier inheritance (this is the staff
// rotation path) but pay the admin-write price for it: X-Reason is
// required (400 without) and the mint lands a "key.mint" audit row
// naming the actor, exactly as POST /v1/admin/keys does. Pre-fix an
// operator credential could spawn further operator credentials here
// with no reason and no audit trail (api-security-1, audit
// 2026-08-28).
func (s *Server) handleAccountKeysCreate(w http.ResponseWriter, r *http.Request) {
	subject, ok := auth.SubjectFrom(r.Context())
	if !ok || subject.Tier == auth.TierAnonymous || subject.Tier == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/unauthorized",
			"Authentication required", http.StatusUnauthorized,
			"/v1/account/keys requires an API key (or a SEP-10 token, on a deployment running auth_mode=sep10 — a deployment accepts one or the other, never both)")
		return
	}
	if s.accounts == nil {
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

	// Delegation clamp: a scoped caller may only mint a child whose
	// scopes are a subset of its own, and an empty request from a
	// scoped caller inherits the caller's scopes rather than defaulting
	// to full access. Without this a key narrowed at mint could mint an
	// unrestricted sibling and escape its own confinement.
	scopes, problem = middleware.ClampMintScopes(subject, scopes)
	if problem != "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/scope-exceeds-caller",
			"Scope exceeds caller", http.StatusForbidden, problem)
		return
	}

	if !s.accountKeyQuotaOK(w, r, subject.Identifier) {
		return
	}

	// Every delegated dimension (tier, rate limit, monthly quota, expiry,
	// email verification) is inherited from the caller, never defaulted:
	// a child from a metered or time-boxed key is metered and time-boxed.
	rec, plaintext, err := s.accounts.Create(r.Context(),
		auth.ChildKeyRequest(subject, req.Label, scopes))
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("account key create failed", "err", err, "identifier", subject.Identifier)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-create-failed",
			"Could not issue key", http.StatusInternalServerError,
			"see X-Request-ID in server logs")
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

// operatorReasonOK enforces the admin-write X-Reason contract
// (platform-spec §7.2) on the self-service key routes when — and only
// when — the caller is operator-tier. Customer callers are untouched:
// their mints/revokes are bounded to their own identifier and tier and
// are not staff actions. Returns ok=false when it has already written
// the 400.
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

// recordAccountKeyMintAudit logs and persists the "key.mint" audit row
// for an operator-tier self-service mint — same shape as the admin mint
// so one query over key.mint rows finds every operator credential ever
// issued, whichever route minted it; "route" tells the two apart.
// Best-effort — same contract as recordAdminKeyMintAudit: a sink
// failure logs at WARN and never blocks the mint.
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
	if s.audit == nil {
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

// recordAccountKeyRevokeAudit logs and persists the "key.revoke" audit
// row for an operator-tier self-service revoke. Best-effort, same
// contract as recordAdminKeyRevokeAudit — the revoke already happened
// and must not be undone by an audit-sink outage.
func (s *Server) recordAccountKeyRevokeAudit(
	r *http.Request, actor auth.Subject, keyID, reason string,
) {
	s.logger.Info("account key revoke (operator)",
		"actor_key_id", actor.KeyID,
		"actor_identifier", actor.Identifier,
		"key_id", keyID,
		"reason", reason)
	if s.audit == nil {
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

// appendKeyAudit stamps the request-derived fields (UA, IP, timestamp)
// onto entry and appends it best-effort, counting a sink failure under
// the given AdminAuditWriteFailuresTotal surface label (C3-067).
func (s *Server) appendKeyAudit(r *http.Request, entry platform.AuditEntry, surface, what string) {
	entry.UserAgent = r.UserAgent()
	entry.Timestamp = time.Now().UTC()
	if ip := middleware.RemoteIP(r); ip != "" {
		entry.IP = net.ParseIP(ip)
	}
	if err := s.audit.Append(r.Context(), entry); err != nil {
		obs.AdminAuditWriteFailuresTotal.WithLabelValues(surface).Inc()
		s.logger.Warn(what+": audit append failed (best-effort)",
			"err", err, "key_id", entry.TargetID)
	}
}

// defaultAccountKeyQuota is the self-service active-key ceiling one
// caller identifier may hold when [Options.AccountKeyQuota] is unset.
//
// 25 is the repo's own pre-existing key cap — the flat
// `MaxKeysPerAccount` the dashboard mint shipped with before F-1257
// replaced it with the tier ladder ([platform.Tier.MaxActiveKeys], free
// 5 → enterprise 250). Reusing that number rather than picking a new one
// means the bound is provably above what any legitimate caller holds
// (nobody rotates into 25 concurrent self-service credentials) while
// still turning an unbounded mint loop into a 409 — the conservative
// direction on a surface where the alternative is an operator-visible
// regression for a paying customer. Operators tune per deployment via
// [Options.AccountKeyQuota].
const defaultAccountKeyQuota = 25

// accountKeyQuotaOK enforces the per-identifier active-key cap before a
// self-service mint. Returns false when it has already written the
// response.
//
// C3-015 (audit-2026-07-23): POST /v1/account/keys minted on every call
// with no count check, and [auth.RedisAPIKeyStore.Create] writes the
// record unconditionally — so one authenticated caller could mint keys in
// a loop, each one a live credential and a permanent Redis record, until
// the store filled. The parallel dashboard path has enforced a quota
// (checkQuota + the store's atomic maxKeys gate) since F-1257; this is
// the same gate for the surface that never got one.
//
// A read failure fails CLOSED (503, no mint): the check exists precisely
// because an unbounded mint is the abuse, so "couldn't count, mint
// anyway" would hand the abuser the bypass. That mirrors the dashboard
// path, whose checkQuota also refuses on a list error.
func (s *Server) accountKeyQuotaOK(w http.ResponseWriter, r *http.Request, identifier string) bool {
	quota := s.accountKeyQuota
	switch {
	case quota < 0:
		return true // explicitly disabled by the operator
	case quota == 0:
		quota = defaultAccountKeyQuota
	}
	existing, err := s.accounts.ListKeysForIdentifier(r.Context(), identifier)
	if err != nil {
		if clientAborted(r, err) {
			return false
		}
		s.logger.Error("account key quota check failed", "err", err, "identifier", identifier)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-store-unavailable",
			"Could not verify key quota", http.StatusServiceUnavailable,
			"the key store could not be read to count existing keys; retry shortly")
		return false
	}
	active := 0
	for _, k := range existing {
		if k.RevokedAt.IsZero() {
			active++
		}
	}
	if active >= quota {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/key-quota-exceeded",
			"API key quota reached", http.StatusConflict,
			fmt.Sprintf("this account already holds %d active API keys (max %d) — revoke one via DELETE /v1/account/keys/{keyID} first",
				active, quota))
		return false
	}
	return true
}

// handleAccountKeysList serves GET /v1/account/keys.
//
// Returns every API key whose Identifier matches the authenticated
// caller's. Mirrors the /v1/account/me wire shape but as a list —
// each entry is a public-safe APIKeyRecord projection (no plaintext
// — that's only retrievable at Create time, by design).
//
// Anonymous → 401. Store unavailable → 503. Authenticated callers
// always get a list (possibly empty if all their keys were
// previously revoked, though revocation isn't shipped today).
//
// Sorted by CreatedAt ascending so customers see their original
// signup key first and rotated keys later.
func (s *Server) handleAccountKeysList(w http.ResponseWriter, r *http.Request) {
	subject, ok := auth.SubjectFrom(r.Context())
	if !ok || subject.Tier == auth.TierAnonymous || subject.Tier == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/unauthorized",
			"Authentication required", http.StatusUnauthorized,
			"/v1/account/keys requires an API key (or a SEP-10 token, on a deployment running auth_mode=sep10 — a deployment accepts one or the other, never both)")
		return
	}
	if s.accounts == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-store-unavailable",
			"Account store not configured", http.StatusServiceUnavailable,
			"this deployment has no AccountStore wired — typically because Redis is unavailable")
		return
	}

	keys, err := s.accounts.ListKeysForIdentifier(r.Context(), subject.Identifier)
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

// handleAccountKeysRevoke serves DELETE /v1/account/keys/{keyID}.
//
// Revokes the API key whose KeyID matches the path parameter,
// scoped to the authenticated caller's Identifier. Anonymous → 401;
// missing keyID → 400; store unwired → 503; everything else → 204
// (including "key not found" — we don't leak whether a keyID
// exists for a different account).
//
// Caller cannot revoke the key they're authenticated with — that
// would orphan the connection mid-request. We return 409 in that
// case so the UI can prompt for an alternate key + retry.
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
	if s.accounts == nil {
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
	// revokeKeyEverywhere (admin_keys.go, GH-978) also clears the
	// Postgres management row for a key that has one — a self-service
	// caller can hold a /v1/register-minted key, which mirrors to both
	// stores, alongside Redis-only keys minted via this same endpoint.
	if err := s.revokeKeyEverywhere(r.Context(), subject.Identifier, keyID, reason); err != nil {
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
