package postgresstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Stellar-Index/StellarIndex/internal/pgarray"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// APIKeyStore implements [platform.APIKeyStore] against the `api_keys` table.
type APIKeyStore struct{ s *Store }

// NewAPIKeyStore returns the Postgres-backed implementation.
func NewAPIKeyStore(s *Store) *APIKeyStore { return &APIKeyStore{s: s} }

const apiKeyColumns = `
	id, account_id,
	created_by_user_id,
	name,
	COALESCE(description, ''),
	key_hash, key_prefix, tier,
	rate_limit_per_min,
	COALESCE(monthly_quota, 0),
	permissions,
	scopes,
	ip_allowlist,
	referer_allowlist,
	expires_at,
	revoked_at, revoked_by_user_id, COALESCE(revoked_reason, ''),
	last_used_at, last_used_ip, COALESCE(last_used_user_agent, ''),
	COALESCE(usage_alert_threshold_pct, 0),
	created_at
`

func scanAPIKey(row interface {
	Scan(...any) error
},
) (platform.APIKey, error) {
	var k platform.APIKey
	var (
		createdByUserID, revokedByUserID sql.NullString
		expiresAt, revokedAt, lastUsedAt sql.NullTime
		lastUsedIP                       sql.NullString
		permissions                      []byte
		scopes                           []string
		ipAllowlist                      []string
		refererAllowlist                 []string
	)
	if err := row.Scan(
		&k.ID, &k.AccountID, &createdByUserID,
		&k.Name, &k.Description,
		&k.KeyHash, &k.KeyPrefix, &k.Tier,
		&k.RateLimitPerMin, &k.MonthlyQuota,
		&permissions, pgarray.Strings(&scopes), pgarray.Strings(&ipAllowlist), pgarray.Strings(&refererAllowlist),
		&expiresAt,
		&revokedAt, &revokedByUserID, &k.RevokedReason,
		&lastUsedAt, &lastUsedIP, &k.LastUsedUserAgent,
		&k.UsageAlertThresholdPct,
		&k.CreatedAt,
	); err != nil {
		return platform.APIKey{}, err
	}
	k.CreatedByUserID = parseUUIDNullString(createdByUserID)
	k.RevokedByUserID = parseUUIDNullString(revokedByUserID)
	if expiresAt.Valid {
		k.ExpiresAt = expiresAt.Time
	}
	if revokedAt.Valid {
		k.RevokedAt = revokedAt.Time
	}
	if lastUsedAt.Valid {
		k.LastUsedAt = lastUsedAt.Time
	}
	if lastUsedIP.Valid && lastUsedIP.String != "" {
		k.LastUsedIP = net.ParseIP(lastUsedIP.String)
	}
	if len(permissions) > 0 {
		if err := json.Unmarshal(permissions, &k.Permissions); err != nil {
			return platform.APIKey{}, fmt.Errorf("decode permissions: %w", err)
		}
	}
	if len(scopes) > 0 {
		k.Scopes = scopes
	}
	k.RefererAllowlist = refererAllowlist
	k.IPAllowlist = parseCIDRArray(ipAllowlist)
	return k, nil
}

// parseUUIDNullString returns the zero UUID when the column was
// NULL or the value didn't parse — both cases the caller treats
// the same: "no creator / revoker on this row".
func parseUUIDNullString(s sql.NullString) uuid.UUID {
	if !s.Valid || s.String == "" {
		return uuid.Nil
	}
	id, err := uuid.Parse(s.String)
	if err != nil {
		return uuid.Nil
	}
	return id
}

