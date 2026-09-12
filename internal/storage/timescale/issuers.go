package timescale

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// IssuerRow is the read-side projection of one row from the
// `issuers` table. Auth flags are pointers so callers can
// distinguish "we know the value" from "no observation yet."
type IssuerRow struct {
	GStrkey    string
	HomeDomain string
	OrgName    string // sep1_payload->>'OrgName' — empty when SEP-1 not fetched
	// OrgVerified is true only when the SEP-1 toml's [[CURRENCIES]] lists this
	// issuer back (bidirectional proof). Without it, OrgName is issuer-self-
	// declared and must NOT be rendered as authoritative (CS-100 impersonation).
	OrgVerified   bool
	AuthRequired  *bool
	AuthRevocable *bool
	AuthImmutable *bool
	AuthClawback  *bool
	// AuthFlagsSource is how the four auth flags above were obtained — one
	// of the AuthFlagsSource* constants, or "" when the provenance is not
	// known (flags unresolved, or persisted before migration 0153). Empty
	// means UNKNOWN, never "current": a consumer must not read the absence
	// of a label as a claim that the reading is live (#374).
	AuthFlagsSource string
	// AuthFlagsAsOfLedger is the ledger the flags are true as of. nil when
	// not known.
	AuthFlagsAsOfLedger *uint32
	SEP1ResolvedAt      *string // RFC 3339; pointer for nullable column
	SEP1Payload         json.RawMessage
	CreationLedger      *uint32
}

// GetIssuer returns the row for one G-strkey. Returns sql.ErrNoRows
// when the issuer hasn't been observed yet.
func (s *Store) GetIssuer(ctx context.Context, gStrkey string) (IssuerRow, error) {
	const q = `
		SELECT
		    g_strkey,
		    COALESCE(home_domain, ''),
		    COALESCE(sep1_payload->>'OrgName', '') AS org_name,
		    COALESCE((sep1_payload->>'OrgVerified')::boolean, false) AS org_verified,
		    auth_required,
		    auth_revocable,
		    auth_immutable,
		    auth_clawback,
		    auth_flags_source,
		    auth_flags_as_of_ledger,
		    to_char(sep1_resolved_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		    sep1_payload,
		    creation_ledger
		  FROM issuers
		 WHERE g_strkey = $1
	`
	var (
		row              IssuerRow
		authReq, authRev sql.NullBool
		authImm, authClb sql.NullBool
		flagsSource      sql.NullString
		flagsAsOf        sql.NullInt64
		resolvedAt       sql.NullString
		payload          sql.NullString
		creation         sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, q, gStrkey).Scan(
		&row.GStrkey,
		&row.HomeDomain,
		&row.OrgName,
		&row.OrgVerified,
		&authReq, &authRev, &authImm, &authClb,
		&flagsSource, &flagsAsOf,
		&resolvedAt, &payload, &creation,
	)
	if err != nil {
		return IssuerRow{}, err
	}
	if authReq.Valid {
		v := authReq.Bool
		row.AuthRequired = &v
	}
	if authRev.Valid {
		v := authRev.Bool
		row.AuthRevocable = &v
	}
	if authImm.Valid {
		v := authImm.Bool
		row.AuthImmutable = &v
	}
	if authClb.Valid {
		v := authClb.Bool
		row.AuthClawback = &v
	}
	if flagsSource.Valid {
		row.AuthFlagsSource = flagsSource.String
	}
	if flagsAsOf.Valid {
		v := uint32(flagsAsOf.Int64) //nolint:gosec // ledger sequences are uint32 on the wire; the column is `integer`
		row.AuthFlagsAsOfLedger = &v
	}
	if resolvedAt.Valid {
		v := resolvedAt.String
		row.SEP1ResolvedAt = &v
	}
	if payload.Valid {
		row.SEP1Payload = json.RawMessage(payload.String)
	}
	if creation.Valid {
		v := uint32(creation.Int64) //nolint:gosec
		row.CreationLedger = &v
	}
	return row, nil
}

// IssuerSummary is one entry in the issuer-directory listing —
// the (g_strkey, optional home_domain, optional org_name, total
// observation count across all issued assets, asset count)
// tuple. Returned by [Store.ListIssuers].
//
// OrgName comes from the SEP-1 payload's `OrgName` field
// (typically `[DOCUMENTATION].ORG_NAME` in stellar.toml).
// Empty when the SEP-1 fetcher hasn't refreshed for this issuer
// yet, or when the toml has no documentation block.
type IssuerSummary struct {
	GStrkey    string
	HomeDomain string
	OrgName    string
	// OrgVerified is true only when the SEP-1 toml's [[CURRENCIES]] lists this
	// issuer back (bidirectional verification). Callers must only merge/group
	// issuers by org when this is true — OrgName alone is spoofable.
	OrgVerified           bool
	AssetCount            int64
	TotalObservationCount int64
}

