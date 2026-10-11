package timescale

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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
	// declared and must NOT be rendered as authoritative (impersonation vector).
	OrgVerified   bool
	AuthRequired  *bool
	AuthRevocable *bool
	AuthImmutable *bool
	AuthClawback  *bool
	// AuthFlagsSource is one of the AuthFlagsSource* constants, or "" when provenance is unknown.
	// Empty means UNKNOWN, never "current": a missing label is not a claim that the reading is live.
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
	q := `
		SELECT
		    g_strkey,
		    COALESCE(home_domain, ''),
		    COALESCE(sep1_payload->>'OrgName', '') AS org_name,
		    COALESCE((sep1_payload->>'OrgVerified')::boolean, false)
		        AND NOT ` + sep1PayloadOutlivedSQL + ` AS org_verified,
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

// IssuerSummary is one entry in the issuer-directory listing, returned by [Store.ListIssuers].
// OrgName comes from the SEP-1 payload's `OrgName` and is empty until the fetcher has refreshed
// the issuer or when the toml has no documentation block.
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

// ListIssuers returns the issuer directory ordered by total observation count desc, the activity
// proxy /v1/issuers exposes. limit clamps to [1, 500]. Issuers without any classic_assets row are
// excluded: they are orphan G-strkeys with no activity.
func (s *Store) ListIssuers(ctx context.Context, limit int) ([]IssuerSummary, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	// The outlived predicate reads issuers columns outside the GROUP BY;
	// grouping on the primary key g_strkey makes that legal.
	//nolint:gosec // G202: sep1PayloadOutlivedSQL is constant SQL; values bind via $N
	q := `
        SELECT i.g_strkey,
               COALESCE(i.home_domain, ''),
               COALESCE(i.sep1_payload->>'OrgName', '') AS org_name,
               COALESCE((i.sep1_payload->>'OrgVerified')::boolean, false)
                   AND NOT ` + sep1PayloadOutlivedSQL + ` AS org_verified,
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
	// Reached: the row holds a payload, so this home_domain has served a
	// stellar.toml before (a domain change clears the payload).
	Reached bool
}

