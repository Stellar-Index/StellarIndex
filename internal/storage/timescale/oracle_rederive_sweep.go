package timescale

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// OracleRederiveSweep scopes [Store.SweepOracleRederive] to one source's
// re-derived on-chain ledger range.
type OracleRederiveSweep struct {
	Source   string
	From, To uint32
	// Generation is the re-derive run's own derive_generation. A row is
	// stale only if a row at exactly this generation shares its identity.
	// Zero is accepted on a dry run only, and then counts a twin at any
	// newer generation: what a sweep after a fresh re-derive would remove,
	// assuming the decoder still emits what the newest stored rows hold.
	Generation int64
	// AssetScoped adds (asset, quote) to the identity, for a source whose
	// (ledger, tx_hash, op_index) is not unique on its own: band packs
	// op*stride+slot, so nested relays in one op share an op_index.
	AssetScoped bool
	DryRun      bool
}

// OracleRederiveSweepResult reports one sweep.
type OracleRederiveSweepResult struct {
	// Deleted counts stale rows deleted, or that a -write run would delete.
	Deleted int64
	// FutureTS counts the Deleted rows stamped at least an hour after the
	// re-derived ts: the future-dated case.
	FutureTS int64
	// OpIndexShifted counts rows kept because they have no identity twin
	// but match a twin-generation row on (ledger, tx_hash, asset, quote, ts)
	// at another op_index. Never deleted: inspect by hand.
	OpIndexShifted int64
	// TSFrom/TSTo span every stale and twin ts in Deleted (zero when none):
	// the oracle_prices_* range a refresh must cover.
	TSFrom, TSTo time.Time
}

// oracleSweepLedgerWindow bounds each read's join to this many ledgers, so
// the twin set held for the hash join stays small on a full-history run.
const oracleSweepLedgerWindow = 500_000

// oracleSweepDeletesPerTx keeps one transaction under the TimescaleDB
// per-DML decompression cap (100k tuples by default): each delete pins
// source, asset, quote and ts, so it decompresses at most one compressed
// batch (≤1000 rows).
const oracleSweepDeletesPerTx = 50

// oracleSweepFutureTS is the band relay window: a stale row this far past
// its corrected ts was future-dated by an older derivation.
const oracleSweepFutureTS = time.Hour

type oracleSweepCandidate struct {
	ledger, opIndex int64
	txHash          string
	asset, quote    string
	ts, twinTS      time.Time
}

