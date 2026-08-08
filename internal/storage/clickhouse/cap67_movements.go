package clickhouse

import (
	"context"
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

// SetCap67MovementsWatermark records completion through `thru`.
func SetCap67MovementsWatermark(ctx context.Context, addr string, thru uint32) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	const q = `INSERT INTO stellar.cap67_movements_watermark (name, thru_ledger) VALUES ('cap67_movements', ?)`
	if err := conn.Exec(ctx, q, thru); err != nil {
		return fmt.Errorf("clickhouse: set cap67 watermark %d: %w", thru, err)
	}
	return nil
}
