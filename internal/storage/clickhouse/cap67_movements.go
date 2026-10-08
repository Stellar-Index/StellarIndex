package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// ProvenanceCAP67Derived stamps account_movements rows derived from CAP-67 transfer events
// (ch-cap67-movements), including the unwatched native XLM SAC; lets the movements handler
// floor its Postgres tail at this feed's watermark.
const ProvenanceCAP67Derived = "cap67_derived"

// Cap67MovementsWatermark returns the highest ledger the cap67 derive completed through (0 =
// never run). Read from the tiny watermark row, not max(ledger) over the movements table.
func Cap67MovementsWatermark(ctx context.Context, addr string) (uint32, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	return cap67WatermarkOn(ctx, conn)
}

func cap67WatermarkOn(ctx context.Context, conn driver.Conn) (uint32, error) {
	// max() collapses un-merged RMT duplicates; the watermark only advances.
	const q = `SELECT max(thru_ledger) FROM stellar.cap67_movements_watermark WHERE name = 'cap67_movements'`
	var wm uint32
	if err := conn.QueryRow(ctx, q).Scan(&wm); err != nil {
		if isSchemaAbsent(err) {
			return 0, nil // table not provisioned — feed not enabled
		}
		return 0, fmt.Errorf("clickhouse: cap67 watermark: %w", err)
	}
	return wm, nil
}

// cap67WMTTL is how long a successful watermark read is served from cache.
const cap67WMTTL = time.Minute

// cap67WMRetryAfter is how long a failed read is answered from cache, matching
// [schemaProbeRetryAfter]: an outage costs one query per window, not one per request.
const cap67WMRetryAfter = schemaProbeRetryAfter

// Cap67MovementsWatermark (method form) serves the handler's floor read through a ~60s cache;
// staleness only makes the Postgres tail serve slightly more, never a gap (the handler also
// ceilings the CH arm at the same cached value). The round-trip runs outside the mutex as a
// single flight so a slow store cannot queue requests behind a context-blind lock.
func (r *ExplorerReader) Cap67MovementsWatermark(ctx context.Context) (uint32, error) {
	cov, err := r.cap67Coverage(ctx)
	return cov.Thru, err
}

// Cap67SupplyCoverage is the ledger range the archive also derived mint,
// burn and clawback over (ok=false: none), from the same cached read.
func (r *ExplorerReader) Cap67SupplyCoverage(ctx context.Context) (from, thru uint32, ok bool, err error) {
	cov, err := r.cap67Coverage(ctx)
	if err != nil {
		return 0, 0, false, err
	}
	from, thru, ok = cov.SupplyRange()
	return from, thru, ok, nil
}

func (r *ExplorerReader) cap67Coverage(ctx context.Context) (Cap67Coverage, error) {
	for {
		flight, owner, hit, cov, err := r.cap67WMClaim(time.Now())
		switch {
		case hit:
			return cov, err
		case owner:
			return r.refreshCap67WM(ctx, flight)
		}
		select {
		case <-flight:
		case <-ctx.Done():
			return Cap67Coverage{}, fmt.Errorf("clickhouse: cap67 watermark: %w", ctx.Err())
		}
	}
}

// cap67WMClaim answers from cache (fresh value or failure inside back-off) or hands back the
// flight to wait on, creating it when the caller is the owner.
func (r *ExplorerReader) cap67WMClaim(now time.Time) (flight chan struct{}, owner, hit bool, cov Cap67Coverage, err error) {
	r.cap67WMMu.Lock()
	defer r.cap67WMMu.Unlock()
	if !r.cap67WMAt.IsZero() && now.Sub(r.cap67WMAt) < cap67WMTTL {
		return nil, false, true, r.cap67Cov, nil
	}
	if r.cap67WMErr != nil && now.Sub(r.cap67WMErrAt) < cap67WMRetryAfter {
		return nil, false, true, Cap67Coverage{}, r.cap67WMErr
	}
	if r.cap67WMFlight != nil {
		return r.cap67WMFlight, false, false, Cap67Coverage{}, nil
	}
	r.cap67WMFlight = make(chan struct{})
	return r.cap67WMFlight, true, false, Cap67Coverage{}, nil
}

