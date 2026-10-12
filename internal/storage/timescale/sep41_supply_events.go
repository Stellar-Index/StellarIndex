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
	// Generation-guarded corrective upsert (migration 0110): a corrected
	// re-derive of the supply-event `amount` (or counterparty) lands in
	// place when its generation is >= the stored one; a live gen-0
	// replay can never revert it.
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
		delta, derr := s.sep41KindTotalsRange(ctx, contractID, cp.lastLedger, floor, asOfLedger)
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

// sep41RollupRow is the parsed sep41_supply_rollup row: the Soroban-era running
// totals + checkpoint ledger, plus the seeded pre-Soroban genesis baseline
// (migration 0088). genesisSeeded reflects genesis_baseline_ledger IS NOT NULL.
type sep41RollupRow struct {
	rollup        SEP41KindTotals
	lastLedger    uint32
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
               genesis_baseline_ledger
          FROM sep41_supply_rollup
         WHERE contract_id = $1
    `
	var mintRaw, burnRaw, clawbackRaw string
	var lastLedger int64
	var gMintRaw, gBurnRaw, gClawbackRaw string
	var genesisLedger sql.NullInt64
	err := s.db.QueryRowContext(ctx, q, contractID).Scan(
		&mintRaw, &burnRaw, &clawbackRaw, &lastLedger,
		&gMintRaw, &gBurnRaw, &gClawbackRaw, &genesisLedger,
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

// sep41SorobanFloor returns the lowest ledger the Soroban-era slice may include for a contract.
// The ClickHouse-seeded genesis baseline covers ledger < genesis_baseline_ledger, so the Postgres
// slice must START at that boundary or the overlap is summed twice. Nothing else enforces the
// partition, and sep41_supply_events does hold rows below it (CAP-67 replays classic movements as
// contract events); double-counted burns can push the net below zero.
//
// SEP41SupplyEventKindResum shares the floor, or verify-rollup would reconcile two copies of the
// same mistake and report green. Zero when no baseline is seeded: those rows are then the ONLY
// record of that history and must keep contributing.
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

// sep41KindTotalsRange sums the delta over (afterLedger, asOfLedger]. The `ledger > afterLedger`
// bound walks only the tail of the (contract_id, ledger DESC) index, so the delta above a fresh
// checkpoint is cheap regardless of history depth.
func (s *Store) sep41KindTotalsRange(ctx context.Context, contractID string, afterLedger, floorLedger, asOfLedger uint32) (SEP41KindTotals, error) {
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
    `
	var mintRaw, burnRaw, clawbackRaw string
	if err := s.db.QueryRowContext(ctx, q, contractID, int(afterLedger), int(floorLedger), int(asOfLedger)).Scan(&mintRaw, &burnRaw, &clawbackRaw); err != nil {
		return SEP41KindTotals{}, fmt.Errorf("timescale: sep41 kind-totals delta %s@(%d,%d]: %w", contractID, afterLedger, asOfLedger, err)
	}
	return parseSEP41Totals(mintRaw, burnRaw, clawbackRaw)
}

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

// sep41ObserverWatermark returns MAX(ledger) across ALL sep41_supply_events at-or-before
// asOfLedger, the producer's progress. It is memoized like [Store.classicObserverWatermark]: the
// value is identical for every contract in a tick and the TTL is far below the staleness threshold,
// so the cache cannot mask a stall.
func (s *Store) sep41ObserverWatermark(ctx context.Context, asOfLedger uint32) (uint32, error) {
	c := &s.sep41Watermark
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.asOf == asOfLedger && !c.fetchedAt.IsZero() && time.Since(c.fetchedAt) < classicWatermarkTTL {
		return c.ledger, nil
	}
	const q = `SELECT COALESCE(MAX(ledger), 0) FROM sep41_supply_events WHERE ledger <= $1`
	var watermark uint32
	if err := s.db.QueryRowContext(ctx, q, int(asOfLedger)).Scan(&watermark); err != nil {
		return 0, fmt.Errorf("timescale: sep41ObserverWatermark@%d: %w", asOfLedger, err)
	}
	c.asOf, c.ledger, c.fetchedAt = asOfLedger, watermark, time.Now()
	return watermark, nil
}

// validateSEP41SupplyEventRows is the row contract the multi-row
// sep41_supply_events writers enforce before touching the database, matching
// InsertSEP41SupplyEvent's guards: one bad row would otherwise fail a CHECK
// and abort every row in the statement. op names the caller in the error.
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

// sep41SupplyConflictKey is the sep41_supply_events ON CONFLICT identity
// (contract_id, ledger, tx_hash, op_index, observed_at, event_kind,
// event_index). observed_at is normalised to a UTC UnixNano so equal instants
// collapse to one key regardless of monotonic-clock / location.
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

// dedupeSEP41SupplyRows collapses rows colliding on the conflict key, keeping the LAST copy of each
// (latest redelivery) in first-seen order; see dedupeSEP41TransferRows. It allocates only when the
// batch has a duplicate.
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

// sep41RollupLockTimeout bounds how long a fold writer waits for another writer's lock on a
// sep41_supply_rollup row. The aggregator advances contracts sequentially and a seed or reset can
// hold a row through a cold full-history fold, so an unbounded wait would stall every later
// contract. A contended pass yields (an `error` outcome) and retries next cadence.
const sep41RollupLockTimeout = 10 * time.Second

