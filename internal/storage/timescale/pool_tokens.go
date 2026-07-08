package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
)

// poolTokensRowLimit bounds every PoolTokens DISTINCT scan. It caps the
// per-pool token fan-out (pools × tokens) so a runaway table can't stall
// the detail page — the same defensive bound ListSourceContractsFromProjection
// uses on its roster scan. Realistic pool counts (a few thousand across the
// densest AMM) × ≤4 tokens sit far below it.
const poolTokensRowLimit = 40000

// PoolTokens maps each of a pool-based protocol's contracts to the token
// contract C-strkeys it holds, sourced from the protocol's per-token
// liquidity / reserve table. It lets the /v1/protocols/{name} roster render
// a human asset pair ("XLM/USDC") in place of two raw C-strkeys.
//
// Returns (nil, nil) — never an error — for a source without such a table:
//   - soroswap carries its token identities in soroswap_pairs (token0/token1)
//     and is enriched from there, not here;
//   - sdex / oracles / bridges hold no pools;
//   - sorocredit's credit-market model (a collateral contract + per-statement
//     debt assets) does not map to a stable pool→reserve set, so it is left
//     to the raw-contract roster (documented gap).
//
// Ordering follows the pool's canonical token order where the table records a
// position (aquarius token_index; phoenix token_a then token_b). comet
// (weighted, order-free on the wire) and blend (a reserve SET, not a pair)
// have no positional order, so their tokens sort deterministically.
//
// Lending markets (blend) return the pool's reserve ASSETS — a set, not a
// pair — from blend_positions.asset.
func (s *Store) PoolTokens(ctx context.Context, source string) (map[string][]string, error) {
	switch source {
	case "phoenix":
		return s.phoenixPoolTokens(ctx)
	// ORDER BY on the DISTINCT queries below makes the LIMIT
	// deterministic: without it, which (pool, token) pairs survive a
	// limit overflow is plan-dependent — a pool could silently lose part
	// of its token set with no error (review 2026-07-08, finding 4).
	case "comet":
		return s.groupedPoolTokens(ctx, "comet", `
			SELECT DISTINCT contract_id, token
			  FROM comet_liquidity
			 WHERE token IS NOT NULL
			 ORDER BY contract_id, token
			 LIMIT `+fmt.Sprint(poolTokensRowLimit))
	case "aquarius":
		return s.aquariusPoolTokens(ctx)
	case "blend":
		return s.groupedPoolTokens(ctx, "blend", `
			SELECT DISTINCT pool, asset
			  FROM blend_positions
			 WHERE asset IS NOT NULL
			 ORDER BY pool, asset
			 LIMIT `+fmt.Sprint(poolTokensRowLimit))
	}
	return nil, nil
}

// phoenixPoolTokens returns pool → [token_a, token_b] from the most recent
// provide_liquidity row per pool (withdraw rows carry no token addresses).
func (s *Store) phoenixPoolTokens(ctx context.Context) (map[string][]string, error) {
	q := fmt.Sprintf(`
		SELECT DISTINCT ON (pool) pool, token_a, token_b
		  FROM phoenix_liquidity
		 WHERE action = 'provide_liquidity' AND token_a IS NOT NULL AND token_b IS NOT NULL
		 ORDER BY pool, ledger_close_time DESC
		 LIMIT %d`, poolTokensRowLimit)
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("timescale: PoolTokens phoenix: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string][]string{}
	for rows.Next() {
		var pool string
		var tokenA, tokenB sql.NullString
		if err := rows.Scan(&pool, &tokenA, &tokenB); err != nil {
			return nil, fmt.Errorf("timescale: PoolTokens phoenix scan: %w", err)
		}
		toks := make([]string, 0, 2)
		if tokenA.Valid && tokenA.String != "" {
			toks = append(toks, tokenA.String)
		}
		if tokenB.Valid && tokenB.String != "" {
			toks = append(toks, tokenB.String)
		}
		if len(toks) > 0 {
			out[pool] = toks
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: PoolTokens phoenix rows: %w", err)
	}
	return out, nil
}

// aquariusPoolTokens returns pool → tokens ordered by the pool's canonical
// token_index (2/3/4-token stableswaps included), from the most recent
// deposit/withdraw row per (pool, token_index).
func (s *Store) aquariusPoolTokens(ctx context.Context) (map[string][]string, error) {
	q := fmt.Sprintf(`
		SELECT DISTINCT ON (contract_id, token_index) contract_id, token_index, token
		  FROM aquarius_liquidity
		 WHERE token IS NOT NULL
		 ORDER BY contract_id, token_index, ledger_close_time DESC
		 LIMIT %d`, poolTokensRowLimit)
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("timescale: PoolTokens aquarius: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Preserve token_index order: the DISTINCT ON ... ORDER BY already emits
	// rows per pool in ascending token_index, so append-in-arrival is correct.
	out := map[string][]string{}
	for rows.Next() {
		var (
			pool       string
			tokenIndex int
			token      string
		)
		if err := rows.Scan(&pool, &tokenIndex, &token); err != nil {
			return nil, fmt.Errorf("timescale: PoolTokens aquarius scan: %w", err)
		}
		if token != "" {
			out[pool] = append(out[pool], token)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: PoolTokens aquarius rows: %w", err)
	}
	return out, nil
}

// groupedPoolTokens runs a two-column (pool, token) DISTINCT query and groups
// the tokens per pool, sorted for a deterministic order-free set (comet
// weighted pools / blend reserve sets). query MUST select exactly (pool,
// token) in that order and is a hard-coded constant per source (never
// request-derived).
func (s *Store) groupedPoolTokens(ctx context.Context, source, query string) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("timescale: PoolTokens %s: %w", source, err)
	}
	defer func() { _ = rows.Close() }()

	sets := map[string]map[string]struct{}{}
	for rows.Next() {
		var pool, token string
		if err := rows.Scan(&pool, &token); err != nil {
			return nil, fmt.Errorf("timescale: PoolTokens %s scan: %w", source, err)
		}
		if token == "" {
			continue
		}
		set := sets[pool]
		if set == nil {
			set = map[string]struct{}{}
			sets[pool] = set
		}
		set[token] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: PoolTokens %s rows: %w", source, err)
	}
	out := make(map[string][]string, len(sets))
	for pool, set := range sets {
		toks := make([]string, 0, len(set))
		for t := range set {
			toks = append(toks, t)
		}
		sort.Strings(toks)
		out[pool] = toks
	}
	return out, nil
}
