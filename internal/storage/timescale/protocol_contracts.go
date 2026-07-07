package timescale

import (
	"context"
	"errors"
	"fmt"
)

// ProtocolContract is the read-side projection of one protocol_contracts
// row — a factory-descended child contract for a gated decoder (ADR-0035).
type ProtocolContract struct {
	Source      string
	ContractID  string
	FactoryID   string
	FirstLedger uint32 // 0 when the seed source didn't carry it
}

// UpsertProtocolContract records (or refreshes) a factory-descended child
// contract for a gated source. Idempotent on (source, contract_id) — the
// live indexer's factory-creation handler calls this on every creation
// event without checking whether the row already exists, and the
// `seed-protocol-contracts` genesis walk re-upserts the same set.
//
// firstLedger may be 0 (unknown); it's stored as NULL in that case so a
// later seed that DOES know the ledger can fill it without being masked by
// a 0 sentinel.
func (s *Store) UpsertProtocolContract(ctx context.Context, source, contractID, factoryID string, firstLedger uint32) error {
	if source == "" || contractID == "" {
		return errors.New("timescale: UpsertProtocolContract: empty source or contract_id")
	}
	if factoryID == "" {
		return fmt.Errorf("timescale: UpsertProtocolContract %s/%s: empty factory_id", source, contractID)
	}
	var ledgerArg any
	if firstLedger != 0 {
		ledgerArg = int64(firstLedger)
	}
	const q = `
		INSERT INTO protocol_contracts (source, contract_id, factory_id, first_ledger, observed_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (source, contract_id) DO UPDATE SET
		    factory_id   = EXCLUDED.factory_id,
		    first_ledger = COALESCE(protocol_contracts.first_ledger, EXCLUDED.first_ledger),
		    observed_at  = EXCLUDED.observed_at
	`
	if _, err := s.db.ExecContext(ctx, q, source, contractID, factoryID, ledgerArg); err != nil {
		return fmt.Errorf("timescale: UpsertProtocolContract %s/%s: %w", source, contractID, err)
	}
	return nil
}

