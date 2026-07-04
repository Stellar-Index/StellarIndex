package v1

import (
	"context"
	"time"
)

// LakeWatermarkReader reports how far the ClickHouse lake's certified capture
// has advanced: the highest captured ledger and that ledger's close time
// (ADR-0041 Decision 4 — every API response derived from the lake's
// current-state / supply projections carries its watermark so the client
// knows HOW FRESH the lake is). Production wiring is
// *clickhouse.ExplorerReader (same lake reader as Explorer /
// ProtocolActivity). Nil → responses omit `as_of_ledger` and never flip
// `flags.stale` from the watermark.
type LakeWatermarkReader interface {
	LakeWatermark(ctx context.Context) (ledger uint32, closedAt time.Time, err error)
}

// lakeStaleThreshold (ADR-0041 Decision 4): a lake-backed response is flagged
// `flags.stale` when the lake watermark's close time trails now by more than
// this. 300s ≈ 60 ledgers at the ~5s close cadence — two orders of magnitude
// beyond the normal galexie→ClickHouse sink lag (single-digit seconds), and
// wide enough not to flap across a routine indexer restart or a slow archive
// partition flush, while still catching a wedged sink long before the
// current-state / supply projections are meaningfully wrong. Deliberately
// aligned with the ops-side data-freshness watchdog's order of magnitude
// rather than the per-price freshness SLA (which is tighter and served-tier).
const lakeStaleThreshold = 300 * time.Second

// lakeWatermarkTTL bounds how often the watermark query actually runs.
// 15s ≈ 3 ledgers of drift at worst — invisible next to the 300s staleness
// threshold — and turns per-request ClickHouse round-trips into at most four
// a minute across ALL lake-backed endpoints (the protocols pillar uses the
// same "cache the tip, don't re-read per request" posture).
const lakeWatermarkTTL = 15 * time.Second

// lakeWatermark returns the cached lake watermark: the ledger to stamp as
// `as_of_ledger` and whether the response must carry `flags.stale` per
// lakeStaleThreshold. ok=false when no watermark reader is wired, the first
// read hasn't succeeded yet, or the lake is empty — callers then omit
// `as_of_ledger` and leave flags untouched (graceful, matching every other
// nil-reader degrade in this package).
//
// The mutex is held across the refresh query; that serialises concurrent
// cold refreshes onto one cheap `max(ledger_seq)` read (small ledgers table,
// ~ms) instead of adding a single-flight layer.
func (s *Server) lakeWatermark(ctx context.Context) (ledger uint32, stale bool, ok bool) {
	if s.lakeWatermarkReader == nil {
		return 0, false, false
	}
	s.lakeWMMu.Lock()
	defer s.lakeWMMu.Unlock()
	if s.lakeWMFetched.IsZero() || time.Since(s.lakeWMFetched) > lakeWatermarkTTL {
		l, closedAt, err := s.lakeWatermarkReader.LakeWatermark(ctx)
		switch {
		case err == nil:
			s.lakeWMLedger, s.lakeWMClosedAt, s.lakeWMFetched = l, closedAt, time.Now()
		case s.lakeWMFetched.IsZero():
			// Never had a value — degrade to "no watermark" rather than
			// failing the caller's request.
			if s.logger != nil {
				s.logger.Warn("lake watermark read failed", "err", err)
			}
			return 0, false, false
		default:
			// Serve the previous value; a stale cached watermark still
			// yields correct `stale` semantics (closedAt only gets older).
			if s.logger != nil {
				s.logger.Warn("lake watermark refresh failed; serving previous", "err", err)
			}
		}
	}
	if s.lakeWMLedger == 0 {
		return 0, false, false
	}
	return s.lakeWMLedger, time.Since(s.lakeWMClosedAt) > lakeStaleThreshold, true
}
