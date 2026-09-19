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

// Cap67MovementsWatermark (method form) serves the explorer handler's
// per-request floor read through a ~60s cache — the watermark advances
// every derive window (~minutes), so staleness within the cache TTL
// only makes the Postgres tail serve slightly more than strictly
// necessary, never a gap (the handler ALSO ceilings the CH arm at the
// same cached value, so the two arms stay consistent with each other).
func (r *ExplorerReader) Cap67MovementsWatermark(ctx context.Context) (uint32, error) {
	r.cap67WMMu.Lock()
	defer r.cap67WMMu.Unlock()
	if time.Since(r.cap67WMAt) < time.Minute && r.cap67WMAt != (time.Time{}) {
		return r.cap67WM, nil
	}
	wm, err := cap67WatermarkOn(ctx, r.conn)
	if err != nil {
		return 0, err
	}
	r.cap67WM, r.cap67WMAt = wm, time.Now()
	return wm, nil
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

// SetCap67MovementsWatermark records completion through `thru` for the
// derive window [from, thru] — and ONLY if that advance is proven: the lake
// holds every ledger in the window AND the window continues the derived
// prefix rather than jumping over part of it (see cap67AdvanceProven for
// both refusals and for the re-derive case, which records nothing).
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
	const q = `SELECT toUInt64(count(DISTINCT ledger_seq)) FROM stellar.ledgers WHERE ledger_seq >= ? AND ledger_seq <= ?`
	var present uint64
	if err := conn.QueryRow(ctx, q, from, thru).Scan(&present); err != nil {
		return fmt.Errorf("clickhouse: cap67 window [%d,%d] contiguity: %w", from, thru, err)
	}
	if want := uint64(thru-from) + 1; present != want {
		return fmt.Errorf("%w: [%d,%d] holds %d of %d ledgers — the watermark stays where it is",
			ErrCap67MovementsHole, from, thru, present, want)
	}
	return nil
}
