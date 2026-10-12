package timescale

import (
	"context"
	"errors"
	"fmt"
)

// ProtocolContract is the read-side projection of one protocol_contracts row, a factory-descended child contract for a gated decoder.
type ProtocolContract struct {
	Source      string
	ContractID  string
	FactoryID   string
	FirstLedger uint32 // 0 when the seed source didn't carry it
}

// UpsertProtocolContract records (or refreshes) a factory-descended child contract for a gated source.
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

// ProtocolContractIndex returns a contract_id to source map over every registered protocol contract, backing the explorer's contract-attribution overlay.
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

// ListProtocolContracts returns every registered child contract for source as full rows, ordered by first_ledger then contract_id (NULL first_ledger maps to 0 and sorts last), backing GET /v1/protocols/{name}.
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

// projectionContractColumn maps a protocol source to the (table, column) in its PROJECTED table
// holding the per-instance contract id, the fallback roster for protocols not seeded into
// protocol_contracts. Sources without one (sdex is op-keyed, soroswap uses soroswap_pairs, blend
// uses the registry) return ok=false. Table and column are hard-coded, never from the request.
//
// aquarius_liquidity carries the emitting pool contract_id plus the pool's token identities;
// treating it as pair-keyed would read 0 contracts for the most active AMM.
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
		// aquarius_liquidity carries the pool's token identities (topics),
		// so a roster built from it always has a PoolTokens pair to render;
		// every Aquarius pool emits a deposit at creation, so it is complete.
		return "aquarius_liquidity", "contract_id", true
	}
	return "", "", false
}

// ListSourceContractsFromProjection returns the DISTINCT contract ids a source's projected table holds, the roster fallback for protocols absent from protocol_contracts; nil for sources without a per-contract table, and capped.
func (s *Store) ListSourceContractsFromProjection(ctx context.Context, source string) ([]string, error) {
	// Oracle sources (band/reflector-*/redstone) share ONE projected table,
	// oracle_updates, so the generic unfiltered DISTINCT below would return
	// EVERY oracle's contracts for each source. Their pinned contracts emit
	// into oracle_updates.contract_id — scope by the source column.
	switch source {
	case "band", "reflector-cex", "reflector-dex", "reflector-fx", "redstone":
		return s.listContractsFilteredBySource(ctx, "oracle_updates", "contract_id", source)
	}
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

// sourceContractCountQuery maps a protocol source to the COUNT yielding its TRUE contract total
// without enumerating it. ok=false for every other source, whose roster IS its count.
//
// sorocredit deploys one Collateral-<uuid> child contract per opened position, far beyond the
// enumerating paths' LIMIT 5000. Each child is announced exactly once by the
// NewCollateralContract event credit_positions holds, and the decoder claims a child's events
// only once announced, so DISTINCT over that column is the whole child set. The query is
// hard-coded, never built from the request.
func sourceContractCountQuery(source string) (query string, ok bool) {
	if source == "sorocredit" {
		return `SELECT count(DISTINCT collateral_contract) FROM credit_positions`, true
	}
	return "", false
}

// CountSourceContracts returns a source's exact contract total WITHOUT enumerating it, for protocols whose contracts exceed the roster LIMIT 5000; ok=false with no query issued when the source has no count path.
func (s *Store) CountSourceContracts(ctx context.Context, source string) (int64, bool, error) {
	q, ok := sourceContractCountQuery(source)
	if !ok {
		return 0, false, nil
	}
	var n int64
	if err := s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, false, fmt.Errorf("timescale: CountSourceContracts %s: %w", source, err)
	}
	return n, true, nil
}

// listContractsFilteredBySource is the source-SCOPED projection roster, for tables holding more
// than one source's rows (oracle_updates). `table` and `column` are caller constants, never the
// request; `source` binds as a parameter. Returns the DISTINCT contract ids that source emitted,
// capped like the unscoped path.
func (s *Store) listContractsFilteredBySource(ctx context.Context, table, column, source string) ([]string, error) {
	// #nosec G201 -- table+column are hard-coded caller constants, never the
	// request; source is a bind parameter.
	q := fmt.Sprintf(`SELECT DISTINCT %s FROM %s WHERE source = $1 AND %s IS NOT NULL LIMIT 5000`, column, table, column)
	rows, err := s.db.QueryContext(ctx, q, source)
	if err != nil {
		return nil, fmt.Errorf("timescale: listContractsFilteredBySource %s: %w", source, err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]string, 0, 8)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("timescale: listContractsFilteredBySource %s scan: %w", source, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: listContractsFilteredBySource %s rows: %w", source, err)
	}
	return out, nil
}

// LoadProtocolContracts returns every registered child contract C-strkey for source as a flat slice to warm a gated decoder's contractid.Registry at startup; empty (not an error) when the source has no rows.
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
