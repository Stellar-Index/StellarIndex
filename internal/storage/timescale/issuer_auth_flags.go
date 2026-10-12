package timescale

import (
	"context"
	"fmt"
)

// Provenance labels for issuers.auth_flags_source. They MIRROR clickhouse.AuthFlagsSource,
// duplicated because the served-tier store must not import the lake reader.
// TestIssuerAuthFlagsSourceMirrorsTheMigrationCheck reconciles them against the migration's
// CHECK, since a member added to one copy compiles green while the database rejects every write.
const (
	// AuthFlagsSourceLive — the flags were decoded from the account's
	// CURRENT on-chain AccountEntry. The account exists.
	AuthFlagsSourceLive = "live"
	// AuthFlagsSourceLastKnownBeforeRemoval — the account has been MERGED
	// AWAY and these are its flags as of AsOfLedger. A historical record,
	// never the issuer's current authorisation policy.
	AuthFlagsSourceLastKnownBeforeRemoval = "last_known_before_removal"
)

// IssuerAuthFlags is one issuer's decoded AccountEntry auth flags, mirroring clickhouse.AccountAuthFlags as a separate type so the served tier does not import the lake reader.
type IssuerAuthFlags struct {
	GStrkey    string
	Required   bool
	Revocable  bool
	Immutable  bool
	Clawback   bool
	HomeDomain string
	// Source is how the four flags were obtained — one of the
	// AuthFlagsSource* constants above, or "" for "do not touch the
	// persisted provenance" (see [Store.PersistIssuerAuthFlags]).
	Source string
	// AsOfLedger is the ledger the reading is true as of. Required when
	// Source is AuthFlagsSourceLastKnownBeforeRemoval — "these flags are
	// old" is only actionable with "as of when", and migration 0153's
	// second CHECK enforces the same thing at the database.
	AsOfLedger *uint32
}

// validate refuses a row whose provenance is internally inconsistent, BEFORE it reaches Postgres.
// Two arms mirror the migration's CHECKs so the failure names the invariant; the third is not
// expressible as a constraint.
//
// The home_domain arm is defence in depth: a merged account's home_domain is a self-declared claim
// that can no longer be checked against SEP-1's [[CURRENCIES]] back-reference, so persisting it
// would be an impersonation surface. The lake reader already blanks it; this refuses to be the
// second way in rather than silently dropping the value.
func (f IssuerAuthFlags) validate() error {
	switch f.Source {
	case "":
		if f.AsOfLedger != nil {
			return fmt.Errorf("timescale: issuer %s: as-of ledger %d without a source", f.GStrkey, *f.AsOfLedger)
		}
	case AuthFlagsSourceLive:
	case AuthFlagsSourceLastKnownBeforeRemoval:
		if f.AsOfLedger == nil {
			return fmt.Errorf("timescale: issuer %s: %s reading carries no as-of ledger",
				f.GStrkey, AuthFlagsSourceLastKnownBeforeRemoval)
		}
		if f.HomeDomain != "" {
			return fmt.Errorf("timescale: issuer %s: %s reading carries home_domain %q — a merged account's self-declared identity is not persistable",
				f.GStrkey, AuthFlagsSourceLastKnownBeforeRemoval, f.HomeDomain)
		}
	default:
		return fmt.Errorf("timescale: issuer %s: unknown auth_flags_source %q", f.GStrkey, f.Source)
	}
	return nil
}

// IssuerGStrkeysNeedingFlags returns issuer G-strkeys without persisted auth flags, oldest-first by primary key so bounded runs make forward progress; limit <= 0 returns every candidate.
func (s *Store) IssuerGStrkeysNeedingFlags(ctx context.Context, limit int) ([]string, error) {
	q := `SELECT g_strkey FROM issuers WHERE auth_required IS NULL ORDER BY g_strkey`
	args := []any{}
	if limit > 0 {
		q += " LIMIT $1"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: IssuerGStrkeysNeedingFlags: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]string, 0, 1024)
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, fmt.Errorf("timescale: IssuerGStrkeysNeedingFlags scan: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: IssuerGStrkeysNeedingFlags rows: %w", err)
	}
	return out, nil
}

