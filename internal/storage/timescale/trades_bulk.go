package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// ─── bulk historical backfill writer ─────────────────────────────────────
//
// [Store.BulkBackfillTrades] is the BACKFILL-ONLY twin of
// [Store.BatchInsertTrades]. It exists because the two have opposite cost
// profiles and only one of them is on the live ingest path.
//
// Measured (integration harness, TimescaleDB 2.26.4-pg15, real `trades`
// hypertable with all nine indexes; see
// test/integration/trades_bulk_backfill_test.go):
//
//   - Landing a batch is ~2.4x slower into a COMPRESSED chunk than into an
//     uncompressed one, and ~3.3x slower for COPY — TimescaleDB has to
//     enforce the trade PK against compressed data. `trades` compresses at 7
//     days (migration 0001), so EVERY chunk a historical backfill targets is
//     compressed and pays that. Nothing in this file can remove that cost.
//   - `usd_volume` resolution costs MORE than the write. It is one
//     [tradeUSDVolume] call per row, and on a cache miss that is one to four
//     serial `prices_1m` round trips ([VWAPUSDFXResolver]). On an empty
//     historical range every one of those is a miss, and
//     [Store.BatchInsertTrades] runs them strictly one after another inside
//     [Store.tradeBatchValues]. Measured on the harness: 6.7k rows/s with the
//     resolvers installed against 26k rows/s without them — i.e. ~74 % of the
//     wall clock is per-row FX latency, not the INSERT.
//
// So the win here is CONCURRENCY over a latency-bound workload, not a
// cleverer statement: resolve `usd_volume` for the whole buffer through a
// bounded worker pool, then land the rows through parallel binary COPY
// streams over DISJOINT conflict-key partitions. COPY itself is worth ~1.4x
// on an uncompressed chunk and ~1.0x on a compressed one — it is the smaller
// half of the change, kept because it is free once the rows are resolved.
//
// WHY THIS IS NOT A CHANGE TO THE LIVE WRITER. [Store.BatchInsertTrades]
// handles a genuinely conflicting stream (replays, dual-sink retries, CEX WS
// redelivery) and its ON CONFLICT ... DO UPDATE is load-bearing. COPY has no
// conflict handling at all: a duplicate PK aborts the whole stream. That is
// only safe on a range PROVEN empty, which is a backfill-shaped precondition
// and never a live-ingest one. The live path is therefore untouched, and this
// writer FALLS BACK to it whenever the precondition does not hold.

// tradeBulkColumns is the COPY column list. It is EXACTLY the column list
// [Store.BatchInsertTrades] and [Store.InsertTrade] write, in the same order,
// so a row landed by either path is byte-identical.
//
// `ingested_at` is deliberately absent (both paths let the column default
// fire), and so are `routed_via` and `signer` — neither insert path writes
// them. They are back-filled after the fact by their own sweepers
// (internal/storage/timescale/routed_via.go and signer.go), which key off
// rows that are already stored, so a bulk-loaded row is picked up exactly
// like an upserted one.
var tradeBulkColumns = []string{
	"source", "ledger", "tx_hash", "op_index", "ts",
	"base_asset", "quote_asset",
	"base_amount", "quote_amount", "usd_volume",
	"maker", "taker", "derive_generation",
}

// Bulk-writer defaults. Both pools are bounded well under
// [PoolMaxOpenConns] so a backfill can never starve the store's other
// callers of connections.
const (
	bulkDefaultResolvers = 8
	bulkDefaultWriters   = 4
	// bulkMinPartition keeps a COPY stream worth opening: below this many
	// rows the per-connection setup outweighs the parallelism.
	bulkMinPartition = 2_000
)

