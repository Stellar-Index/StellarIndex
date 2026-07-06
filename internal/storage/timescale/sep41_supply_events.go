package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/lib/pq"
)

// SEP41EventKind discriminates the three supply-affecting SEP-41
// event variants. Stable string values matching the migration's
// CHECK constraint and `internal/canonical/discovery`'s symbol
// names.
type SEP41EventKind string

const (
	SEP41EventMint     SEP41EventKind = "mint"
	SEP41EventBurn     SEP41EventKind = "burn"
	SEP41EventClawback SEP41EventKind = "clawback"
)

// IsValid reports whether k is one of the three supply-affecting
// kinds. Caller surfaces unknown kinds as a 400 / decode error
// (the observer in PR 2/4 enforces this at Decode time).
func (k SEP41EventKind) IsValid() bool {
	switch k {
	case SEP41EventMint, SEP41EventBurn, SEP41EventClawback:
		return true
	}
	return false
}

// SEP41SupplyEvent is one mint / burn / clawback event row.
// Mirrors the sep41_supply_events columns.
type SEP41SupplyEvent struct {
	ContractID string
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	// EventIndex is the contract event's index within its operation —
	// the per-event discriminator added to the PK by migration 0057
	// (F-1324) so multiple supply events from one op don't collide.
	EventIndex   uint32
	ObservedAt   time.Time
	Kind         SEP41EventKind
	Amount       *big.Int
	Counterparty string // empty when not present (reserved for future variants)
}

// InsertSEP41SupplyEvent appends one event row, idempotent on the
// (contract_id, ledger, tx_hash, op_index, observed_at, event_kind,
// event_index) PK (event_index + event_kind added by migration 0057,
// F-1324, so multiple supply events from one op don't collide).
// Re-running the indexer over the same range writes the same
// rows; ON CONFLICT DO NOTHING keeps the running sum
// monotonically correct across replays.
//
// Defensive: rejects empty ContractID / TxHash / nil Amount /
// invalid Kind before touching the DB.
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
	const q = `
        INSERT INTO sep41_supply_events (
            contract_id, ledger, tx_hash, op_index, observed_at,
            event_kind, event_index, amount, counterparty
        ) VALUES (
            $1, $2, $3, $4, $5,
            $6, $7, $8, $9
        )
        ON CONFLICT (contract_id, ledger, tx_hash, op_index, observed_at,
                     event_kind, event_index) DO NOTHING
    `
	var counterparty sql.NullString
	if e.Counterparty != "" {
		counterparty = sql.NullString{String: e.Counterparty, Valid: true}
	}
	_, err := s.db.ExecContext(ctx, q,
		e.ContractID, int(e.Ledger), e.TxHash, int(e.OpIndex), e.ObservedAt.UTC(),
		string(e.Kind), int(e.EventIndex), e.Amount.String(), counterparty,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertSEP41SupplyEvent %s@%d: %w", e.ContractID, e.Ledger, err)
	}
	return nil
}

// SEP41NetMintAtOrBefore returns the running supply sum for
// `contractID` at-or-before `asOfLedger`:
//
//	Σ amount where event_kind='mint'
//	  − Σ amount where event_kind IN ('burn', 'clawback')
//
// per ADR-0011 Algorithm 3. Returns a non-nil *big.Int (zero is
// the answer for a contract with no supply-affecting events
// observed yet) on success.
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

// SEP41KindTotals carries the per-kind running sums for one
// SEP-41 contract. Used by the storage reader to surface the
// algorithm's three components separately rather than as the
// pre-netted [SEP41NetMintAtOrBefore]: ADR-0011 Algorithm 3's
// SEP41SupplyComponents tracks them distinct (operators want to
// see clawback volume separately from voluntary burns for
// compliance dashboards).
type SEP41KindTotals struct {
	Mint     *big.Int
	Burn     *big.Int
	Clawback *big.Int
}