// SweepOracleRederive deletes the oracle_updates rows a ts-changing
// re-derive left behind: an on-chain row in [From, To] at a generation
// below Generation whose identity (source, ledger, tx_hash, op_index, plus
// asset and quote when AssetScoped) has a row at Generation with another
// ts. The newest row of an identity is never a candidate, and a row with
// no Generation twin is never deleted; every DELETE re-checks its twin by
// exact primary key. Each delete is bounded by ledger and by ts, and the
// source_entry_counts tally is decremented in the same transaction.
func (s *Store) SweepOracleRederive(ctx context.Context, sw OracleRederiveSweep) (OracleRederiveSweepResult, error) {
	var res OracleRederiveSweepResult
	if sw.Source == "" || sw.From == 0 || sw.To < sw.From {
		return res, fmt.Errorf("timescale: SweepOracleRederive: need a source and 0 < from <= to, got %q [%d,%d]", sw.Source, sw.From, sw.To)
	}
	if sw.Generation < 0 || (sw.Generation == 0 && !sw.DryRun) {
		return res, errors.New("timescale: SweepOracleRederive: a deleting sweep needs the run's positive derive_generation")
	}
	for lo := uint64(sw.From); lo <= uint64(sw.To); lo += oracleSweepLedgerWindow {
		hi := min(lo+oracleSweepLedgerWindow-1, uint64(sw.To))
		cands, err := s.oracleSweepCandidates(ctx, sw, lo, hi)
		if err != nil {
			return res, err
		}
		shifted, err := s.oracleSweepOpIndexShifted(ctx, sw, lo, hi)
		if err != nil {
			return res, err
		}
		res.OpIndexShifted += shifted
		deleted, err := s.oracleSweepDelete(ctx, sw, cands)
		res.add(deleted)
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

func (r *OracleRederiveSweepResult) add(deleted []oracleSweepCandidate) {
	r.Deleted += int64(len(deleted))
	for _, c := range deleted {
		if !c.ts.Before(c.twinTS.Add(oracleSweepFutureTS)) {
			r.FutureTS++
		}
		for _, t := range []time.Time{c.ts, c.twinTS} {
			if r.TSFrom.IsZero() || t.Before(r.TSFrom) {
				r.TSFrom = t
			}
			if t.After(r.TSTo) {
				r.TSTo = t
			}
		}
	}
}

// oracleSweepTwinSQL is the identity twin of row o, as a predicate on g.
// $4 is the twin generation (0 = any newer), $5 the asset scoping.
const oracleSweepTwinSQL = `
       g.source = o.source AND g.ledger = o.ledger
   AND g.tx_hash = o.tx_hash AND g.op_index = o.op_index
   AND g.ts <> o.ts
   AND g.derive_generation > o.derive_generation
   AND ($4::bigint = 0 OR g.derive_generation = $4::bigint)
   AND (NOT $5::boolean OR (g.asset = o.asset AND g.quote = o.quote))`

func (s *Store) oracleSweepCandidates(ctx context.Context, sw OracleRederiveSweep, lo, hi uint64) ([]oracleSweepCandidate, error) {
	q := `
        SELECT o.ledger, o.tx_hash, o.op_index, o.asset, o.quote, o.ts, min(g.ts)
          FROM oracle_updates o
          JOIN oracle_updates g ON ` + oracleSweepTwinSQL + `
           AND g.source = $1 AND g.ledger BETWEEN $2::integer AND $3::integer
         WHERE o.source = $1 AND o.ledger BETWEEN $2::integer AND $3::integer AND o.ledger > 0
         GROUP BY o.ledger, o.tx_hash, o.op_index, o.asset, o.quote, o.ts
         ORDER BY o.ts`
	rows, err := s.db.QueryContext(ctx, q, sw.Source, int64(lo), int64(hi), sw.Generation, sw.AssetScoped) //nolint:gosec // ledgers are uint32 widened to uint64.
	if err != nil {
		return nil, fmt.Errorf("timescale: SweepOracleRederive candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []oracleSweepCandidate
	for rows.Next() {
		var c oracleSweepCandidate
		if err := rows.Scan(&c.ledger, &c.txHash, &c.opIndex, &c.asset, &c.quote, &c.ts, &c.twinTS); err != nil {
			return nil, fmt.Errorf("timescale: SweepOracleRederive candidates scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: SweepOracleRederive candidates: %w", err)
	}
	return out, nil
}

func (s *Store) oracleSweepOpIndexShifted(ctx context.Context, sw OracleRederiveSweep, lo, hi uint64) (int64, error) {
	q := `
        SELECT count(*)
          FROM oracle_updates o
         WHERE o.source = $1 AND o.ledger BETWEEN $2::integer AND $3::integer AND o.ledger > 0
           AND EXISTS (SELECT 1 FROM oracle_updates g
                        WHERE g.source = o.source AND g.ledger = o.ledger AND g.tx_hash = o.tx_hash
                          AND g.asset = o.asset AND g.quote = o.quote AND g.ts = o.ts
                          AND g.op_index <> o.op_index
                          AND g.derive_generation > o.derive_generation
                          AND ($4::bigint = 0 OR g.derive_generation = $4::bigint))
           AND NOT EXISTS (SELECT 1 FROM oracle_updates g WHERE ` + oracleSweepTwinSQL + `)`
	var n int64
	if err := s.db.QueryRowContext(ctx, q, sw.Source, int64(lo), int64(hi), sw.Generation, sw.AssetScoped).Scan(&n); err != nil { //nolint:gosec // ledgers are uint32 widened to uint64.
		return 0, fmt.Errorf("timescale: SweepOracleRederive op_index-shift count: %w", err)
	}
	return n, nil
}

// oracleSweepDeleteSQL deletes one stale row by its exact primary key plus
// its segmentby columns, and only while its twin ($9 = twin ts) still
// exists at the run's generation.
const oracleSweepDeleteSQL = `
    DELETE FROM oracle_updates o
     WHERE o.source = $1 AND o.asset = $2 AND o.quote = $3 AND o.ts = $4
       AND o.ledger = $5 AND o.tx_hash = $6 AND o.op_index = $7
       AND o.derive_generation < $8::bigint
       AND EXISTS (SELECT 1 FROM oracle_updates g
                    WHERE g.source = $1 AND g.ts = $9 AND g.ledger = $5
                      AND g.tx_hash = $6 AND g.op_index = $7
                      AND g.derive_generation = $8::bigint
                      AND (NOT $10::boolean OR (g.asset = $2 AND g.quote = $3)))`

// oracleSweepDelete returns the candidates it deleted; on a dry run, all of them.
func (s *Store) oracleSweepDelete(ctx context.Context, sw OracleRederiveSweep, cands []oracleSweepCandidate) ([]oracleSweepCandidate, error) {
	if sw.DryRun {
		return cands, nil
	}
	var deleted []oracleSweepCandidate
	for start := 0; start < len(cands); start += oracleSweepDeletesPerTx {
		got, err := s.oracleSweepDeleteBatch(ctx, sw, cands[start:min(start+oracleSweepDeletesPerTx, len(cands))])
		if err != nil {
			return deleted, err
		}
		deleted = append(deleted, got...)
	}
	return deleted, nil
}

func (s *Store) oracleSweepDeleteBatch(ctx context.Context, sw OracleRederiveSweep, batch []oracleSweepCandidate) ([]oracleSweepCandidate, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("timescale: SweepOracleRederive begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var deleted []oracleSweepCandidate
	for _, c := range batch {
		r, err := tx.ExecContext(ctx, oracleSweepDeleteSQL,
			sw.Source, c.asset, c.quote, c.ts, c.ledger, c.txHash, c.opIndex,
			sw.Generation, c.twinTS, sw.AssetScoped)
		if err != nil {
			return nil, fmt.Errorf("timescale: SweepOracleRederive delete: %w", err)
		}
		k, err := r.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("timescale: SweepOracleRederive delete: %w", err)
		}
		if k > 0 {
			deleted = append(deleted, c)
		}
	}
	if len(deleted) > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE source_entry_counts SET entry_count = entry_count - $2, updated_at = now() WHERE source = $1`,
			sw.Source, int64(len(deleted))); err != nil {
			return nil, fmt.Errorf("timescale: SweepOracleRederive tally: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("timescale: SweepOracleRederive commit: %w", err)
	}
	return deleted, nil
}
