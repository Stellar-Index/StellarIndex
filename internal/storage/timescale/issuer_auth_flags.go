package timescale

import (
	"context"
	"fmt"
)

// Provenance labels for issuers.auth_flags_source (migration 0153).
//
// These MIRROR clickhouse.AuthFlagsSource. They are duplicated rather than
// imported because the served-tier store must not import the lake reader
// (see [IssuerAuthFlags]) — the copies are reconciled against the migration's
// CHECK by TestIssuerAuthFlagsSourceMirrorsTheMigrationCheck, because an
// enumerated string set whose copies are never colocated drifts silently:
// a member added to one compiles green while the database rejects every write.
const (
	// AuthFlagsSourceLive — the flags were decoded from the account's
	// CURRENT on-chain AccountEntry. The account exists.
	AuthFlagsSourceLive = "live"
	// AuthFlagsSourceLastKnownBeforeRemoval — the account has been MERGED
	// AWAY and these are its flags as of AsOfLedger. A historical record,
	// never the issuer's current authorisation policy.
	AuthFlagsSourceLastKnownBeforeRemoval = "last_known_before_removal"
)

// IssuerAuthFlags is one issuer's decoded AccountEntry auth flags, ready
// to persist. Mirrors clickhouse.AccountAuthFlags; kept as a separate
// type so the served-tier store does not import the lake reader.
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

// validate refuses a row whose provenance is internally inconsistent, BEFORE
// it reaches Postgres. Two of the three arms mirror migration 0153's CHECKs
// so the failure names the invariant instead of surfacing as a constraint
// violation; the third is not expressible as a column constraint at all.
//
// The home_domain arm is defence in depth. A merged account's home_domain is
// a self-declared identity claim that can no longer be checked against SEP-1's
// bidirectional [[CURRENCIES]] back-reference, so persisting one would create
// an impersonation surface on exactly the accounts that can no longer be
// verified on-chain — and it is not hypothetical: 979 of 985 recovered
// pre-images in a 1,000-issuer r1 sample (2026-09-03) carry one, including
// `stellarkraken.com` and `stellarbrunch.com` on accounts that no longer
// exist. clickhouse.RemovedAccountsLastKnownAuthFlags already blanks it at
// the reader, which is the primary defence; this refuses to be the second
// way in rather than silently dropping the value.
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

// IssuerGStrkeysNeedingFlags returns issuer G-strkeys whose auth flags are
// not yet persisted, oldest-first by primary key so repeated bounded runs
// make forward progress instead of re-walking the same head.
//
// `limit` <= 0 returns every candidate.
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

// IssuerGStrkeysNeedingRecheck returns issuer G-strkeys whose persisted auth
// flags are a LAST-KNOWN reading taken from an account that had been merged
// away, oldest-first by primary key like [Store.IssuerGStrkeysNeedingFlags].
//
// `limit` <= 0 returns every candidate.
//
// # WHY A SECOND QUEUE
//
// A Stellar account can be re-created at the same address after an
// account_merge, at which point a `last_known_before_removal` reading stops
// being true. Nothing else would ever revisit it: the primary queue is
// `auth_required IS NULL`, and these rows HAVE auth_required, so filling them
// takes them out of the drain's sight for good. Without this queue the
// provenance column would be a one-way latch — the fix for the residue would
// have created a fresh class of permanently-stale rows.
//
// `live` rows are deliberately NOT re-checked here. They can go stale too (an
// issuer that merges its account tomorrow), but the API's read path already
// re-reads a live AccountEntry per request and outranks the persisted value,
// so a stale `live` row is corrected on the next drain over the same data
// rather than needing its own queue. A `last_known` row is the one the read
// path CANNOT correct on its own: absence from the current-state projection
// is what a merged account and a lake-coverage gap both look like, so the API
// may never conclude "removed" — only this job, which reads an actual
// `removed` row, may.
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

// PersistIssuerAuthFlags writes decoded auth flags for the given issuers,
// returning how many rows it actually changed.
//
// # WHY THIS EXISTS
//
// The flags already resolve at READ time — Server.enrichIssuerFromAccountState
// decodes them from the lake's AccountEntry per request, and measured
// 39/40 of the top issuers resolve. But that path depends on the
// ClickHouse lake plus a warm account-state cache: under burst load the
// refresh gate degrades and a cold issuer page renders "not yet
// resolved". Persisting the decoded values gives the read path a durable
// fallback that survives a cold cache, a restart, and a load spike.
//
// UPDATE, not upsert: an issuer row must already exist. This job fills
// columns on known issuers; it is not a discovery path, and inventing
// issuer rows from account entries would put accounts in the issuers
// table that never issued anything.
//
// home_domain is only written when we HAVE one and the row does not —
// COALESCE keeps an existing value, because the SEP-1 resolver's domain
// is better sourced than the AccountEntry's and must not be clobbered.
//
// # PROVENANCE (#374)
//
// auth_flags_source + auth_flags_as_of_ledger move TOGETHER or not at all.
// Writing a new source beside a retained as-of ledger would assert that the
// reading is true as of a ledger it was not taken from — a quieter, more
// credible falsehood than the unlabelled column this replaces. An empty
// Source therefore leaves BOTH columns untouched rather than nulling them:
// "unknown provenance" is a safe state to leave alone, and clearing a
// known-good label would be a regression, not a no-op.
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

	const q = `
		UPDATE issuers SET
		    auth_required  = $2,
		    auth_revocable = $3,
		    auth_immutable = $4,
		    auth_clawback  = $5,
		    home_domain    = COALESCE(NULLIF(home_domain, ''), NULLIF($6, '')),
		    auth_flags_source = CASE WHEN $7::text = ''
		                             THEN auth_flags_source ELSE $7::text END,
		    auth_flags_as_of_ledger = CASE WHEN $7::text = ''
		                             THEN auth_flags_as_of_ledger ELSE $8::integer END
		 WHERE g_strkey = $1`
	stmt, err := tx.PrepareContext(ctx, q)
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
