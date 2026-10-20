package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// SEP41EventKind discriminates the three supply-affecting SEP-41 event variants.
type SEP41EventKind string

const (
	SEP41EventMint     SEP41EventKind = "mint"
	SEP41EventBurn     SEP41EventKind = "burn"
	SEP41EventClawback SEP41EventKind = "clawback"
)

// IsValid reports whether k is one of the three supply-affecting kinds.
func (k SEP41EventKind) IsValid() bool {
	switch k {
	case SEP41EventMint, SEP41EventBurn, SEP41EventClawback:
		return true
	}
	return false
}

// SEP41SupplyEvent is one mint / burn / clawback event row.
type SEP41SupplyEvent struct {
	ContractID string
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	// EventIndex is the contract event's index within its operation —
	// the per-event discriminator added to the PK by migration 0057 so
	// multiple supply events from one op don't collide.
	EventIndex   uint32
	ObservedAt   time.Time
	Kind         SEP41EventKind
	Amount       *big.Int
	Counterparty string // empty when not present (reserved for future variants)
}

// InsertSEP41SupplyEvent appends one event row, idempotent on the PK (event_index and event_kind separate multiple supply events from one op).
func (s *Store) InsertSEP41SupplyEvent(ctx context.Context, e SEP41SupplyEvent) error {
	if e.ContractID == "" {
		return errors.New("timescale: InsertSEP41SupplyEvent: ContractID is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertSEP41SupplyEvent: TxHash is empty")
	}
	if !e.Kind.IsValid() {
		return fmt.Errorf("timescale: InsertSEP41SupplyEvent: invalid Kind %q (want mint/burn/clawback)", e.Kind)
	}
	if e.Amount == nil {
		return fmt.Errorf("timescale: InsertSEP41SupplyEvent: Amount is nil (contract=%s tx=%s)", e.ContractID, e.TxHash)
	}
	if e.Amount.Sign() < 0 {
		return fmt.Errorf("timescale: InsertSEP41SupplyEvent: negative Amount %s (event amounts are non-negative; kind discriminates direction)", e.Amount)
	}
	// Generation-guarded corrective upsert (migration 0110): a corrected re-derive of the supply-event `amount` (or
	// counterparty) lands in place when its generation is >= the stored one; a live gen-0 replay never reverts it.
	const q = `
        INSERT INTO sep41_supply_events (
            contract_id, ledger, tx_hash, op_index, observed_at,
            event_kind, event_index, amount, counterparty,
            derive_generation
        ) VALUES (
            $1, $2, $3, $4, $5,
            $6, $7, $8, $9,
            $10
        )
        ON CONFLICT (contract_id, ledger, tx_hash, op_index, observed_at,
                     event_kind, event_index) DO UPDATE SET
            amount            = EXCLUDED.amount,
            counterparty      = EXCLUDED.counterparty,
            derive_generation = EXCLUDED.derive_generation
          WHERE sep41_supply_events.derive_generation <= EXCLUDED.derive_generation
    `
	var counterparty sql.NullString
	if e.Counterparty != "" {
		counterparty = sql.NullString{String: e.Counterparty, Valid: true}
	}
	_, err := s.db.ExecContext(ctx, q,
		e.ContractID, int(e.Ledger), e.TxHash, int(e.OpIndex), e.ObservedAt.UTC(),
		string(e.Kind), int(e.EventIndex), e.Amount.String(), counterparty,
		s.deriveGeneration,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertSEP41SupplyEvent %s@%d: %w", e.ContractID, e.Ledger, err)
	}
	return nil
}

// SEP41NetMintAtOrBefore returns the running supply sum for `contractID` at or before `asOfLedger` as sum(mint) - sum(burn, clawback); non-nil on success, zero meaning no supply-affecting events yet.
func (s *Store) SEP41NetMintAtOrBefore(ctx context.Context, contractID string, asOfLedger uint32) (*big.Int, error) {
	const q = `
        SELECT COALESCE(
            sum(CASE WHEN event_kind = 'mint' THEN amount ELSE -amount END),
            0
        )::text
          FROM sep41_supply_events
         WHERE contract_id = $1
           AND ledger      <= $2
    `
	var raw string
	if err := s.db.QueryRowContext(ctx, q, contractID, int(asOfLedger)).Scan(&raw); err != nil {
		return nil, fmt.Errorf("timescale: SEP41NetMintAtOrBefore %s@%d: %w", contractID, asOfLedger, err)
	}
	v, ok := new(big.Int).SetString(raw, 10)
	if !ok {
		return nil, fmt.Errorf("timescale: SEP41NetMintAtOrBefore: parse %q", raw)
	}
	return v, nil
}

// SEP41KindTotals carries the per-kind running sums for one SEP-41 contract, kept distinct (not pre-netted) so clawback volume stays visible apart from voluntary burns.
type SEP41KindTotals struct {
	Mint     *big.Int
	Burn     *big.Int
	Clawback *big.Int
}

