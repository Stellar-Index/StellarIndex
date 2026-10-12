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
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// ─── bulk historical backfill writer ─────────────────────────────────────
//
// [Store.BulkBackfillTrades] is the backfill-only twin of [Store.BatchInsertTrades], which is the
// live ingest path.
//
// Writes into compressed chunks are slow (PK enforced against compressed data) and `usd_volume`
// resolution costs up to four serial `prices_1m` round trips per row on a cache miss
// ([VWAPUSDFXResolver]). So resolve through a bounded worker pool, then land rows through
// parallel binary COPY streams over disjoint conflict-key partitions.
//
// COPY has no conflict handling (a duplicate PK aborts the stream), so it is only safe on a range
// proven empty; otherwise this falls back to the live writer, whose ON CONFLICT ... DO UPDATE is
// load-bearing for replays, dual-sink retries and CEX WS redelivery.

// tradeBulkColumns is exactly the column list [Store.BatchInsertTrades] and [Store.InsertTrade]
// write, in the same order, so a row landed by either path is byte-identical. `ingested_at`
// (column default), `routed_via` and `signer` are absent: their sweepers back-fill from stored rows.
var tradeBulkColumns = []string{
	"source", "ledger", "tx_hash", "op_index", "ts",
	"base_asset", "quote_asset",
	"base_amount", "quote_amount", "usd_volume",
	"maker", "taker", "derive_generation",
}

// Bulk-writer defaults. Both pools stay well under [PoolMaxOpenConns] so a backfill cannot
// starve the store's other callers of connections.
const (
	bulkDefaultResolvers = 8
	bulkDefaultWriters   = 4
	// bulkMinPartition: below this many rows per-connection setup outweighs the parallelism.
	bulkMinPartition = 2_000
)

// BulkBackfillOptions tunes [Store.BulkBackfillTrades].
type BulkBackfillOptions struct {
	// Resolvers is the number of goroutines computing `usd_volume`; 0 = [bulkDefaultResolvers],
	// 1 is serial like [Store.BatchInsertTrades].
	Resolvers int

	// Writers is the number of parallel COPY streams; 0 = [bulkDefaultWriters]. Partitions are
	// contiguous slices of the conflict-key-sorted rows, so no two writers touch the same trade PK
	// and the AB/BA deadlock [sortTradesByConflictKey] prevents cannot arise between them.
	Writers int

	// MaxTuplesDecompressedPerDML, when > 0, is applied as
	// `SET LOCAL timescaledb.max_tuples_decompressed_per_dml_transaction` in each COPY transaction.
	//
	// A knob, not a default: a `trades` chunk is 7 days wide and can hold this source's rows
	// outside the backfilled window, so TimescaleDB may still decompress segments to enforce the
	// PK. If the server cap is hit the COPY errors and the writer falls back to the upsert path;
	// raising the cap pushes a stubborn window through instead.
	MaxTuplesDecompressedPerDML int
}

// BulkBackfillPath names which writer actually landed the rows.
type BulkBackfillPath string

const (
	// BulkBackfillPathCopy means the range was proven empty and rows landed through parallel COPY.
	BulkBackfillPathCopy BulkBackfillPath = "copy"
	// BulkBackfillPathUpsert means the precondition failed and rows went through the ordinary
	// generation-guarded [Store.BatchInsertTrades] upsert.
	BulkBackfillPathUpsert BulkBackfillPath = "upsert-fallback"
)

// BulkBackfillResult reports what [Store.BulkBackfillTrades] did.
type BulkBackfillResult struct {
	// Path is the writer that ran.
	Path BulkBackfillPath
	// Attempted is the number of storable, deduped rows presented to Postgres, not len(trades):
	// unstorable rows (the SDEX one-side-zero fill) and intra-batch PK duplicates drop first,
	// as in [Store.BatchInsertTrades].
	Attempted int
	// Copied is the number of rows the COPY streams reported landing; zero on the fallback path.
	Copied int64
	// FallbackReason is empty on the COPY path, else why the precondition was refused, so an
	// operator can see a run reverted to the slow path.
	FallbackReason string
}