// beginSEP41RollupTx opens a fold-writer transaction whose lock waits are
// bounded by [sep41RollupLockTimeout]. Deliberately no statement_timeout: a
// cold fold that cannot finish leaves the contract on the slower full-sum read
// path forever, which is the cost the fold exists to remove.
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

// foldSEP41RollupLocked adds a contract's newly-settled events into its fold,
// on a transaction that ALREADY holds the row lock ([lockSEP41RollupRow]).
// Returns the checkpoint ledger before and after, and whether the projector's
// cursor row was absent (so nothing could be folded).
func foldSEP41RollupLocked(ctx context.Context, tx *sql.Tx, contractID string) (fromLedger, toLedger int64, cursorAbsent bool, err error) {
	// One statement under the caller's lock:
	//   locked   - the boundary and floor. The row cannot change while locked and the snapshot
	//              postdates the lock, so boundary and summed events are one consistent view.
	//              MATERIALIZED evaluates it once.
	//   bound.mx - the contract's max ledger; folding strictly below defers the mid-write tip.
	//   settled  - the durable cursor; NULL when the projector never committed a cycle, which the
	//              delta treats as 0 (fold nothing).
	//   delta    - the settled tail sum over (from_ledger, settled bound), floored.
	//   UPDATE   - add the delta and move last_ledger to the max ledger folded.
	const q = `
        WITH locked AS MATERIALIZED (
            SELECT last_ledger                          AS from_ledger,
                   COALESCE(genesis_baseline_ledger, 0) AS floor_ledger
              FROM sep41_supply_rollup
             WHERE contract_id = $1
               FOR UPDATE
        ),
        bound AS (
            SELECT COALESCE(max(ledger), 0) AS mx
              FROM sep41_supply_events
             WHERE contract_id = $1
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
                (SELECT through IS NULL FROM settled)                              AS cursor_absent
              FROM sep41_supply_events e
             WHERE e.contract_id = $1
               AND e.ledger       > (SELECT from_ledger  FROM locked)
               AND e.ledger      >= (SELECT floor_ledger FROM locked)
               AND e.ledger       < (SELECT mx FROM bound)
               AND e.ledger      <= (SELECT COALESCE(through, 0) FROM settled)
        )
        UPDATE sep41_supply_rollup r
           SET mint_total     = r.mint_total     + d.d_mint,
               burn_total     = r.burn_total     + d.d_burn,
               clawback_total = r.clawback_total + d.d_clawback,
               last_ledger    = d.to_ledger,
               updated_at     = now()
          FROM delta d
         WHERE r.contract_id = $1
        RETURNING d.from_ledger, r.last_ledger, d.cursor_absent
    `
	if err := tx.QueryRowContext(ctx, q, contractID,
		sep41SupplyCursorSource, sep41SupplyCursorSub,
	).Scan(&fromLedger, &toLedger, &cursorAbsent); err != nil {
		return 0, 0, false, fmt.Errorf("fold: %w", err)
	}
	return fromLedger, toLedger, cursorAbsent, nil
}

// lockSEP41RollupRow materialises a contract's sep41_supply_rollup row and takes its write lock, in
// statements of their own, ahead of the fold.
//
// The lock must not be taken inside the folding statement. Under READ COMMITTED the statement
// snapshot is fixed at start, but `SELECT ... FOR UPDATE` returns the LATEST committed row. A fold
// locking inside itself could read last_ledger = 0 from a reset committed after its snapshot while
// summing events as they stood before it, and move last_ledger back above the missing rows.
// Locking first makes the snapshot see every earlier reset and event write; a later reset waits
// and re-folds next cadence.
//
// ON CONFLICT DO NOTHING waits out an in-progress UPDATE of the row, harmless because it precedes
// the fold's snapshot.
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

// refoldSEP41RollupRow zeroes one contract's fold and re-folds it in the same
// transaction. The UPDATE takes the row lock in a statement of its own, ahead
// of the fold ([lockSEP41RollupRow] says why). Reports false for a contract
// with no rollup row: there is no fold to invalidate.
func (s *Store) refoldSEP41RollupRow(ctx context.Context, contractID string) (bool, error) {
	tx, err := beginSEP41RollupTx(ctx, s.db)
	if err != nil {
		return false, fmt.Errorf("%s: %w", contractID, err)
	}
	defer func() { _ = tx.Rollback() }()
	const zero = `
        UPDATE sep41_supply_rollup
           SET mint_total = 0, burn_total = 0, clawback_total = 0,
               last_ledger = 0, updated_at = now()
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

// sep41RollupZeroFallbackTimeout bounds the zeroing fallback, which runs
// detached from the caller's ctx: a cancelled reset must still not leave a
// stale fold behind.
const sep41RollupZeroFallbackTimeout = time.Minute

func zeroSEP41RollupFold(ctx context.Context, db *sql.DB, contractIDs []string) (int64, error) {
	zctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sep41RollupZeroFallbackTimeout)
	defer cancel()
	const q = `
        UPDATE sep41_supply_rollup
           SET mint_total = 0, burn_total = 0, clawback_total = 0,
               last_ledger = 0, updated_at = now()
         WHERE contract_id = ANY($1)
    `
	res, err := db.ExecContext(zctx, q, contractIDs)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
