package timescale

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// FreezeEventSink is the timescale-backed freeze.EventSink recording each clear-to-firing transition in `freeze_events`; RecordFreeze is a no-op while an unrecovered row exists, and Redis stays the liveness source of truth.
type FreezeEventSink struct {
	db        *sql.DB
	clock     func() time.Time
	getLedger LedgerProvider
	// onFreeze, when non-nil, runs after a RecordFreeze that actually inserted a row
	// (idempotent no-ops don't fire); the aggregator wires customerwebhook fan-out here.
	onFreeze FreezeHook
}

// FreezeHook is the best-effort post-insert callback (errors are logged and dropped so they cannot undo the INSERT); frozenValue is empty on a first-tick freeze, never the column's "0" filler.
type FreezeHook func(ctx context.Context, asset, quote canonical.Asset, frozenValue string, decision anomaly.Decision)

// LedgerProvider reads the most recently ingested ledger sequence stamped as `frozen_at_ledger`; implementations must be concurrent-safe and cheap as it runs on the aggregator hot path.
type LedgerProvider interface {
	LatestLedger() uint32
}

// NewFreezeEventSink constructs the sink; nil clock falls back to time.Now and nil getLedger stamps frozen_at_ledger 0 (tests only, production wires a real provider).
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

// WithFreezeHook installs a closure that runs after a successful row insert (idempotent no-ops don't fire).
func WithFreezeHook(hook FreezeHook) FreezeEventSinkOption {
	return func(s *FreezeEventSink) {
		s.onFreeze = hook
	}
}

// WithFreezeLedgerProvider wires the ledger seam so inserts stamp a real frozen_at_ledger instead of the 0 sentinel.
func WithFreezeLedgerProvider(p LedgerProvider) FreezeEventSinkOption {
	return func(s *FreezeEventSink) {
		s.getLedger = p
	}
}

// RecordFreeze implements freeze.EventSink.
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

	// frozen_value is NUMERIC NOT NULL, so a first-tick freeze (no prior bucket) writes 0.
	// The decimal string is forwarded verbatim; the driver parses NUMERIC without precision loss.
	frozenValueArg := frozenValue
	if frozenValueArg == "" {
		frozenValueArg = "0"
	}

	// Two workers racing on one pair could both pass WHERE NOT EXISTS and leave duplicate open
	// rows, so check and insert run under pg_advisory_xact_lock keyed on a hash of (asset, quote).
	// Advisory because the "no row yet" branch has nothing to row-lock, and Timescale forbids a
	// partial UNIQUE index that omits the partition key.
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
	// Fire the post-insert hook only when a row was actually appended.
	// The idempotency check + ON CONFLICT DO NOTHING means
	// RowsAffected==0 is the "already firing, this is just a TTL
	// refresh" path; firing the webhook then would spam subscribers.
	if s.onFreeze != nil {
		if affected, err := res.RowsAffected(); err == nil && affected > 0 {
			// The original value, not frozenValueArg: "0" is this column's
			// NOT NULL filler, and the hook's consumers read it as a price.
			s.onFreeze(ctx, asset, quote, frozenValue, decision)
		}
	}
	return nil
}