// BulkBackfillOptions tunes [Store.BulkBackfillTrades]. The zero value is the
// supported default.
type BulkBackfillOptions struct {
	// Resolvers is the number of goroutines that compute `usd_volume`
	// concurrently. 0 = [bulkDefaultResolvers]. 1 makes resolution serial,
	// which is what [Store.BatchInsertTrades] does.
	Resolvers int

	// Writers is the number of parallel COPY streams. 0 =
	// [bulkDefaultWriters]. Partitions are contiguous slices of the
	// conflict-key-sorted row set, so no two writers can touch the same
	// trade PK and the AB/BA deadlock the batch writer's sort exists to
	// prevent (see [sortTradesByConflictKey]) cannot arise between them.
	Writers int

	// MaxTuplesDecompressedPerDML, when > 0, is applied as
	// `SET LOCAL timescaledb.max_tuples_decompressed_per_dml_transaction`
	// inside each COPY transaction.
	//
	// It is a KNOB, not a default: a backfill into a provably-empty range
	// should decompress nothing, but a `trades` chunk is 7 days wide and can
	// hold this source's rows OUTSIDE the backfilled ledger window, in which
	// case TimescaleDB may still decompress the overlapping segments to
	// enforce the PK. When it does and the server cap is hit, the COPY errors
	// and this writer falls back to the upsert path — raising the cap here is
	// the documented lever for pushing a stubborn window through instead.
	MaxTuplesDecompressedPerDML int
}

// BulkBackfillPath names which writer actually landed the rows.
type BulkBackfillPath string

const (
	// BulkBackfillPathCopy means the target range was proven empty and the
	// rows landed through the parallel COPY writer.
	BulkBackfillPathCopy BulkBackfillPath = "copy"
	// BulkBackfillPathUpsert means the precondition did not hold and the
	// rows were handed to [Store.BatchInsertTrades] instead — the ordinary
	// generation-guarded corrective upsert, unchanged.
	BulkBackfillPathUpsert BulkBackfillPath = "upsert-fallback"
)

// BulkBackfillResult reports what [Store.BulkBackfillTrades] did.
type BulkBackfillResult struct {
	// Path is the writer that ran.
	Path BulkBackfillPath
	// Attempted is the number of storable, intra-batch-deduped rows the
	// writer presented to Postgres. It is NOT len(trades): unstorable rows
	// (the SDEX one-side-zero fill, INV-6) and intra-batch PK duplicates are
	// dropped first, exactly as [Store.BatchInsertTrades] drops them.
	Attempted int
	// Copied is the number of rows the COPY streams reported landing. Zero
	// on the fallback path (the upsert reports through its own metrics).
	Copied int64
	// FallbackReason is empty on the COPY path and otherwise says why the
	// precondition was refused. Surfaced so an operator can see that a run
	// silently reverted to the slow path rather than assuming it did not.
	FallbackReason string
}