// BulkBackfillTrades writes a large buffer of historical trades, using COPY only when the target range is provably empty per source (otherwise it falls back to the upsert so no PK collision is skipped).
func (s *Store) BulkBackfillTrades(ctx context.Context, trades []canonical.Trade, opts BulkBackfillOptions) (BulkBackfillResult, error) {
	if len(trades) == 0 {
		return BulkBackfillResult{Path: BulkBackfillPathCopy}, nil
	}

	// Probe before any per-row work: falling back after resolving usd_volume would double-count
	// obs.TradeInsertsTotal against the upsert path's own resolution pass.
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
	// Copy before sorting: filterStorableTrades returns the caller's slice unchanged when all rows
	// are valid, and the caller's buffer must not be reordered.
	rows := make([]canonical.Trade, len(storable))
	copy(rows, storable)
	sortTradesByConflictKey(rows)
	rows = dedupeSortedTradesByConflictKey(rows)

	usd, err := s.resolveBulkUSDVolumes(ctx, rows, opts.Resolvers)
	if err != nil {
		return BulkBackfillResult{}, err
	}

	landed, copied, cerr := s.copyTradePartitions(ctx, rows, usd, opts)
	// Account for what committed before handling the failure, or a partial run leaves
	// source_entry_counts short (the recovery upsert sees those rows as conflicts and skips them).
	for _, p := range landed {
		if err := s.bumpBulkSourceCounts(ctx, rows[p[0]:p[1]]); err != nil {
			return BulkBackfillResult{}, err
		}
		s.recordBulkLandedEffects(ctx, rows[p[0]:p[1]])
	}
	if cerr != nil {
		if bulkCopyFallsBack(cerr) {
			// Replay the whole buffer through the idempotent upsert; it
			// converges over the partitions that already landed.
			return s.bulkFallback(ctx, trades, fmt.Sprintf("COPY refused (%v)", cerr))
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

// bulkRangeIsEmpty proves (or refuses) the COPY precondition. See [Store.BulkBackfillTrades] for
// why the (source, ledger, ts) box is a sufficient superset of the PK space the buffer occupies.
//
// Every bind parameter is explicitly cast: an untyped parameter beside a timestamptz comparison
// fails with 42883, which in a precondition check would silently disable the fast path.
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

// resolveBulkUSDVolumes computes `usd_volume` for every row through the SAME [tradeUSDVolume]
// waterfall the row paths use, fanned across a bounded worker pool.
//
// This is where the backfill's wall clock goes: on a historical range the FX resolver misses its
// cache on most rows. The result is a pure function of the row and of DB state a backfill does
// not modify, so concurrency changes latency only. A [Store.reDeriveNullVolumeGuard] failure is
// returned for the lowest row index, so the error does not depend on which goroutine lost a race.
func (s *Store) resolveBulkUSDVolumes(ctx context.Context, rows []canonical.Trade, workers int) ([]sql.NullString, error) {
	if workers <= 0 {
		workers = bulkDefaultResolvers
	}
	workers = min(workers, len(rows))
	out := make([]sql.NullString, len(rows))
	errAt := make([]error, len(rows))

	// Merely swallowing a panic is worse than dying: rows the dead worker never reached keep a
	// NULL usd_volume with errAt[i] nil, so the backfill would report success on an under-valued
	// batch. Recovery and failure record land together: a panicked resolve fails the call.
	var panicked panicRecord
	var wg sync.WaitGroup
	next := make(chan int, workers)
	go feedRowIndexes(next, len(rows), &panicked)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				worker.Report(slog.Default(), bulkResolverWorkerName, rec)
				panicked.record("usd-volume resolver", rec)
				// A dead worker stops receiving, which would strand the feeder on a send nobody
				// takes and leak it. Drain on the way out so it reaches close(next).
				drainIndexes(next)
			}()
			for i := range next {
				s.resolveRowUSDVolume(ctx, rows, out, errAt, i)
			}
		}()
	}
	wg.Wait()
	// Checked before errAt: a panic leaves an unknown subset of rows unresolved, so no per-row
	// verdict can be trusted to be complete.
	if err := panicked.err(); err != nil {
		return nil, err
	}
	if err := firstBulkRowErr(errAt); err != nil {
		return nil, err
	}
	return out, nil
}

// Stable worker names for stellarindex_worker_panics_total; the alert reads this label.
const (
	bulkResolverWorkerName = "timescale-bulk-backfill-usd-volume-resolver"
	bulkFeederWorkerName   = "timescale-bulk-backfill-usd-volume-feeder"
	bulkCopyWorkerName     = "timescale-bulk-backfill-copy-partition"
)

// resolveRowUSDVolume computes row i's `usd_volume` into out[i], or records the re-derive guard's
// refusal in errAt[i].
func (s *Store) resolveRowUSDVolume(
	ctx context.Context, rows []canonical.Trade, out []sql.NullString, errAt []error, i int,
) {
	v, err := s.resolveUSDVolume(ctx, rows[i])
	if err != nil {
		errAt[i] = err
		return
	}
	obs.TradeInsertsTotal.WithLabelValues(rows[i].Source, s.usdLabel(rows[i], v)).Inc()
	if v != nil {
		out[i] = sql.NullString{String: *v, Valid: true}
	}
}

