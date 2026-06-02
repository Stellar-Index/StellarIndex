package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// LedgerIngestRow is one row of the ledger_ingest_log
// substrate-continuity record (migration 0051, ADR-0033 Phase 1/2).
type LedgerIngestRow struct {
	LedgerSeq               uint32
	LedgerCloseTime         time.Time
	LedgerHash              []byte // 32-byte raw
	PrevLedgerHash          []byte // 32-byte raw
	SorobanEventCount       int
	ClassicTradeEffectCount int
}

// ChainBreak is a ledger whose prev_ledger_hash does not match the
// previous ledger's ledger_hash — a substrate corruption signal.
type ChainBreak struct {
	LedgerSeq    uint32
	WantPrevHash string // hex of ledger[seq-1].ledger_hash
	GotPrevHash  string // hex of ledger[seq].prev_ledger_hash
}

// UpsertLedgerIngestLog writes (or refreshes) the substrate record
// for one fully-processed ledger. ON CONFLICT DO UPDATE so a
// re-census (census-backfill correcting a pre-event_index-fix count,
// or a replay) overwrites stale counts rather than being silently
// dropped.
func (s *Store) UpsertLedgerIngestLog(ctx context.Context, row LedgerIngestRow) error {
	if len(row.LedgerHash) != 32 {
		return fmt.Errorf("timescale: UpsertLedgerIngestLog: ledger_hash len %d, want 32 (ledger %d)", len(row.LedgerHash), row.LedgerSeq)
	}
	if len(row.PrevLedgerHash) != 32 {
		return fmt.Errorf("timescale: UpsertLedgerIngestLog: prev_ledger_hash len %d, want 32 (ledger %d)", len(row.PrevLedgerHash), row.LedgerSeq)
	}
	const q = `
        INSERT INTO ledger_ingest_log (
            ledger_seq, ledger_close_time, ledger_hash, prev_ledger_hash,
            soroban_event_count, classic_trade_effect_count, persisted_at
        ) VALUES ($1, $2, $3, $4, $5, $6, now())
        ON CONFLICT (ledger_seq) DO UPDATE SET
            ledger_close_time          = EXCLUDED.ledger_close_time,
            ledger_hash                = EXCLUDED.ledger_hash,
            prev_ledger_hash           = EXCLUDED.prev_ledger_hash,
            soroban_event_count        = EXCLUDED.soroban_event_count,
            classic_trade_effect_count = EXCLUDED.classic_trade_effect_count,
            persisted_at               = now()`
	if _, err := s.db.ExecContext(ctx, q,
		int64(row.LedgerSeq), row.LedgerCloseTime, row.LedgerHash, row.PrevLedgerHash,
		row.SorobanEventCount, row.ClassicTradeEffectCount,
	); err != nil {
		return fmt.Errorf("timescale: UpsertLedgerIngestLog (ledger %d): %w", row.LedgerSeq, err)
	}
	return nil
}