// BulkBackfillTrades writes a large buffer of historical trades, using the
// fast COPY path when — and only when — the target range is PROVABLY empty.
//
// PRECONDITION, CHECKED HERE, NEVER ASSUMED FROM THE CALLER. For every source
// in the buffer this probes `trades` for any stored row with that source and a
// ledger inside the buffer's ledger extent and a `ts` inside its ts extent.
// Any row a COPY'd row could collide with on the PK
// (source, ledger, tx_hash, op_index, ts) necessarily satisfies that
// predicate, so an empty result is a proof — not a heuristic — that no COPY'd
// row can conflict. The probe is scoped by ledger AND ts: the ledger bound is
// the repo's no-unbounded-trade-scan rule, the ts bound lets TimescaleDB prune
// to the backfilled chunks.
//
// If the probe finds ANYTHING, this does not COPY. It hands the ORIGINAL,
// unmodified buffer to [Store.BatchInsertTrades] and returns
// [BulkBackfillPathUpsert]. Behaviour on a non-empty range is therefore
// bit-for-bit today's behaviour, including the INV-3 generation guard and the
// per-source outcome metrics.
//
// ROW IDENTITY. The rows this writes are the rows [Store.BatchInsertTrades]
// would write. It runs the same [Store.filterStorableTrades] gate, the same
// [sortTradesByConflictKey] + [dedupeSortedTradesByConflictKey] collapse, and
// the same [tradeUSDVolume] resolution behind the same
// [Store.reDeriveNullVolumeGuard] — the only difference is that resolution is
// fanned across goroutines instead of run in a loop, which cannot change a
// per-row result because the resolver is a pure read of static historical
// state during a backfill. The same side effects follow the landed rows: the
// `source_entry_counts` tally, the unit-ratio sentinel, the classic-asset /
// issuer registry hook and the insert-outcome metrics.
//
// CONCURRENT MUTATION OF THE RANGE. The probe and the COPY are not one
// transaction, so a writer that lands a conflicting row in between makes a
// COPY stream fail with a unique violation. That is caught: the partitions
// that DID commit are accounted for (their `source_entry_counts` bump and
// landed-row side effects run), then the whole buffer is replayed through
// [Store.BatchInsertTrades], whose upsert is idempotent against those rows —
// it re-presents them as conflicts, so it does not double-bump the tally.
// Stored data converges exactly; the only residue is that the ATTEMPT
// counters (obs.TradeInsertsTotal, and obs.SourceInsertErrorsTotal for a row
// that fails Validate) count the recovered rows twice, which is the correct
// reading of "attempts" and is confined to this error path. The result
// reports the fallback. (`ch-rebuild` already refuses a window the live projector's cursor
// is inside — checkCHRebuildLiveOverlap — so this is the belt to that
// braces.)
func (s *Store) BulkBackfillTrades(ctx context.Context, trades []canonical.Trade, opts BulkBackfillOptions) (BulkBackfillResult, error) {
	if len(trades) == 0 {
		return BulkBackfillResult{Path: BulkBackfillPathCopy}, nil
	}

	// Probe BEFORE any per-row work: falling back after resolving usd_volume
	// would double-count obs.TradeInsertsTotal against the upsert path's own
	// resolution pass.
	empty, reason, err := s.bulkRangeIsEmpty(ctx, trades)
	if err != nil {
		return BulkBackfillResult{}, err
	}
	if !empty {
		return s.bulkFallback(ctx, trades, reason)
	}

	storable := s.filterStorableTrades(trades)
	if len(storable) == 0 {
		return BulkBackfillResult{Path: BulkBackfillPathCopy}, nil
	}
	// Copy before sorting: filterStorableTrades returns the caller's slice
	// unchanged in the all-valid case, and the caller's buffer must not be
	// reordered under it (BatchInsertTrades sorts in place because its input
	// is a short-lived batch; this one is the whole drain buffer).
	rows := make([]canonical.Trade, len(storable))
	copy(rows, storable)
	sortTradesByConflictKey(rows)
	rows = dedupeSortedTradesByConflictKey(rows)

	usd, err := s.resolveBulkUSDVolumes(ctx, rows, opts.Resolvers)
	if err != nil {
		return BulkBackfillResult{}, err
	}

	landed, copied, cerr := s.copyTradePartitions(ctx, rows, usd, opts)
	// Whatever committed, committed — account for it BEFORE deciding what to
	// do about the failure, or a partially-failed run leaves
	// source_entry_counts short by the rows that did land (the recovery
	// upsert re-presents them as conflicts, so its own bump will not count
	// them either).
	for _, p := range landed {
		if err := s.bumpBulkSourceCounts(ctx, rows[p[0]:p[1]]); err != nil {
			return BulkBackfillResult{}, err
		}
		s.recordBulkLandedEffects(ctx, rows[p[0]:p[1]])
	}
	if cerr != nil {
		if isUniqueViolation(cerr) {
			// The range stopped being empty under us. Replay the whole buffer
			// through the idempotent upsert; it converges over the partitions
			// that already landed.
			return s.bulkFallback(ctx, trades, fmt.Sprintf("COPY hit a unique violation (%v)", cerr))
		}
		return BulkBackfillResult{}, cerr
	}
	if copied != int64(len(rows)) {
		return BulkBackfillResult{}, fmt.Errorf(
			"timescale: BulkBackfillTrades: COPY reported %d rows, sent %d", copied, len(rows))
	}

	return BulkBackfillResult{
		Path:      BulkBackfillPathCopy,
		Attempted: len(rows),
		Copied:    copied,
	}, nil
}

// bulkFallback hands the untouched buffer to the ordinary batch upsert.
func (s *Store) bulkFallback(ctx context.Context, trades []canonical.Trade, reason string) (BulkBackfillResult, error) {
	if err := s.BatchInsertTrades(ctx, trades); err != nil {
		return BulkBackfillResult{}, err
	}
	return BulkBackfillResult{
		Path:           BulkBackfillPathUpsert,
		Attempted:      len(trades),
		FallbackReason: reason,
	}, nil
}

// bulkSourceExtent is one source's (ledger, ts) bounding box within a buffer.
type bulkSourceExtent struct {
	minLedger, maxLedger uint32
	minTS, maxTS         time.Time
}

