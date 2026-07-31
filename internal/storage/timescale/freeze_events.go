package timescale

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// FreezeEventSink is the timescale-backed implementation of
// freeze.EventSink. Records every clear→firing transition into the
// `freeze_events` hypertable so the explorer /anomalies timeline
// has durable history.
//
// Idempotent on the (asset, quote) currently-firing row: if a row
// with recovered_at IS NULL already exists for the pair, RecordFreeze
// is a no-op. The pre-existing Redis-marker write that drives the
// API's flags.frozen field stays the source-of-truth for liveness;
// this struct only mirrors the transitions for offline reads.
type FreezeEventSink struct {
	db        *sql.DB
	clock     func() time.Time
	getLedger LedgerProvider
	// onFreeze, when non-nil, is invoked AFTER a successful
	// RecordFreeze that actually inserted a new row (idempotent
	// no-ops don't fire). Production wiring: the aggregator
	// binary plugs a customerwebhook.Fanout.Publish closure so
	// dashboard-registered hooks subscribed to `anomaly.freeze`
	// receive a callback. F-1249 (codex audit-2026-05-12).
	onFreeze FreezeHook
}

// FreezeHook is the callback shape for post-insert side-effects.
// Best-effort: errors are logged and dropped by the sink so a
// downstream failure (e.g. customerwebhook fan-out) doesn't take
// the load-bearing INSERT down.
type FreezeHook func(ctx context.Context, asset, quote canonical.Asset, frozenValue string, decision anomaly.Decision)

// LedgerProvider is the seam for reading the most-recently-ingested
// ledger sequence. Used to stamp `frozen_at_ledger` on inserts so
// the timeline can be re-anchored on a specific ledger range later.
//
// Implementations must be concurrent-safe and cheap (it's called on
// the aggregator's hot path). A typical implementation reads an
// atomic int that the indexer updates on every cursor advance.
type LedgerProvider interface {
	LatestLedger() uint32
}

// NewFreezeEventSink constructs the sink. clock + getLedger are
// optional; nil clock falls back to time.Now, nil getLedger means
// frozen_at_ledger is stamped 0 (acceptable for tests; production
// always wires a real provider).
func NewFreezeEventSink(s *Store, opts ...FreezeEventSinkOption) *FreezeEventSink {
	sink := &FreezeEventSink{
		db:    s.db,
		clock: time.Now,
	}
	for _, opt := range opts {
		opt(sink)
	}
	return sink
}

// FreezeEventSinkOption tunes a FreezeEventSink at construction.
type FreezeEventSinkOption func(*FreezeEventSink)

// WithFreezeClock injects a deterministic clock for tests.
func WithFreezeClock(clock func() time.Time) FreezeEventSinkOption {
	return func(s *FreezeEventSink) {
		s.clock = clock
	}
}

// WithFreezeLedgerProvider wires the ledger seam so inserts capture
// frozen_at_ledger.
func WithFreezeLedgerProvider(p LedgerProvider) FreezeEventSinkOption {
	return func(s *FreezeEventSink) {
		s.getLedger = p
	}
}

// WithFreezeHook installs a post-insert side-effect closure.
// Invoked AFTER a successful row insert (idempotent no-ops
// don't fire). F-1249 (codex audit-2026-05-12): wired by the
// aggregator binary to bridge into customerwebhook.Fanout.Publish
// so dashboard hooks subscribed to `anomaly.freeze` get
// callbacks. Best-effort — hook panics/errors don't propagate.
func WithFreezeHook(hook FreezeHook) FreezeEventSinkOption {
	return func(s *FreezeEventSink) {
		s.onFreeze = hook
	}
}