// IssuerSep1CandidateByStrkey returns the (g_strkey, home_domain) pair for one issuer, the targeted
// counterpart of IssuersNeedingSep1Refresh for `sep1-refresh -issuer <g>`. It returns sql.ErrNoRows
// for an unknown issuer, or an error when the account has no home_domain to fetch.
func (s *Store) IssuerSep1CandidateByStrkey(ctx context.Context, gStrkey string) (IssuerSep1Candidate, error) {
	const q = `SELECT g_strkey, COALESCE(home_domain, ''), sep1_payload IS NOT NULL FROM issuers WHERE g_strkey = $1`
	var c IssuerSep1Candidate
	if err := s.db.QueryRowContext(ctx, q, gStrkey).Scan(&c.GStrkey, &c.HomeDomain, &c.Reached); err != nil {
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

// IssuersNeedingSep1Refresh returns up to `limit` issuers with a home_domain, whose sep1_resolved_at
// is missing or older than `staleness`, and whose retry deferral has expired. It orders by
// sep1_resolved_at ASC NULLS FIRST so never-resolved issuers and the oldest payloads surface first.
//
// `staleness` of 0 means "refresh anything" but does NOT override the deferral: a domain on the
// failure ladder is skipped until sep1_next_attempt_after passes, and `sep1-refresh -issuer <G>`
// forces one.
//
// `limit` is clamped INTO [1, Sep1RefreshMaxLimit], not reset to a default: resetting would give an
// operator who raised LIMIT past the ceiling a smaller budget, which reads as a slow job.
func (s *Store) IssuersNeedingSep1Refresh(ctx context.Context, staleness time.Duration, limit int) ([]IssuerSep1Candidate, error) {
	if limit <= 0 {
		limit = 1
	}
	if limit > Sep1RefreshMaxLimit {
		limit = Sep1RefreshMaxLimit
	}
	// `IS NULL OR <= NOW()` rather than COALESCE keeps a plain column predicate the planner can
	// answer from issuers_sep1_refresh_queue_idx's INCLUDE payload.
	const q = `
        SELECT g_strkey, home_domain, sep1_payload IS NOT NULL
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
		if err := rows.Scan(&c.GStrkey, &c.HomeDomain, &c.Reached); err != nil {
			return nil, fmt.Errorf("timescale: IssuersNeedingSep1Refresh scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: IssuersNeedingSep1Refresh rows: %w", err)
	}
	return out, nil
}

// IssuerSep1Cached is the parsed `sep1_payload` JSONB column, the subset of [metadata.SEP1] the
// `sep1-refresh` cron persists. [GetIssuerSep1Cached] returns it so the API can overlay SEP-1 data
// without a live HTTPS fetch.
type IssuerSep1Cached struct {
	OrgName       string               `json:"OrgName,omitempty"`
	Version       string               `json:"Version,omitempty"`
	Documentation map[string]string    `json:"Documentation,omitempty"`
	Currencies    []IssuerSep1Currency `json:"Currencies,omitempty"`
	FetchedAt     string               `json:"FetchedAt,omitempty"`
	// OutlivedDomain is [sep1PayloadOutlivedSQL] for the row, read from the
	// fetch-state columns rather than the payload's own FetchedAt key.
	OutlivedDomain bool `json:"-"`
}

// sep1PayloadOutlivedSQL is true for an issuers row whose payload is older than
// [Sep1AttestationMaxAge] (or of unrecorded age) while its domain is failing now. A failed fetch
// keeps the payload, so without this a dead domain's last document vouches forever; age alone is our
// refresh lagging, and a fresh payload on a failing domain is a flake.
var sep1PayloadOutlivedSQL = `(COALESCE(sep1_consecutive_failures, 0) > 0
       AND NOT COALESCE(sep1_payload_fetched_at >= NOW() - '` + intervalArg(Sep1AttestationMaxAge) + `'::interval, false))`

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
	// Nil when the issuer omitted the key, or the payload predates it.
	IsAssetAnchored        *bool  `json:"IsAssetAnchored,omitempty"`
	AttestationOfReserve   string `json:"AttestationOfReserve,omitempty"`
	RedemptionInstructions string `json:"RedemptionInstructions,omitempty"`
	Regulated              *bool  `json:"Regulated,omitempty"`
	ApprovalServer         string `json:"ApprovalServer,omitempty"`
	ApprovalCriteria       string `json:"ApprovalCriteria,omitempty"`
}

// IssuerSep1Unreachable reports whether the issuer's current failure streak is one the payload alone
// cannot show: attempts that ended without a storable document and that no systemic-outage unwind
// took back. [GetIssuerSep1Cached] returns (nil, nil) for both no-payload cases, which are opposite
// findings: OUR backlog, or the ISSUER's publication (e.g. a malformed stellar.toml).
//
// It reads sep1_consecutive_failures, not sep1_resolved_at: the cron stamps resolved_at BEFORE each
// fetch and keeps it through [Store.UnwindIssuerSep1Backoff], so it is set on every touched row
// after our own outage, while the counter still separates their failures from ours.
func (s *Store) IssuerSep1Unreachable(ctx context.Context, gStrkey string) (bool, error) {
	const q = `SELECT COALESCE(sep1_consecutive_failures, 0) > 0 FROM issuers WHERE g_strkey = $1`
	var unreachable bool
	if err := s.db.QueryRowContext(ctx, q, gStrkey).Scan(&unreachable); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// No issuer row at all: nothing has been attempted, because
			// there is nothing to attempt it against.
			return false, nil
		}
		return false, fmt.Errorf("timescale: IssuerSep1Unreachable: %w", err)
	}
	return unreachable, nil
}

// GetIssuerSep1Cached returns the cached SEP-1 payload for an issuer
// G-strkey from `issuers.sep1_payload`: (nil, nil) when the row exists with
// no payload yet, (nil, sql.ErrNoRows) when the issuer is unknown. It stands
// in for a live per-request HTTPS fetch that dominated /v1/assets/{id} p95.
func (s *Store) GetIssuerSep1Cached(ctx context.Context, gStrkey string) (*IssuerSep1Cached, error) {
	q := `SELECT sep1_payload, ` + sep1PayloadOutlivedSQL + ` FROM issuers WHERE g_strkey = $1`
	var (
		payload  sql.NullString
		outlived bool
	)
	if err := s.db.QueryRowContext(ctx, q, gStrkey).Scan(&payload, &outlived); err != nil {
		return nil, err
	}
	if !payload.Valid || payload.String == "" {
		return nil, nil
	}
	var out IssuerSep1Cached
	if err := json.Unmarshal([]byte(payload.String), &out); err != nil {
		return nil, fmt.Errorf("timescale: GetIssuerSep1Cached: parse: %w", err)
	}
	out.OutlivedDomain = outlived
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

// sep1ImageFrom builds one [Sep1Image] from a projected (code, declaredIssuer, image) triple, or
// reports that the entry does not qualify.
//
// An entry counts only when its declared Issuer names gStrkey, the account whose stellar.toml
// carried it. The check stays in Go via [Sep1EntryBindsTo], shared with the bound-currency scan, so
// the logo and attestation rules cannot drift apart.
func sep1ImageFrom(gStrkey, code, declaredIssuer, image string) (Sep1Image, bool) {
	code = strings.TrimSpace(code)
	if image == "" || code == "" {
		return Sep1Image{}, false
	}
	if !Sep1EntryBindsTo(declaredIssuer, gStrkey) {
		return Sep1Image{}, false
	}
	return Sep1Image{Code: code, Issuer: gStrkey, Image: image}, true
}

// allSep1ImagesQuery projects (code, issuer, image) triples out of every cached SEP-1 payload
// SERVER-SIDE, one row per declared currency; the column holds hundreds of MB of JSON, so parsing
// it in Go dominated the read.
//
// `jsonb_array_elements` raises 22023 on a non-array, and one hostile TOML (`Currencies = "nope"`)
// would fail the statement and blank every logo. A WHERE jsonb_typeof guard depends on the planner
// pushing it below the lateral; the CASE substitutes an empty array inside the function's own
// argument, so no plan can separate guard from guarded. TestAllSep1ImagesProjection pins it.
//
// Key lookups are case-SENSITIVE where encoding/json is not, safe only because
// [marshalSep1Payload], the sole writer, spells every key as a Go literal.
//
//nolint:gosec // G202: sep1PayloadOutlivedSQL is constant SQL; values bind via $N
var allSep1ImagesQuery = `
SELECT i.g_strkey,
       c.value ->> 'Code'   AS code,
       c.value ->> 'Issuer' AS issuer,
       c.value ->> 'Image'  AS image
  FROM issuers i
  CROSS JOIN LATERAL jsonb_array_elements(
       CASE WHEN jsonb_typeof(i.sep1_payload -> 'Currencies') = 'array'
            THEN i.sep1_payload -> 'Currencies'
            ELSE '[]'::jsonb
       END) AS c(value)
 WHERE i.sep1_payload IS NOT NULL
   AND NOT ` + sep1PayloadOutlivedSQL + `
   AND jsonb_typeof(c.value)            = 'object'
   AND jsonb_typeof(c.value -> 'Image') = 'string'
   AND jsonb_typeof(c.value -> 'Code')  = 'string'`

// AllSep1Images returns every populated [[CURRENCIES]] image across issuers whose sep1_payload is
// set and has not outlived a failing domain ([sep1PayloadOutlivedSQL]), for the /v1/assets logo
// overlay. The API caches it on a DETACHED context and applies its own URL-scheme filter
// ([isSafeImageURL]). Non-qualifying entries are skipped, not fatal: one issuer's junk must not
// blank the map.
//
// g_strkey is projected beside the DECLARED issuer ([sep1ImageFrom]) because keyed purely on
// TOML-supplied (code, issuer), any account could publish a "USDC" entry naming another issuer's
// G-key with an attacker image. A TOML may describe only the issuer that served it.
func (s *Store) AllSep1Images(ctx context.Context) ([]Sep1Image, error) {
	rows, err := s.db.QueryContext(ctx, allSep1ImagesQuery)
	if err != nil {
		return nil, fmt.Errorf("timescale: AllSep1Images: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Sep1Image, 0, 64)
	for rows.Next() {
		var gStrkey string
		var code, declaredIssuer, image sql.NullString
		if err := rows.Scan(&gStrkey, &code, &declaredIssuer, &image); err != nil {
			return nil, fmt.Errorf("timescale: AllSep1Images scan: %w", err)
		}
		if img, ok := sep1ImageFrom(gStrkey, code.String, declaredIssuer.String, image.String); ok {
			out = append(out, img)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: AllSep1Images rows: %w", err)
	}
	return out, nil
}

// SyncIssuerHomeDomain writes an issuer's ON-CHAIN home_domain onto its row, reporting whether the
// row changed. An empty domain is refused as "not read"; an account READ as declaring none goes
// through [Store.ClearIssuerHomeDomain], so the two can never be confused.
//
// # Why this overwrites
//
// The SEP-1 resolver READS this column to choose which domain to fetch, so a write-once clause
// would keep a lapsed domain: whoever registers it next could serve a stellar.toml listing the
// issuer back, satisfy the bidirectional check and inherit the verified org identity.
//
// The predicate skips rows that already agree. A changed row has its SEP-1 state unbound in the
// same statement; see [sep1ResetOnHomeDomainChange].
func (s *Store) SyncIssuerHomeDomain(ctx context.Context, gStrkey, homeDomain string) (bool, error) {
	if homeDomain == "" {
		return false, nil
	}
	return s.writeIssuerHomeDomain(ctx, "SyncIssuerHomeDomain", gStrkey, homeDomain)
}

// ClearIssuerHomeDomain blanks the home_domain of an issuer whose live AccountEntry was read and
// declares none, reporting whether the row changed. Keeping a cleared domain is the same takeover
// risk as keeping one the account moved away from (see [Store.SyncIssuerHomeDomain]).
func (s *Store) ClearIssuerHomeDomain(ctx context.Context, gStrkey string) (bool, error) {
	return s.writeIssuerHomeDomain(ctx, "ClearIssuerHomeDomain", gStrkey, "")
}

func (s *Store) writeIssuerHomeDomain(ctx context.Context, op, gStrkey, homeDomain string) (bool, error) {
	const newHD = `NULLIF($2, '')`
	//nolint:gosec // G202: fragments are constant SQL (newHD/sep1ResetOnHomeDomainChange); values bind via $N
	q := `
        UPDATE issuers
           SET home_domain = ` + newHD + `,` + sep1ResetOnHomeDomainChange(newHD) + `
         WHERE g_strkey = $1
           AND home_domain IS DISTINCT FROM ` + newHD
	res, err := s.db.ExecContext(ctx, q, gStrkey, homeDomain)
	if err != nil {
		return false, fmt.Errorf("timescale: %s: %w", op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("timescale: %s rows: %w", op, err)
	}
	return n > 0, nil
}

// sep1ResetOnHomeDomainChange is the SET list every writer of issuers.home_domain appends; newHD is
// the expression the same UPDATE assigns. SET expressions read the PRE-update row, so
// `home_domain` below is the value being replaced.
//
// sep1_payload records no source domain and its readers (org_verified, logos, RWA attestations)
// pair it with the CURRENT column. A payload outliving its domain would be served as the new
// domain's claim, and kept if the new domain fails, since a failed fetch keeps the payload.
// Clearing resolved_at and the retry ladder too puts the row at the head of the queue instead of
// behind the old domain's deferral, and keeps the census from reading it as "domain reached,
// served nothing".
func sep1ResetOnHomeDomainChange(newHD string) string {
	return strings.ReplaceAll(`
               sep1_payload              = CASE WHEN @changed THEN NULL ELSE sep1_payload END,
               sep1_resolved_at          = CASE WHEN @changed THEN NULL ELSE sep1_resolved_at END,
               sep1_payload_fetched_at   = CASE WHEN @changed THEN NULL ELSE sep1_payload_fetched_at END,
               sep1_consecutive_failures = CASE WHEN @changed THEN 0 ELSE sep1_consecutive_failures END,
               sep1_next_attempt_after   = CASE WHEN @changed THEN NULL ELSE sep1_next_attempt_after END`,
		"@changed", "("+newHD+") IS DISTINCT FROM home_domain")
}

// ErrSep1PayloadRejected marks a payload Postgres refused on its content
// (SQLSTATE class 22, e.g. a `\u0000` jsonb will not hold): the document's
// fault, not the database's, so the caller keeps the retry-ladder step.
var ErrSep1PayloadRejected = errors.New("sep1 payload rejected by the database")

// SetIssuerSep1Payload writes a SEP-1 fetch result to the issuers row (sep1_payload and
// sep1_resolved_at = now()) and clears the retry ladder, so a RECOVERING domain returns to the plain
// -older-than cadence. In the same transaction it appends one issuer_identity_history row per
// identity field the payload changes on an established issuer ([sep1IdentityChanges]), so an
// overwrite is never blind. The caller serialises the payload.
//
// It is the only writer of sep1_payload_fetched_at; sep1_resolved_at is stamped by every attempt, so
// only this column says how old the held payload is once a domain starts failing.
//
// fetchedFrom is the home_domain the candidate was read under; the write lands only while the row
// still holds it, else the old domain's toml would bind to the new one. The bool reports whether
// the payload was stored.
func (s *Store) SetIssuerSep1Payload(ctx context.Context, gStrkey, fetchedFrom string, payload []byte) (stored bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("timescale: SetIssuerSep1Payload begin: %w", err)
	}
	defer func() {
		if err != nil || !stored {
			_ = tx.Rollback()
		}
	}()

	// Locks the row so the diff and the overwrite see the same held payload.
	var held []byte
	err = tx.QueryRowContext(ctx, `
        SELECT sep1_payload FROM issuers
         WHERE g_strkey = $1 AND home_domain = $2
           FOR UPDATE`, gStrkey, fetchedFrom).Scan(&held)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("timescale: SetIssuerSep1Payload read held: %w", err)
	}
	changes, err := sep1IdentityChanges(held, payload)
	if err != nil {
		return false, fmt.Errorf("timescale: SetIssuerSep1Payload: %w", err)
	}

	const q = `
        UPDATE issuers
           SET sep1_payload              = $2::jsonb,
               sep1_resolved_at          = NOW(),
               sep1_payload_fetched_at   = NOW(),
               sep1_consecutive_failures = 0,
               sep1_next_attempt_after   = NULL
         WHERE g_strkey = $1
           AND home_domain = $3
    `
	if _, err = tx.ExecContext(ctx, q, gStrkey, string(payload), fetchedFrom); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "22") {
			return false, fmt.Errorf("timescale: SetIssuerSep1Payload: %w: %w", ErrSep1PayloadRejected, err)
		}
		return false, fmt.Errorf("timescale: SetIssuerSep1Payload: %w", err)
	}
	for _, c := range changes {
		if _, err = tx.ExecContext(ctx, `
            INSERT INTO issuer_identity_history
                   (g_strkey, home_domain, field, currency, old_value, new_value)
            VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), NULLIF($6, ''))`,
			gStrkey, fetchedFrom, c.Field, c.Currency, c.Old, c.New); err != nil {
			return false, fmt.Errorf("timescale: SetIssuerSep1Payload history: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return false, fmt.Errorf("timescale: SetIssuerSep1Payload commit: %w", err)
	}
	return true, nil
}

// Sep1 retry-ladder tuning. The base is one DAY on purpose: it is a healthy domain's normal cadence,
// so a FIRST failure costs nothing and a systemic outage on our side keeps every schedule intact.
//
// Steps are base * 2^(prior failures), capped: 1d, 2d, 4d, ... 30d. The cap keeps this a deferral,
// not an eviction, so an issuer that publishes a stellar.toml years later is still found.
const (
	Sep1BackoffBase = 24 * time.Hour
	Sep1BackoffCap  = 30 * 24 * time.Hour
	// sep1BackoffMaxShift bounds the exponent so POWER() cannot reach
	// float infinity on a row with an absurd counter; 2^20 days is
	// already ~2,870 years, far past the cap that clamps it.
	sep1BackoffMaxShift = 20
)

// MarkIssuerSep1Failed records a terminating attempt that produced no payload (dead domain, TLS
// error, SSRF-blocked, unparseable TOML, failed write), advances the retry ladder and returns the
// new consecutive-failure count.
//
// Two jobs, easy to conflate:
//   - sep1_resolved_at = NOW() is QUEUE HYGIENE: IssuersNeedingSep1Refresh orders
//     `sep1_resolved_at ASC NULLS FIRST`, so an unstamped row stays first and dead domains would
//     fill the queue front. Every failure path must stamp it.
//   - sep1_consecutive_failures / sep1_next_attempt_after are the BUDGET for repeat failures.
//
// One statement, so count and deferral cannot disagree; the SET expressions read the PRE-UPDATE
// count, so a first failure yields count 1 and a one-day deferral. Both interval parameters carry
// an explicit ::interval cast: an untyped bind beside an interval operator raises 42883 at runtime.
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

// UnwindIssuerSep1Backoff takes back exactly one ladder step for each given issuer and lifts their
// deferral, leaving sep1_resolved_at alone.
//
// This is the systemic-outage escape hatch. Each key failed exactly once in the run being unwound,
// so decrementing by one restores the pre-run count; clearing sep1_next_attempt_after returns the
// row to the plain -older-than cadence. sep1_resolved_at KEEPS its bump so the queue order advances
// and the run cannot re-walk the same head.
//
// An outage on OUR side (DNS, egress, a bad resolver) fails every domain; believing it would push
// the whole population toward the 30-day cap and go quiet while the freshness watchdog stays green.
// The caller decides what "systemic" means; this is the undo.
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
// The registry carries every classic asset with a TRUSTLINE, not just traded ones, and minting many
// codes and airdropping trustlines is a spam pattern, so an unbounded, uncached query on a public
// per-issuer endpoint is a denial-of-service risk.
//
// 500 matches the RWA surface's per-issuer cap and the assets-listing page clamp. Dropped rows are
// the tail of `ORDER BY observation_count DESC`, the never-traded end. The truncation is NOT
// surfaced on the wire: this is a DoS bound, not pagination.
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

// IssuerAuthFlagsOnRecord is what one issuers row currently HOLDS in its auth-flag columns, values
// and not just the key, so the drain's chain re-check can tell a reading the chain still agrees
// with from one it has moved past. The flags are pointers as in [IssuerRow]: NULL means "no
// observation yet", not `false`, and reading them as equal would leave a half-filled row so.
type IssuerAuthFlagsOnRecord struct {
	GStrkey    string
	Required   *bool
	Revocable  *bool
	Immutable  *bool
	Clawback   *bool
	HomeDomain string
	// Source is the persisted provenance label, "" when the row predates
	// migration 0153.
	Source string
	// AsOfLedger is the ledger the persisted reading claims to be true as
	// of; nil when the row carries none.
	AsOfLedger *uint32
}

// IssuersNeedingChainRecheck returns every issuer whose auth-flag columns are already FILLED from a
// live (or unlabelled) reading, by primary key, with the values currently on record. `limit` <= 0
// returns every candidate, the intended setting: the caller writes back only rows the chain
// disagrees with.
//
// It is a third queue because [Store.IssuerGStrkeysNeedingFlags] is `auth_required IS NULL` and
// [Store.IssuerGStrkeysNeedingRecheck] covers only `last_known_before_removal` rows, so a filled,
// live-sourced row would never be read again, yet `issuers.home_domain` rides on it (see
// [Store.SyncIssuerHomeDomain]).
//
// `last_known_before_removal` rows are EXCLUDED (their removal ledger is fixed, so rewriting is a
// no-op), except a merged row still holding a home_domain stored while live: the persist clears a
// merged account's identity, so it is offered until that write lands.
func (s *Store) IssuersNeedingChainRecheck(ctx context.Context, limit int) ([]IssuerAuthFlagsOnRecord, error) {
	q := `
        SELECT g_strkey,
               auth_required, auth_revocable, auth_immutable, auth_clawback,
               COALESCE(home_domain, ''),
               COALESCE(auth_flags_source, ''),
               auth_flags_as_of_ledger
          FROM issuers
         WHERE auth_required IS NOT NULL
           AND (auth_flags_source IS DISTINCT FROM $1 OR home_domain IS NOT NULL)
         ORDER BY g_strkey
    `
	args := []any{AuthFlagsSourceLastKnownBeforeRemoval}
	if limit > 0 {
		q += " LIMIT $2"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: IssuersNeedingChainRecheck: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]IssuerAuthFlagsOnRecord, 0, 1024)
	for rows.Next() {
		rec, err := scanIssuerAuthFlagsOnRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: IssuersNeedingChainRecheck rows: %w", err)
	}
	return out, nil
}

// scanIssuerAuthFlagsOnRecord maps one row of the query above, keeping the
// NULL-vs-false distinction the pointers exist for.
func scanIssuerAuthFlagsOnRecord(rows *sql.Rows) (IssuerAuthFlagsOnRecord, error) {
	var (
		rec                 IssuerAuthFlagsOnRecord
		req, rev, imm, claw sql.NullBool
		asOf                sql.NullInt64
	)
	if err := rows.Scan(&rec.GStrkey, &req, &rev, &imm, &claw,
		&rec.HomeDomain, &rec.Source, &asOf); err != nil {
		return rec, fmt.Errorf("timescale: IssuersNeedingChainRecheck scan: %w", err)
	}
	rec.Required = nullBoolPtr(req)
	rec.Revocable = nullBoolPtr(rev)
	rec.Immutable = nullBoolPtr(imm)
	rec.Clawback = nullBoolPtr(claw)
	if asOf.Valid && asOf.Int64 >= 0 {
		l := uint32(asOf.Int64) //nolint:gosec // ledger sequence, bounded by the column's integer type
		rec.AsOfLedger = &l
	}
	return rec, nil
}

// nullBoolPtr keeps SQL NULL as nil rather than collapsing it to false.
func nullBoolPtr(v sql.NullBool) *bool {
	if !v.Valid {
		return nil
	}
	b := v.Bool
	return &b
}