// refreshCap67WM runs the owned flight and publishes its outcome; released even on panic so
// a bad read cannot wedge later callers.
func (r *ExplorerReader) refreshCap67WM(ctx context.Context, flight chan struct{}) (cov Cap67Coverage, err error) {
	completed := false
	defer func() {
		r.cap67WMMu.Lock()
		now := time.Now()
		switch {
		case !completed:
		case err == nil:
			r.cap67Cov, r.cap67WMAt, r.cap67WMErr = cov, now, nil
		case !errors.Is(ctx.Err(), context.Canceled):
			// A caller's own disconnect says nothing about the store; a deadline or backend
			// error does, and backs everyone off.
			r.cap67WMErr, r.cap67WMErrAt = err, now
		}
		r.cap67WMFlight = nil
		r.cap67WMMu.Unlock()
		close(flight)
	}()
	cov, err = cap67CoverageOn(ctx, r.conn)
	completed = true
	return cov, err
}

// ErrCap67MovementsHole reports a refused watermark advance: the lake lacks a ledger of the
// window. Delay, not failure: ch-live-catchup heals it and the next run re-derives.
var ErrCap67MovementsHole = errors.New("clickhouse: cap67 movements window is not contiguous in the lake")

// ErrCap67MovementsSkippedPrefix reports a refused advance whose window starts above
// watermark+1; stamping its top would claim never-derived ledgers.
var ErrCap67MovementsSkippedPrefix = errors.New("clickhouse: cap67 movements window skips ledgers below it")

// ErrCap67MovementsEventShortfall reports a refused advance: the ledgers are present but
// stellar.contract_events holds fewer rows than they declare. Delay, not failure.
var ErrCap67MovementsEventShortfall = errors.New("clickhouse: cap67 movements window is missing contract events")