// bulkExtents computes the per-source bounding box the emptiness probe needs.
func bulkExtents(trades []canonical.Trade) map[string]bulkSourceExtent {
	out := make(map[string]bulkSourceExtent, 4)
	for i := range trades {
		t := &trades[i]
		ts := t.Timestamp.UTC()
		e, seen := out[t.Source]
		if !seen {
			out[t.Source] = bulkSourceExtent{
				minLedger: t.Ledger, maxLedger: t.Ledger, minTS: ts, maxTS: ts,
			}
			continue
		}
		e.minLedger = min(e.minLedger, t.Ledger)
		e.maxLedger = max(e.maxLedger, t.Ledger)
		if ts.Before(e.minTS) {
			e.minTS = ts
		}
		if ts.After(e.maxTS) {
			e.maxTS = ts
		}
		out[t.Source] = e
	}
	return out
}

// bulkRangeIsEmpty proves (or refuses) the COPY precondition. See
// [Store.BulkBackfillTrades] for why the (source, ledger, ts) box is a
// sufficient superset of the PK space the buffer occupies.
//
// Every bind parameter is explicitly cast. An untyped parameter next to a
// timestamptz comparison is the 42883 class this repo has been bitten by
// (feedback_untyped_param_beside_interval_42883) — it compiles clean and
// fails at run time, which for a PRECONDITION check would mean the fast path
// is never taken rather than a visible error.
func (s *Store) bulkRangeIsEmpty(ctx context.Context, trades []canonical.Trade) (bool, string, error) {
	const q = `
        SELECT EXISTS (
            SELECT 1
              FROM trades
             WHERE source = $1::text
               AND ledger BETWEEN $2::int AND $3::int
               AND ts     >= $4::timestamptz
               AND ts     <= $5::timestamptz
        )`
	for source, e := range bulkExtents(trades) {
		var occupied bool
		if err := s.db.QueryRowContext(ctx, q,
			source, int64(e.minLedger), int64(e.maxLedger), e.minTS, e.maxTS,
		).Scan(&occupied); err != nil {
			return false, "", fmt.Errorf(
				"timescale: BulkBackfillTrades empty-range probe (%s, ledgers %d-%d): %w",
				source, e.minLedger, e.maxLedger, err)
		}
		if occupied {
			return false, fmt.Sprintf(
				"source %q already has stored rows in ledgers %d-%d",
				source, e.minLedger, e.maxLedger), nil
		}
	}
	return true, "", nil
}

// resolveBulkUSDVolumes computes `usd_volume` for every row through the SAME
// [tradeUSDVolume] waterfall the row paths use, fanned across a bounded
// worker pool.
//
// This is where the backfill's wall clock actually goes: on a historical
// range the FX resolver misses its cache on most rows and pays a serial
// `prices_1m` round trip (or several) for each miss. The result is a pure
// function of the row and of DB state that a backfill does not modify, so
// running the calls concurrently changes latency and nothing else.
//
// A [Store.reDeriveNullVolumeGuard] failure is returned for the LOWEST row
// index that tripped it, so the error a caller sees does not depend on which
// goroutine lost the race.
func (s *Store) resolveBulkUSDVolumes(ctx context.Context, rows []canonical.Trade, workers int) ([]sql.NullString, error) {
	if workers <= 0 {
		workers = bulkDefaultResolvers
	}
	workers = min(workers, len(rows))
	out := make([]sql.NullString, len(rows))
	errAt := make([]error, len(rows))

	var wg sync.WaitGroup
	next := make(chan int, workers)
	go func() {
		defer close(next)
		for i := range rows {
			next <- i
		}
	}()
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				v := tradeUSDVolume(ctx, rows[i], s.usdVolumeQuoteSpec, s.usdVolumeFXResolver)
				if err := s.reDeriveNullVolumeGuard(rows[i], v); err != nil {
					errAt[i] = err
					continue
				}
				obs.TradeInsertsTotal.WithLabelValues(rows[i].Source, usdPopulatedLabel(v != nil)).Inc()
				if v != nil {
					out[i] = sql.NullString{String: *v, Valid: true}
				}
			}
		}()
	}
	wg.Wait()
	for i := range errAt {
		if errAt[i] != nil {
			return nil, errAt[i]
		}
	}
	return out, nil
}