// LoadProtocolContracts returns every child contract C-strkey registered
// for source, as a flat slice. Used by the indexer / projector / audit
// commands at startup to warm a gated decoder's contractid.Registry.
//
// Returns an empty slice (not nil + error) when the source has no rows —
// the steady-state for a fresh deployment that hasn't run
// `stellarindex-ops seed-protocol-contracts -source <name>` yet. The gate
// then sees an empty registry and (correctly, per ADR-0035) drops every
// child event until seeded; running the genesis walk is a deploy
// precondition.
// ListProtocolContracts returns every registered child contract for
// source as full rows (contract + deploying factory + first-observed
// ledger), ordered by first_ledger then contract_id so the API serves
// a stable, chronologically-meaningful listing. The flat-ID
// LoadProtocolContracts above stays as the decoder-warmup seam; this
// richer projection backs GET /v1/protocols/{name}.
//
// first_ledger is NULL when the seeding path didn't know it; that maps
// to FirstLedger 0 here (and the NULLs sort last).
// ProtocolContractIndex returns a contract_id → source map over every
// registered protocol contract, regardless of source. Backs the explorer's
// contract-attribution overlay (the "this contract IS a Blend pool" hinge):
// the contracts directory + contract detail look each contract_id up in this
// map to tag it with its owning protocol. The table is small (factory-
// descended pools across all gated sources — tens to low-hundreds of rows),
// so loading it whole and mapping in-process is cheaper than a per-id query.
func (s *Store) ProtocolContractIndex(ctx context.Context) (map[string]string, error) {
	const q = `SELECT contract_id, source FROM protocol_contracts`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("timescale: ProtocolContractIndex: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]string, 256)
	for rows.Next() {
		var contractID, source string
		if err := rows.Scan(&contractID, &source); err != nil {
			return nil, fmt.Errorf("timescale: ProtocolContractIndex scan: %w", err)
		}
		out[contractID] = source
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ProtocolContractIndex rows: %w", err)
	}
	return out, nil
}

func (s *Store) ListProtocolContracts(ctx context.Context, source string) ([]ProtocolContract, error) {
	if source == "" {
		return nil, errors.New("timescale: ListProtocolContracts: empty source")
	}
	const q = `
		SELECT contract_id, factory_id, COALESCE(first_ledger, 0)
		  FROM protocol_contracts
		 WHERE source = $1
		 ORDER BY first_ledger ASC NULLS LAST, contract_id ASC
	`
	rows, err := s.db.QueryContext(ctx, q, source)
	if err != nil {
		return nil, fmt.Errorf("timescale: ListProtocolContracts %s: %w", source, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]ProtocolContract, 0, 64)
	for rows.Next() {
		pc := ProtocolContract{Source: source}
		var firstLedger int64
		if err := rows.Scan(&pc.ContractID, &pc.FactoryID, &firstLedger); err != nil {
			return nil, fmt.Errorf("timescale: ListProtocolContracts %s scan: %w", source, err)
		}
		pc.FirstLedger = uint32(firstLedger) //nolint:gosec // ledger sequences fit uint32
		out = append(out, pc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ListProtocolContracts %s rows: %w", source, err)
	}
	return out, nil
}

// projectionContractColumn maps a protocol source to the (table, column) in
// its PROJECTED table that holds the per-instance contract id. Used as the
// fallback roster for protocols not seeded into protocol_contracts (only blend
// is today) — defindex/phoenix/comet/cctp/rozo/aquarius all have a per-contract
// projected table. sdex is op-keyed (no contract, N/A), soroswap uses
// soroswap_pairs, blend uses the registry — those return ok=false and fall back
// to their own path. The table+column are HARD-CODED here (never from the
// request), so the formatted query carries no injected SQL.
//
// Aquarius was previously listed as "pair-keyed, no per-contract column" and so
// its /v1/protocols/aquarius roster read 0 contracts despite being the most
// active AMM (14.9k events/24h, 300+ pools). The aquarius_reserves hypertable
// (migration 0089) added this session carries the emitting POOL contract_id, so
// aquarius now has a per-pool roster source (2026-07-07, #91).
func projectionContractColumn(source string) (table, column string, ok bool) {
	switch source {
	case "defindex":
		return "defindex_flows", "contract_id", true
	case "cctp":
		return "cctp_events", "contract_id", true
	case "rozo":
		return "rozo_events", "contract_id", true
	case "phoenix":
		return "phoenix_liquidity", "pool", true
	case "comet":
		return "comet_liquidity", "contract_id", true
	case "aquarius":
		return "aquarius_reserves", "contract_id", true
	}
	return "", "", false
}

// ListSourceContractsFromProjection returns the DISTINCT contract ids a
// source's projected table holds — the fallback contract roster for protocols
// the protocol_contracts registry doesn't carry yet (the factory-enumeration is
// pending the team answer, but the decoder is already capturing the contracts'
// events). Returns nil for sources without a per-contract projected table
// (caller keeps its registry/pairs path). Capped so a pathological table can't
// blow the response.
func (s *Store) ListSourceContractsFromProjection(ctx context.Context, source string) ([]string, error) {
	table, column, ok := projectionContractColumn(source)
	if !ok {
		return nil, nil
	}
	// #nosec G201 -- table+column come from the hard-coded switch above, never
	// the request; source is only used as a known-key lookup.
	q := fmt.Sprintf(`SELECT DISTINCT %s FROM %s WHERE %s IS NOT NULL LIMIT 5000`, column, table, column)
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("timescale: ListSourceContractsFromProjection %s: %w", source, err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]string, 0, 128)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("timescale: ListSourceContractsFromProjection %s scan: %w", source, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ListSourceContractsFromProjection %s rows: %w", source, err)
	}
	return out, nil
}

func (s *Store) LoadProtocolContracts(ctx context.Context, source string) ([]string, error) {
	if source == "" {
		return nil, errors.New("timescale: LoadProtocolContracts: empty source")
	}
	const q = `
		SELECT contract_id
		  FROM protocol_contracts
		 WHERE source = $1
	`
	rows, err := s.db.QueryContext(ctx, q, source)
	if err != nil {
		return nil, fmt.Errorf("timescale: LoadProtocolContracts %s: %w", source, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]string, 0, 64)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("timescale: LoadProtocolContracts %s scan: %w", source, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: LoadProtocolContracts %s rows: %w", source, err)
	}
	return out, nil
}