// SEP41KindTotalsAtOrBefore returns the per-kind sums for `contractID` at or before `asOfLedger` from the sep41_supply_rollup checkpoint plus the live tail above it, a disjoint union.
func (s *Store) SEP41KindTotalsAtOrBefore(ctx context.Context, contractID string, asOfLedger uint32) (SEP41KindTotals, error) {
	cp, ok, err := s.sep41RollupCheckpoint(ctx, contractID)
	if err != nil {
		return SEP41KindTotals{}, err
	}

	// The Soroban slice must START at the baseline boundary whenever a
	// baseline is seeded — see sep41SorobanFloor.
	floor := sep41SorobanFloor(cp)

	// ─── Soroban-era totals (ledger ≥ the genesis boundary, from PG) ───
	var soroban SEP41KindTotals
	switch {
	case !ok || cp.lastLedger > asOfLedger:
		// No checkpoint yet, or the checkpoint is AHEAD of the requested
		// ledger (a historical read below the watermark) — the rollup can't
		// be subtracted back, so take the exact full aggregate.
		soroban, err = s.sep41KindTotalsFullSum(ctx, contractID, floor, asOfLedger)
		if err != nil {
			return SEP41KindTotals{}, err
		}
	default:
		// Checkpoint ≤ asOfLedger: add only the live tail delta above it.
		delta, derr := s.sep41KindTotalsRange(ctx, contractID, cp.lastLedger, cp.lastClosedAt, floor, asOfLedger)
		if derr != nil {
			return SEP41KindTotals{}, derr
		}
		soroban = SEP41KindTotals{
			Mint:     new(big.Int).Add(cp.rollup.Mint, delta.Mint),
			Burn:     new(big.Int).Add(cp.rollup.Burn, delta.Burn),
			Clawback: new(big.Int).Add(cp.rollup.Clawback, delta.Clawback),
		}
	}

	// ─── Pre-Soroban genesis baseline (ledger < boundary, seeded from CH) ─
	if ok && cp.genesisSeeded && asOfLedger >= cp.genesisLedger {
		return SEP41KindTotals{
			Mint:     new(big.Int).Add(soroban.Mint, cp.genesis.Mint),
			Burn:     new(big.Int).Add(soroban.Burn, cp.genesis.Burn),
			Clawback: new(big.Int).Add(soroban.Clawback, cp.genesis.Clawback),
		}, nil
	}
	return soroban, nil
}

// SEP41GenesisBaselineSeeded reports whether a pre-Soroban genesis baseline is seeded for the contract (false when no rollup row exists).
func (s *Store) SEP41GenesisBaselineSeeded(ctx context.Context, contractID string) (bool, error) {
	const q = `
        SELECT genesis_baseline_ledger IS NOT NULL
          FROM sep41_supply_rollup
         WHERE contract_id = $1
    `
	var seeded bool
	err := s.db.QueryRowContext(ctx, q, contractID).Scan(&seeded)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("timescale: sep41 genesis-baseline seeded %s: %w", contractID, err)
	}
	return seeded, nil
}

// sep41RollupRow is the parsed sep41_supply_rollup row: Soroban-era totals + checkpoint ledger, plus the seeded genesis baseline (0088).
// genesisSeeded is genesis_baseline_ledger IS NOT NULL.
type sep41RollupRow struct {
	rollup        SEP41KindTotals
	lastLedger    uint32
	lastClosedAt  time.Time // close time of lastLedger; zero when unknown
	genesis       SEP41KindTotals
	genesisLedger uint32
	genesisSeeded bool
}

// sep41RollupCheckpoint reads the per-contract rollup row. ok=false when no
// checkpoint exists yet — the caller falls back to the full aggregate.
func (s *Store) sep41RollupCheckpoint(ctx context.Context, contractID string) (sep41RollupRow, bool, error) {
	const q = `
        SELECT mint_total::text, burn_total::text, clawback_total::text, last_ledger,
               genesis_mint_total::text, genesis_burn_total::text, genesis_clawback_total::text,
               genesis_baseline_ledger,
               CASE WHEN bound_ledger = last_ledger THEN bound_observed_at END
          FROM sep41_supply_rollup
         WHERE contract_id = $1
    `
	var mintRaw, burnRaw, clawbackRaw string
	var boundObservedAt sql.NullTime
	var lastLedger int64
	var gMintRaw, gBurnRaw, gClawbackRaw string
	var genesisLedger sql.NullInt64
	err := s.db.QueryRowContext(ctx, q, contractID).Scan(
		&mintRaw, &burnRaw, &clawbackRaw, &lastLedger,
		&gMintRaw, &gBurnRaw, &gClawbackRaw, &genesisLedger, &boundObservedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return sep41RollupRow{}, false, nil
	}
	if err != nil {
		return sep41RollupRow{}, false, fmt.Errorf("timescale: sep41 rollup checkpoint %s: %w", contractID, err)
	}
	rollup, err := parseSEP41Totals(mintRaw, burnRaw, clawbackRaw)
	if err != nil {
		return sep41RollupRow{}, false, err
	}
	genesis, err := parseSEP41Totals(gMintRaw, gBurnRaw, gClawbackRaw)
	if err != nil {
		return sep41RollupRow{}, false, err
	}
	row := sep41RollupRow{
		rollup:        rollup,
		lastLedger:    uint32(lastLedger),
		lastClosedAt:  boundObservedAt.Time,
		genesis:       genesis,
		genesisSeeded: genesisLedger.Valid,
	}
	if genesisLedger.Valid {
		row.genesisLedger = uint32(genesisLedger.Int64)
	}
	return row, true, nil
}