// bulkPartitions splits n rows into at most `writers` contiguous ranges, none
// smaller than [bulkMinPartition]. Contiguity is what makes the split safe:
// the input is conflict-key sorted and deduped, so two partitions can never
// present the same trade PK.
func bulkPartitions(n, writers int) [][2]int {
	if writers <= 0 {
		writers = bulkDefaultWriters
	}
	writers = min(writers, max(1, n/bulkMinPartition))
	per := (n + writers - 1) / writers
	out := make([][2]int, 0, writers)
	for lo := 0; lo < n; lo += per {
		out = append(out, [2]int{lo, min(lo+per, n)})
	}
	return out
}

// copyTradePartitions streams the resolved rows into `trades` over parallel
// binary COPY connections.
//
// It returns the index ranges that COMMITTED, not just a total, and it
// returns them EVEN WHEN ANOTHER PARTITION FAILED. Each partition is its own
// transaction, so a failure rolls that partition back and leaves the others
// durably landed; the caller needs to know which rows those were to keep the
// `source_entry_counts` tally exact while it recovers.
func (s *Store) copyTradePartitions(ctx context.Context, rows []canonical.Trade, usd []sql.NullString, opts BulkBackfillOptions) (landed [][2]int, copied int64, err error) {
	values := s.bulkTradeValues(rows, usd)
	parts := bulkPartitions(len(rows), opts.Writers)
	counts := make([]int64, len(parts))
	errs := make([]error, len(parts))

	var wg sync.WaitGroup
	for i, p := range parts {
		wg.Add(1)
		go func(i int, lo, hi int) {
			defer wg.Done()
			counts[i], errs[i] = s.copyTradeRange(ctx, values[lo:hi], opts.MaxTuplesDecompressedPerDML)
		}(i, p[0], p[1])
	}
	wg.Wait()

	for i := range parts {
		if errs[i] != nil {
			if err == nil {
				err = errs[i]
			}
			continue
		}
		landed = append(landed, parts[i])
		copied += counts[i]
	}
	return landed, copied, err
}

// copyTradeRange runs one partition's COPY on one pooled connection, inside a
// pgx-native transaction (the pgx stdlib driver does not expose COPY through
// database/sql, the same reason [Store.copyMerge] reaches for conn.Raw).
func (s *Store) copyTradeRange(ctx context.Context, values [][]any, decompressCap int) (int64, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("timescale: BulkBackfillTrades: conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	var copied int64
	rerr := conn.Raw(func(driverConn any) error {
		stdlibConn, ok := driverConn.(*stdlib.Conn)
		if !ok {
			// Same guard as copyMerge: a silent driver swap must fail loudly
			// rather than quietly skip the bulk load.
			return fmt.Errorf("timescale: BulkBackfillTrades: driver conn %T is not *stdlib.Conn", driverConn)
		}
		n, err := copyTradesOnConn(ctx, stdlibConn.Conn(), values, decompressCap)
		copied = n
		return err
	})
	return copied, rerr
}

func copyTradesOnConn(ctx context.Context, pgxConn *pgx.Conn, values [][]any, decompressCap int) (_ int64, err error) {
	tx, err := pgxConn.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("timescale: BulkBackfillTrades: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if decompressCap > 0 {
		// SET LOCAL: scoped to this transaction, so it can never leak onto a
		// pooled connection that later serves live ingest.
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			"SET LOCAL timescaledb.max_tuples_decompressed_per_dml_transaction = %d", decompressCap)); err != nil {
			return 0, fmt.Errorf("timescale: BulkBackfillTrades: decompression cap: %w", err)
		}
	}
	n, err := tx.CopyFrom(ctx, pgx.Identifier{"trades"}, tradeBulkColumns, pgx.CopyFromRows(values))
	if err != nil {
		return 0, fmt.Errorf("timescale: BulkBackfillTrades: copy %d rows: %w", len(values), err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("timescale: BulkBackfillTrades: commit: %w", err)
	}
	return n, nil
}