// IssuerGStrkeysNeedingRecheck returns issuer G-strkeys whose persisted auth flags are a last-known reading from a merged-away account, oldest-first by primary key; limit <= 0 returns every candidate.
func (s *Store) IssuerGStrkeysNeedingRecheck(ctx context.Context, limit int) ([]string, error) {
	q := `SELECT g_strkey FROM issuers WHERE auth_flags_source = $1 ORDER BY g_strkey`
	args := []any{AuthFlagsSourceLastKnownBeforeRemoval}
	if limit > 0 {
		q += " LIMIT $2"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: IssuerGStrkeysNeedingRecheck: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]string, 0, 1024)
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, fmt.Errorf("timescale: IssuerGStrkeysNeedingRecheck scan: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: IssuerGStrkeysNeedingRecheck rows: %w", err)
	}
	return out, nil
}

// persistHomeDomain is what [Store.PersistIssuerAuthFlags] assigns to
// home_domain; it is named because the SEP-1 reset must compare against the
// exact same expression.
const persistHomeDomain = `CASE $7::text
		        WHEN '` + AuthFlagsSourceLive + `' THEN NULLIF($6, '')
		        WHEN '` + AuthFlagsSourceLastKnownBeforeRemoval + `' THEN NULL
		        ELSE COALESCE(NULLIF($6, ''), home_domain) END`

//nolint:gosec // G202: fragments are constant SQL (column names, CASE literals, persistHomeDomain/sep1ResetOnHomeDomainChange); values bind via $N
var persistIssuerAuthFlagsQuery = `
		UPDATE issuers SET
		    auth_required  = $2,
		    auth_revocable = $3,
		    auth_immutable = $4,
		    auth_clawback  = $5,
		    home_domain    = ` + persistHomeDomain + `,` + sep1ResetOnHomeDomainChange(persistHomeDomain) + `,
		    auth_flags_source = CASE WHEN $7::text = ''
		                             THEN auth_flags_source ELSE $7::text END,
		    auth_flags_as_of_ledger = CASE WHEN $7::text = ''
		                             THEN auth_flags_as_of_ledger ELSE $8::integer END
		 WHERE g_strkey = $1
		   AND ($7::text = '' OR $8::integer IS NULL OR auth_flags_as_of_ledger IS NULL
		        OR auth_flags_as_of_ledger <= $8::integer)`

// PersistIssuerAuthFlags writes decoded auth flags for the given issuers and returns rows changed; UPDATE not upsert so account entries never invent issuer rows, and a changed home_domain unbinds the row's SEP-1 state.
func (s *Store) PersistIssuerAuthFlags(ctx context.Context, flags []IssuerAuthFlags) (int, error) {
	if len(flags) == 0 {
		return 0, nil
	}
	for _, f := range flags {
		if err := f.validate(); err != nil {
			return 0, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("timescale: PersistIssuerAuthFlags begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, persistIssuerAuthFlagsQuery)
	if err != nil {
		return 0, fmt.Errorf("timescale: PersistIssuerAuthFlags prepare: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	changed := 0
	for _, f := range flags {
		var asOf any
		if f.AsOfLedger != nil {
			asOf = int64(*f.AsOfLedger)
		}
		res, err := stmt.ExecContext(ctx, f.GStrkey,
			f.Required, f.Revocable, f.Immutable, f.Clawback, f.HomeDomain,
			f.Source, asOf)
		if err != nil {
			return 0, fmt.Errorf("timescale: PersistIssuerAuthFlags[%s]: %w", f.GStrkey, err)
		}
		if n, err := res.RowsAffected(); err == nil {
			changed += int(n)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("timescale: PersistIssuerAuthFlags commit: %w", err)
	}
	return changed, nil
}
