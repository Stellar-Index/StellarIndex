package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// ProvenanceCAP67Derived stamps account_movements rows derived from the
// lake's CAP-67 transfer events by `stellarindex-ops ch-cap67-movements`
// (inventory #1) — the post-P23 continuation of the classic_derived
// archive, covering EVERY asset including the deliberately-unwatched
// native XLM SAC. The provenance split is what lets the movements
// handler floor its Postgres tail at this feed's watermark.
const ProvenanceCAP67Derived = "cap67_derived"

// Cap67MovementsWatermark returns the highest ledger the cap67
// movement derive has completed THROUGH (0 = never run). Read from the
// tiny stellar.cap67_movements_watermark row rather than
// max(ledger) over the 6.7B-row movements table — the watermark is
// consulted per catch-up run AND (cached) per /movements request.
func Cap67MovementsWatermark(ctx context.Context, addr string) (uint32, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	return cap67WatermarkOn(ctx, conn)
}

func cap67WatermarkOn(ctx context.Context, conn driver.Conn) (uint32, error) {
	// max() collapses un-merged RMT duplicate rows; the watermark only
	// ever advances, so max IS the latest.
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

// cap67WMRetryAfter is how long a failed watermark read is answered from
// cache (as the same error) before the store is asked again, matching
// [schemaProbeRetryAfter]: an outage costs one query per window, not one
// per /movements request.
const cap67WMRetryAfter = schemaProbeRetryAfter

// Cap67MovementsWatermark (method form) serves the explorer handler's
// per-request floor read through a ~60s cache — the watermark advances
// every derive window (~minutes), so staleness within the cache TTL
// only makes the Postgres tail serve slightly more than strictly
// necessary, never a gap (the handler ALSO ceilings the CH arm at the
// same cached value, so the two arms stay consistent with each other).
//
// The round-trip runs OUTSIDE the mutex as a single flight: concurrent
// callers wait on the flight or their own ctx, whichever ends first, so
// a slow store cannot queue every request behind a context-blind lock.
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

// cap67WMClaim answers from cache (hit) — a fresh value, or a recent
// failure still inside its back-off — or else hands back the flight to
// wait on, creating it when this caller is the owner.
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

// refreshCap67WM runs the owned flight and publishes its outcome. The
// flight is released even on panic, so one bad read cannot wedge every
// later caller on a channel that never closes.
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
			// A caller's own disconnect says nothing about the store; a
			// deadline or backend error does, and backs everyone off.
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

// ErrCap67MovementsHole reports a REFUSED watermark advance: the lake does
// not hold every ledger of the window the derive asked to record, so
// advancing would step past a hole. Callers treat it as delay, not failure —
// the hole heals via ch-live-catchup and the next run re-derives the window.
var ErrCap67MovementsHole = errors.New("clickhouse: cap67 movements window is not contiguous in the lake")

// ErrCap67MovementsSkippedPrefix reports a REFUSED watermark advance whose
// window starts ABOVE watermark+1: the ledgers in between were never derived
// by this run, so stamping the window's top would claim them as done.
var ErrCap67MovementsSkippedPrefix = errors.New("clickhouse: cap67 movements window skips ledgers below it")

// ErrCap67MovementsEventShortfall reports a REFUSED watermark advance: the
// window's ledgers are all present but stellar.contract_events holds fewer
// rows than they declare, so the derive read an incomplete event set. Like a
// hole it is delay, not failure — the next run re-derives once events return.
var ErrCap67MovementsEventShortfall = errors.New("clickhouse: cap67 movements window is missing contract events")

// SetCap67MovementsWatermark records completion through `thru` for the
// derive window [from, thru] — and ONLY if that advance is proven: the lake
// holds every ledger in the window and every event those ledgers declare, AND
// the window continues the derived prefix rather than jumping over part of it
// (see cap67AdvanceProven for the refusals and for the re-derive case, which
// records nothing).
//
// WHY the proof is required at the WRITE: the watermark is read back as
// max(thru_ledger) and the derive resumes at watermark+1 with no trailing
// re-derive, so an advance over a ledger the lake was missing drops that
// ledger's classic/native account movements PERMANENTLY and invisibly (the
// raw lake self-heals via ch-live-catchup; account_movements never revisits
// it). Cap67Range clamps the range it hands out to the contiguous tip, but a
// range is resolved once per run and its upper bound can be operator-supplied
// (`ch-cap67-movements -to N`); the advance itself is the one place the
// invariant "the watermark is the top of a hole-free prefix" holds for EVERY
// caller. Fail-closed: a window that cannot be proven does not advance.
//
// Contiguity is keyed off stellar.ledgers, the per-ledger commit marker
// Sink.Flush writes LAST (present in ledgers ⟹ that ledger's contract_events
// are durable) — the same substrate ContiguousWatermark reads.
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

// cap67AdvanceProven reports whether recording [from, thru] keeps the
// watermark the top of a hole-free, fully-derived prefix — erroring (never
// silently skipping) when it would not. Two ways an advance is unproven:
//
//   - the window skips ledgers below it. `from` above watermark+1 means this
//     run never derived (watermark, from): a `-from` typo, or an operator
//     jumping the derive forward past a stretch it meant to backfill later,
//     would otherwise stamp those ledgers done. A FIRST run (watermark 0) has
//     no prefix to keep — the -floor-ledger boundary is where coverage starts.
//   - the lake has a hole inside the window (see cap67WindowContiguous).
//   - the window's ledgers are present but their events are not (see
//     cap67WindowEventsPresent).
//
// A window entirely at or below the watermark is a legitimate idempotent
// re-derive (account_movements is a ReplacingMergeTree): it claims nothing
// new, so it is neither refused nor written.
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

// cap67WindowContiguous errors (wrapping ErrCap67MovementsHole) unless
// stellar.ledgers holds every ledger in [from, thru]. DISTINCT because
// stellar.ledgers is a ReplacingMergeTree: un-merged duplicates must not
// count as separate ledgers and mask a hole. The scan is a primary-key range
// (ORDER BY ledger_seq, PARTITION BY intDiv(ledger_seq, 1000000)) over one
// derive window, so it reads at most `-window` pruned rows — orders of
// magnitude cheaper than the tip-resolving window function, and paid once per
// window rather than per row.
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

// cap67WindowEventsPresent errors (wrapping ErrCap67MovementsEventShortfall)
// when stellar.contract_events holds fewer rows in [from, thru] than
// stellar.ledgers declares via soroban_event_count — the window-scoped form of
// EventCensusShortfalls. Ledger contiguity alone does not prove the events: a
// dropped or unrestored contract_events partition leaves ledgers intact, and
// the derive would read it as a stretch with no movements. Both sides are
// primary-key ranges over one window; unmerged RMT duplicates can only raise
// present, so they never cause a false refusal.
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

// Supply-kind coverage rows in stellar.cap67_movements_watermark. Mint, burn
// and clawback joined the derive after deployments had advanced the main
// watermark over transfer-only windows, so the ledgers derived with them form
// their own range [from, thru]: from only moves down (read with min) and thru
// only up (read with max), so unmerged RMT rows never misreport it.
const (
	cap67SupplyFromName = "cap67_movements_supply_from"
	cap67SupplyThruName = "cap67_movements_supply_thru"
)

// Cap67Coverage is the derive's progress in one read: Thru is the main
// watermark; [SupplyFrom, SupplyThru] the contiguous range also derived with
// mint, burn and clawback (SupplyFrom 0 = none yet).
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

// ExtendCap67SupplyCoverage records that [lo, hi] was derived with the supply
// kinds. The range grows only by a window that overlaps or abuts it, so a
// stretch derived transfer-only (by an older binary) is never claimed, and
// only once the newly claimed ledgers are proven present with all their
// events, as for the main watermark. A first window writes from before thru:
// a crash between them leaves an empty range at from, which never over-claims.
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