// bulkTradeValues renders the COPY tuples.
//
// Every conversion mirrors what the row paths bind. `maker`/`taker` go
// through [nullStrXfer] because both INSERT statements wrap those two
// placeholders in a NULLIF against the empty string: an absent counterparty
// must land as SQL NULL, never as a zero-length text value. Amounts go over
// as their decimal strings, which is what canonical.Amount's driver.Valuer
// produces for the INSERT path and what the sep41 COPY writers already send
// for a NUMERIC column.
func (s *Store) bulkTradeValues(rows []canonical.Trade, usd []sql.NullString) [][]any {
	out := make([][]any, len(rows))
	for i := range rows {
		t := &rows[i]
		out[i] = []any{
			t.Source, int64(t.Ledger), t.TxHash, int64(t.OpIndex), t.Timestamp.UTC(),
			t.Pair.Base.String(), t.Pair.Quote.String(),
			t.BaseAmount.String(), t.QuoteAmount.String(), usd[i],
			nullStrXfer(t.Maker), nullStrXfer(t.Taker), s.deriveGeneration,
		}
	}
	return out
}

// bumpBulkSourceCounts applies the `source_entry_counts` tally the batch
// path's `bump` CTE applies — once for the whole buffer instead of once per
// batch, because every COPY'd row is a genuine insert (the range was proven
// empty and the buffer is PK-deduped), so the count is exact without a
// RETURNING round trip.
//
// ORDER BY source mirrors the batch path: a mixed-source buffer row-locks one
// tally row per source, and a deterministic order is what keeps two writers
// from forming an AB/BA cycle on them (2026-07-09).
func (s *Store) bumpBulkSourceCounts(ctx context.Context, rows []canonical.Trade) error {
	perSource := make(map[string]int64, 4)
	for i := range rows {
		perSource[rows[i].Source]++
	}
	sources := make([]string, 0, len(perSource))
	counts := make([]int64, 0, len(perSource))
	for src := range perSource {
		sources = append(sources, src)
	}
	sort.Strings(sources)
	for _, src := range sources {
		counts = append(counts, perSource[src])
	}
	const q = `
        INSERT INTO source_entry_counts AS sec (source, entry_count, updated_at)
        SELECT src, cnt, now()
          FROM unnest($1::text[], $2::bigint[]) AS s(src, cnt)
         ORDER BY src
        ON CONFLICT (source) DO UPDATE
          SET entry_count = sec.entry_count + EXCLUDED.entry_count,
              updated_at  = EXCLUDED.updated_at`
	if _, err := s.db.ExecContext(ctx, q, sources, counts); err != nil {
		return fmt.Errorf("timescale: BulkBackfillTrades: source_entry_counts: %w", err)
	}
	return nil
}

// recordBulkLandedEffects runs the landed-row side effects the batch path
// runs from its RETURNING projection: the per-source outcome metric, the
// last-insert stamp, the unit-ratio sentinel and the classic-asset / issuer
// registry hook. Every COPY'd row landed, so "landed" is the whole set.
//
// The registry hook is soft-fail and dedupe-cached exactly as it is on the
// row paths — a registry write must not sink an already-committed load.
// (obs.TradeInsertsTotal, the usd-populated/not split, is already emitted
// during resolution — see resolveBulkUSDVolumes — so it is not repeated here.)
func (s *Store) recordBulkLandedEffects(ctx context.Context, rows []canonical.Trade) {
	perSource := make(map[string]int, 4)
	seenAssets := make(map[string]registryObservation, 16)
	assetOf := make(map[string]canonical.Asset, 16)
	for i := range rows {
		t := &rows[i]
		perSource[t.Source]++
		recordDexTradeUnitRatio(*t)
		for _, side := range [2]canonical.Asset{t.Pair.Base, t.Pair.Quote} {
			key := side.String()
			if prev, ok := seenAssets[key]; !ok || t.Ledger > prev.ledger {
				seenAssets[key] = registryObservation{ledger: t.Ledger, ts: t.Timestamp}
				assetOf[key] = side
			}
		}
	}
	now := float64(time.Now().Unix())
	for source, landed := range perSource {
		obs.TradeInsertOutcomeTotal.WithLabelValues(source, "new").Add(float64(landed))
		obs.SourceLastInsertUnix.WithLabelValues(source).Set(now)
	}
	for key, obsv := range seenAssets {
		if err := s.registerClassicAssetSeen(ctx, assetOf[key], obsv.ledger, obsv.ts); err != nil {
			slog.Default().Debug("timescale: bulk classic-asset registry upsert failed (soft-skip)",
				"asset", key, "ledger", obsv.ledger, "err", err)
		}
	}
}

// isUniqueViolation reports whether err is Postgres 23505.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