// SaveLadder persists lifecycle state onto the open `freeze_events` row for (asset, quote) as the durable copy surviving a Redis flush; UPDATE only, as RecordFreeze owns row creation.
func (s *FreezeEventSink) SaveLadder(ctx context.Context, asset, quote canonical.Asset, state freeze.State) error {
	var holdUntil any
	if !state.HoldUntil.IsZero() {
		holdUntil = state.HoldUntil.UTC()
	}
	// A RETIRE (NULL hold_until) also drops the per-window ladders, so a later freeze on the same
	// still-open row cannot inherit the ended freeze's windows. An active pair-level write leaves
	// them alone: its caller names no window. A retire nulls hold_until only; extensions_used /
	// escalated / corroborated stay as history for the anomalies timeline.
	const q = `
		UPDATE freeze_events
		   SET hold_until      = $3::timestamptz,
		       extensions_used = CASE WHEN $3::timestamptz IS NULL THEN extensions_used ELSE $4 END,
		       escalated       = CASE WHEN $3::timestamptz IS NULL THEN escalated ELSE $5 END,
		       corroborated    = CASE WHEN $3::timestamptz IS NULL THEN corroborated ELSE $6 END,
		       window_ladders  = CASE WHEN $3::timestamptz IS NULL THEN NULL
		                              ELSE window_ladders END
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
	// A write matching NOTHING is the silent failure that matters: with the ladder migration
	// unapplied (or the row closed under a tick) every MarkHold looks fine while the durable
	// ladder never exists. The Writer is best-effort and cannot act, so report ErrNotFound and
	// let the caller COUNT it.
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

// LoadLadder reads the durable ladder for (asset, quote), ok=false unless there is an OPEN row (recovered_at IS NULL) with hold_until NOT NULL, so an operator-ended freeze is not resurrected.
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

// The production sink must carry the window dimension. freeze.Writer
// discovers it by type assertion, so without this a signature drift would
// compile cleanly and silently fall back to the pair-keyed ladder.
var _ freeze.WindowLadderStore = (*FreezeEventSink)(nil)

// unownedLadderKey is the `window_ladders` key of a ladder with no
// recorded owning window — the pair-level ladder of a row written before
// migration 0163. Zero is not a window, so it cannot collide with one.
const unownedLadderKey = "0"

// ladderQuerier is the read seam [loadOpenLadderRow] needs, satisfied by
// both *sql.DB and *sql.Tx so the read runs inside SaveWindowLadder's
// transaction and outside one for LoadWindowLadders.
type ladderQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// openLadderRow is the durable ladder as stored on a pair's open
// `freeze_events` row: the 0119 pair-level columns plus the 0163
// per-window map.
type openLadderRow struct {
	// pair is the 0119 pair-level ladder; pair.Active() is false when
	// hold_until IS NULL, i.e. no ladder has been recorded or it has been
	// retired. hold_until is the switch the whole record is honoured on.
	pair freeze.State
	// windows is the decoded `window_ladders`, keyed as stored. nil when
	// the column IS NULL (a row written before 0163).
	windows map[string]freeze.State
}

// loadOpenLadderRow reads the newest open row's ladder for (asset,
// quote). ok=false means there is no open row at all.
func loadOpenLadderRow(ctx context.Context, q ladderQuerier, asset, quote canonical.Asset) (openLadderRow, bool, error) {
	const query = `
		SELECT frozen_at, hold_until,
		       COALESCE(extensions_used, 0),
		       COALESCE(escalated, false),
		       COALESCE(corroborated, false),
		       window_ladders::text
		  FROM freeze_events
		 WHERE asset_id = $1 AND quote_id = $2
		   AND recovered_at IS NULL
		 ORDER BY frozen_at DESC
		 LIMIT 1
	`
	var (
		firedAt   time.Time
		holdUntil sql.NullTime
		raw       sql.NullString
		row       openLadderRow
	)
	err := q.QueryRowContext(ctx, query, asset.String(), quote.String()).
		Scan(&firedAt, &holdUntil, &row.pair.ExtensionsUsed, &row.pair.Escalated, &row.pair.Corroborated, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return openLadderRow{}, false, nil
	}
	if err != nil {
		return openLadderRow{}, false, err
	}
	if holdUntil.Valid {
		row.pair.FiredAt = firedAt.UTC()
		row.pair.HoldUntil = holdUntil.Time.UTC()
	} else {
		row.pair = freeze.State{}
	}
	if raw.Valid {
		if err := json.Unmarshal([]byte(raw.String), &row.windows); err != nil {
			return openLadderRow{}, false, fmt.Errorf("decode window_ladders: %w", err)
		}
		if row.windows == nil {
			row.windows = map[string]freeze.State{} // a stored JSON null
		}
	}
	return row, true, nil
}

// entries returns the row's live per-window map, applying three rules:
//
//   - hold_until IS NULL means no ladder, whatever window_ladders holds. An older binary retires
//     a ladder by nulling hold_until and does not know the map exists.
//   - window_ladders IS NULL with a live pair-level ladder is a pre-per-window row; its ladder
//     has no recorded owner, so it is carried as the UNOWNED entry.
//   - window_ladders OUTRUN by the pair-level columns is a map gone stale under a rolled-back
//     binary ([pairLadderAhead]). It fails closed ([foldPairLadderInto]) rather than winning
//     for being non-NULL, which would discard an escalation.
//
// The write path inherits the third rule: [FreezeEventSink.SaveWindowLadder] merges into this
// result, so the next tick persists the fold instead of the stale summary.
func (r openLadderRow) entries() map[string]freeze.State {
	out := map[string]freeze.State{}
	if !r.pair.Active() {
		return out
	}
	if r.windows == nil {
		out[unownedLadderKey] = r.pair
		return out
	}
	for k, st := range r.windows {
		out[k] = st
	}
	if pairLadderAhead(r.pair, summariseLadders(out)) {
		foldPairLadderInto(out, r.pair)
	}
	return out
}

// pairLadderAhead reports whether the pair-level columns record a WORSE freeze than `summary`,
// the fold of the row's own window_ladders: escalated where no entry is, a higher rung, or a
// later hold.
//
// This binary writes the columns only as the map's summary or nulls both together, so they
// disagree only after a binary ROLLBACK with the schema left in place: the older binary keeps
// advancing the columns and the map goes stale.
//
// The hold is compared at STORED precision. hold_until is timestamptz (microseconds) while a map
// entry keeps nanoseconds, so they are never bit-equal. A bare After would, under a store that
// rounds, read every freeze as stale and mint an ownerless ladder for windows never frozen. A
// real disagreement is a lifecycle extension, i.e. minutes.
func pairLadderAhead(pair, summary freeze.State) bool {
	if pair.Escalated && !summary.Escalated {
		return true
	}
	if pair.ExtensionsUsed > summary.ExtensionsUsed {
		return true
	}
	return pair.HoldUntil.Sub(summary.HoldUntil) > time.Microsecond
}

// foldPairLadderInto makes a stale per-window map fail closed against the pair-level ladder that
// outran it.
//
// Which entries are stale is unknowable (the older binary's pair-level write is last-writer-wins
// across windows and names none), so every held entry is raised to the fail-closed fold of itself
// and the pair ladder (furthest hold, highest rung, escalated if either is), and the pair ladder
// is carried as the UNOWNED entry. Folding, not replacing, matters: a 5m window can overwrite the
// columns with a LATER hold while the map still holds a 1h window's escalation.
//
// This over-holds deliberately and boundedly: a never-frozen window rehydrates the ownerless
// ladder after a Redis loss until it lapses or an operator lifts the freeze.
func foldPairLadderInto(entries map[string]freeze.State, pair freeze.State) {
	for k, st := range entries {
		if st.Active() {
			entries[k] = summariseLadders(map[string]freeze.State{"entry": st, "pair": pair})
		}
	}
	if _, carried := entries[unownedLadderKey]; !carried {
		entries[unownedLadderKey] = pair
	}
}

// summariseLadders folds per-window ladders into the pair-level columns, fail-closed: furthest
// hold, highest rung, escalated/corroborated if ANY window is. Nothing held yields the zero State.
//
// Pair-level readers stay correct without knowing about windows: [freeze.Recovery] asks whether
// any window is still in a hold before closing the row, and a rolled-back binary can over-hold a
// window but never drop an escalation.
func summariseLadders(entries map[string]freeze.State) freeze.State {
	var out freeze.State
	for _, st := range entries {
		if !st.Active() {
			continue
		}
		if out.FiredAt.IsZero() || st.FiredAt.Before(out.FiredAt) {
			out.FiredAt = st.FiredAt
		}
		if st.HoldUntil.After(out.HoldUntil) {
			out.HoldUntil = st.HoldUntil
		}
		if st.ExtensionsUsed > out.ExtensionsUsed {
			out.ExtensionsUsed = st.ExtensionsUsed
		}
		out.Escalated = out.Escalated || st.Escalated
		out.Corroborated = out.Corroborated || st.Corroborated
	}
	return out
}

// windowLadderKey renders a window as its `window_ladders` key: whole
// seconds, decimal.
func windowLadderKey(window time.Duration) string {
	return strconv.FormatInt(int64(window/time.Second), 10)
}

// SaveWindowLadder persists `state` as `window`'s ladder on the pair's open row, leaving other windows untouched; an inactive state retires the entry and retiring the last nulls hold_until.
func (s *FreezeEventSink) SaveWindowLadder(
	ctx context.Context,
	asset, quote canonical.Asset,
	window time.Duration,
	state freeze.State,
) error {
	if window < time.Second {
		return fmt.Errorf("timescale: SaveWindowLadder %s/%s: window %s is not a whole-second aggregation window",
			asset.String(), quote.String(), window)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("timescale: SaveWindowLadder: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // safe no-op after COMMIT

	pairKey := pairAdvisoryLockKey(asset.String(), quote.String())
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, pairKey); err != nil {
		return fmt.Errorf("timescale: SaveWindowLadder: advisory lock: %w", err)
	}
	row, ok, err := loadOpenLadderRow(ctx, tx, asset, quote)
	if err != nil {
		return fmt.Errorf("timescale: SaveWindowLadder %s/%s: %w", asset.String(), quote.String(), err)
	}
	if !ok {
		return ErrNotFound
	}
	entries := row.entries()
	if state.Active() {
		entries[windowLadderKey(window)] = state
	} else {
		delete(entries, windowLadderKey(window))
	}
	if err := writeWindowLadders(ctx, tx, asset, quote, entries); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("timescale: SaveWindowLadder: commit: %w", err)
	}
	return nil
}

// writeWindowLadders stamps `entries` and their pair-level summary onto
// the pair's open row(s), inside the caller's transaction.
func writeWindowLadders(
	ctx context.Context,
	tx *sql.Tx,
	asset, quote canonical.Asset,
	entries map[string]freeze.State,
) error {
	body, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("timescale: SaveWindowLadder: encode window_ladders: %w", err)
	}
	summary := summariseLadders(entries)
	var holdUntil any
	if !summary.HoldUntil.IsZero() {
		holdUntil = summary.HoldUntil.UTC()
	}
	const q = `
		UPDATE freeze_events
		   SET window_ladders  = $3::jsonb,
		       hold_until      = $4::timestamptz,
		       extensions_used = CASE WHEN $4::timestamptz IS NULL THEN extensions_used ELSE $5 END,
		       escalated       = CASE WHEN $4::timestamptz IS NULL THEN escalated ELSE $6 END,
		       corroborated    = CASE WHEN $4::timestamptz IS NULL THEN corroborated ELSE $7 END
		 WHERE asset_id = $1 AND quote_id = $2 AND recovered_at IS NULL
	`
	if _, err := tx.ExecContext(ctx, q,
		asset.String(), quote.String(),
		string(body), holdUntil, summary.ExtensionsUsed, summary.Escalated, summary.Corroborated,
	); err != nil {
		return fmt.Errorf("timescale: SaveWindowLadder %s/%s: %w", asset.String(), quote.String(), err)
	}
	return nil
}

// LoadWindowLadders reads every window's durable ladder for (asset, quote).
func (s *FreezeEventSink) LoadWindowLadders(
	ctx context.Context,
	asset, quote canonical.Asset,
) (map[time.Duration]freeze.State, freeze.State, bool, error) {
	row, ok, err := loadOpenLadderRow(ctx, s.db, asset, quote)
	if err != nil {
		return nil, freeze.State{}, false, fmt.Errorf("timescale: LoadWindowLadders %s/%s: %w",
			asset.String(), quote.String(), err)
	}
	if !ok || !row.pair.Active() {
		return nil, freeze.State{}, false, nil
	}
	ladders := map[time.Duration]freeze.State{}
	var unowned freeze.State
	for key, st := range row.entries() {
		if key == unownedLadderKey {
			unowned = st
			continue
		}
		secs, perr := strconv.ParseInt(key, 10, 64)
		if perr != nil || secs <= 0 {
			continue // not a window this build wrote; ignore rather than fail the rehydrate
		}
		ladders[time.Duration(secs)*time.Second] = st
	}
	return ladders, unowned, true, nil
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

// ListOpen returns every (asset, quote) with an unrecovered `freeze_events` row as a lock-free snapshot; racing a fresh RecordFreeze is safe because both end states are idempotent.
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

// MarkRecovered closes the currently-firing row for (asset, quote) for the recovery worker; releasedBy must be "operator:<actor>" or "system:<worker>", and no open row returns ErrNotFound.
func (s *FreezeEventSink) MarkRecovered(ctx context.Context, asset, quote canonical.Asset, releasedBy string) error {
	if !validReleasedBy(releasedBy) {
		return fmt.Errorf("timescale: MarkRecovered %s/%s: released_by %q must be \"operator:<actor>\" or \"system:<worker>\"",
			asset.String(), quote.String(), releasedBy)
	}
	now := s.clock().UTC()
	var ledger uint32
	if s.getLedger != nil {
		ledger = s.getLedger.LatestLedger()
	}

	const q = `
		UPDATE freeze_events
		   SET recovered_at        = $3,
		       recovered_at_ledger = $4,
		       released_by         = $5
		 WHERE asset_id = $1 AND quote_id = $2 AND recovered_at IS NULL
	`
	res, err := s.db.ExecContext(ctx, q,
		asset.String(), quote.String(), now, int64(ledger), releasedBy)
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

func validReleasedBy(v string) bool {
	for _, prefix := range []string{"operator:", "system:"} {
		if rest, ok := strings.CutPrefix(v, prefix); ok {
			return strings.TrimSpace(rest) != ""
		}
	}
	return false
}

// FreezeEventRow is one freeze_events row for the /v1/anomalies read path.
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
	HoldUntil         *time.Time
	ExtensionsUsed    *int
	Escalated         *bool
	Corroborated      *bool
}

// ListFreezeEvents returns freeze events newest-first; firingOnly restricts to unrecovered rows via the partial index, otherwise the capped full timeline; limit <= 500.
func (s *Store) ListFreezeEvents(ctx context.Context, firingOnly bool, limit int) ([]FreezeEventRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `
		SELECT asset_id, quote_id, frozen_at, frozen_at_ledger, reason,
		       frozen_value::text,
		       recovered_at, recovered_at_ledger,
		       COALESCE(detail::text, ''),
		       hold_until, extensions_used, escalated, corroborated
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
			r            FreezeEventRow
			recAt        sql.NullTime
			recLedger    sql.NullInt64
			holdUntil    sql.NullTime
			extsUsed     sql.NullInt64
			escalated    sql.NullBool
			corroborated sql.NullBool
		)
		if err := rows.Scan(&r.AssetID, &r.QuoteID, &r.FrozenAt, &r.FrozenAtLedger,
			&r.Reason, &r.FrozenValue, &recAt, &recLedger, &r.Detail,
			&holdUntil, &extsUsed, &escalated, &corroborated); err != nil {
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
		if holdUntil.Valid {
			t := holdUntil.Time.UTC()
			r.HoldUntil = &t
		}
		if extsUsed.Valid {
			v := int(extsUsed.Int64)
			r.ExtensionsUsed = &v
		}
		if escalated.Valid {
			v := escalated.Bool
			r.Escalated = &v
		}
		if corroborated.Valid {
			v := corroborated.Bool
			r.Corroborated = &v
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ListFreezeEvents rows: %w", err)
	}
	return out, nil
}