// FindLedgerIngestGaps returns every contiguous run of missing ledger
// sequences in [from, to] (inclusive). A genuinely complete substrate
// returns no gaps. Covers both interior gaps (a LAG window over the
// present sequences) and the leading/trailing boundary (present range
// narrower than [from, to]).
//
// Cheap: this runs over the narrow ledger_ingest_log btree on
// ledger_seq, NOT over trades. There is no statement_timeout dance
// because the table is one slim row per ledger.
func (s *Store) FindLedgerIngestGaps(ctx context.Context, from, to uint32) ([]LedgerGap, error) {
	if to < from {
		return nil, errors.New("timescale: FindLedgerIngestGaps: to < from")
	}

	// Present-range bounds first; cheap and tells us about boundary gaps.
	var (
		minSeq sql.NullInt64
		maxSeq sql.NullInt64
		cnt    int64
	)
	const boundsQ = `
        SELECT MIN(ledger_seq), MAX(ledger_seq), COUNT(*)
        FROM ledger_ingest_log
        WHERE ledger_seq BETWEEN $1 AND $2`
	if err := s.db.QueryRowContext(ctx, boundsQ, int64(from), int64(to)).Scan(&minSeq, &maxSeq, &cnt); err != nil {
		return nil, fmt.Errorf("timescale: FindLedgerIngestGaps bounds: %w", err)
	}

	// Nothing present at all → the whole window is one gap.
	if cnt == 0 || !minSeq.Valid {
		return []LedgerGap{newLedgerGap(int64(from), int64(to))}, nil
	}

	var gaps []LedgerGap
	if uint32(minSeq.Int64) > from {
		gaps = append(gaps, newLedgerGap(int64(from), minSeq.Int64-1))
	}

	// Interior gaps via LAG over present sequences.
	const interiorQ = `
        SELECT prev_seq + 1 AS gap_from, ledger_seq - 1 AS gap_to
        FROM (
            SELECT ledger_seq,
                   LAG(ledger_seq) OVER (ORDER BY ledger_seq) AS prev_seq
            FROM ledger_ingest_log
            WHERE ledger_seq BETWEEN $1 AND $2
        ) t
        WHERE prev_seq IS NOT NULL AND ledger_seq - prev_seq > 1
        ORDER BY gap_from`
	rows, err := s.db.QueryContext(ctx, interiorQ, int64(from), int64(to))
	if err != nil {
		return nil, fmt.Errorf("timescale: FindLedgerIngestGaps interior: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var gf, gt int64
		if err := rows.Scan(&gf, &gt); err != nil {
			return nil, fmt.Errorf("timescale: FindLedgerIngestGaps scan: %w", err)
		}
		gaps = append(gaps, newLedgerGap(gf, gt))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: FindLedgerIngestGaps rows: %w", err)
	}

	if uint32(maxSeq.Int64) < to {
		gaps = append(gaps, newLedgerGap(maxSeq.Int64+1, int64(to)))
	}
	return gaps, nil
}

// newLedgerGap builds the package's LedgerGap (Start/End inclusive,
// Size precomputed) from an inclusive [start, end] range.
func newLedgerGap(start, end int64) LedgerGap {
	return LedgerGap{Start: start, End: end, Size: end - start + 1}
}

// VerifyLedgerHashChain returns every ledger in (from, to] whose
// prev_ledger_hash does not equal the previous ledger's ledger_hash.
// An empty result means the chain is unbroken across every adjacent
// pair we hold in the range — the cryptographic half of Claim 1.
//
// Only adjacent pairs BOTH present in the table are checked; missing
// ledgers are FindLedgerIngestGaps's job, not this query's.
func (s *Store) VerifyLedgerHashChain(ctx context.Context, from, to uint32) ([]ChainBreak, error) {
	if to < from {
		return nil, errors.New("timescale: VerifyLedgerHashChain: to < from")
	}
	const q = `
        SELECT a.ledger_seq,
               encode(b.ledger_hash, 'hex'),
               encode(a.prev_ledger_hash, 'hex')
        FROM ledger_ingest_log a
        JOIN ledger_ingest_log b ON b.ledger_seq = a.ledger_seq - 1
        WHERE a.ledger_seq BETWEEN $1 AND $2
          AND a.prev_ledger_hash <> b.ledger_hash
        ORDER BY a.ledger_seq`
	rows, err := s.db.QueryContext(ctx, q, int64(from), int64(to))
	if err != nil {
		return nil, fmt.Errorf("timescale: VerifyLedgerHashChain: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var breaks []ChainBreak
	for rows.Next() {
		var (
			seq             int64
			wantHex, gotHex string
		)
		if err := rows.Scan(&seq, &wantHex, &gotHex); err != nil {
			return nil, fmt.Errorf("timescale: VerifyLedgerHashChain scan: %w", err)
		}
		breaks = append(breaks, ChainBreak{LedgerSeq: uint32(seq), WantPrevHash: wantHex, GotPrevHash: gotHex})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: VerifyLedgerHashChain rows: %w", err)
	}
	return breaks, nil
}

// ClassicTradeEffectCountsByLedger returns the LCM-derived
// classic-trade-effect count per ledger in [from, to] — the "expected"
// side of the SDEX projection reconciliation (ADR-0033 Claim 2b,
// classic). SDEX predates Soroban so there is no soroban_events to
// re-derive from; this census (one ClaimAtom = one SDEX trade) is the
// decoder-independent oracle instead. Ledgers with zero trade effects
// are omitted (ReconcileCounts treats absent as 0).
//
// Reads ledger_ingest_log, so it is only meaningful where the substrate
// record covers the range — callers should verify continuity
// (FindLedgerIngestGaps) first.
func (s *Store) ClassicTradeEffectCountsByLedger(ctx context.Context, from, to uint32) (map[uint32]int, error) {
	if to < from {
		return nil, fmt.Errorf("timescale: ClassicTradeEffectCountsByLedger: to (%d) < from (%d)", to, from)
	}
	const q = `
        SELECT ledger_seq, classic_trade_effect_count
        FROM ledger_ingest_log
        WHERE ledger_seq BETWEEN $1 AND $2 AND classic_trade_effect_count > 0`
	rows, err := s.db.QueryContext(ctx, q, int64(from), int64(to))
	if err != nil {
		return nil, fmt.Errorf("timescale: ClassicTradeEffectCountsByLedger [%d,%d]: %w", from, to, err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[uint32]int)
	for rows.Next() {
		var seq, n int64
		if err := rows.Scan(&seq, &n); err != nil {
			return nil, fmt.Errorf("timescale: ClassicTradeEffectCountsByLedger scan: %w", err)
		}
		out[uint32(seq)] = int(n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ClassicTradeEffectCountsByLedger rows: %w", err)
	}
	return out, nil
}

// SorobanEventsTimeBound returns the [lo, hi] ledger_close_time span of
// the ledgers [from, to] as recorded in ledger_ingest_log, plus whether
// the log FULLY covers that range (one row per ledger). It exists to
// make soroban_events ledger-range queries prune chunks: that table is
// partitioned by ledger_close_time, so a `WHERE ledger BETWEEN` filter
// alone scans every chunk. Adding `AND ledger_close_time BETWEEN lo AND
// hi` restores pruning.
//
// The fullyCovered guard is a correctness requirement, not just an
// optimization: a soroban_events row's ledger_close_time equals its
// ledger's close time exactly (same LCM), so [lo, hi] over a FULLY
// covered range bounds every soroban_events row whose ledger is in
// [from, to]. If the log is missing ledgers in the range, a partial
// [lo, hi] could exclude rows whose ledger is in range — so callers
// MUST only apply the bound when fullyCovered is true, and otherwise
// fall back to the (correct, slower) unpruned scan.
func (s *Store) SorobanEventsTimeBound(ctx context.Context, from, to uint32) (lo, hi time.Time, fullyCovered bool, err error) {
	if to < from {
		return time.Time{}, time.Time{}, false, fmt.Errorf("timescale: SorobanEventsTimeBound: to (%d) < from (%d)", to, from)
	}
	const q = `
        SELECT min(ledger_close_time), max(ledger_close_time), count(*)
        FROM ledger_ingest_log WHERE ledger_seq BETWEEN $1 AND $2`
	var (
		loN, hiN sql.NullTime
		cnt      int64
	)
	if err := s.db.QueryRowContext(ctx, q, int64(from), int64(to)).Scan(&loN, &hiN, &cnt); err != nil {
		return time.Time{}, time.Time{}, false, fmt.Errorf("timescale: SorobanEventsTimeBound [%d,%d]: %w", from, to, err)
	}
	if !loN.Valid || !hiN.Valid {
		return time.Time{}, time.Time{}, false, nil
	}
	expected := int64(to) - int64(from) + 1
	return loN.Time, hiN.Time, cnt == expected, nil
}

// LedgerIngestExtent returns the min and max ledger_seq present in the
// table (ok=false when the table is empty). Used to bound watermark
// computation without scanning trades.
func (s *Store) LedgerIngestExtent(ctx context.Context) (minSeq, maxSeq uint32, ok bool, err error) {
	var lo, hi sql.NullInt64
	const q = `SELECT MIN(ledger_seq), MAX(ledger_seq) FROM ledger_ingest_log`
	if err = s.db.QueryRowContext(ctx, q).Scan(&lo, &hi); err != nil {
		return 0, 0, false, fmt.Errorf("timescale: LedgerIngestExtent: %w", err)
	}
	if !lo.Valid || !hi.Valid {
		return 0, 0, false, nil
	}
	return uint32(lo.Int64), uint32(hi.Int64), true, nil
}