// parseCIDRArray decodes the textual cidr[] form Postgres ships.
// Malformed entries are skipped rather than failing the whole row;
// the dashboard surfaces "X invalid prefixes" via a separate
// validation pass when the operator edits the allowlist.
func parseCIDRArray(in []string) []netip.Prefix {
	if len(in) == 0 {
		return nil
	}
	out := make([]netip.Prefix, 0, len(in))
	for _, raw := range in {
		p, err := netip.ParsePrefix(raw)
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Create inserts a pre-hashed key; the capped path (maxActiveKeysPerAccount > 0) runs under the account's advisory lock and returns [platform.ErrAPIKeyQuotaExceeded] at the cap, zero or negative disables it.
func (r *APIKeyStore) Create(ctx context.Context, k platform.APIKey, maxActiveKeysPerAccount int) (platform.APIKey, error) {
	args, err := buildAPIKeyCreateArgs(k)
	if err != nil {
		return platform.APIKey{}, err
	}
	q := apiKeyInsertUncapped
	if maxActiveKeysPerAccount > 0 {
		q = apiKeyInsertCapped
		args = append(args, maxActiveKeysPerAccount)
	}

	// Uncapped path: no quota race to defend against; skip the
	// advisory lock + tx wrapper.
	if maxActiveKeysPerAccount <= 0 {
		row := r.s.db.QueryRowContext(ctx, q, args...)
		return finalizeAPIKeyCreate(scanAPIKey(row))
	}

	tx, err := r.s.db.BeginTx(ctx, nil)
	if err != nil {
		return platform.APIKey{}, fmt.Errorf("postgresstore: APIKeyStore.Create: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockAccount(ctx, tx, lockNamespaceAPIKey, k.AccountID); err != nil {
		return platform.APIKey{}, fmt.Errorf("postgresstore: APIKeyStore.Create: %w", err)
	}
	row := tx.QueryRowContext(ctx, q, args...)
	out, err := finalizeAPIKeyCreate(scanAPIKey(row))
	if err != nil {
		return platform.APIKey{}, err
	}
	if err := tx.Commit(); err != nil {
		return platform.APIKey{}, fmt.Errorf("postgresstore: APIKeyStore.Create: commit: %w", err)
	}
	return out, nil
}

// buildAPIKeyCreateArgs marshals the platform.APIKey fields into
// the positional args the apiKeyInsert* statements expect. Split
// out of [APIKeyStore.Create] so that function stays under the
// funlen lint cap.
func buildAPIKeyCreateArgs(k platform.APIKey) ([]any, error) {
	permissionsJSON, err := encodePermissions(k.Permissions)
	if err != nil {
		return nil, fmt.Errorf("encode permissions: %w", err)
	}
	return []any{
		k.ID, k.AccountID, uuidOrEmpty(k.CreatedByUserID),
		k.Name, k.Description,
		k.KeyHash, k.KeyPrefix, string(k.Tier),
		k.RateLimitPerMin, k.MonthlyQuota,
		permissionsJSON,
		nonNilStringArray(k.Scopes),
		ipAllowlistArray(k.IPAllowlist),
		nonNilStringArray(k.RefererAllowlist),
		nullTime(k.ExpiresAt),
		k.UsageAlertThresholdPct,
	}, nil
}

// finalizeAPIKeyCreate maps the row-scan result to the public
// (APIKey, sentinel-error) contract. ErrNoRows → quota exceeded,
// unique-violation → ErrConflict, anything else → wrapped error.
func finalizeAPIKeyCreate(out platform.APIKey, err error) (platform.APIKey, error) {
	if err == nil {
		return out, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return platform.APIKey{}, platform.ErrAPIKeyQuotaExceeded
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgErrUniqueViolation {
		return platform.APIKey{}, fmt.Errorf("create api key: %w", platform.ErrConflict)
	}
	return platform.APIKey{}, fmt.Errorf("create api key: %w", err)
}

// apiKeyInsertCapped is the gated INSERT used when the caller
// passes maxActiveKeysPerAccount > 0. Combined with the
// pg_advisory_xact_lock the per-account quota cannot be
// exceeded by concurrent callers.
const apiKeyInsertCapped = `
	WITH active_count AS (
	    SELECT COUNT(*) AS n
	      FROM api_keys
	     WHERE account_id = $2 AND revoked_at IS NULL
	)
	INSERT INTO api_keys (
		id, account_id, created_by_user_id,
		name, description,
		key_hash, key_prefix, tier,
		rate_limit_per_min, monthly_quota,
		permissions,
		scopes,
		ip_allowlist,
		referer_allowlist,
		expires_at,
		usage_alert_threshold_pct
	)
	SELECT $1, $2, NULLIF($3::text, '')::uuid,
	       $4, NULLIF($5, ''),
	       $6, $7, $8,
	       $9, NULLIF($10, 0)::bigint,
	       $11::jsonb,
	       $12::text[],
	       $13::cidr[],
	       $14::text[],
	       $15,
	       NULLIF($16, 0)
	  FROM active_count
	 WHERE active_count.n < $17
	RETURNING ` + apiKeyColumns

// apiKeyInsertUncapped is the unfenced INSERT path used by
// staff-seeding callers that bypass the per-account cap (e.g.
// stellarindex-ops).
const apiKeyInsertUncapped = `
	INSERT INTO api_keys (
		id, account_id, created_by_user_id,
		name, description,
		key_hash, key_prefix, tier,
		rate_limit_per_min, monthly_quota,
		permissions,
		scopes,
		ip_allowlist,
		referer_allowlist,
		expires_at,
		usage_alert_threshold_pct
	)
	VALUES ($1, $2, NULLIF($3::text, '')::uuid,
	        $4, NULLIF($5, ''),
	        $6, $7, $8,
	        $9, NULLIF($10, 0)::bigint,
	        $11::jsonb,
	        $12::text[],
	        $13::cidr[],
	        $14::text[],
	        $15,
	        NULLIF($16, 0))
	RETURNING ` + apiKeyColumns

func (r *APIKeyStore) Get(ctx context.Context, id string) (platform.APIKey, error) {
	const q = `SELECT ` + apiKeyColumns + ` FROM api_keys WHERE id = $1`
	row := r.s.db.QueryRowContext(ctx, q, id)
	out, err := scanAPIKey(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return platform.APIKey{}, platform.ErrNotFound
		}
		return platform.APIKey{}, fmt.Errorf("get api key: %w", err)
	}
	return out, nil
}

func (r *APIKeyStore) GetByHash(ctx context.Context, keyHash []byte) (platform.APIKey, error) {
	const q = `SELECT ` + apiKeyColumns + ` FROM api_keys WHERE key_hash = $1`
	row := r.s.db.QueryRowContext(ctx, q, keyHash)
	out, err := scanAPIKey(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return platform.APIKey{}, platform.ErrNotFound
		}
		return platform.APIKey{}, fmt.Errorf("get api key by hash: %w", err)
	}
	return out, nil
}

// CountActiveForAccount counts the account's non-revoked keys, the same predicate apiKeyInsertCapped gates on.
func (r *APIKeyStore) CountActiveForAccount(ctx context.Context, accountID uuid.UUID) (int, error) {
	const q = `SELECT COUNT(*) FROM api_keys WHERE account_id = $1 AND revoked_at IS NULL`
	var n int
	if err := r.s.db.QueryRowContext(ctx, q, accountID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count active api keys: %w", err)
	}
	return n, nil
}

// ListActiveForAccount returns the account's non-revoked keys, oldest first, bounded by the per-account active cap.
func (r *APIKeyStore) ListActiveForAccount(ctx context.Context, accountID uuid.UUID) ([]platform.APIKey, error) {
	const q = `SELECT ` + apiKeyColumns + ` FROM api_keys
		WHERE account_id = $1 AND revoked_at IS NULL
		ORDER BY created_at ASC, id ASC`
	return r.queryKeys(ctx, "list active api keys", q, accountID)
}

// ListForAccount returns every active key plus at most revokedLimit recent revoked keys, oldest first; moreRevoked reports older revoked keys were left out.
func (r *APIKeyStore) ListForAccount(
	ctx context.Context, accountID uuid.UUID, revokedLimit int,
) (keys []platform.APIKey, moreRevoked bool, err error) {
	revokedLimit = max(revokedLimit, 0)
	active, err := r.ListActiveForAccount(ctx, accountID)
	if err != nil {
		return nil, false, err
	}
	// One row past the limit is the cheapest proof that more history exists.
	const q = `SELECT ` + apiKeyColumns + ` FROM api_keys
		WHERE account_id = $1 AND revoked_at IS NOT NULL
		ORDER BY created_at DESC, id DESC
		LIMIT $2`
	revoked, err := r.queryKeys(ctx, "list revoked api keys", q, accountID, revokedLimit+1)
	if err != nil {
		return nil, false, err
	}
	if len(revoked) > revokedLimit {
		revoked = revoked[:revokedLimit]
		moreRevoked = true
	}
	keys = slices.Concat(active, revoked)
	slices.SortFunc(keys, func(a, b platform.APIKey) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return keys, moreRevoked, nil
}

func (r *APIKeyStore) queryKeys(ctx context.Context, op, q string, args ...any) ([]platform.APIKey, error) {
	rows, err := r.s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = rows.Close() }()
	var out []platform.APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, fmt.Errorf("%s scan: %w", op, err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Update writes the editable fields of k.ID only when accountID owns it; a CHECK violation surfaces as a generic error, not platform.ErrConflict.
func (r *APIKeyStore) Update(ctx context.Context, accountID uuid.UUID, k platform.APIKey) error {
	permissionsJSON, err := encodePermissions(k.Permissions)
	if err != nil {
		return fmt.Errorf("encode permissions: %w", err)
	}
	const q = `
		UPDATE api_keys SET
			name = $2,
			description = NULLIF($3, ''),
			rate_limit_per_min = $4,
			monthly_quota = NULLIF($5, 0)::bigint,
			permissions = $6::jsonb,
			scopes = $7::text[],
			ip_allowlist = $8::cidr[],
			referer_allowlist = $9::text[],
			expires_at = $10,
			usage_alert_threshold_pct = NULLIF($11, 0)
		WHERE id = $1 AND account_id = $12
	`
	res, err := r.s.db.ExecContext(ctx, q,
		k.ID, k.Name, k.Description,
		k.RateLimitPerMin, k.MonthlyQuota,
		permissionsJSON,
		nonNilStringArray(k.Scopes),
		ipAllowlistArray(k.IPAllowlist),
		nonNilStringArray(k.RefererAllowlist),
		nullTime(k.ExpiresAt),
		k.UsageAlertThresholdPct,
		accountID,
	)
	if err != nil {
		return fmt.Errorf("update api key: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return platform.ErrNotFound
	}
	return nil
}

// Revoke soft-deletes id only when accountID owns it; idempotent, an already-revoked key keeps its revoked_at and gets the new reason.
func (r *APIKeyStore) Revoke(ctx context.Context, accountID uuid.UUID, id string, byUserID uuid.UUID, reason string) error {
	const q = `
		UPDATE api_keys SET
			revoked_at = COALESCE(revoked_at, now()),
			revoked_by_user_id = NULLIF($2::text, '')::uuid,
			revoked_reason = NULLIF($3, '')
		WHERE id = $1 AND account_id = $4
	`
	res, err := r.s.db.ExecContext(ctx, q, id, uuidOrEmpty(byUserID), reason, accountID)
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return platform.ErrNotFound
	}
	return nil
}

// TouchUsage updates the last-seen fields of the authenticated key id; unconditional, the caller debounces.
func (r *APIKeyStore) TouchUsage(ctx context.Context, id string, ip net.IP, userAgent string) error {
	const q = `
		UPDATE api_keys SET
			last_used_at = now(),
			last_used_ip = NULLIF($2, '')::inet,
			last_used_user_agent = NULLIF($3, '')
		WHERE id = $1
	`
	ipStr := ""
	if ip != nil {
		ipStr = ip.String()
	}
	res, err := r.s.db.ExecContext(ctx, q, id, ipStr, userAgent)
	if err != nil {
		return fmt.Errorf("touch api key usage: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return platform.ErrNotFound
	}
	return nil
}

// ─── helpers ──────────────────────────────────────────────────────

// encodePermissions serialises to JSON bytes ready for the jsonb
// column. Empty Permissions{} encodes to {"all":false} which is
// distinct from the schema default {"all":true}; callers who
// want the default must set All=true explicitly before Create.
func encodePermissions(p platform.KeyPermissions) ([]byte, error) {
	return json.Marshal(p)
}

// uuidOrEmpty returns the UUID text, or "" for the zero UUID, so the SQL NULLIF on the empty string binds NULL for "absent".
func uuidOrEmpty(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

// nullTime maps the Go zero time.Time to a SQL NULL. Postgres
// timestamptz columns reject the Go zero so we gate at the
// driver boundary rather than in every column update.
func nullTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t, Valid: true}
}

// ipAllowlistArray serialises a netip.Prefix slice into a Postgres
// cidr[] driver value. pq doesn't ship a typed cidr-array helper,
// so we hand-roll the textual `{cidr,cidr}` form which Postgres
// accepts for parameterised inserts via array_in.
type cidrArray []netip.Prefix

func ipAllowlistArray(prefixes []netip.Prefix) cidrArray { return cidrArray(prefixes) }

// Value formats the prefix list as a Postgres array literal like `{10.0.0.0/8,192.168.1.0/24}`; an empty slice gives `{}`.
func (a cidrArray) Value() (driver.Value, error) {
	if len(a) == 0 {
		return "{}", nil
	}
	parts := make([]string, len(a))
	for i, p := range a {
		parts[i] = p.String()
	}
	return "{" + strings.Join(parts, ",") + "}", nil
}

// nonNilStringArray keeps a nil `[]string` from encoding as SQL NULL, which violates the NOT NULL on `referer_allowlist` (23502) and would 500 the default create-key request; a non-nil empty slice encodes as `{}`.
func nonNilStringArray(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// Compile-time interface check.
var _ platform.APIKeyStore = (*APIKeyStore)(nil)
