package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/StellarIndex/stellar-index/internal/auth"
	"github.com/StellarIndex/stellar-index/internal/platform"
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
	CreatedAt       time.Time    `json:"created_at,omitempty"`
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
	EmailVerifiedAt time.Time `json:"email_verified_at,omitempty"`
	LastLoginAt     time.Time `json:"last_login_at,omitempty"`
}

// AccountInfo is the magic-link-session caller's parent account.
type AccountInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Slug   string `json:"slug,omitempty"`
	Tier   string `json:"tier,omitempty"`
	Status string `json:"status,omitempty"`
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
// + 5xx responses, throttled = 429 rate-limit rejections. Note
// `requests` counts ALLOWED traffic only (throttled requests are
// tallied separately and never eat monthly quota).
//
// On the fallback path (rollup reader unwired or not yet swept)
// rows degrade to the legacy shape: one row per day, Endpoint
// empty, errors/throttled zero. Clients sum `requests` grouped by
// `date` for daily totals in either shape.
type UsageRow struct {
	Date      string `json:"date"`               // YYYY-MM-DD
	Endpoint  string `json:"endpoint,omitempty"` // route pattern; empty on the legacy fallback
	Requests  int    `json:"requests"`
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
// YYYY-MM-DD UTC; Requests is the daily INCR count.
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
// boundary. Requests counts allowed traffic (all non-429 outcomes);
// Errors is 4xx (excl. 429) + 5xx; Throttled is 429s.
type UsageEndpointDay struct {
	Date      string // YYYY-MM-DD UTC
	Endpoint  string // route pattern
	Requests  int64
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
	Label     string   `json:"label,omitempty"`
	Scopes    []string `json:"scopes,omitempty"`
}

// createKeyRequest is the inbound POST body. The server adopts the
// caller's Identifier (so callers can only mint keys that share
// their owner reference) and ignores Tier — the new key inherits
// the caller's tier verbatim. Operator callers mint for other
// identifiers/tiers via POST /v1/admin/keys.
//
// Scopes is optional: absent/empty mints a full-access key (the
// pre-scopes posture); non-empty confines the key to the listed
// route families (platform.KnownKeyScopes vocabulary). A caller can
// only NARROW — scopes never grant anything the key's tier wouldn't
// already reach.
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
					EmailVerifiedAt: sess.EmailVerifiedAt,
					LastLoginAt:     sess.LastLoginAt,
				},
				AccountInfo: &AccountInfo{
					ID:     sess.AccountID,
					Name:   sess.AccountName,
					Slug:   sess.AccountSlug,
					Tier:   sess.AccountTier,
					Status: sess.AccountStatus,
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
		CreatedAt:       subject.CreatedAt,
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
// (fresh deployment / worker not yet swept).
//
// Subject keying matches `middleware.UsageTracker`'s
// `usageKeyForSubject` so the writer + both readers stay in
// lock-step (`key:<KeyID>` for API-key callers; `id:<Identifier>`
// when KeyID is empty). Anonymous callers receive 401. The
// `?from=` / `?to=` query params are reserved in the OpenAPI spec
// but ignored — every successful response is the trailing 30-day
// window today; full from/to honouring lands when an operator
// surface needs it.
//
// Backend-absent posture: the handler returns `[]` in the
// wire-shape envelope (200 OK with an empty data array). Callers
// that distinguish "no usage reported" from "usage backend not
// wired" can probe `/v1/readyz` (NOT `/healthz` — the
// per-dependency `checks` field is `/readyz`-only).
func (s *Server) handleAccountUsage(w http.ResponseWriter, r *http.Request) {
	subject, ok := auth.SubjectFrom(r.Context())
	if !ok || subject.Tier == auth.TierAnonymous || subject.Tier == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/unauthorized",
			"Authentication required", http.StatusUnauthorized,
			"/v1/account/usage requires an API key or SEP-10 token")
		return
	}
	// Mirror UsageTracker's subject derivation (key:<KeyID> or
	// id:<Identifier>). MUST stay in sync — if the writer and
	// reader pick different keys, /v1/account/usage returns []
	// despite incoming requests being recorded.
	key := ""
	switch {
	case subject.KeyID != "":
		key = "key:" + subject.KeyID
	case subject.Identifier != "":
		key = "id:" + subject.Identifier
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

// readUsageRollup reads the per-endpoint rollups for the subject.
// ok=false means "fall back to the legacy per-day totals": reader
// unwired, read error, or zero rows.
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
	for i, d := range days {
		out[i] = UsageRow{
			Date:      d.Date,
			Endpoint:  d.Endpoint,
			Requests:  int(d.Requests),
			Errors:    int(d.Errors),
			Throttled: int(d.Throttled),
		}
	}
	return out, true
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
		out[i] = UsageRow{
			Date:     d.Date,
			Requests: int(d.Requests),
		}
	}
	return out
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
func (s *Server) handleAccountKeysCreate(w http.ResponseWriter, r *http.Request) {
	subject, ok := auth.SubjectFrom(r.Context())
	if !ok || subject.Tier == auth.TierAnonymous || subject.Tier == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/unauthorized",
			"Authentication required", http.StatusUnauthorized,
			"/v1/account/keys requires an API key or SEP-10 token")
		return
	}
	if s.accounts == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-store-unavailable",
			"Account store not configured", http.StatusServiceUnavailable,
			"this deployment has no AccountStore wired — typically because Redis is unavailable")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4*1024))
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/body-too-large",
			"Request body too large", http.StatusBadRequest,
			"/v1/account/keys body must be under 4 KiB")
		return
	}
	var req createKeyRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-body",
				"Malformed JSON body", http.StatusBadRequest,
				"could not parse request body as JSON")
			return
		}
	}
	if req.Label == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-label",
			"Label is required", http.StatusBadRequest,
			"the new key needs a label so the customer can identify it later")
		return
	}
	if len(req.Label) > 128 {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/label-too-long",
			"Label too long", http.StatusBadRequest,
			"label must be 128 characters or fewer")
		return
	}

	scopes, problem := validateScopes(req.Scopes)
	if problem != "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-scope",
			"Invalid scope", http.StatusBadRequest, problem)
		return
	}

	rec, plaintext, err := s.accounts.Create(r.Context(), auth.CreateAPIKeyRequest{
		Identifier: subject.Identifier,
		Label:      req.Label,
		Tier:       subject.Tier,
		Scopes:     scopes,
		// Inherit the caller's per-key budget when set; otherwise
		// leave zero so the per-tier default applies.
		RateLimitPerMin: subject.RateLimitPerMin,
	})
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
			"/v1/account/keys requires an API key or SEP-10 token")
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
			CreatedAt:       k.CreatedAt,
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
			"/v1/account/keys requires an API key or SEP-10 token")
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
	if err := s.accounts.RevokeKeyByID(r.Context(), subject.Identifier, keyID); err != nil {
		s.logger.Error("account keys revoke failed", "err", err,
			"identifier", subject.Identifier, "key_id", keyID)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/account-revoke-failed",
			"Could not revoke key", http.StatusInternalServerError,
			"see X-Request-ID in server logs")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