// CountFiringFreezes returns the EXACT number of currently-firing freezes (recovered_at IS NULL), via the partial index ListFreezeEvents' firingOnly arm uses.
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

// FreezeReasonCounts tallies freezes per reason over the trailing `sinceDays` days (frozen_at within the window).
func (s *Store) FreezeReasonCounts(ctx context.Context, sinceDays int) ([]FreezeReasonCount, error) {
	if sinceDays <= 0 {
		sinceDays = 30
	}
	const q = `
		SELECT reason, count(*)::bigint
		  FROM freeze_events
		 WHERE frozen_at > now() - make_interval(days => $1)
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

// FreezeDailyReasonCount is one (UTC day, reason) cell of the /anomalies day×reason calendar heatmap.
type FreezeDailyReasonCount struct {
	Day    time.Time
	Reason string
	Count  int64
}

// FreezeDailyReasonCounts tallies freezes per (UTC day, reason) over the trailing `sinceDays`; frozen_at stays bare in WHERE so chunk exclusion prunes, and zero-freeze days have no rows.
func (s *Store) FreezeDailyReasonCounts(ctx context.Context, sinceDays int) ([]FreezeDailyReasonCount, error) {
	if sinceDays <= 0 {
		sinceDays = 30
	}
	const q = `
		SELECT date_trunc('day', frozen_at AT TIME ZONE 'UTC') AS day,
		       reason, count(*)::bigint
		  FROM freeze_events
		 WHERE frozen_at > now() - make_interval(days => $1)
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

// encodeFreezeDetail captures the Decision's diagnostic context as the freeze_events.detail jsonb.
// The schema is loose by design: freeze paths carry different fields, all preserved without a
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
	// "phase2:..." reasons surface as 'divergence' (multi-source disagreement); single-source and
	// outlier paths fall through to the default mapping.
	if len(decision.Reason) > 7 && decision.Reason[:7] == "phase2:" {
		return "divergence"
	}
	if decision.Action == anomaly.ActionFreeze {
		return "outlier_storm"
	}
	// Unrecognized shapes map to 'other', NOT 'manual': 'manual' is reserved for operator-initiated
	// freezes, and defaulting to it would show an automated decision as a human action.
	return "other"
}