// SetCap67MovementsWatermark records completion through `thru` for window [from, thru], only
// if the advance is proven (see cap67AdvanceProven). Proof is required at the write: the
// derive resumes at watermark+1, so an advance over a missing ledger would drop its
// movements permanently and invisibly, and the range may be operator-supplied
// (`-to N`). Fail-closed. Contiguity is keyed off stellar.ledgers, the commit marker
// Sink.Flush writes last (present there implies contract_events are durable).
func SetCap67MovementsWatermark(ctx context.Context, addr string, from, thru uint32) error {
	if from == 0 || thru < from {
		return fmt.Errorf("clickhouse: cap67 watermark window [%d,%d] is not a ledger range (genesis is ledger 1)", from, thru)
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	advance, err := cap67AdvanceProven(ctx, conn, from, thru)
	if err != nil {
		return err
	}
	if !advance {
		return nil // a re-derive at or below the watermark claims nothing new
	}
	const q = `INSERT INTO stellar.cap67_movements_watermark (name, thru_ledger) VALUES ('cap67_movements', ?)`
	if err := conn.Exec(ctx, q, thru); err != nil {
		return fmt.Errorf("clickhouse: set cap67 watermark %d: %w", thru, err)
	}
	return nil
}

// cap67AdvanceProven reports whether recording [from, thru] keeps the watermark the top of a
// hole-free, fully-derived prefix, erroring (never skipping) when not. Unproven when:
//   - `from` is above watermark+1 (a typo or operator jump would stamp undone ledgers);
//     a first run (watermark 0) has no prefix, -floor-ledger is where coverage starts;
//   - the lake has a hole in the window (cap67WindowContiguous);
//   - the events are missing (cap67WindowEventsPresent).
//
// A window at or below the watermark is an idempotent re-derive (ReplacingMergeTree):
// neither refused nor written.
func cap67AdvanceProven(ctx context.Context, conn driver.Conn, from, thru uint32) (bool, error) {
	wm, err := cap67WatermarkOn(ctx, conn)
	if err != nil {
		return false, err
	}
	if thru <= wm {
		return false, nil
	}
	if wm > 0 && from > wm+1 {
		return false, fmt.Errorf("%w: window [%d,%d] starts above watermark+1 (%d), so [%d,%d] was never derived — the watermark stays where it is",
			ErrCap67MovementsSkippedPrefix, from, thru, wm+1, wm+1, from-1)
	}
	if err := cap67WindowContiguous(ctx, conn, from, thru); err != nil {
		return false, err
	}
	if err := cap67WindowEventsPresent(ctx, conn, from, thru); err != nil {
		return false, err
	}
	return true, nil
}

// cap67WindowContiguous errors (wrapping ErrCap67MovementsHole) unless stellar.ledgers holds
// every ledger in [from, thru]. DISTINCT because un-merged RMT duplicates must not mask a
// hole. A primary-key range over one derive window, so it reads at most `-window` pruned rows.
func cap67WindowContiguous(ctx context.Context, conn driver.Conn, from, thru uint32) error {
	present, err := windowLedgersPresent(ctx, conn, from, thru)
	if err != nil {
		return fmt.Errorf("clickhouse: cap67 window [%d,%d] contiguity: %w", from, thru, err)
	}
	if want := uint64(thru-from) + 1; present != want {
		return fmt.Errorf("%w: [%d,%d] holds %d of %d ledgers — the watermark stays where it is",
			ErrCap67MovementsHole, from, thru, present, want)
	}
	return nil
}

// windowLedgersPresent counts the distinct ledgers stellar.ledgers holds in
// [from, thru] — the derive watermarks' contiguity proof.
func windowLedgersPresent(ctx context.Context, conn driver.Conn, from, thru uint32) (uint64, error) {
	const q = `SELECT toUInt64(count(DISTINCT ledger_seq)) FROM stellar.ledgers WHERE ledger_seq >= ? AND ledger_seq <= ?`
	var present uint64
	err := conn.QueryRow(ctx, q, from, thru).Scan(&present)
	return present, err
}

// cap67WindowEventsPresent errors (wrapping ErrCap67MovementsEventShortfall) when
// contract_events holds fewer rows in [from, thru] than ledgers declares via
// soroban_event_count. Contiguity alone misses a dropped events partition. Unmerged RMT
// duplicates only raise present, so they never cause a false refusal.
func cap67WindowEventsPresent(ctx context.Context, conn driver.Conn, from, thru uint32) error {
	var expected, present uint64
	if err := conn.QueryRow(ctx, cap67WindowEventCensusQuery, from, thru, from, thru).Scan(&expected, &present); err != nil {
		return fmt.Errorf("clickhouse: cap67 window [%d,%d] event census: %w", from, thru, err)
	}
	if present < expected {
		return fmt.Errorf("%w: [%d,%d] holds %d of %d contract events — the watermark stays where it is",
			ErrCap67MovementsEventShortfall, from, thru, present, expected)
	}
	return nil
}

// cap67WindowEventCensusQuery dedupes soroban_event_count per ledger as
// eventCensusExpected does. Binds: from, thru, from, thru.
const cap67WindowEventCensusQuery = `
	SELECT
		(SELECT toUInt64(sum(cnt)) FROM (
			SELECT ledger_seq, argMax(soroban_event_count, ingested_at) AS cnt
			FROM stellar.ledgers WHERE ledger_seq BETWEEN ? AND ?
			GROUP BY ledger_seq
		)),
		(SELECT toUInt64(count()) FROM stellar.contract_events WHERE ledger_seq BETWEEN ? AND ?)`

// Supply-kind coverage rows in stellar.cap67_movements_watermark: mint/burn/clawback joined
// later, so their derived ledgers form their own range. from only moves down (min) and thru
// only up (max), so unmerged RMT rows never misreport it.
const (
	cap67SupplyFromName = "cap67_movements_supply_from"
	cap67SupplyThruName = "cap67_movements_supply_thru"
)

// Cap67Coverage is the derive's progress: Thru is the main watermark; [SupplyFrom, SupplyThru]
// the range also derived with mint, burn and clawback (SupplyFrom 0 = none yet).
type Cap67Coverage struct {
	Thru       uint32
	SupplyFrom uint32
	SupplyThru uint32
}

// SupplyRange is the supply-kind range clamped to the main watermark; ok is
// false when it is empty.
func (c Cap67Coverage) SupplyRange() (from, thru uint32, ok bool) {
	thru = min(c.SupplyThru, c.Thru)
	if c.SupplyFrom == 0 || thru < c.SupplyFrom {
		return 0, 0, false
	}
	return c.SupplyFrom, thru, true
}

const cap67CoverageQuery = `SELECT
	maxIf(thru_ledger, name = 'cap67_movements'),
	minIf(thru_ledger, name = '` + cap67SupplyFromName + `'),
	maxIf(thru_ledger, name = '` + cap67SupplyThruName + `')
FROM stellar.cap67_movements_watermark`

func cap67CoverageOn(ctx context.Context, conn driver.Conn) (Cap67Coverage, error) {
	var c Cap67Coverage
	if err := conn.QueryRow(ctx, cap67CoverageQuery).Scan(&c.Thru, &c.SupplyFrom, &c.SupplyThru); err != nil {
		if isSchemaAbsent(err) {
			return Cap67Coverage{}, nil // table not provisioned — feed not enabled
		}
		return Cap67Coverage{}, fmt.Errorf("clickhouse: cap67 coverage: %w", err)
	}
	return c, nil
}

// Cap67MovementsCoverage reads the derive's progress uncached (the derive
// job's view; the API reads it through ExplorerReader's cache).
func Cap67MovementsCoverage(ctx context.Context, addr string) (Cap67Coverage, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return Cap67Coverage{}, err
	}
	defer func() { _ = conn.Close() }()
	return cap67CoverageOn(ctx, conn)
}