// RecordFreeze implements freeze.EventSink.
//
// Idempotent: if a row already exists for (asset, quote) with
// recovered_at IS NULL, this is a no-op (the pair is already
// recorded as currently-firing; another Mark call is just a TTL
// refresh from the orchestrator's perspective). Otherwise INSERT
// a new row.
//
// Implementation note: the idempotency check + INSERT happen in the
// same transaction so two concurrent Mark calls for the same pair
// can't both insert. The (asset_id, quote_id, frozen_at) PK has
// timestamp resolution; if two callers try to insert at the
// identical microsecond, one wins on PK-conflict and the other
// silently no-ops via ON CONFLICT DO NOTHING.
func (s *FreezeEventSink) RecordFreeze(ctx context.Context, asset, quote canonical.Asset, frozenValue string, decision anomaly.Decision) error {
	now := s.clock().UTC()
	var ledger uint32
	if s.getLedger != nil {
		ledger = s.getLedger.LatestLedger()
	}

	detail, err := encodeFreezeDetail(decision)
	if err != nil {
		return fmt.Errorf("timescale: RecordFreeze: encode detail: %w", err)
	}

	// Translate the anomaly Decision into the table's reason CHECK.
	// Phase 1 deviations + Phase 2 confidence-based decisions both
	// land here; we expose the most-specific reason we have.
	reason := mapFreezeReason(decision)

	// frozen_value column is NUMERIC NOT NULL — write 0 when the
	// orchestrator had no prior bucket to freeze on (first-tick
	// freeze). The decimal-string value is forwarded verbatim;
	// pgx/lib-pq parses NUMERIC literals from strings without
	// precision loss.
	frozenValueArg := frozenValue
	if frozenValueArg == "" {
		frozenValueArg = "0"
	}

	// F-1250 (codex audit-2026-05-12): atomic dedupe under
	// concurrent RecordFreeze calls. Two aggregator workers
	// racing on the same (asset, quote) pair used to both pass
	// the `WHERE NOT EXISTS` check and each insert a still-firing
	// row, leaving duplicate open rows for the same pair —
	// every recovery worker now had to clear N rows instead of 1.
	//
	// The fix: wrap the check + insert in a transaction guarded
	// by `pg_advisory_xact_lock` keyed on a stable hash of
	// (asset, quote). The lock is process-local to the txn so
	// it auto-releases on COMMIT/ROLLBACK and never strands the
	// row. Advisory locks (vs row locks) work here because the
	// "no row yet" branch has nothing to row-lock against;
	// Timescale also forbids unique constraints that don't
	// include the partition key, so a partial UNIQUE index
	// isn't an option.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("timescale: RecordFreeze: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // safe no-op after COMMIT

	pairKey := pairAdvisoryLockKey(asset.String(), quote.String())
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, pairKey); err != nil {
		return fmt.Errorf("timescale: RecordFreeze: advisory lock: %w", err)
	}

	const q = `
		INSERT INTO freeze_events (
		    asset_id, quote_id,
		    frozen_at, frozen_at_ledger,
		    reason, frozen_value,
		    detail
		)
		SELECT $1, $2, $3, $4, $5, $6::NUMERIC, $7
		WHERE NOT EXISTS (
		    SELECT 1 FROM freeze_events
		    WHERE asset_id = $1 AND quote_id = $2 AND recovered_at IS NULL
		)
		ON CONFLICT (asset_id, quote_id, frozen_at) DO NOTHING
	`
	res, err := tx.ExecContext(ctx, q,
		asset.String(), quote.String(),
		now, int64(ledger),
		reason,
		frozenValueArg,
		detail,
	)
	if err != nil {
		return fmt.Errorf("timescale: RecordFreeze %s/%s: %w",
			asset.String(), quote.String(), err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("timescale: RecordFreeze: commit: %w", err)
	}
	// F-1249 (codex audit-2026-05-12): fire the post-insert hook
	// only when a row was actually appended. The idempotency check
	// + ON CONFLICT DO NOTHING means RowsAffected==0 is the
	// "already firing, this is just a TTL refresh" path; firing
	// the webhook then would spam subscribers.
	if s.onFreeze != nil {
		if affected, err := res.RowsAffected(); err == nil && affected > 0 {
			s.onFreeze(ctx, asset, quote, frozenValueArg, decision)
		}
	}
	return nil
}