// UpsertSEP41GenesisBaseline seeds a contract's pre-Soroban per-kind opening balance into sep41_supply_rollup and re-derives the worker-owned fold in one transaction; genesis columns are SET, not added, so re-running converges.
func (s *Store) UpsertSEP41GenesisBaseline(ctx context.Context, contractID string, genesis SEP41KindTotals, baselineLedger uint32) error {
	if err := validateSEP41GenesisBaseline(contractID, genesis); err != nil {
		return err
	}
	tx, err := beginSEP41RollupTx(ctx, s.db)
	if err != nil {
		return fmt.Errorf("timescale: UpsertSEP41GenesisBaseline %s: %w", contractID, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockSEP41RollupRow(ctx, tx, contractID); err != nil {
		return fmt.Errorf("timescale: UpsertSEP41GenesisBaseline %s: %w", contractID, err)
	}
	const q = `
        UPDATE sep41_supply_rollup
           SET genesis_mint_total      = $2,
               genesis_burn_total      = $3,
               genesis_clawback_total  = $4,
               genesis_baseline_ledger = $5,
               genesis_seeded_at       = now(),
               mint_total              = 0,
               burn_total              = 0,
               clawback_total          = 0,
               last_ledger             = 0,
               bound_ledger            = NULL,
               bound_observed_at       = NULL,
               updated_at              = now()
         WHERE contract_id = $1
    `
	if _, err := tx.ExecContext(ctx, q,
		contractID, genesis.Mint.String(), genesis.Burn.String(), genesis.Clawback.String(),
		int(baselineLedger),
	); err != nil {
		return fmt.Errorf("timescale: UpsertSEP41GenesisBaseline %s: %w", contractID, err)
	}
	if _, _, _, err := foldSEP41RollupLocked(ctx, tx, contractID); err != nil {
		return fmt.Errorf("timescale: UpsertSEP41GenesisBaseline %s re-fold: %w", contractID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("timescale: UpsertSEP41GenesisBaseline %s commit: %w", contractID, err)
	}
	return nil
}

// validateSEP41GenesisBaseline rejects a seed the rollup row cannot hold.
func validateSEP41GenesisBaseline(contractID string, genesis SEP41KindTotals) error {
	if contractID == "" {
		return errors.New("timescale: UpsertSEP41GenesisBaseline: empty contractID")
	}
	if genesis.Mint == nil || genesis.Burn == nil || genesis.Clawback == nil {
		return fmt.Errorf("timescale: UpsertSEP41GenesisBaseline %s: nil genesis total", contractID)
	}
	if genesis.Mint.Sign() < 0 || genesis.Burn.Sign() < 0 || genesis.Clawback.Sign() < 0 {
		return fmt.Errorf("timescale: UpsertSEP41GenesisBaseline %s: negative genesis total (mint=%s burn=%s clawback=%s) — per-kind sums are non-negative",
			contractID, genesis.Mint, genesis.Burn, genesis.Clawback)
	}
	return nil
}

// sep41SorobanFloor is the lowest ledger the Soroban slice may include; the baseline covers < genesis_baseline_ledger, else it sums twice.
// Resum shares it so verify-rollup can't agree with itself; 0 when no baseline is seeded.
func sep41SorobanFloor(cp sep41RollupRow) uint32 {
	if !cp.genesisSeeded {
		return 0
	}
	return cp.genesisLedger
}

// sep41KindTotalsFullSum is the full per-contract aggregate at-or-before asOfLedger, the
// correctness backstop the fast path falls back to; it must stay equal to rollup plus delta.
func (s *Store) sep41KindTotalsFullSum(ctx context.Context, contractID string, floorLedger, asOfLedger uint32) (SEP41KindTotals, error) {
	const q = `
        SELECT
            COALESCE(sum(amount) FILTER (WHERE event_kind = 'mint'),     0)::text AS mint_total,
            COALESCE(sum(amount) FILTER (WHERE event_kind = 'burn'),     0)::text AS burn_total,
            COALESCE(sum(amount) FILTER (WHERE event_kind = 'clawback'), 0)::text AS clawback_total
          FROM sep41_supply_events
         WHERE contract_id = $1
           AND ledger      >= $2
           AND ledger      <= $3
    `
	var mintRaw, burnRaw, clawbackRaw string
	if err := s.db.QueryRowContext(ctx, q, contractID, int(floorLedger), int(asOfLedger)).Scan(&mintRaw, &burnRaw, &clawbackRaw); err != nil {
		return SEP41KindTotals{}, fmt.Errorf("timescale: SEP41KindTotalsAtOrBefore %s@%d: %w", contractID, asOfLedger, err)
	}
	return parseSEP41Totals(mintRaw, burnRaw, clawbackRaw)
}

// sep41KindTotalsRange sums the delta over (afterLedger, asOfLedger]; `ledger > afterLedger` walks only the index tail, cheap at any
// history depth. afterClosedAt (afterLedger's close time) bounds observed_at so planning prunes chunks instead of visiting all of them;
// every later ledger closed after it, and the margin only absorbs clock skew between writers.
func (s *Store) sep41KindTotalsRange(ctx context.Context, contractID string, afterLedger uint32, afterClosedAt time.Time, floorLedger, asOfLedger uint32) (SEP41KindTotals, error) {
	const q = `
        SELECT
            COALESCE(sum(amount) FILTER (WHERE event_kind = 'mint'),     0)::text AS mint_total,
            COALESCE(sum(amount) FILTER (WHERE event_kind = 'burn'),     0)::text AS burn_total,
            COALESCE(sum(amount) FILTER (WHERE event_kind = 'clawback'), 0)::text AS clawback_total
          FROM sep41_supply_events
         WHERE contract_id = $1
           AND ledger      >  $2
           AND ledger      >= $3
           AND ledger      <= $4
           AND observed_at >= $5
    `
	lower := time.Time{}
	if !afterClosedAt.IsZero() {
		lower = afterClosedAt.Add(-sep41BoundMargin)
	}
	var mintRaw, burnRaw, clawbackRaw string
	if err := s.db.QueryRowContext(ctx, q, contractID, int(afterLedger), int(floorLedger), int(asOfLedger), lower.UTC()).Scan(&mintRaw, &burnRaw, &clawbackRaw); err != nil {
		return SEP41KindTotals{}, fmt.Errorf("timescale: sep41 kind-totals delta %s@(%d,%d]: %w", contractID, afterLedger, asOfLedger, err)
	}
	return parseSEP41Totals(mintRaw, burnRaw, clawbackRaw)
}

// sep41WatermarkWindow is how far back sep41ObserverWatermark looks before falling back to the full read.
const sep41WatermarkWindow = 7 * 24 * time.Hour

// sep41BoundMargin widens the delta read's observed_at bound; a day is far below the 7-day chunk interval, so pruning is unaffected.
const sep41BoundMargin = 24 * time.Hour

// parseSEP41Totals parses the three ::text NUMERIC sums into non-nil
// *big.Int components (ADR-0003: i128 sums never truncate).
func parseSEP41Totals(mintRaw, burnRaw, clawbackRaw string) (SEP41KindTotals, error) {
	mint, err := parseSEP41Numeric(mintRaw, "mint_total")
	if err != nil {
		return SEP41KindTotals{}, err
	}
	burn, err := parseSEP41Numeric(burnRaw, "burn_total")
	if err != nil {
		return SEP41KindTotals{}, err
	}
	clawback, err := parseSEP41Numeric(clawbackRaw, "clawback_total")
	if err != nil {
		return SEP41KindTotals{}, err
	}
	return SEP41KindTotals{Mint: mint, Burn: burn, Clawback: clawback}, nil
}

// MinSEP41ComponentLedger returns how far the SEP-41 supply-event PRODUCER has progressed at or before `asOfLedger` (the watermark across all contracts); zero when the contract has no events (gate-skip signal), and not MAX(ledger).
func (s *Store) MinSEP41ComponentLedger(ctx context.Context, contractID string, asOfLedger uint32) (uint32, error) {
	const instrumentedQ = `
		SELECT EXISTS (SELECT 1 FROM sep41_supply_events
		                WHERE contract_id = $1 AND ledger <= $2)
	`
	var instrumented bool
	if err := s.db.QueryRowContext(ctx, instrumentedQ, contractID, int(asOfLedger)).Scan(&instrumented); err != nil {
		return 0, fmt.Errorf("timescale: MinSEP41ComponentLedger %s@%d: %w", contractID, asOfLedger, err)
	}
	if !instrumented {
		return 0, nil
	}
	return s.sep41ObserverWatermark(ctx, asOfLedger)
}

// sep41ObserverWatermark returns MAX(ledger) over ALL sep41_supply_events at-or-before asOfLedger, memoized like
// [Store.classicObserverWatermark] (identical per contract in a tick; TTL far below the staleness threshold, so it cannot mask a stall).
func (s *Store) sep41ObserverWatermark(ctx context.Context, asOfLedger uint32) (uint32, error) {
	c := &s.sep41Watermark
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.asOf == asOfLedger && !c.fetchedAt.IsZero() && time.Since(c.fetchedAt) < classicWatermarkTTL {
		return c.ledger, nil
	}
	// observed_at is the ledger close time, so when the recent window holds a row at or before asOfLedger it holds the max;
	// bounding it prunes to the newest chunks (1.2 s unbounded on r1, 2 ms bounded). A historical asOf or a stalled producer
	// finds the window empty and pays the full read.
	const recentQ = `SELECT COALESCE(MAX(ledger), 0) FROM sep41_supply_events WHERE ledger <= $1 AND observed_at >= $2::timestamptz`
	const fullQ = `SELECT COALESCE(MAX(ledger), 0) FROM sep41_supply_events WHERE ledger <= $1`
	var watermark uint32
	if err := s.db.QueryRowContext(ctx, recentQ, int(asOfLedger), time.Now().Add(-sep41WatermarkWindow).UTC()).Scan(&watermark); err != nil {
		return 0, fmt.Errorf("timescale: sep41ObserverWatermark@%d: %w", asOfLedger, err)
	}
	if watermark == 0 {
		if err := s.db.QueryRowContext(ctx, fullQ, int(asOfLedger)).Scan(&watermark); err != nil {
			return 0, fmt.Errorf("timescale: sep41ObserverWatermark@%d: %w", asOfLedger, err)
		}
	}
	c.asOf, c.ledger, c.fetchedAt = asOfLedger, watermark, time.Now()
	return watermark, nil
}

// validateSEP41SupplyEventRows is the pre-DB row contract for the multi-row writers (matches InsertSEP41SupplyEvent): one bad row would
// fail a CHECK and abort the whole statement. op names the caller in the error.
func validateSEP41SupplyEventRows(op string, rows []SEP41SupplyEvent) error {
	for i := range rows {
		e := &rows[i]
		if e.ContractID == "" {
			return fmt.Errorf("timescale: %s: row %d empty ContractID", op, i)
		}
		if e.TxHash == "" {
			return fmt.Errorf("timescale: %s: row %d empty TxHash", op, i)
		}
		if e.Amount == nil {
			return fmt.Errorf("timescale: %s: row %d nil Amount", op, i)
		}
		if e.Amount.Sign() < 0 {
			return fmt.Errorf("timescale: %s: row %d negative Amount %s", op, i, e.Amount)
		}
		if !e.Kind.IsValid() {
			return fmt.Errorf("timescale: %s: row %d invalid Kind %q", op, i, e.Kind)
		}
	}
	return nil
}

func parseSEP41Numeric(raw, label string) (*big.Int, error) {
	v, ok := new(big.Int).SetString(raw, 10)
	if !ok {
		return nil, fmt.Errorf("timescale: parse %s %q", label, raw)
	}
	return v, nil
}

// InsertSEP41SupplyEventBatch is the multi-row sibling of InsertSEP41SupplyEvent for full-history re-derives, where per-row round-trips cap write throughput.
func (s *Store) InsertSEP41SupplyEventBatch(ctx context.Context, rows []SEP41SupplyEvent) error {
	if len(rows) == 0 {
		return nil
	}
	if err := validateSEP41SupplyEventRows("InsertSEP41SupplyEventBatch", rows); err != nil {
		return err
	}

	// Postgres rejects ON CONFLICT DO UPDATE when one statement presents the same conflict key
	// twice ("cannot affect row a second time"), so collapse intra-batch duplicates (last wins).
	insertRows := dedupeSEP41SupplyRows(rows)

	const ncols = 10
	var sb strings.Builder
	// Generation-guarded: a higher-or-equal-generation re-derive updates amount in place, and a
	// live gen-0 replay can never revert it.
	sb.WriteString(`
        INSERT INTO sep41_supply_events (
            contract_id, ledger, tx_hash, op_index, event_index,
            observed_at, event_kind, amount, counterparty,
            derive_generation
        ) VALUES `)
	args := make([]any, 0, ncols*len(insertRows))
	for i := range insertRows {
		if i > 0 {
			sb.WriteString(", ")
		}
		base := i * ncols
		fmt.Fprintf(&sb,
			"($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			base+1, base+2, base+3, base+4, base+5,
			base+6, base+7, base+8, base+9, base+10,
		)
		e := &insertRows[i]
		args = append(args,
			e.ContractID,
			int64(e.Ledger),
			e.TxHash,
			// int, not int16: narrowing would wrap an op with >32,767 supply events negative and
			// violate `CHECK (op_index >= 0)`, failing the ENTIRE batch, unlike the single-row writer.
			int(e.OpIndex),
			int(e.EventIndex),
			e.ObservedAt.UTC(),
			string(e.Kind),
			e.Amount.String(),
			sql.NullString{String: e.Counterparty, Valid: e.Counterparty != ""},
			s.deriveGeneration,
		)
	}
	sb.WriteString(` ON CONFLICT (contract_id, ledger, tx_hash, op_index, observed_at, event_kind, event_index) DO UPDATE SET
            amount            = EXCLUDED.amount,
            counterparty      = EXCLUDED.counterparty,
            derive_generation = EXCLUDED.derive_generation
          WHERE sep41_supply_events.derive_generation <= EXCLUDED.derive_generation`)

	if _, err := s.db.ExecContext(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("timescale: InsertSEP41SupplyEventBatch (%d rows): %w", len(insertRows), err)
	}
	return nil
}

// sep41SupplyConflictKey is the ON CONFLICT identity (contract_id, ledger, tx_hash, op_index, observed_at, event_kind, event_index);
// observed_at is normalised to UTC UnixNano so equal instants collapse.
type sep41SupplyConflictKey struct {
	contractID      string
	ledger          uint32
	txHash          string
	opIndex         uint32
	observedAtNanos int64
	eventKind       string
	eventIndex      uint32
}

func sep41SupplyKeyOf(e *SEP41SupplyEvent) sep41SupplyConflictKey {
	return sep41SupplyConflictKey{
		contractID:      e.ContractID,
		ledger:          e.Ledger,
		txHash:          e.TxHash,
		opIndex:         e.OpIndex,
		observedAtNanos: e.ObservedAt.UTC().UnixNano(),
		eventKind:       string(e.Kind),
		eventIndex:      e.EventIndex,
	}
}

// dedupeSEP41SupplyRows keeps the LAST copy of rows colliding on the conflict key, in first-seen order ([dedupeSEP41TransferRows]).
func dedupeSEP41SupplyRows(rows []SEP41SupplyEvent) []SEP41SupplyEvent {
	firstDup := -1
	seen := make(map[sep41SupplyConflictKey]int, len(rows))
	for i := range rows {
		k := sep41SupplyKeyOf(&rows[i])
		if _, ok := seen[k]; ok {
			firstDup = i
			break
		}
		seen[k] = i
	}
	if firstDup < 0 {
		return rows // already unique
	}
	out := make([]SEP41SupplyEvent, 0, len(rows))
	pos := make(map[sep41SupplyConflictKey]int, len(rows))
	for i := range rows {
		k := sep41SupplyKeyOf(&rows[i])
		if idx, ok := pos[k]; ok {
			out[idx] = rows[i] // last wins
			continue
		}
		pos[k] = len(out)
		out = append(out, rows[i])
	}
	return out
}

// sep41SupplyCursorSource / sep41SupplyCursorSub name the ingestion_cursors row recording how far
// the sep41_supply domain is DURABLY written: the projector's per-source cursor, advanced by
// compare-and-swap once a cycle's sink writes have all committed. They are literals so storage
// does not depend on the source packages;
// TestSEP41SupplyRollup_SettledBoundIsTheDurableCursor pins the pairing.
const (
	sep41SupplyCursorSource = "projector"
	sep41SupplyCursorSub    = "sep41_supply"
)

// SEP41RollupAdvance reports what one AdvanceSEP41SupplyRollup pass folded, for the worker's metrics + logging.
type SEP41RollupAdvance struct {
	ContractID string
	FromLedger uint32 // checkpoint ledger before the pass
	ToLedger   uint32 // checkpoint ledger after the pass
	Advanced   bool   // true when newly-settled rows were folded in
	// CursorAbsent is true when the projector's sep41_supply cursor row does
	// not exist, so the pass folded nothing and the reader stays on the
	// full-sum path until it appears. Distinct from a steady-state no-op.
	CursorAbsent bool
}

// AdvanceSEP41SupplyRollup folds a contract's newly-settled sep41_supply_events into its sep41_supply_rollup checkpoint; idempotent and monotonic, summed in NUMERIC (ADR-0003).
func (s *Store) AdvanceSEP41SupplyRollup(ctx context.Context, contractID string) (SEP41RollupAdvance, error) {
	if contractID == "" {
		return SEP41RollupAdvance{}, errors.New("timescale: AdvanceSEP41SupplyRollup: empty contractID")
	}
	tx, err := beginSEP41RollupTx(ctx, s.db)
	if err != nil {
		return SEP41RollupAdvance{}, fmt.Errorf("timescale: AdvanceSEP41SupplyRollup %s: %w", contractID, err)
	}
	defer func() { _ = tx.Rollback() }()

	// Hold the row lock BEFORE the folding statement starts, so that
	// statement's snapshot is taken with the lock already ours.
	if err := lockSEP41RollupRow(ctx, tx, contractID); err != nil {
		return SEP41RollupAdvance{}, fmt.Errorf("timescale: AdvanceSEP41SupplyRollup %s: %w", contractID, err)
	}
	fromLedger, toLedger, cursorAbsent, err := foldSEP41RollupLocked(ctx, tx, contractID)
	if err != nil {
		return SEP41RollupAdvance{}, fmt.Errorf("timescale: AdvanceSEP41SupplyRollup %s: %w", contractID, err)
	}
	if err := tx.Commit(); err != nil {
		return SEP41RollupAdvance{}, fmt.Errorf("timescale: AdvanceSEP41SupplyRollup %s commit: %w", contractID, err)
	}
	return SEP41RollupAdvance{
		ContractID:   contractID,
		FromLedger:   uint32(fromLedger),
		ToLedger:     uint32(toLedger),
		Advanced:     toLedger > fromLedger,
		CursorAbsent: cursorAbsent,
	}, nil
}

// sep41RollupLockTimeout bounds a fold writer's wait on a rollup row lock: a seed or reset can hold it through a cold fold, which would
// stall every later contract. A contended pass yields (an `error` outcome) and retries next cadence.
const sep41RollupLockTimeout = 10 * time.Second

// beginSEP41RollupTx opens a fold-writer txn with lock waits bounded by [sep41RollupLockTimeout]. No statement_timeout on purpose: a cold
// fold that cannot finish leaves the contract on the slower full-sum read path forever.
func beginSEP41RollupTx(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%d'", sep41RollupLockTimeout.Milliseconds())); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("set lock_timeout: %w", err)
	}
	return tx, nil
}

// foldSEP41RollupLocked folds a contract's newly-settled events on a txn that ALREADY holds the row lock ([lockSEP41RollupRow]).
// Returns the checkpoint ledger before and after, and whether the projector's cursor row was absent (nothing foldable).
func foldSEP41RollupLocked(ctx context.Context, tx *sql.Tx, contractID string) (fromLedger, toLedger int64, cursorAbsent bool, err error) {
	lower, err := sep41FoldLowerBound(ctx, tx, contractID)
	if err != nil {
		return 0, 0, false, err
	}
	// One statement under the caller's lock. `locked` is the boundary and floor: it cannot change while locked and the snapshot
	// postdates the lock, so boundary and summed events are one consistent view (MATERIALIZED evaluates once). `bound.mx` is the
	// contract's max ledger since $4, the global max whenever that is past from_ledger; folding strictly below defers the mid-write tip. `settled` is the durable cursor, NULL if the
	// projector never committed a cycle (delta treats it as 0). `delta` is the settled tail sum over (from_ledger, settled
	// bound), floored. `unbounded` is the checkpoint's close time when its bound is missing (a row folded before 0219), so an
	// eventless fold still fills it in. UPDATE adds the delta, moves last_ledger to the max ledger folded and records its close
	// time.
	const q = `
        WITH locked AS MATERIALIZED (
            SELECT last_ledger                          AS from_ledger,
                   COALESCE(genesis_baseline_ledger, 0) AS floor_ledger,
                   bound_ledger
              FROM sep41_supply_rollup
             WHERE contract_id = $1
               FOR UPDATE
        ),
        bound AS (
            SELECT COALESCE(max(ledger), 0) AS mx
              FROM sep41_supply_events
             WHERE contract_id = $1
               AND observed_at >= $4
        ),
        unbounded AS (
            SELECT max(e.observed_at) AS at
              FROM sep41_supply_events e, locked l
             WHERE l.from_ledger > 0
               AND l.bound_ledger IS DISTINCT FROM l.from_ledger
               AND e.contract_id = $1
               AND e.ledger = l.from_ledger
        ),
        settled AS (
            SELECT (SELECT last_ledger
                      FROM ingestion_cursors
                     WHERE source = $2 AND sub_source = $3) AS through
        ),
        delta AS (
            SELECT
                (SELECT from_ledger FROM locked)                                   AS from_ledger,
                COALESCE(sum(e.amount) FILTER (WHERE e.event_kind = 'mint'),     0) AS d_mint,
                COALESCE(sum(e.amount) FILTER (WHERE e.event_kind = 'burn'),     0) AS d_burn,
                COALESCE(sum(e.amount) FILTER (WHERE e.event_kind = 'clawback'), 0) AS d_clawback,
                COALESCE(max(e.ledger), (SELECT from_ledger FROM locked))           AS to_ledger,
                max(e.observed_at)                                                 AS to_observed_at,
                (SELECT through IS NULL FROM settled)                              AS cursor_absent
              FROM sep41_supply_events e
             WHERE e.contract_id = $1
               AND e.ledger       > (SELECT from_ledger  FROM locked)
               AND e.ledger      >= (SELECT floor_ledger FROM locked)
               AND e.ledger       < (SELECT mx FROM bound)
               AND e.ledger      <= (SELECT COALESCE(through, 0) FROM settled)
               AND e.observed_at >= $4
        )
        UPDATE sep41_supply_rollup r
           SET mint_total     = r.mint_total     + d.d_mint,
               burn_total     = r.burn_total     + d.d_burn,
               clawback_total = r.clawback_total + d.d_clawback,
               last_ledger    = d.to_ledger,
               bound_ledger      = CASE WHEN d.to_observed_at IS NOT NULL THEN d.to_ledger
                                        WHEN u.at IS NOT NULL THEN d.from_ledger
                                        ELSE r.bound_ledger END,
               bound_observed_at = COALESCE(d.to_observed_at, u.at, r.bound_observed_at),
               updated_at     = now()
          FROM delta d, unbounded u
         WHERE r.contract_id = $1
        RETURNING d.from_ledger, r.last_ledger, d.cursor_absent
    `
	if err := tx.QueryRowContext(ctx, q, contractID,
		sep41SupplyCursorSource, sep41SupplyCursorSub, lower.UTC(),
	).Scan(&fromLedger, &toLedger, &cursorAbsent); err != nil {
		return 0, 0, false, fmt.Errorf("fold: %w", err)
	}
	return fromLedger, toLedger, cursorAbsent, nil
}

// sep41FoldLowerBound is the fold's observed_at floor: the checkpoint's close time less [sep41BoundMargin], so planning prunes the
// hypertable's chunks (88 ms planning unbounded vs 0.5 ms on r1). Exact because every ledger after the checkpoint closed after it.
// Zero, admitting every row, when the bound is unknown or a reset cleared it. The caller holds the row lock, so the fold sees this row.
func sep41FoldLowerBound(ctx context.Context, tx *sql.Tx, contractID string) (time.Time, error) {
	const q = `
        SELECT CASE WHEN bound_ledger = last_ledger THEN bound_observed_at END
          FROM sep41_supply_rollup
         WHERE contract_id = $1
    `
	var at sql.NullTime
	if err := tx.QueryRowContext(ctx, q, contractID).Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf("fold bound: %w", err)
	}
	if !at.Valid {
		return time.Time{}, nil
	}
	return at.Time.Add(-sep41BoundMargin), nil
}

// lockSEP41RollupRow materialises and locks the rollup row in statements of their own, BEFORE the fold: a lock taken inside it
// returns the LATEST row against an older event snapshot, moving last_ledger above missing rows.
func lockSEP41RollupRow(ctx context.Context, tx *sql.Tx, contractID string) error {
	const ensure = `
        INSERT INTO sep41_supply_rollup (contract_id)
        VALUES ($1)
        ON CONFLICT (contract_id) DO NOTHING
    `
	if _, err := tx.ExecContext(ctx, ensure, contractID); err != nil {
		return fmt.Errorf("ensure rollup row: %w", err)
	}
	const lock = `
        SELECT 1
          FROM sep41_supply_rollup
         WHERE contract_id = $1
           FOR UPDATE
    `
	var one int
	if err := tx.QueryRowContext(ctx, lock, contractID).Scan(&one); err != nil {
		return fmt.Errorf("lock rollup row: %w", err)
	}
	return nil
}

// SEP41RollupCheckpoint is the worker-owned fold state of one sep41_supply_rollup row (per-kind running totals up to LastLedger), excluding the separately seeded genesis-baseline columns.
type SEP41RollupCheckpoint struct {
	ContractID string
	Fold       SEP41KindTotals
	LastLedger uint32
}

// ListSEP41RollupCheckpoints returns the worker-owned fold state of every sep41_supply_rollup row, or only contractIDs when non-empty; the checkpoint side of the derived-checkpoint reconcile.
func (s *Store) ListSEP41RollupCheckpoints(ctx context.Context, contractIDs []string) ([]SEP41RollupCheckpoint, error) {
	const base = `
        SELECT contract_id, mint_total::text, burn_total::text, clawback_total::text, last_ledger
          FROM sep41_supply_rollup
    `
	var (
		rows *sql.Rows
		err  error
	)
	if len(contractIDs) == 0 {
		rows, err = s.db.QueryContext(ctx, base+` ORDER BY contract_id`)
	} else {
		rows, err = s.db.QueryContext(ctx, base+` WHERE contract_id = ANY($1) ORDER BY contract_id`, contractIDs)
	}
	if err != nil {
		return nil, fmt.Errorf("timescale: ListSEP41RollupCheckpoints: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SEP41RollupCheckpoint
	for rows.Next() {
		var cid, mintRaw, burnRaw, clawbackRaw string
		var lastLedger int64
		if err := rows.Scan(&cid, &mintRaw, &burnRaw, &clawbackRaw, &lastLedger); err != nil {
			return nil, fmt.Errorf("timescale: ListSEP41RollupCheckpoints scan: %w", err)
		}
		fold, perr := parseSEP41Totals(mintRaw, burnRaw, clawbackRaw)
		if perr != nil {
			return nil, perr
		}
		out = append(out, SEP41RollupCheckpoint{ContractID: cid, Fold: fold, LastLedger: uint32(lastLedger)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ListSEP41RollupCheckpoints rows: %w", err)
	}
	return out, nil
}

// SEP41SupplyEventKindResum is the authoritative per-kind re-sum of sep41_supply_events for contractID up to asOfLedger, the truth side of the derived-checkpoint reconcile; it sums the exact PG rows the checkpoint folds.
func (s *Store) SEP41SupplyEventKindResum(ctx context.Context, contractID string, asOfLedger uint32, statementTimeout time.Duration) (SEP41KindTotals, error) {
	if contractID == "" {
		return SEP41KindTotals{}, errors.New("timescale: SEP41SupplyEventKindResum: empty contractID")
	}
	if statementTimeout <= 0 {
		return SEP41KindTotals{}, fmt.Errorf("timescale: SEP41SupplyEventKindResum: statementTimeout must be > 0, got %s", statementTimeout)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return SEP41KindTotals{}, fmt.Errorf("timescale: SEP41SupplyEventKindResum begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Tx-scoped timeout so PG does not abort the full-history scan (as in CountRowsByLedger).
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%d'", statementTimeout.Milliseconds())); err != nil {
		return SEP41KindTotals{}, fmt.Errorf("timescale: SEP41SupplyEventKindResum SET: %w", err)
	}
	// Same genesis floor as the fast path; unfloored, verify-rollup would compare two copies of
	// the same double-count and report green.
	cp, _, cerr := s.sep41RollupCheckpoint(ctx, contractID)
	if cerr != nil {
		return SEP41KindTotals{}, cerr
	}
	const q = `
        SELECT
            COALESCE(sum(amount) FILTER (WHERE event_kind = 'mint'),     0)::text AS mint_total,
            COALESCE(sum(amount) FILTER (WHERE event_kind = 'burn'),     0)::text AS burn_total,
            COALESCE(sum(amount) FILTER (WHERE event_kind = 'clawback'), 0)::text AS clawback_total
          FROM sep41_supply_events
         WHERE contract_id = $1
           AND ledger      >= $2
           AND ledger      <= $3
    `
	var mintRaw, burnRaw, clawbackRaw string
	if err := tx.QueryRowContext(ctx, q, contractID, int(sep41SorobanFloor(cp)), int(asOfLedger)).Scan(&mintRaw, &burnRaw, &clawbackRaw); err != nil {
		return SEP41KindTotals{}, fmt.Errorf("timescale: SEP41SupplyEventKindResum %s@%d: %w", contractID, asOfLedger, err)
	}
	return parseSEP41Totals(mintRaw, burnRaw, clawbackRaw)
}

// ResetSEP41SupplyRollupFold rebuilds the worker-owned fold columns of sep41_supply_rollup from zero over re-derived history (nil/empty resets every row), since the worker only folds ledger > last_ledger.
func (s *Store) ResetSEP41SupplyRollupFold(ctx context.Context, contractIDs []string) (int64, error) {
	ids := contractIDs
	if len(ids) == 0 {
		cps, err := s.ListSEP41RollupCheckpoints(ctx, nil)
		if err != nil {
			return 0, fmt.Errorf("timescale: ResetSEP41SupplyRollupFold: %w", err)
		}
		ids = make([]string, len(cps))
		for i, cp := range cps {
			ids[i] = cp.ContractID
		}
	}
	var (
		refolded int64
		failed   []string
		errs     []error
	)
	for i, id := range ids {
		if err := ctx.Err(); err != nil {
			failed = append(failed, ids[i:]...)
			errs = append(errs, err)
			break
		}
		ok, err := s.refoldSEP41RollupRow(ctx, id)
		switch {
		case err != nil:
			failed = append(failed, id)
			errs = append(errs, err)
		case ok:
			refolded++
		}
	}
	if len(failed) == 0 {
		return refolded, nil
	}
	zeroed, zerr := zeroSEP41RollupFold(ctx, s.db, failed)
	if zerr != nil {
		return refolded, fmt.Errorf("timescale: ResetSEP41SupplyRollupFold: %d contract(s) not re-folded and the zeroing fallback failed, so they may still serve a stale fold: %w",
			len(failed), errors.Join(append(errs, zerr)...))
	}
	return refolded + zeroed, fmt.Errorf("timescale: ResetSEP41SupplyRollupFold: %d contract(s) zeroed instead of re-folded (served exactly via the full-sum read until the worker re-folds them): %w",
		len(failed), errors.Join(errs...))
}

// refoldSEP41RollupRow zeroes one contract's fold and re-folds it in the same txn; the UPDATE locks the row first ([lockSEP41RollupRow]).
// Reports false for a contract with no rollup row.
func (s *Store) refoldSEP41RollupRow(ctx context.Context, contractID string) (bool, error) {
	tx, err := beginSEP41RollupTx(ctx, s.db)
	if err != nil {
		return false, fmt.Errorf("%s: %w", contractID, err)
	}
	defer func() { _ = tx.Rollback() }()
	const zero = `
        UPDATE sep41_supply_rollup
           SET mint_total = 0, burn_total = 0, clawback_total = 0,
               last_ledger = 0, bound_ledger = NULL, bound_observed_at = NULL, updated_at = now()
         WHERE contract_id = $1
    `
	res, err := tx.ExecContext(ctx, zero, contractID)
	if err != nil {
		return false, fmt.Errorf("%s: zero: %w", contractID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if _, _, _, err := foldSEP41RollupLocked(ctx, tx, contractID); err != nil {
		return false, fmt.Errorf("%s: %w", contractID, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("%s: commit: %w", contractID, err)
	}
	return true, nil
}

// sep41RollupZeroFallbackTimeout bounds the zeroing fallback, which runs detached so a cancelled reset still clears a stale fold.
const sep41RollupZeroFallbackTimeout = time.Minute

func zeroSEP41RollupFold(ctx context.Context, db *sql.DB, contractIDs []string) (int64, error) {
	zctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sep41RollupZeroFallbackTimeout)
	defer cancel()
	const q = `
        UPDATE sep41_supply_rollup
           SET mint_total = 0, burn_total = 0, clawback_total = 0,
               last_ledger = 0, bound_ledger = NULL, bound_observed_at = NULL, updated_at = now()
         WHERE contract_id = ANY($1)
    `
	res, err := db.ExecContext(zctx, q, contractIDs)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