// SEP41KindTotalsAtOrBefore returns the per-kind sums for
// `contractID` at-or-before `asOfLedger`.
//
// Fast path (incident 2026-07-06, migration 0085). Reads the
// per-contract checkpoint from `sep41_supply_rollup` and adds only the
// live tail delta above it:
//
//	rollup(ledger ≤ last_ledger)  ⊕  Σ(last_ledger < ledger ≤ asOfLedger)
//
// — a bounded scan on the (contract_id, ledger DESC) index instead of
// aggregating the whole per-contract history on every call. The
// aggregator's rollup worker keeps `last_ledger` within one cadence of
// the tip, so the delta is a handful of ledgers. Before this the
// reader ran `Σ … FILTER (WHERE event_kind = …)` over ALL of a
// contract's rows; once `sep41_supply_events` grew to hundreds of
// millions of rows (the 2026-07-05 full-history re-derive) that full
// aggregate took minutes — and, because the hypertable is chunked by
// `observed_at` while the query bounds only contract_id + ledger, it
// could prune no chunks and scanned every one.
//
// Fallback path. When the contract has no rollup row yet (the worker
// hasn't folded it) OR the request ledger predates the checkpoint (a
// rare historical/backfill read), it computes the original full
// at-or-before aggregate. rollup ⊕ delta and the full aggregate return
// identical totals — the rollup sums exactly the rows the full query
// would (proof: the checkpoint folds ledger ≤ last_ledger and the delta
// folds last_ledger < ledger ≤ asOfLedger; their disjoint union is
// ledger ≤ asOfLedger).
//
// Genesis baseline (migration 0088, incident 2026-07-06). When the contract
// has a SEEDED pre-Soroban baseline (genesis_baseline_ledger IS NOT NULL) and
// asOfLedger is at-or-above that boundary, the per-kind pre-Soroban totals are
// ADDED to the Soroban-era totals so the result is LIFETIME supply. A classic
// asset's SAC-wrapper was largely issued before Soroban existed; those mints
// live only in the ClickHouse lake below ledger [clickhouse.SorobanGenesisLedger]
// and are seeded once via `stellarindex-ops supply seed-sep41-genesis`. Tokens
// with no pre-genesis flows carry a zero baseline, so their served total is
// unchanged (no double-count). The baseline slice and the Soroban-era slice are
// a disjoint ledger partition, so summing them cannot double-count. For a
// historical read strictly below the baseline boundary the genesis is NOT added
// (the pre-Soroban answer would be a ledger-bounded subset the seed doesn't
// carry) — the aggregator always reads at the chain tip, so this affects only
// rare backfill reads.
//
// Each component is non-nil; zero is a valid answer for a contract with
// no events of that kind observed yet (e.g. a token that's never been
// clawed back returns Clawback=0).
func (s *Store) SEP41KindTotalsAtOrBefore(ctx context.Context, contractID string, asOfLedger uint32) (SEP41KindTotals, error) {
	cp, ok, err := s.sep41RollupCheckpoint(ctx, contractID)
	if err != nil {
		return SEP41KindTotals{}, err
	}

	// ─── Soroban-era totals (ledger ≥ SorobanGenesisLedger, from PG) ───
	var soroban SEP41KindTotals
	switch {
	case !ok || cp.lastLedger > asOfLedger:
		// No checkpoint yet, or the checkpoint is AHEAD of the requested
		// ledger (a historical read below the watermark) — the rollup can't
		// be subtracted back, so take the exact full aggregate.
		soroban, err = s.sep41KindTotalsFullSum(ctx, contractID, asOfLedger)
		if err != nil {
			return SEP41KindTotals{}, err
		}
	default:
		// Checkpoint ≤ asOfLedger: add only the live tail delta above it.
		delta, derr := s.sep41KindTotalsRange(ctx, contractID, cp.lastLedger, asOfLedger)
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

// SEP41GenesisBaselineSeeded reports whether a pre-Soroban genesis baseline has
// been seeded for the contract (genesis_baseline_ledger IS NOT NULL, migration
// 0088). The SEP-41 computer uses it to distinguish a legitimately-missing
// baseline (negative Soroban-era total is expected until the operator seeds it
// — benign `missing_baseline` outcome) from a genuine data inconsistency
// (baseline seeded and total STILL negative — physically impossible → paging
// `compute_error`). Returns false when no rollup row exists yet.
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

// UpsertSEP41GenesisBaseline seeds (or re-seeds) a contract's pre-Soroban
// per-kind opening balance into sep41_supply_rollup (migration 0088, incident
// 2026-07-06). It SETS the genesis columns (not add) so re-running is
// idempotent — the CH-lake pre-genesis sum is deterministic — and never
// double-counts. It leaves the worker-owned Soroban-era rollup columns
// (mint_total / burn_total / clawback_total / last_ledger) untouched: the
// INSERT arm gives them their column DEFAULT (0), the ON CONFLICT arm updates
// only the genesis columns, so the seed and the rollup worker coexist on the
// same row regardless of which runs first.
//
// baselineLedger is the EXCLUSIVE upper ledger bound of the seeded sum
// (typically clickhouse.SorobanGenesisLedger); it is stored so the reader can
// gate the baseline on `asOfLedger >= baselineLedger` and so a re-seed is
// auditable (with genesis_seeded_at). i128-safe — the three totals are Postgres
// NUMERIC (ADR-0003).
func (s *Store) UpsertSEP41GenesisBaseline(ctx context.Context, contractID string, genesis SEP41KindTotals, baselineLedger uint32) error {
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
	const q = `
        INSERT INTO sep41_supply_rollup
            (contract_id, genesis_mint_total, genesis_burn_total, genesis_clawback_total,
             genesis_baseline_ledger, genesis_seeded_at)
        VALUES ($1, $2, $3, $4, $5, now())
        ON CONFLICT (contract_id) DO UPDATE SET
            genesis_mint_total      = EXCLUDED.genesis_mint_total,
            genesis_burn_total      = EXCLUDED.genesis_burn_total,
            genesis_clawback_total  = EXCLUDED.genesis_clawback_total,
            genesis_baseline_ledger = EXCLUDED.genesis_baseline_ledger,
            genesis_seeded_at       = now()
    `
	if _, err := s.db.ExecContext(ctx, q,
		contractID, genesis.Mint.String(), genesis.Burn.String(), genesis.Clawback.String(),
		int(baselineLedger),
	); err != nil {
		return fmt.Errorf("timescale: UpsertSEP41GenesisBaseline %s: %w", contractID, err)
	}
	return nil
}

// sep41KindTotalsFullSum is the original full per-contract aggregate,
// bounded at-or-before asOfLedger. The correctness backstop the fast
// path falls back to; kept identical so rollup ⊕ delta is provably the
// same number.
func (s *Store) sep41KindTotalsFullSum(ctx context.Context, contractID string, asOfLedger uint32) (SEP41KindTotals, error) {
	const q = `
        SELECT
            COALESCE(sum(amount) FILTER (WHERE event_kind = 'mint'),     0)::text AS mint_total,
            COALESCE(sum(amount) FILTER (WHERE event_kind = 'burn'),     0)::text AS burn_total,
            COALESCE(sum(amount) FILTER (WHERE event_kind = 'clawback'), 0)::text AS clawback_total
          FROM sep41_supply_events
         WHERE contract_id = $1
           AND ledger      <= $2
    `
	var mintRaw, burnRaw, clawbackRaw string
	if err := s.db.QueryRowContext(ctx, q, contractID, int(asOfLedger)).Scan(&mintRaw, &burnRaw, &clawbackRaw); err != nil {
		return SEP41KindTotals{}, fmt.Errorf("timescale: SEP41KindTotalsAtOrBefore %s@%d: %w", contractID, asOfLedger, err)
	}
	return parseSEP41Totals(mintRaw, burnRaw, clawbackRaw)
}

// sep41KindTotalsRange sums the delta over the half-open ledger window
// (afterLedger, asOfLedger]. The `ledger > afterLedger` lower bound
// walks only the tail of the (contract_id, ledger DESC) index, so the
// live delta above a fresh checkpoint is cheap regardless of history
// depth.
func (s *Store) sep41KindTotalsRange(ctx context.Context, contractID string, afterLedger, asOfLedger uint32) (SEP41KindTotals, error) {
	const q = `
        SELECT
            COALESCE(sum(amount) FILTER (WHERE event_kind = 'mint'),     0)::text AS mint_total,
            COALESCE(sum(amount) FILTER (WHERE event_kind = 'burn'),     0)::text AS burn_total,
            COALESCE(sum(amount) FILTER (WHERE event_kind = 'clawback'), 0)::text AS clawback_total
          FROM sep41_supply_events
         WHERE contract_id = $1
           AND ledger      >  $2
           AND ledger      <= $3
    `
	var mintRaw, burnRaw, clawbackRaw string
	if err := s.db.QueryRowContext(ctx, q, contractID, int(afterLedger), int(asOfLedger)).Scan(&mintRaw, &burnRaw, &clawbackRaw); err != nil {
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

// MinSEP41ComponentLedger returns MAX(ledger) of sep41_supply_events
// rows for `contractID` at-or-before `asOfLedger`. There's only one
// component-feeding table for SEP-41 supply (event sums), so the
// "min across components" semantics reduce to just the table's max.
// Zero when the contract has no events yet — gate-skip signal.
// F-1236 (codex audit-2026-05-12).
func (s *Store) MinSEP41ComponentLedger(ctx context.Context, contractID string, asOfLedger uint32) (uint32, error) {
	const q = `
		SELECT COALESCE(MAX(ledger), 0)
		  FROM sep41_supply_events
		 WHERE contract_id = $1 AND ledger <= $2
	`
	var ledger uint32
	if err := s.db.QueryRowContext(ctx, q, contractID, int(asOfLedger)).Scan(&ledger); err != nil {
		return 0, fmt.Errorf("timescale: MinSEP41ComponentLedger %s@%d: %w", contractID, asOfLedger, err)
	}
	return ledger, nil
}

func parseSEP41Numeric(raw, label string) (*big.Int, error) {
	v, ok := new(big.Int).SetString(raw, 10)
	if !ok {
		return nil, fmt.Errorf("timescale: parse %s %q", label, raw)
	}
	return v, nil
}

// InsertSEP41SupplyEventBatch persists rows via a single multi-row
// INSERT .. ON CONFLICT DO NOTHING (the batch sibling of
// InsertSEP41SupplyEvent — added for the 2026-07-05 full-history
// re-derive, where per-row round-trips capped writes at ~520/s).
// Rows are validated with the same rules as the single-row path.
func (s *Store) InsertSEP41SupplyEventBatch(ctx context.Context, rows []SEP41SupplyEvent) error {
	if len(rows) == 0 {
		return nil
	}
	for i := range rows {
		e := &rows[i]
		if e.ContractID == "" {
			return fmt.Errorf("timescale: InsertSEP41SupplyEventBatch: row %d empty ContractID", i)
		}
		if e.TxHash == "" {
			return fmt.Errorf("timescale: InsertSEP41SupplyEventBatch: row %d empty TxHash", i)
		}
		if e.Amount == nil {
			return fmt.Errorf("timescale: InsertSEP41SupplyEventBatch: row %d nil Amount", i)
		}
		if !e.Kind.IsValid() {
			return fmt.Errorf("timescale: InsertSEP41SupplyEventBatch: row %d invalid Kind %q", i, e.Kind)
		}
	}

	const ncols = 9
	var sb strings.Builder
	sb.WriteString(`
        INSERT INTO sep41_supply_events (
            contract_id, ledger, tx_hash, op_index, event_index,
            observed_at, event_kind, amount, counterparty
        ) VALUES `)
	args := make([]any, 0, ncols*len(rows))
	for i := range rows {
		if i > 0 {
			sb.WriteString(", ")
		}
		base := i * ncols
		fmt.Fprintf(&sb,
			"($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			base+1, base+2, base+3, base+4, base+5,
			base+6, base+7, base+8, base+9,
		)
		e := &rows[i]
		args = append(args,
			e.ContractID,
			int64(e.Ledger),
			e.TxHash,
			int16(e.OpIndex),
			int16(e.EventIndex),
			e.ObservedAt.UTC(),
			string(e.Kind),
			e.Amount.String(),
			sql.NullString{String: e.Counterparty, Valid: e.Counterparty != ""},
		)
	}
	sb.WriteString(` ON CONFLICT (contract_id, ledger, tx_hash, op_index, observed_at, event_kind, event_index) DO NOTHING`)

	if _, err := s.db.ExecContext(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("timescale: InsertSEP41SupplyEventBatch (%d rows): %w", len(rows), err)
	}
	return nil
}

// SEP41RollupAdvance reports what one AdvanceSEP41SupplyRollup pass
// folded, for the worker's metrics + logging.
type SEP41RollupAdvance struct {
	ContractID string
	FromLedger uint32 // checkpoint ledger before the pass
	ToLedger   uint32 // checkpoint ledger after the pass
	Advanced   bool   // true when newly-settled rows were folded in
}

// AdvanceSEP41SupplyRollup folds a contract's newly-SETTLED
// sep41_supply_events into its sep41_supply_rollup checkpoint — the
// incremental maintainer that keeps the SEP41KindTotalsAtOrBefore fast
// path cheap (migration 0085, incident 2026-07-06). This is the ONLY
// writer of sep41_supply_rollup.
//
// It sums only rows with `ledger > last_ledger` AND strictly below the
// contract's current max ledger. The `< max(ledger)` guard defers the
// tip ledger — which may still be mid-write in the indexer (a separate
// process) — so a partially-written ledger is never half-folded into
// the running total (that would permanently undercount, since the
// reader's delta only picks up `ledger > last_ledger`). The tip folds
// on a later pass, once a higher ledger exists to prove it settled;
// meanwhile the reader's live delta covers it, so nothing is lost.
//
// Idempotent + monotonic: re-running with no newly-settled rows is a
// no-op (zero delta, unchanged last_ledger); the per-kind totals only
// ever grow by the summed delta. i128-safe — amounts are summed and
// accumulated in Postgres NUMERIC (ADR-0003).
//
// NOTE: a re-derive that rewrites sep41_supply_events history BELOW an
// existing checkpoint must reset the fold columns first so the worker
// re-folds from zero; the incremental watermark cannot see edits it
// already passed. `ch-rebuild -sep41 -write` does this automatically via
// [Store.ResetSEP41SupplyRollupFold] (which preserves the genesis
// baseline, unlike a bare `TRUNCATE sep41_supply_rollup`).
func (s *Store) AdvanceSEP41SupplyRollup(ctx context.Context, contractID string) (SEP41RollupAdvance, error) {
	if contractID == "" {
		return SEP41RollupAdvance{}, errors.New("timescale: AdvanceSEP41SupplyRollup: empty contractID")
	}
	cp, _, err := s.sep41RollupCheckpoint(ctx, contractID)
	if err != nil {
		return SEP41RollupAdvance{}, err
	}
	fromLedger := cp.lastLedger

	// One statement:
	//   bound.mx — the contract's current max ledger; we fold strictly
	//              below it so the (possibly mid-write) tip is deferred.
	//   delta    — the settled tail sum over (from_ledger, mx).
	//   UPSERT   — add the delta into the running totals and move
	//              last_ledger to the max ledger actually folded
	//              (COALESCE back to from_ledger when nothing settled).
	const q = `
        WITH bound AS (
            SELECT COALESCE(max(ledger), 0) AS mx
              FROM sep41_supply_events
             WHERE contract_id = $1
        ),
        delta AS (
            SELECT
                COALESCE(sum(e.amount) FILTER (WHERE e.event_kind = 'mint'),     0) AS d_mint,
                COALESCE(sum(e.amount) FILTER (WHERE e.event_kind = 'burn'),     0) AS d_burn,
                COALESCE(sum(e.amount) FILTER (WHERE e.event_kind = 'clawback'), 0) AS d_clawback,
                COALESCE(max(e.ledger), $2)                                        AS to_ledger
              FROM sep41_supply_events e, bound
             WHERE e.contract_id = $1
               AND e.ledger       > $2
               AND e.ledger       < bound.mx
        )
        INSERT INTO sep41_supply_rollup
            (contract_id, mint_total, burn_total, clawback_total, last_ledger, updated_at)
        SELECT $1, d_mint, d_burn, d_clawback, to_ledger, now() FROM delta
        ON CONFLICT (contract_id) DO UPDATE SET
            mint_total     = sep41_supply_rollup.mint_total     + EXCLUDED.mint_total,
            burn_total     = sep41_supply_rollup.burn_total     + EXCLUDED.burn_total,
            clawback_total = sep41_supply_rollup.clawback_total + EXCLUDED.clawback_total,
            last_ledger    = EXCLUDED.last_ledger,
            updated_at     = now()
        RETURNING last_ledger
    `
	var toLedger int64
	if err := s.db.QueryRowContext(ctx, q, contractID, int(fromLedger)).Scan(&toLedger); err != nil {
		return SEP41RollupAdvance{}, fmt.Errorf("timescale: AdvanceSEP41SupplyRollup %s: %w", contractID, err)
	}
	return SEP41RollupAdvance{
		ContractID: contractID,
		FromLedger: fromLedger,
		ToLedger:   uint32(toLedger),
		Advanced:   uint32(toLedger) > fromLedger,
	}, nil
}

// ResetSEP41SupplyRollupFold zeroes the WORKER-OWNED fold columns
// (mint_total, burn_total, clawback_total, last_ledger) of
// sep41_supply_rollup so the aggregator's rollup worker
// ([Store.AdvanceSEP41SupplyRollup]) re-folds a re-derived
// sep41_supply_events history FROM ZERO instead of trusting a checkpoint
// that no longer matches the events beneath it. Incident 2026-07-06 follow-up.
//
// Why it exists. `ch-rebuild -sep41 -write` rewrites sep41_supply_events
// history BELOW an existing checkpoint. The worker only ever folds
// `ledger > last_ledger`, so it never re-examines the rewritten range —
// leaving the checkpoint stale in one of two ways:
//   - a FULL re-derive re-populates the whole history, but the checkpoint's
//     stale totals get the re-folded tail ADDED on top → served supply
//     double-counts (the KALE 2× served-value bug);
//   - a SCOPED recovery ADDS previously-missing rows at ledgers ≤ last_ledger,
//     which the worker's `> last_ledger` fold never sees → served undercount.
//
// Resetting the fold columns forces a clean re-fold over the corrected set.
//
// This is the automated form of the manual "TRUNCATE sep41_supply_rollup" the
// migration-0085 header prescribed — but it PRESERVES the migration-0088
// pre-Soroban genesis-baseline columns (genesis_mint_total / genesis_burn_total
// / genesis_clawback_total / genesis_baseline_ledger / genesis_seeded_at),
// which a bare TRUNCATE would drop. Those are seeded separately
// (`stellarindex-ops supply seed-sep41-genesis`, from the ClickHouse lake) and
// a Soroban-era re-derive must never wipe them.
//
// Scope:
//   - contractIDs nil/empty → FULL reset: every rollup row's fold columns (the
//     whole-table TRUNCATE-equivalent, for a full-history re-derive).
//   - contractIDs non-empty → SCOPED reset: only those contracts' rows (for a
//     `-contracts` scoped dropped-rows recovery).
//
// Correctness during the gap: a reset row's `last_ledger` returns to 0, so the
// reader ([Store.SEP41KindTotalsAtOrBefore]) serves the exact full-sum fallback
// until the worker re-folds it — supply stays correct throughout, just off the
// fast path for a cadence or two. Returns the number of rows reset.
func (s *Store) ResetSEP41SupplyRollupFold(ctx context.Context, contractIDs []string) (int64, error) {
	var (
		res sql.Result
		err error
	)
	if len(contractIDs) == 0 {
		const q = `
            UPDATE sep41_supply_rollup
               SET mint_total = 0, burn_total = 0, clawback_total = 0,
                   last_ledger = 0, updated_at = now()
        `
		res, err = s.db.ExecContext(ctx, q)
	} else {
		const q = `
            UPDATE sep41_supply_rollup
               SET mint_total = 0, burn_total = 0, clawback_total = 0,
                   last_ledger = 0, updated_at = now()
             WHERE contract_id = ANY($1)
        `
		res, err = s.db.ExecContext(ctx, q, pq.Array(contractIDs))
	}
	if err != nil {
		return 0, fmt.Errorf("timescale: ResetSEP41SupplyRollupFold: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
