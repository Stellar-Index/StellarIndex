package timescale

import (
	"context"
	"fmt"
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
func (s *Store) PersistIssuerAuthFlags(ctx context.Context, flags []IssuerAuthFlags) (int, error) {
	if len(flags) == 0 {
		return 0, nil
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
		    home_domain    = COALESCE(NULLIF(home_domain, ''), NULLIF($6, ''))
		 WHERE g_strkey = $1`
	stmt, err := tx.PrepareContext(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("timescale: PersistIssuerAuthFlags prepare: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	changed := 0
	for _, f := range flags {
		res, err := stmt.ExecContext(ctx, f.GStrkey, f.Required, f.Revocable, f.Immutable, f.Clawback, f.HomeDomain)
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