// ListIssuers returns the issuer directory ordered by total
// observation count desc — the proxy-for-activity ranking the
// /v1/issuers endpoint exposes. limit clamps to [1, 500].
//
// Joins issuers with classic_assets and aggregates so the
// home_domain (when populated by the SEP-1 fetcher) flows through
// without a per-row lookup. issuers without any classic_assets row
// are excluded — without an asset, an issuer entry is just an
// orphan G-strkey we have no activity for.
func (s *Store) ListIssuers(ctx context.Context, limit int) ([]IssuerSummary, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	const q = `
        SELECT i.g_strkey,
               COALESCE(i.home_domain, ''),
               COALESCE(i.sep1_payload->>'OrgName', '') AS org_name,
               COALESCE((i.sep1_payload->>'OrgVerified')::boolean, false) AS org_verified,
               count(c.asset_id)::bigint           AS asset_count,
               COALESCE(sum(c.observation_count), 0)::bigint AS total_obs
          FROM issuers i
          JOIN classic_assets c ON c.issuer_g_strkey = i.g_strkey
         GROUP BY i.g_strkey, i.home_domain, i.sep1_payload
         ORDER BY total_obs DESC, i.g_strkey ASC
         LIMIT $1
    `
	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("timescale: ListIssuers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]IssuerSummary, 0, limit)
	for rows.Next() {
		var r IssuerSummary
		if err := rows.Scan(&r.GStrkey, &r.HomeDomain, &r.OrgName, &r.OrgVerified, &r.AssetCount, &r.TotalObservationCount); err != nil {
			return nil, fmt.Errorf("timescale: ListIssuers scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ListIssuers rows: %w", err)
	}
	return out, nil
}

// IssuerAsset is one entry in the issuer's asset list.
type IssuerAsset struct {
	AssetID          string
	Code             string
	Slug             string
	FirstSeenLedger  uint32
	LastSeenLedger   uint32
	ObservationCount int64
}

// IssuerSep1Candidate is one row returned by IssuersNeedingSep1Refresh —
// the (g_strkey, home_domain) pair for an issuer the SEP-1 fetcher
// should resolve next.
type IssuerSep1Candidate struct {
	GStrkey    string
	HomeDomain string
}

// IssuerSep1CandidateByStrkey returns the (g_strkey, home_domain) pair for one
// specific issuer — the targeted counterpart of IssuersNeedingSep1Refresh, used
// by `sep1-refresh -issuer <g>` to force-refresh a single account on demand
// (e.g. a newly-onboarded verified org) without waiting for it to surface
// through the staleness queue. Returns sql.ErrNoRows if the issuer is unknown,
// or an error with no home_domain if the account has none (nothing to fetch).
func (s *Store) IssuerSep1CandidateByStrkey(ctx context.Context, gStrkey string) (IssuerSep1Candidate, error) {
	const q = `SELECT g_strkey, COALESCE(home_domain, '') FROM issuers WHERE g_strkey = $1`
	var c IssuerSep1Candidate
	if err := s.db.QueryRowContext(ctx, q, gStrkey).Scan(&c.GStrkey, &c.HomeDomain); err != nil {
		return IssuerSep1Candidate{}, fmt.Errorf("timescale: IssuerSep1CandidateByStrkey: %w", err)
	}
	if c.HomeDomain == "" {
		return IssuerSep1Candidate{}, fmt.Errorf("timescale: issuer %s has no home_domain to resolve", gStrkey)
	}
	return c, nil
}

// Sep1RefreshMaxLimit is the ceiling IssuersNeedingSep1Refresh will
// honour for a single run. It exists so a mistyped operator override
// can't ask for the whole 77k-row population in one statement; the run
// deadline is the real budget.
const Sep1RefreshMaxLimit = 5000

// IssuersNeedingSep1Refresh returns up to `limit` issuers whose
// home_domain is set, whose sep1_resolved_at is missing or older than
// `staleness`, and whose retry deferral (migration 0159) has expired.
// Ordered by sep1_resolved_at ASC NULLS FIRST so never-resolved issuers
// + the oldest cached payloads surface first — same fairness rule a
// daemon worker would use.
//
// `staleness` of 0 means "refresh anything" — useful for a forced
// rerun after a code change to the SEP-1 parser. It does NOT override
// the deferral: a domain that has failed its way onto the ladder is
// skipped until sep1_next_attempt_after passes, and `sep1-refresh
// -issuer <G>` is the way to force one.
//
// `limit` is clamped INTO [1, Sep1RefreshMaxLimit] rather than reset to
// a default. It used to snap any out-of-range value to 100, so an
// operator raising LIMIT past the ceiling silently got a FIFTH of the
// old budget instead of more — the failure mode reads as "the job is
// slow", never as "your setting was rejected".
func (s *Store) IssuersNeedingSep1Refresh(ctx context.Context, staleness time.Duration, limit int) ([]IssuerSep1Candidate, error) {
	if limit <= 0 {
		limit = 1
	}
	if limit > Sep1RefreshMaxLimit {
		limit = Sep1RefreshMaxLimit
	}
	// The deferral arm is written `IS NULL OR <= NOW()` and NOT as
	// `COALESCE(sep1_next_attempt_after, NOW()) <= NOW()` so it stays a
	// plain column predicate the planner can answer from
	// issuers_sep1_refresh_queue_idx's INCLUDE payload.
	const q = `
        SELECT g_strkey, home_domain
          FROM issuers
         WHERE home_domain IS NOT NULL
           AND home_domain != ''
           AND (sep1_resolved_at IS NULL
                OR sep1_resolved_at < NOW() - $1::interval)
           AND (sep1_next_attempt_after IS NULL
                OR sep1_next_attempt_after <= NOW())
         ORDER BY sep1_resolved_at ASC NULLS FIRST, g_strkey ASC
         LIMIT $2
    `
	rows, err := s.db.QueryContext(ctx, q, intervalArg(staleness), limit)
	if err != nil {
		return nil, fmt.Errorf("timescale: IssuersNeedingSep1Refresh: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]IssuerSep1Candidate, 0, limit)
	for rows.Next() {
		var c IssuerSep1Candidate
		if err := rows.Scan(&c.GStrkey, &c.HomeDomain); err != nil {
			return nil, fmt.Errorf("timescale: IssuersNeedingSep1Refresh scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: IssuersNeedingSep1Refresh rows: %w", err)
	}
	return out, nil
}

// IssuerSep1Cached is the parsed shape of the `sep1_payload` JSONB
// column. Subset of [metadata.SEP1] limited to the fields the
// `sep1-refresh` cron persists. Returned by [GetIssuerSep1Cached]
// so the API can apply the SEP-1 overlay without making a live
// HTTPS fetch to the issuer's home_domain.
type IssuerSep1Cached struct {
	OrgName       string               `json:"OrgName,omitempty"`
	Version       string               `json:"Version,omitempty"`
	Documentation map[string]string    `json:"Documentation,omitempty"`
	Currencies    []IssuerSep1Currency `json:"Currencies,omitempty"`
	FetchedAt     string               `json:"FetchedAt,omitempty"`
}

// IssuerSep1Currency mirrors [metadata.Currency] — fields the
// /v1/assets/{id} handler overlays per-asset.
type IssuerSep1Currency struct {
	Code            string `json:"Code,omitempty"`
	Issuer          string `json:"Issuer,omitempty"`
	Decimals        int    `json:"Decimals,omitempty"`
	DisplayDecimals int    `json:"DisplayDecimals,omitempty"`
	Name            string `json:"Name,omitempty"`
	Description     string `json:"Description,omitempty"`
	Conditions      string `json:"Conditions,omitempty"`
	Image           string `json:"Image,omitempty"`
	FixedNumber     string `json:"FixedNumber,omitempty"`
	MaxNumber       string `json:"MaxNumber,omitempty"`
	IsUnlimited     bool   `json:"IsUnlimited,omitempty"`
	AnchorAsset     string `json:"AnchorAsset,omitempty"`
	AnchorAssetType string `json:"AnchorAssetType,omitempty"`
	Status          string `json:"Status,omitempty"`
}

// GetIssuerSep1Cached returns the cached SEP-1 payload for an issuer
// G-strkey, parsed from the `issuers.sep1_payload` JSONB column. Returns
// (nil, nil) when the issuer row exists but has no payload yet (the
// sep1-refresh cron hasn't visited it). Returns (nil, sql.ErrNoRows)
// when the issuer is completely unknown.
//
// Replaces the live HTTPS fetch the API used to do per-request via
// [metadata.Resolver.Resolve] — that fetch dominated /v1/assets/{id}
// p95 (4+ seconds on cold issuers). The DB-cached path is one indexed
// SELECT.
func (s *Store) GetIssuerSep1Cached(ctx context.Context, gStrkey string) (*IssuerSep1Cached, error) {
	const q = `SELECT sep1_payload FROM issuers WHERE g_strkey = $1`
	var payload sql.NullString
	if err := s.db.QueryRowContext(ctx, q, gStrkey).Scan(&payload); err != nil {
		return nil, err
	}
	if !payload.Valid || payload.String == "" {
		return nil, nil
	}
	var out IssuerSep1Cached
	if err := json.Unmarshal([]byte(payload.String), &out); err != nil {
		return nil, fmt.Errorf("timescale: GetIssuerSep1Cached: parse: %w", err)
	}
	return &out, nil
}

// Sep1Image is one (code, issuer, image) triple taken from a verified
// issuer's cached SEP-1 [[CURRENCIES]] payload. Only entries carrying a
// non-empty Code, Issuer AND Image are returned.
type Sep1Image struct {
	Code   string
	Issuer string
	Image  string
}

// AllSep1Images returns every populated [[CURRENCIES]] image across all
// issuers whose sep1_payload is set — the raw material for the
// /v1/assets listing logo overlay. It is one indexed scan over the (few
// dozen) verified issuers; the API layer caches the result with a TTL so
// this never runs on the per-request hot path, and applies its own
// URL-scheme safety filter. Malformed payloads are skipped, not fatal.
// sep1ImagesFromPayload extracts the currency images one issuer's
// cached SEP-1 payload is entitled to declare.
//
// The provenance rule is the whole point: a currency entry counts only
// when its declared Issuer names gStrkey — the account whose
// stellar.toml actually carried it. Split out of [Store.AllSep1Images]
// so the rule is testable without a database.
//
// The rule itself is [sep1EntryBindsTo], shared with the bound-currency
// scan rather than restated here: two copies of a provenance check are
// two chances to drift, and a drift between them would mean an entry
// good enough to overlay a logo but not good enough to attest an asset,
// or the reverse.
func sep1ImagesFromPayload(gStrkey, payload string) []Sep1Image {
	var parsed IssuerSep1Cached
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		// One issuer's corrupt payload must not blank the whole map.
		return nil
	}
	out := make([]Sep1Image, 0, len(parsed.Currencies))
	for _, c := range parsed.Currencies {
		code := strings.TrimSpace(c.Code)
		if c.Image == "" || code == "" {
			continue
		}
		if !sep1EntryBindsTo(c.Issuer, gStrkey) {
			continue
		}
		out = append(out, Sep1Image{Code: code, Issuer: gStrkey, Image: c.Image})
	}
	return out
}

func (s *Store) AllSep1Images(ctx context.Context) ([]Sep1Image, error) {
	// g_strkey is selected so each currency's DECLARED issuer can be
	// checked against the account whose stellar.toml actually carried
	// it. Without that check the map was keyed purely on
	// TOML-supplied (code, issuer): any Stellar account could publish
	//
	//     [[CURRENCIES]] code = "USDC" issuer = "<Circle's G-key>"
	//                    image = "https://attacker.example/x.png"
	//
	// and — since nothing here filtered on org_verified either, and
	// projectCatalogueRows assigns the result unconditionally — take
	// over the logo served for USDC on /v1/assets and the explorer
	// homepage, giving a per-visitor beacon under a verified brand
	// (cold audit 2026-08-03). A TOML may still describe only the
	// issuer that served it.
	const q = `SELECT g_strkey, sep1_payload FROM issuers WHERE sep1_payload IS NOT NULL`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("timescale: AllSep1Images: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Sep1Image, 0, 64)
	for rows.Next() {
		var (
			gStrkey string
			payload sql.NullString
		)
		if err := rows.Scan(&gStrkey, &payload); err != nil {
			return nil, fmt.Errorf("timescale: AllSep1Images scan: %w", err)
		}
		if !payload.Valid || payload.String == "" {
			continue
		}
		out = append(out, sep1ImagesFromPayload(gStrkey, payload.String)...)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: AllSep1Images rows: %w", err)
	}
	return out, nil
}

// SetIssuerSep1Payload writes a SEP-1 fetch result back to the
// issuers row — sep1_payload (jsonb) + sep1_resolved_at = now() — and
// clears the retry ladder (migration 0159).
//
// Clearing on success is what makes a RECOVERING domain cheap: an
// issuer that finally publishes a stellar.toml after months of 404s
// returns to the plain -older-than cadence on its very first success,
// rather than serving out the 30-day deferral it earned while it was
// dead. Caller is responsible for serialising the payload.
func (s *Store) SetIssuerSep1Payload(ctx context.Context, gStrkey string, payload []byte) error {
	const q = `
        UPDATE issuers
           SET sep1_payload              = $2::jsonb,
               sep1_resolved_at          = NOW(),
               sep1_consecutive_failures = 0,
               sep1_next_attempt_after   = NULL
         WHERE g_strkey = $1
    `
	_, err := s.db.ExecContext(ctx, q, gStrkey, string(payload))
	if err != nil {
		return fmt.Errorf("timescale: SetIssuerSep1Payload: %w", err)
	}
	return nil
}

// Sep1 retry-ladder tuning. The base is one DAY on purpose: it is the
// cadence a healthy domain already gets, so a domain's FIRST failure
// costs it nothing at all. That matters more than it looks — when a
// systemic outage on our side fails every domain at once, the whole
// population lands on step 1 and keeps its normal schedule.
//
// Steps are base * 2^(prior failures), capped: 1d, 2d, 4d, 8d, 16d,
// 30d, 30d, … A dead domain settles at ~1 attempt/month. The cap is
// what keeps this a deferral rather than an eviction — nothing is ever
// removed from the queue, so an issuer that publishes a stellar.toml
// years later is still found without operator action.
const (
	Sep1BackoffBase = 24 * time.Hour
	Sep1BackoffCap  = 30 * 24 * time.Hour
	// sep1BackoffMaxShift bounds the exponent so POWER() cannot reach
	// float infinity on a row with an absurd counter; 2^20 days is
	// already ~2,870 years, far past the cap that clamps it.
	sep1BackoffMaxShift = 20
)

// MarkIssuerSep1Failed records a terminating attempt that produced no
// payload — dead domain, TLS error, SSRF-blocked, unparseable TOML,
// failed write — and advances the retry ladder. Returns the issuer's
// new consecutive-failure count.
//
// Two separate jobs, and they are easy to conflate:
//
//   - sep1_resolved_at = NOW() is QUEUE HYGIENE, and predates the
//     ladder. IssuersNeedingSep1Refresh orders `sep1_resolved_at ASC
//     NULLS FIRST`, so a row left NULL stays candidate #1 on every
//     subsequent run; the ~43k pubnet issuers with dead home_domains
//     used to occupy the whole front of the queue and good issuers
//     behind them were never reached. Every failure path must stamp it.
//
//   - sep1_consecutive_failures / sep1_next_attempt_after are the
//     BUDGET. Stamping alone only reorders the queue; it still hands a
//     domain that has 404'd two hundred times exactly as many attempts
//     as one that answers. The ladder is what stops that.
//
// The whole update is one statement so the count and the deferral
// derived from it cannot disagree. The SET expressions read the
// PRE-UPDATE value of sep1_consecutive_failures (Postgres semantics),
// which is why the deferral uses `prior failures` and the count uses
// `prior + 1`: a first failure yields count 1 and a one-day deferral.
//
// Both interval parameters carry an explicit ::interval cast. An
// untyped bind parameter beside an interval operator leaves Postgres
// unable to resolve the operator and raises 42883 at runtime on every
// call, while compiling and reviewing perfectly.
func (s *Store) MarkIssuerSep1Failed(ctx context.Context, gStrkey string) (int, error) {
	const q = `
        UPDATE issuers
           SET sep1_resolved_at          = NOW(),
               sep1_consecutive_failures = COALESCE(sep1_consecutive_failures, 0) + 1,
               sep1_next_attempt_after   = NOW() + LEAST(
                   $2::interval * POWER(2::double precision,
                       LEAST(COALESCE(sep1_consecutive_failures, 0), $4::int)::double precision),
                   $3::interval)
         WHERE g_strkey = $1
        RETURNING sep1_consecutive_failures
    `
	var failures int
	err := s.db.QueryRowContext(ctx, q, gStrkey,
		intervalArg(Sep1BackoffBase), intervalArg(Sep1BackoffCap), sep1BackoffMaxShift,
	).Scan(&failures)
	if err != nil {
		return 0, fmt.Errorf("timescale: MarkIssuerSep1Failed: %w", err)
	}
	return failures, nil
}

// UnwindIssuerSep1Backoff takes back exactly one ladder step for each
// of the given issuers and lifts their deferral, leaving
// sep1_resolved_at alone.
//
// This is the systemic-outage escape hatch. Each key passed here failed
// exactly once during the run being unwound, so decrementing by one
// restores the pre-run count precisely; clearing
// sep1_next_attempt_after returns the row to the plain -older-than
// cadence. sep1_resolved_at deliberately KEEPS its bump, so the queue
// order still advances and the run cannot re-walk the same head.
//
// Why it exists: an outage on OUR side — DNS broken, egress blocked, a
// bad resolver deploy — fails every domain in the run, and a backoff
// that believed those failures would push the entire population toward
// the 30-day cap over a few days and then go quiet. `max(sep1_resolved_at)`
// keeps ticking the whole time, so the data-freshness watchdog stays
// green. The caller decides what "systemic" means; this is the undo.
func (s *Store) UnwindIssuerSep1Backoff(ctx context.Context, gStrkeys []string) (int64, error) {
	if len(gStrkeys) == 0 {
		return 0, nil
	}
	const q = `
        UPDATE issuers
           SET sep1_consecutive_failures = GREATEST(COALESCE(sep1_consecutive_failures, 1) - 1, 0),
               sep1_next_attempt_after   = NULL
         WHERE g_strkey = ANY($1::text[])
    `
	res, err := s.db.ExecContext(ctx, q, gStrkeys)
	if err != nil {
		return 0, fmt.Errorf("timescale: UnwindIssuerSep1Backoff: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("timescale: UnwindIssuerSep1Backoff rows: %w", err)
	}
	return n, nil
}

// intervalArg renders a duration as a Postgres interval literal. Whole
// seconds is exact for every value the ladder uses and avoids the
// locale-dependent parsing of a fractional form.
func intervalArg(d time.Duration) string {
	return fmt.Sprintf("%d seconds", int64(d/time.Second))
}

// issuerAssetsHardCap bounds one issuer's asset list.
//
// The query used to have no LIMIT at all, on the documented assumption
// that an issuer has "typically <20" assets. That assumption held only
// while the registry was fed by trades: an issuer had to get each of its
// codes traded to appear at all. Since migration 0158 the registry also
// carries every classic asset with a TRUSTLINE — 512,496 of them against
// 199,793 traded ones — and minting many codes and airdropping trustlines
// for them is an established spam pattern on this network. An unbounded,
// uncached query on a public per-issuer endpoint is not something to leave
// standing once that population is in the table.
//
// 500 matches the cap the RWA surface already applies per issuer and the
// clamp the assets listing applies to a page. The rows dropped are the
// tail of `ORDER BY observation_count DESC`, i.e. the never-traded end.
//
// SURFACING the truncation on the wire is deliberately NOT done here: the
// response has no field for it and adding one is a wire-shape change. The
// cap is a denial-of-service bound, not a pagination scheme.
const issuerAssetsHardCap = 500

// ListIssuerAssets returns the classic assets issued by the given
// G-strkey, ordered by observation count desc (a cheap TRADING-activity
// proxy — a held-but-never-traded asset carries 0 and sorts to the tail),
// capped at [issuerAssetsHardCap].
func (s *Store) ListIssuerAssets(ctx context.Context, gStrkey string) ([]IssuerAsset, error) {
	const q = `
		SELECT
		    asset_id,
		    code,
		    COALESCE(slug, code),
		    first_seen_ledger,
		    last_seen_ledger,
		    observation_count
		  FROM classic_assets
		 WHERE issuer_g_strkey = $1
		 ORDER BY observation_count DESC, asset_id ASC
		 LIMIT $2
	`
	rows, err := s.db.QueryContext(ctx, q, gStrkey, issuerAssetsHardCap)
	if err != nil {
		return nil, fmt.Errorf("timescale: ListIssuerAssets %s: %w", gStrkey, err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]IssuerAsset, 0, 8)
	for rows.Next() {
		var a IssuerAsset
		var first, last int64
		if err := rows.Scan(&a.AssetID, &a.Code, &a.Slug, &first, &last, &a.ObservationCount); err != nil {
			return nil, fmt.Errorf("timescale: ListIssuerAssets scan: %w", err)
		}
		a.FirstSeenLedger = uint32(first) //nolint:gosec
		a.LastSeenLedger = uint32(last)   //nolint:gosec
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ListIssuerAssets rows: %w", err)
	}
	return out, nil
}