// ExtendCap67SupplyCoverage records [lo, hi] as derived with the supply kinds. The range
// grows only by an overlapping or abutting window (never claiming transfer-only stretches),
// and only once the ledgers are proven present with their events. A first window writes from
// before thru, so a crash leaves an empty range, never an over-claim.
func ExtendCap67SupplyCoverage(ctx context.Context, addr string, lo, hi uint32) error {
	if lo == 0 || hi < lo {
		return fmt.Errorf("clickhouse: cap67 supply window [%d,%d] is not a ledger range (genesis is ledger 1)", lo, hi)
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	return extendCap67SupplyCoverageOn(ctx, conn, lo, hi)
}

func extendCap67SupplyCoverageOn(ctx context.Context, conn driver.Conn, lo, hi uint32) error {
	cov, err := cap67CoverageOn(ctx, conn)
	if err != nil {
		return err
	}
	from := cov.SupplyFrom
	if from == 0 {
		if err := cap67WindowProven(ctx, conn, lo, hi); err != nil {
			return err
		}
		if err := setCap67SupplyBound(ctx, conn, cap67SupplyFromName, lo); err != nil {
			return err
		}
		return setCap67SupplyBound(ctx, conn, cap67SupplyThruName, hi)
	}
	thru := max(cov.SupplyThru, from-1)
	if lo > thru+1 || hi+1 < from {
		return nil
	}
	if lo < from {
		if err := cap67WindowProven(ctx, conn, lo, from-1); err != nil {
			return err
		}
		if err := setCap67SupplyBound(ctx, conn, cap67SupplyFromName, lo); err != nil {
			return err
		}
	}
	if hi > thru {
		if err := cap67WindowProven(ctx, conn, thru+1, hi); err != nil {
			return err
		}
		return setCap67SupplyBound(ctx, conn, cap67SupplyThruName, hi)
	}
	return nil
}

// cap67WindowProven errors unless [lo, hi] holds every ledger and every
// event those ledgers declare.
func cap67WindowProven(ctx context.Context, conn driver.Conn, lo, hi uint32) error {
	if err := cap67WindowContiguous(ctx, conn, lo, hi); err != nil {
		return err
	}
	return cap67WindowEventsPresent(ctx, conn, lo, hi)
}

func setCap67SupplyBound(ctx context.Context, conn driver.Conn, name string, ledger uint32) error {
	const q = `INSERT INTO stellar.cap67_movements_watermark (name, thru_ledger) VALUES (?, ?)`
	if err := conn.Exec(ctx, q, name, ledger); err != nil {
		return fmt.Errorf("clickhouse: set %s %d: %w", name, ledger, err)
	}
	return nil
}