// SaveLadder persists the ADR-0019 lifecycle state onto the currently-firing
// `freeze_events` row for (asset, quote). Implements freeze.LadderStore.
//
// Migration 0119. The ladder — hold_until / extensions_used / escalated /
// corroborated — used to live only in the aggregator's memory and in the
// Redis marker's JSON, so a Redis flush took an ESCALATED freeze (one
// ADR-0019 holds "until manual unfreeze") down with it. This is the durable
// copy, written on every lifecycle transition by freeze.Writer.MarkHold.
//
// Deliberately an UPDATE of the OPEN row and nothing else:
//
//   - No INSERT. RecordFreeze owns row creation and is the only path that
//     may open a row; a SaveLadder that could insert would race it and open
//     a second firing row for one pair — the F-1250 class this file already
//     serialises against.
//   - No row → [ErrNotFound], NOT a silent nil. The freeze fired before
//     0119, RecordFreeze's insert failed (best-effort by contract), the
//     operator just closed the row out from under a tick — or, the case
//     that actually bites, migration 0119 has not been applied while the
//     new binary is already running, in which case EVERY ladder write
//     matches nothing and the durable ladder simply is not there when a
//     flush needs it. The caller cannot act on any of these (the write is
//     best-effort), but it must be able to COUNT them; returning nil made
//     the whole failure mode invisible.
//
// Idempotent and monotonic in practice: every write is the whole current
// state, so a lost write is corrected by the next tick's write.
func (s *FreezeEventSink) SaveLadder(ctx context.Context, asset, quote canonical.Asset, state freeze.State) error {
	var holdUntil any
	if !state.HoldUntil.IsZero() {
		holdUntil = state.HoldUntil.UTC()
	}
	const q = `
		UPDATE freeze_events
		   SET hold_until      = $3,
		       extensions_used = $4,
		       escalated       = $5,
		       corroborated    = $6
		 WHERE asset_id = $1 AND quote_id = $2 AND recovered_at IS NULL
	`
	res, err := s.db.ExecContext(ctx, q,
		asset.String(), quote.String(),
		holdUntil, state.ExtensionsUsed, state.Escalated, state.Corroborated,
	)
	if err != nil {
		return fmt.Errorf("timescale: SaveLadder %s/%s: %w",
			asset.String(), quote.String(), err)
	}
	// A ladder write that matched NOTHING is the silent-failure shape that
	// matters most here: it is what "migration 0119 has not been applied yet
	// while the new binary is already running" looks like from the caller's
	// side — every MarkHold succeeds, the Redis marker is written, and the
	// durable ladder is simply never there when a flush needs it. Same for a
	// row closed out from under a tick. Neither is an error the Writer can
	// act on (it is best-effort by contract), so report it as ErrNotFound
	// and let the caller COUNT it.
	n, err := res.RowsAffected()
	if err != nil {
		// Driver cannot report it — do not manufacture a false negative.
		return nil //nolint:nilerr // RowsAffected support is driver-dependent; the UPDATE itself succeeded
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// LoadLadder reads the durable ADR-0019 ladder for (asset, quote).
// Implements freeze.LadderStore.
//
// Returns ok=false unless there is an OPEN row (recovered_at IS NULL) that
// carries a lifecycle (hold_until NOT NULL). Both conditions are
// load-bearing and neither is redundant:
//
//   - recovered_at IS NULL is what distinguishes "Redis lost the marker"
//     from "an operator ended this freeze". `stellarindex-ops
//     freeze-unfreeze` clears the marker AND stamps recovered_at, so after
//     a supported override this returns ok=false and the override stands.
//   - hold_until NOT NULL excludes rows written by a pre-0119 binary, which
//     have no ladder to restore. Those fall back to today's Redis-only
//     behaviour rather than rehydrating a zero ladder, which would read as
//     "freeze fired just now, 0 extensions" and reset the escalation clock.
//
// FiredAt comes from the row's own `frozen_at` — the freeze's true age,
// already stored, never duplicated into a second column.
//
// The caller ([freeze.Writer.LoadState]) applies the freshness bound on
// hold_until; this method reports the stored state verbatim so an operator
// tool can display a lapsed ladder without the reader's policy applied.
func (s *FreezeEventSink) LoadLadder(ctx context.Context, asset, quote canonical.Asset) (freeze.State, bool, error) {
	const q = `
		SELECT frozen_at, hold_until,
		       COALESCE(extensions_used, 0),
		       COALESCE(escalated, false),
		       COALESCE(corroborated, false)
		  FROM freeze_events
		 WHERE asset_id = $1 AND quote_id = $2
		   AND recovered_at IS NULL
		   AND hold_until IS NOT NULL
		 ORDER BY frozen_at DESC
		 LIMIT 1
	`
	var (
		firedAt   time.Time
		holdUntil time.Time
		st        freeze.State
	)
	err := s.db.QueryRowContext(ctx, q, asset.String(), quote.String()).
		Scan(&firedAt, &holdUntil, &st.ExtensionsUsed, &st.Escalated, &st.Corroborated)
	if errors.Is(err, sql.ErrNoRows) {
		return freeze.State{}, false, nil
	}
	if err != nil {
		return freeze.State{}, false, fmt.Errorf("timescale: LoadLadder %s/%s: %w",
			asset.String(), quote.String(), err)
	}
	st.FiredAt = firedAt.UTC()
	st.HoldUntil = holdUntil.UTC()
	return st, true, nil
}

// pairAdvisoryLockKey derives a stable int64 advisory-lock key
// from the (asset, quote) pair. FNV-1a 64-bit; collisions are
// possible across distinct pairs but cosmetic (false serialisation,
// no correctness loss).
func pairAdvisoryLockKey(asset, quote string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(asset))
	_, _ = h.Write([]byte{':'})
	_, _ = h.Write([]byte(quote))
	return int64(h.Sum64()) //nolint:gosec // narrowing to int64 is acceptable; pg accepts signed bigint
}

// ListOpen returns every (asset, quote) currently in firing state
// — `freeze_events` rows with recovered_at IS NULL. Snapshot read,
// no locking; the recovery worker can race a fresh RecordFreeze
// safely because both end states (still firing / cleared) are
// idempotent on the open-row PK.
//
// Returns the freeze package's OpenFreezePair shape so the
// recovery worker (which lives in `internal/aggregate/freeze`)
// avoids a hard dependency on this storage adapter.
func (s *FreezeEventSink) ListOpen(ctx context.Context) ([]freeze.OpenFreezePair, error) {
	const q = `
		SELECT asset_id, quote_id
		  FROM freeze_events
		 WHERE recovered_at IS NULL
	`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("timescale: ListOpen: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []freeze.OpenFreezePair
	for rows.Next() {
		var assetID, quoteID string
		if err := rows.Scan(&assetID, &quoteID); err != nil {
			return nil, fmt.Errorf("timescale: ListOpen scan: %w", err)
		}
		asset, err := canonical.ParseAsset(assetID)
		if err != nil {
			return nil, fmt.Errorf("timescale: ListOpen parse asset %q: %w", assetID, err)
		}
		quote, err := canonical.ParseAsset(quoteID)
		if err != nil {
			return nil, fmt.Errorf("timescale: ListOpen parse quote %q: %w", quoteID, err)
		}
		out = append(out, freeze.OpenFreezePair{Asset: asset, Quote: quote})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ListOpen rows: %w", err)
	}
	return out, nil
}

// MarkRecovered closes out the currently-firing row for (asset,
// quote). Called by a recovery worker (or the aggregator when it
// detects a previously-frozen pair has cleared) — NOT by the
// freeze.Writer.Mark path.
//
// Idempotent: if no open row exists, returns ErrNotFound. Caller
// can swallow and continue.
func (s *FreezeEventSink) MarkRecovered(ctx context.Context, asset, quote canonical.Asset) error {
	now := s.clock().UTC()
	var ledger uint32
	if s.getLedger != nil {
		ledger = s.getLedger.LatestLedger()
	}

	const q = `
		UPDATE freeze_events
		   SET recovered_at        = $3,
		       recovered_at_ledger = $4
		 WHERE asset_id = $1 AND quote_id = $2 AND recovered_at IS NULL
	`
	res, err := s.db.ExecContext(ctx, q,
		asset.String(), quote.String(), now, int64(ledger))
	if err != nil {
		return fmt.Errorf("timescale: MarkRecovered %s/%s: %w",
			asset.String(), quote.String(), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("timescale: MarkRecovered %s/%s: rows affected: %w",
			asset.String(), quote.String(), err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// FreezeEventRow is one freeze_events row for the /v1/anomalies read
// path. RecoveredAt is nil while the freeze is currently firing.
// Detail is the raw jsonb text (the API passes it through).
type FreezeEventRow struct {
	AssetID           string
	QuoteID           string
	FrozenAt          time.Time
	FrozenAtLedger    int64
	Reason            string
	FrozenValue       string
	RecoveredAt       *time.Time
	RecoveredAtLedger *int64
	Detail            string // "" when NULL
}

// ListFreezeEvents returns freeze events newest-first. firingOnly
// restricts to currently-firing (recovered_at IS NULL) via the
// partial index; otherwise the full timeline (capped). limit ≤ 500.
func (s *Store) ListFreezeEvents(ctx context.Context, firingOnly bool, limit int) ([]FreezeEventRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `
		SELECT asset_id, quote_id, frozen_at, frozen_at_ledger, reason,
		       frozen_value::text,
		       recovered_at, recovered_at_ledger,
		       COALESCE(detail::text, '')
		  FROM freeze_events`
	if firingOnly {
		q += ` WHERE recovered_at IS NULL`
	}
	q += ` ORDER BY frozen_at DESC LIMIT $1`
	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("timescale: ListFreezeEvents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []FreezeEventRow
	for rows.Next() {
		var (
			r         FreezeEventRow
			recAt     sql.NullTime
			recLedger sql.NullInt64
		)
		if err := rows.Scan(&r.AssetID, &r.QuoteID, &r.FrozenAt, &r.FrozenAtLedger,
			&r.Reason, &r.FrozenValue, &recAt, &recLedger, &r.Detail); err != nil {
			return nil, fmt.Errorf("timescale: ListFreezeEvents scan: %w", err)
		}
		if recAt.Valid {
			t := recAt.Time.UTC()
			r.RecoveredAt = &t
		}
		if recLedger.Valid {
			v := recLedger.Int64
			r.RecoveredAtLedger = &v
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ListFreezeEvents rows: %w", err)
	}
	return out, nil
}

// CountFiringFreezes returns the EXACT number of currently-firing
// freezes (recovered_at IS NULL), using the same partial index
// ListFreezeEvents' firingOnly arm selects on.
//
// It exists because /v1/anomalies used to derive its firing_count as
// `len(ListFreezeEvents(ctx, true, 500))` — a LIMIT-capped page, so a
// freeze storm of any size reported exactly 500 (C1-051,
// audit-2026-07-23). A count is also strictly cheaper: no row
// materialisation, no detail JSONB read.
func (s *Store) CountFiringFreezes(ctx context.Context) (int64, error) {
	const q = `SELECT count(*) FROM freeze_events WHERE recovered_at IS NULL`
	var n int64
	if err := s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("timescale: CountFiringFreezes: %w", err)
	}
	return n, nil
}

// FreezeReasonCount pairs a freeze reason with its count in the window.
type FreezeReasonCount struct {
	Reason string
	Count  int64
}

// FreezeReasonCounts tallies freezes per reason over the trailing
// `sinceDays` days (frozen_at within the window). Powers the
// /anomalies per-reason breakdown.
func (s *Store) FreezeReasonCounts(ctx context.Context, sinceDays int) ([]FreezeReasonCount, error) {
	if sinceDays <= 0 {
		sinceDays = 30
	}
	const q = `
		SELECT reason, count(*)::bigint
		  FROM freeze_events
		 WHERE frozen_at > now() - ($1 || ' days')::interval
		 GROUP BY reason
		 ORDER BY count(*) DESC`
	rows, err := s.db.QueryContext(ctx, q, sinceDays)
	if err != nil {
		return nil, fmt.Errorf("timescale: FreezeReasonCounts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []FreezeReasonCount
	for rows.Next() {
		var c FreezeReasonCount
		if err := rows.Scan(&c.Reason, &c.Count); err != nil {
			return nil, fmt.Errorf("timescale: FreezeReasonCounts scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// FreezeDailyReasonCount is one (UTC day, reason) cell of the
// /anomalies day×reason calendar heatmap.
type FreezeDailyReasonCount struct {
	Day    time.Time
	Reason string
	Count  int64
}

// FreezeDailyReasonCounts tallies freezes per (UTC day, reason) over
// the trailing `sinceDays` days — FreezeReasonCounts with a day
// dimension. Days with zero freezes have no rows (the chart renders
// the absence, it is not fabricated here).
//
// Index reasoning: same access path as FreezeReasonCounts — the
// WHERE keeps frozen_at bare (no function on the column), so
// hypertable chunk exclusion prunes to the window's chunks;
// date_trunc appears only in SELECT/GROUP BY. sinceDays is capped
// by the handler's parseWindowDays (≤365), bounding the scan.
func (s *Store) FreezeDailyReasonCounts(ctx context.Context, sinceDays int) ([]FreezeDailyReasonCount, error) {
	if sinceDays <= 0 {
		sinceDays = 30
	}
	const q = `
		SELECT date_trunc('day', frozen_at AT TIME ZONE 'UTC') AS day,
		       reason, count(*)::bigint
		  FROM freeze_events
		 WHERE frozen_at > now() - ($1 || ' days')::interval
		 GROUP BY day, reason
		 ORDER BY day ASC, reason ASC`
	rows, err := s.db.QueryContext(ctx, q, sinceDays)
	if err != nil {
		return nil, fmt.Errorf("timescale: FreezeDailyReasonCounts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []FreezeDailyReasonCount
	for rows.Next() {
		var c FreezeDailyReasonCount
		if err := rows.Scan(&c.Day, &c.Reason, &c.Count); err != nil {
			return nil, fmt.Errorf("timescale: FreezeDailyReasonCounts scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// encodeFreezeDetail captures the Decision's diagnostic context as
// the freeze_events.detail jsonb. Loose schema by design — different
// freeze paths (Phase 1 class-deviation, Phase 2 multi-signal) carry
// different fields and we want to preserve them all without a
// migration per addition.
func encodeFreezeDetail(decision anomaly.Decision) ([]byte, error) {
	if decision.Reason == "" && decision.DeviationPct == 0 && decision.Class == "" {
		return nil, nil
	}
	d := map[string]any{
		"action":        string(decision.Action),
		"class":         string(decision.Class),
		"deviation_pct": decision.DeviationPct,
		"reason":        decision.Reason,
	}
	return json.Marshal(d)
}

// mapFreezeReason translates the Decision into one of the values
// allowed by the freeze_events.reason CHECK constraint.
func mapFreezeReason(decision anomaly.Decision) string {
	// Phase 2 freezes carry "phase2:..." in Reason — currently
	// surfaced as 'divergence' (multi-source disagreement is the
	// driver). Phase 1 single-source / outlier paths fall through
	// to the default mapping.
	if len(decision.Reason) > 7 && decision.Reason[:7] == "phase2:" {
		return "divergence"
	}
	if decision.Action == anomaly.ActionFreeze {
		return "outlier_storm"
	}
	// Defensive fall-through for a decision shape this mapper doesn't
	// recognize. 'other', NOT 'manual' (audit 2026-07-31): 'manual' is
	// reserved for genuinely operator-initiated freezes, so defaulting
	// to it recorded any unrecognized automated decision as a human
	// action on the anomalies timeline. Vocabulary extended by
	// migration 0124.
	return "other"
}

// noteForLogger returns nil because the log-on-failure semantics are
// already handled by the freeze.Writer wrapper. Exposed for tests
// that want to assert the sink swallows errors gracefully.
//
// (Currently unreferenced in production code; retained for future
// use when the recovery worker lands.)
//
//nolint:unused // referenced from tests
func noteForLogger(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}