// feedRowIndexes publishes 0..n-1 to ch and closes it.
func feedRowIndexes(ch chan<- int, n int, panicked *panicRecord) {
	// close(ch) stays the outermost defer: containing a panic without it would leave every worker
	// blocked on a channel that never closes, and wg.Wait would never return.
	defer close(ch)
	defer func() {
		if rec := recover(); rec != nil {
			worker.Report(slog.Default(), bulkFeederWorkerName, rec)
			panicked.record("usd-volume feeder", rec)
		}
	}()
	for i := range n {
		ch <- i
	}
}

// firstBulkRowErr returns the lowest-index per-row failure, independent of goroutine races.
func firstBulkRowErr(errAt []error) error {
	for i := range errAt {
		if errAt[i] != nil {
			return errAt[i]
		}
	}
	return nil
}

// panicRecord turns a contained fan-out panic into a returned error, so a fan-out that died
// part-way is never mistaken for a completed one. The first panic wins, independent of races.
type panicRecord struct {
	mu    sync.Mutex
	first error
}

// record takes an already-recovered panic value; the caller does the recover() and the
// [worker.Report]. recover() only fires in a function the goroutine deferred, so a helper called
// from the deferred closure is one frame too deep and would contain nothing.
func (p *panicRecord) record(where string, rec any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.first == nil {
		p.first = fmt.Errorf("timescale: BulkBackfillTrades: %s panicked: %v", where, rec)
	}
}

// drainIndexes consumes the rest of ch so a blocked producer can finish and close it.
func drainIndexes(ch <-chan int) {
	for range ch {
	}
}

func (p *panicRecord) err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.first
}

// bulkPartitions splits n rows into at most `writers` contiguous ranges of at least
// [bulkMinPartition]. Contiguity makes it safe: the input is conflict-key sorted and deduped, so
// two partitions never present the same trade PK.
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

// copyTradePartitions streams the resolved rows into `trades` over parallel binary COPY
// connections. It returns the ranges that committed even when another partition failed: each is
// its own transaction, and the caller needs them to keep `source_entry_counts` exact.
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
			// A merely contained panic would leave errs[i] nil, so the partition would count as
			// landed and source_entry_counts would credit rows never copied.
			defer func() {
				if rec := recover(); rec != nil {
					worker.Report(slog.Default(), bulkCopyWorkerName, rec)
					errs[i] = fmt.Errorf(
						"timescale: BulkBackfillTrades: copy partition %d panicked: %v", i, rec)
				}
			}()
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

// copyTradeRange runs one partition's COPY in a pgx-native transaction on one pooled connection
// (database/sql does not expose COPY, as in [Store.copyMerge]).
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
			// A silent driver swap must fail loudly, not skip the bulk load.
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
		// SET LOCAL so the cap cannot leak onto a pooled connection serving live ingest.
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
// Conversions mirror the row paths: `maker`/`taker` go through [nullStrXfer] so an absent
// counterparty lands as SQL NULL, not empty text; amounts are decimal strings so NUMERIC keeps
// full precision.
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

// bumpBulkSourceCounts applies the `source_entry_counts` tally once per buffer: every COPY'd row
// is a genuine insert (range proven empty, buffer PK-deduped), so no RETURNING is needed.
// ORDER BY source keeps row-lock order deterministic so two writers cannot form an AB/BA cycle.
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

// recordBulkLandedEffects runs the batch path's landed-row side effects (outcome metric,
// last-insert stamp, unit-ratio sentinel, classic-asset registry hook) for the whole set.
// The registry hook is soft-fail: a registry write must not sink an already-committed load.
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
			noteRegistryObservation(seenAssets, key, newRegistryObservation(t.Ledger, t.Timestamp))
			assetOf[key] = side
		}
	}
	now := float64(time.Now().Unix())
	for source, landed := range perSource {
		obs.TradeInsertOutcomeTotal.WithLabelValues(source, "new").Add(float64(landed))
		obs.SourceLastInsertUnix.WithLabelValues(source).Set(now)
	}
	for key, obsv := range seenAssets {
		if err := s.registerClassicAssetRange(ctx, assetOf[key], obsv); err != nil {
			slog.Default().Debug("timescale: bulk classic-asset registry upsert failed (soft-skip)",
				"asset", key, "ledger", obsv.maxLedger, "err", err)
		}
	}
}

// bulkCopyFallsBack reports whether the upsert fallback recovers a COPY error: 23505 (range
// stopped being empty) or 53400 (PK check hit the compressed-DML decompression cap).
func bulkCopyFallsBack(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "23505" || pgErr.Code == "53400")
}
