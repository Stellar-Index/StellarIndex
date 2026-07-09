package timescale

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// BespokeBlock is the served-tier, per-category bespoke analytics for a
// protocol page (the v1 API maps it 1:1 to api/v1.ProtocolBespoke — timescale
// can't import v1). A generic container (headline KPIs + named time-series +
// named top-N tables) filled with content tailored to the protocol's category,
// so the UI renders the three shapes generically while the DATA is bespoke.
//
// All numeric values are PRE-FORMATTED strings here: the store owns formatting
// so amounts that exceed 2^53 stay exact (ADR-0003) and percentages/counts read
// the way the page shows them.
type BespokeBlock struct {
	Category string
	KPIs     []BespokeKPI
	Series   []BespokeSeries
	Tables   []BespokeTable
	Notes    []string
}

// BespokeKPI is one headline metric card.
type BespokeKPI struct {
	Label string
	Value string
	Unit  string
	Hint  string
}

// BespokeSeries is a named time-series for a chart.
type BespokeSeries struct {
	Name   string
	Unit   string
	Points []BespokeSeriesPt
}

// BespokeSeriesPt is one (date, value) point; Value is a numeric string.
type BespokeSeriesPt struct {
	Date  string
	Value string
}

// BespokeTable is a named top-N table — column headers + string rows.
type BespokeTable struct {
	Title   string
	Columns []string
	Rows    [][]string
}

// BuildProtocolBespoke assembles the bespoke block for source (the protocol
// name) given its category, over a trailing windowDays. Returns nil (not an
// error) for a category with no bespoke metrics yet, so the page degrades to
// its generic analytics. windowDays bounds every query to the ts-indexed recent
// window so the projected-table scans stay cheap.
func (s *Store) BuildProtocolBespoke(ctx context.Context, source, category string, windowDays int) (*BespokeBlock, error) {
	if windowDays <= 0 {
		windowDays = 90
	}
	switch category {
	case "bridge":
		return s.bespokeBridge(ctx, source, windowDays)
	case "dex", "amm":
		return s.bespokeDEX(ctx, source, windowDays)
	case "lending":
		return s.bespokeLending(ctx, source, windowDays)
	case "yield":
		return s.bespokeYield(ctx, source, windowDays)
	case "oracle":
		return s.bespokeOracle(ctx, source, windowDays)
	}
	// Categories with no bespoke block yet land here.
	return nil, nil
}

// bespokeBridge builds the bridge (CCTP / Rozo) bespoke block from cctp_events:
// total + daily cross-chain transfer volume and a by-destination-domain table.
// Reference implementation for the per-category pattern.
func (s *Store) bespokeBridge(ctx context.Context, source string, windowDays int) (*BespokeBlock, error) {
	// Only cctp_events carries the domain + amount shape today; rozo_events is
	// empty. Keep this scoped to cctp; other bridges omit the block.
	if source != "cctp" {
		return nil, nil
	}
	since := fmt.Sprintf("%d days", windowDays)
	blk := &BespokeBlock{
		Category: "bridge",
		Notes: []string{
			"Volume is the summed CCTP event amount (USDC, 6-decimal units) over the window; deposit_for_burn + mint_and_withdraw legs both count.",
		},
	}

	// KPIs: total volume + transfer count over the window.
	var totalVol, txCount string
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(sum(amount),0)::text, count(*)::text
		FROM cctp_events WHERE ts > now() - $1::interval AND amount IS NOT NULL`, since).
		Scan(&totalVol, &txCount)
	if err != nil {
		return nil, fmt.Errorf("timescale: bespokeBridge KPIs: %w", err)
	}
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("Transfer volume (%dd)", windowDays), Value: totalVol, Unit: "USDC-units", Hint: "summed event amount in 6-decimal USDC units"},
		BespokeKPI{Label: fmt.Sprintf("Transfers (%dd)", windowDays), Value: txCount},
	)

	// Daily volume series.
	series, err := s.scanDailySeries(ctx, `
		SELECT to_char(date_trunc('day', ts), 'YYYY-MM-DD'), COALESCE(sum(amount),0)::text
		FROM cctp_events WHERE ts > now() - $1::interval AND amount IS NOT NULL
		GROUP BY 1 ORDER BY 1 ASC`, since)
	if err != nil {
		return nil, err
	}
	if len(series) > 0 {
		blk.Series = append(blk.Series, BespokeSeries{Name: "Daily transfer volume", Unit: "USDC-units", Points: series})
	}

	// By counterparty domain.
	tbl, err := s.scanTable(ctx,
		BespokeTable{Title: "By counterparty domain", Columns: []string{"Domain", "Transfers", "Volume (USDC-units)"}},
		`SELECT COALESCE(counterparty_domain::text,'—'), count(*)::text, COALESCE(sum(amount),0)::text
		   FROM cctp_events WHERE ts > now() - $1::interval
		  GROUP BY counterparty_domain ORDER BY count(*) DESC LIMIT 25`, since)
	if err != nil {
		return nil, err
	}
	if len(tbl.Rows) > 0 {
		blk.Tables = append(blk.Tables, tbl)
	}
	return blk, nil
}

// scanDailySeries runs a (date_text, value_text) query and returns the points.
func (s *Store) scanDailySeries(ctx context.Context, query string, args ...any) ([]BespokeSeriesPt, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: bespoke series: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []BespokeSeriesPt
	for rows.Next() {
		var p BespokeSeriesPt
		if err := rows.Scan(&p.Date, &p.Value); err != nil {
			return nil, fmt.Errorf("timescale: bespoke series scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// scanTable runs a query whose columns match base.Columns and fills base.Rows
// (every value scanned as text). The header is taken from base.
func (s *Store) scanTable(ctx context.Context, base BespokeTable, query string, args ...any) (BespokeTable, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return base, fmt.Errorf("timescale: bespoke table %q: %w", base.Title, err)
	}
	defer func() { _ = rows.Close() }()
	n := len(base.Columns)
	for rows.Next() {
		cells := make([]string, n)
		ptrs := make([]any, n)
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return base, fmt.Errorf("timescale: bespoke table %q scan: %w", base.Title, err)
		}
		base.Rows = append(base.Rows, cells)
	}
	return base, rows.Err()
}

// bespokeDEX builds the DEX/AMM bespoke block (soroswap / phoenix / aquarius
// / comet / sdex) from the dex_volume_by_pair_1d continuous aggregate
// (migration 0064): windowed USD volume + trade count + unique pairs KPIs, a
// daily USD-volume series, and a top-pairs-by-volume table. Queries the CAGG,
// never raw `trades` (a direct 90d GROUP BY over the 313M-row hypertable
// measured ~15.7s).
func (s *Store) bespokeDEX(ctx context.Context, source string, windowDays int) (*BespokeBlock, error) {
	since := fmt.Sprintf("%d days", windowDays)
	blk := &BespokeBlock{
		Category: "dex",
		Notes: []string{
			"Volume is summed USD volume (vol) from the dex_volume_by_pair_1d daily continuous aggregate over the window; trades whose quote never resolved to a USD price contribute 0 to volume but still count toward trade and pair totals.",
			"Base turnover is in base-asset base units (per-asset decimals), not USD.",
		},
	}

	// KPIs: window USD volume, trade count, unique pairs.
	var vol, txCount, pairs string
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(sum(vol),0)::text,
		       COALESCE(sum(trades),0)::text,
		       count(DISTINCT (base_asset, quote_asset))::text
		FROM dex_volume_by_pair_1d
		WHERE source = $1 AND bucket > now() - $2::interval`, source, since).
		Scan(&vol, &txCount, &pairs)
	if err != nil {
		return nil, fmt.Errorf("timescale: bespokeDEX KPIs: %w", err)
	}
	if txCount != "0" {
		blk.KPIs = append(blk.KPIs,
			BespokeKPI{Label: fmt.Sprintf("USD volume (%dd)", windowDays), Value: vol, Unit: "USD", Hint: "summed usd_volume of priced trades over the window"},
			BespokeKPI{Label: fmt.Sprintf("Trades (%dd)", windowDays), Value: txCount},
			BespokeKPI{Label: fmt.Sprintf("Active pairs (%dd)", windowDays), Value: pairs, Hint: "distinct base/quote pairs traded in the window"},
		)

		// Daily USD-volume series.
		series, err := s.scanDailySeries(ctx, `
			SELECT to_char(bucket, 'YYYY-MM-DD'), COALESCE(sum(vol),0)::text
			FROM dex_volume_by_pair_1d
			WHERE source = $1 AND bucket > now() - $2::interval
			GROUP BY 1 ORDER BY 1 ASC`, source, since)
		if err != nil {
			return nil, err
		}
		if len(series) > 0 {
			blk.Series = append(blk.Series, BespokeSeries{Name: "Daily USD volume", Unit: "USD", Points: series})
		}

		// Top pairs by USD volume.
		tbl, err := s.scanTable(ctx,
			BespokeTable{Title: "Top pairs by USD volume", Columns: []string{"Base", "Quote", "Trades", "USD volume", "Base turnover"}},
			`SELECT base_asset, quote_asset,
			        COALESCE(sum(trades),0)::text,
			        COALESCE(sum(vol),0)::text,
			        COALESCE(sum(base_vol),0)::text
			   FROM dex_volume_by_pair_1d
			  WHERE source = $1 AND bucket > now() - $2::interval
			  GROUP BY base_asset, quote_asset
			  ORDER BY sum(vol) DESC NULLS LAST, sum(trades) DESC LIMIT 25`, source, since)
		if err != nil {
			return nil, err
		}
		if len(tbl.Rows) > 0 {
			blk.Tables = append(blk.Tables, tbl)
		}
	}

	// Per-source captured-liquidity augments (reserves / net-flow depth /
	// staking / skim) — independent of trade volume so a quiet window still
	// surfaces depth, and empty-safe until the decoders have captured
	// anything on r1.
	if err := s.dexSourceAugments(ctx, blk, source, windowDays); err != nil {
		return nil, err
	}

	// Omit an all-empty block — a dormant DEX, or soroswap-router whose
	// swaps are ContractCall-derived and never land in `trades` — rather
	// than render an all-zero DEX panel.
	if len(blk.KPIs) == 0 {
		return nil, nil
	}
	return blk, nil
}

// dexSourceAugments adds the per-source captured-liquidity surfaces to a DEX
// block, keyed on source. Each augment is empty-safe (a no-op when its table
// holds nothing in the window):
//
//	aquarius → pool reserves / depth (aquarius_reserves, migration 0089)
//	comet    → net liquidity flow (comet_liquidity, migration 0042) + CS-026 caveat
//	phoenix  → net liquidity flow (phoenix_liquidity) + LP staking (phoenix_stake_events, migration 0044)
//	soroswap → skim KPIs (soroswap_skim_events, migration 0043)
//
// Split out of bespokeDEX so the per-source dispatch stays under the
// cognitive-complexity ceiling.
func (s *Store) dexSourceAugments(ctx context.Context, blk *BespokeBlock, source string, windowDays int) error {
	switch source {
	case "aquarius":
		return s.aquariusReserveBlocks(ctx, blk, windowDays)
	case "comet":
		return s.cometLiquidityBlocks(ctx, blk, windowDays)
	case "phoenix":
		if err := s.phoenixLiquidityBlocks(ctx, blk, windowDays); err != nil {
			return err
		}
		return s.phoenixStakeKPIs(ctx, blk, windowDays)
	case "soroswap":
		return s.soroswapSkimKPIs(ctx, blk, windowDays)
	}
	return nil
}

// aquariusReserveBlocks augments the Aquarius DEX block with the pool
// liquidity-depth surface derived from aquarius_reserves (migration 0089) —
// the first Aquarius TVL/depth signal on the analytics axis. It adds a
// pool-depth KPI (pools with a live reserve snapshot), a latest-snapshot
// recency KPI, and a per-pool latest-reserves table in native token base
// units. Aquarius pools have no independently published price, so USD TVL
// is NOT computed — depth is reported in native units with the caveat in
// the appended Note (mirrors bespokeDEX's "quote never resolved to USD
// contributes 0" honesty on the volume side).
//
// Empty-safe: a no-op when no reserves have been captured, so the block
// renders cleanly with just the volume KPIs (r1 captures no reserves until
// the reserves decoder deploys).
func (s *Store) aquariusReserveBlocks(ctx context.Context, blk *BespokeBlock, windowDays int) error {
	pools, err := s.LatestAquariusReserves(ctx, windowDays)
	if err != nil {
		return fmt.Errorf("timescale: bespokeDEX aquarius reserves: %w", err)
	}
	if len(pools) == 0 {
		return nil // no reserves captured yet — leave the volume-only block as is
	}

	var (
		legs   int
		latest time.Time
	)
	for _, p := range pools {
		legs += len(p.Legs)
		if p.ObservedAt.After(latest) {
			latest = p.ObservedAt
		}
	}
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{
			Label: fmt.Sprintf("Pools with live reserves (%dd)", windowDays),
			Value: strconv.Itoa(len(pools)),
			Unit:  "pools",
			Hint:  "distinct pools with an update_reserves (TVL/depth) snapshot in the window",
		},
		BespokeKPI{
			Label: "Reserve legs tracked",
			Value: strconv.Itoa(legs),
			Hint:  "total token positions across the latest per-pool reserve snapshots",
		},
		BespokeKPI{
			Label: "Latest reserve snapshot",
			Value: latest.UTC().Format("2006-01-02 15:04"),
			Unit:  "UTC",
			Hint:  "most recent update_reserves observation across pools",
		},
	)

	// Per-pool latest reserves — one row per (pool, token position). No USD
	// ranking is possible (Aquarius has no published price), so rows are
	// ordered most-recently-updated pool first and capped.
	const maxRows = 100
	tbl := BespokeTable{
		Title:   "Pool liquidity depth (latest reserves)",
		Columns: []string{"Pool", "Token", "Reserve (base units)", "Observed (UTC)"},
	}
	for _, p := range pools {
		if len(tbl.Rows) >= maxRows {
			break
		}
		observed := p.ObservedAt.UTC().Format("2006-01-02 15:04")
		for _, leg := range p.Legs {
			if len(tbl.Rows) >= maxRows {
				break
			}
			token := leg.Token
			if token == "" {
				token = "—"
			}
			tbl.Rows = append(tbl.Rows, []string{p.ContractID, token, leg.Reserve.String(), observed})
		}
	}
	if len(tbl.Rows) > 0 {
		blk.Tables = append(blk.Tables, tbl)
	}

	blk.Notes = append(blk.Notes,
		"Pool liquidity depth is the latest per-pool POST-STATE reserve vector (aquarius_reserves, migration 0089) in native token base units (per-asset decimals) — NOT USD. Aquarius pools have no independently published price and update_reserves carries positional reserves with no token address, so a clean USD TVL is not computed; the token address is resolved positionally from the pool's most recent deposit/withdraw and shows '—' when none was observed in the window.",
	)
	return nil
}

// cometLiquidityBlocks augments the Comet DEX block with the pool
// liquidity-flow surface derived from comet_liquidity (migration 0042). It
// adds LP-activity KPIs (pools with flow / token legs / events) and a
// per-(pool, token) net-flow table in native token base units.
//
// Depth here is a WINDOW net flow (added − removed over the window), NOT an
// absolute reserve or USD TVL — Comet emits no post-state reserve snapshot
// and has no published price. Comet is now contract-identity gated (curated
// one-pool allowlist, 2026-07-08 — CS-026 closed): the decoder's Matches()
// requires the emitting contract to be in comet.MainnetGatedSet, so a
// look-alike Balancer-v1 deployment can no longer land NEW rows here. Rows
// captured before the gate shipped were written by the prior topic-only-
// match decoder and are not retroactively re-verified, so historical rows
// may predate the gate. Both caveats are appended as Notes.
//
// Empty-safe: a no-op when no liquidity events were captured, so the block
// renders cleanly with just the volume KPIs.
func (s *Store) cometLiquidityBlocks(ctx context.Context, blk *BespokeBlock, windowDays int) error {
	flows, err := s.LatestCometLiquidityFlows(ctx, windowDays)
	if err != nil {
		return fmt.Errorf("timescale: bespokeDEX comet liquidity: %w", err)
	}
	if len(flows) == 0 {
		return nil // no liquidity events captured yet — leave the volume-only block as is
	}

	var events int64
	pools := map[string]struct{}{}
	for _, f := range flows {
		events += f.Events
		pools[f.ContractID] = struct{}{}
	}
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{
			Label: fmt.Sprintf("Pools with LP activity (%dd)", windowDays),
			Value: strconv.Itoa(len(pools)),
			Unit:  "pools",
			Hint:  "distinct pools with a join/exit/deposit/withdraw liquidity event in the window",
		},
		BespokeKPI{
			Label: fmt.Sprintf("LP events (%dd)", windowDays),
			Value: strconv.FormatInt(events, 10),
			Hint:  "total comet_liquidity events (join_pool / exit_pool / deposit / withdraw) in the window",
		},
		BespokeKPI{
			Label: "Token legs with flow",
			Value: strconv.Itoa(len(flows)),
			Hint:  "distinct (pool, token) legs with net liquidity flow in the window",
		},
	)

	const maxRows = 100
	tbl := BespokeTable{
		Title:   "Net liquidity flow by pool/token (window)",
		Columns: []string{"Pool", "Token", "Added", "Removed", "Net", "Events"},
	}
	for _, f := range flows {
		if len(tbl.Rows) >= maxRows {
			break
		}
		tbl.Rows = append(tbl.Rows, []string{
			f.ContractID, f.Token, f.Added.String(), f.Removed.String(),
			f.Net.String(), strconv.FormatInt(f.Events, 10),
		})
	}
	if len(tbl.Rows) > 0 {
		blk.Tables = append(blk.Tables, tbl)
	}

	blk.Notes = append(blk.Notes,
		"Net liquidity flow is a WINDOW delta (added − removed) from comet_liquidity per-event amounts (migration 0042) in native token base units (per-asset decimals) — NOT an absolute pool reserve or USD TVL. Comet emits no post-state reserve snapshot and has no independently published price; Net can be negative when removals exceed adds in the window.",
		"CS-026 caveat: Comet is contract-identity GATED as of 2026-07-08 (curated one-pool allowlist — comet.MainnetGatedSet, today exactly one pool, the Blend BLND/USDC backstop CAS3FL6T…; ADR-0035/0040) — a look-alike contract emitting the shared Balancer-v1 (\"POOL\", …) topic bytes can no longer land new rows here; a foreign emitter now fails the decoder's contract-identity check and surfaces via the completeness recognition audit instead. Rows dated before the gate shipped were captured by the prior topic-only-match decoder and were NOT retroactively re-verified, so historical rows may predate the gate and include a non-curated emitter. See docs/protocols/comet.md.",
	)
	return nil
}

// phoenixLiquidityBlocks augments the Phoenix DEX block with the pool
// liquidity-flow surface derived from phoenix_liquidity (migration 0044). It
// adds LP-activity KPIs (pools with flow / provides / withdraws) and a
// per-pool two-token net-flow table in native token base units.
//
// Depth here is a WINDOW net flow (provide − withdraw over the window), NOT
// an absolute reserve or USD TVL — Phoenix pool events carry the moved
// amounts, not post-state reserves, and Phoenix has no published price.
// Phoenix IS contract-identity gated (the curated-set gate, 2026-07-02 —
// earlier than Comet's 2026-07-08 gate), so unlike Comet there is no
// historical-rows-may-predate-the-gate caveat here.
//
// Empty-safe: a no-op when no liquidity events were captured, so the block
// renders cleanly with just the volume KPIs.
func (s *Store) phoenixLiquidityBlocks(ctx context.Context, blk *BespokeBlock, windowDays int) error {
	flows, err := s.LatestPhoenixLiquidityFlows(ctx, windowDays)
	if err != nil {
		return fmt.Errorf("timescale: bespokeDEX phoenix liquidity: %w", err)
	}
	if len(flows) == 0 {
		return nil // no liquidity events captured yet — leave the volume-only block as is
	}

	var provides, withdraws int64
	for _, f := range flows {
		provides += f.Provides
		withdraws += f.Withdraws
	}
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{
			Label: fmt.Sprintf("Pools with LP activity (%dd)", windowDays),
			Value: strconv.Itoa(len(flows)),
			Unit:  "pools",
			Hint:  "distinct pools with a provide/withdraw liquidity event in the window",
		},
		BespokeKPI{
			Label: fmt.Sprintf("Liquidity provides (%dd)", windowDays),
			Value: strconv.FormatInt(provides, 10),
			Hint:  "provide_liquidity events in the window",
		},
		BespokeKPI{
			Label: fmt.Sprintf("Liquidity withdraws (%dd)", windowDays),
			Value: strconv.FormatInt(withdraws, 10),
			Hint:  "withdraw_liquidity events in the window",
		},
	)

	const maxRows = 100
	tbl := BespokeTable{
		Title:   "Net liquidity flow by pool (window)",
		Columns: []string{"Pool", "Token A", "Net A", "Token B", "Net B", "Provides", "Withdraws"},
	}
	for _, f := range flows {
		if len(tbl.Rows) >= maxRows {
			break
		}
		tokenA, tokenB := f.TokenA, f.TokenB
		if tokenA == "" {
			tokenA = "—"
		}
		if tokenB == "" {
			tokenB = "—"
		}
		tbl.Rows = append(tbl.Rows, []string{
			f.Pool, tokenA, f.NetA.String(), tokenB, f.NetB.String(),
			strconv.FormatInt(f.Provides, 10), strconv.FormatInt(f.Withdraws, 10),
		})
	}
	if len(tbl.Rows) > 0 {
		blk.Tables = append(blk.Tables, tbl)
	}

	blk.Notes = append(blk.Notes,
		"Net liquidity flow is a WINDOW delta (provide − withdraw) from phoenix_liquidity per-event amounts (migration 0044) in native token base units (per-asset decimals) — NOT an absolute pool reserve or USD TVL. Phoenix pool events carry the moved amounts, not post-state reserves; token addresses are resolved from the pool's most recent provide_liquidity (withdraw events omit them) and show '—' when none was observed in the window. Net can be negative when withdrawals exceed provides.",
	)
	return nil
}

// phoenixStakeKPIs augments the Phoenix DEX block with LP-staking KPIs
// derived from phoenix_stake_events (migration 0044) — bonded / unbonded /
// net-staked LP-share amounts and unique stakers over the window. Amounts
// are LP-share-token base units, not USD. Empty-safe: a no-op when no
// bond/unbond event was captured.
func (s *Store) phoenixStakeKPIs(ctx context.Context, blk *BespokeBlock, windowDays int) error {
	st, err := s.PhoenixStakeWindowStats(ctx, windowDays)
	if err != nil {
		return fmt.Errorf("timescale: bespokeDEX phoenix stake: %w", err)
	}
	if st == nil {
		return nil // no staking activity captured yet
	}
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("LP staked (%dd)", windowDays), Value: st.Bonded.String(), Unit: "LP-token-units", Hint: "summed bond amount (LP-share base units) in the window"},
		BespokeKPI{Label: fmt.Sprintf("LP unstaked (%dd)", windowDays), Value: st.Unbonded.String(), Unit: "LP-token-units", Hint: "summed unbond amount (LP-share base units) in the window"},
		BespokeKPI{Label: fmt.Sprintf("Net LP staked (%dd)", windowDays), Value: st.NetStaked.String(), Unit: "LP-token-units", Hint: "bond − unbond (window delta; can be negative)"},
		BespokeKPI{Label: fmt.Sprintf("Unique stakers (%dd)", windowDays), Value: strconv.FormatInt(st.UniqueStakers, 10)},
	)
	blk.Notes = append(blk.Notes,
		"LP staking figures are summed phoenix_stake_events bond/unbond amounts (migration 0044) in LP-share-token base units — a WINDOW delta, not an absolute staked total, and not USD.",
	)
	return nil
}

// soroswapSkimKPIs augments the Soroswap DEX block with skim KPIs derived
// from soroswap_skim_events (migration 0043) — the caller-initiated claim of
// pool balance above recorded reserves (rare). Amounts are native token base
// units, not USD; skim is not a trade and never feeds VWAP. Empty-safe: a
// no-op when no skim was captured.
func (s *Store) soroswapSkimKPIs(ctx context.Context, blk *BespokeBlock, windowDays int) error {
	sk, err := s.SoroswapSkimWindowStats(ctx, windowDays)
	if err != nil {
		return fmt.Errorf("timescale: bespokeDEX soroswap skim: %w", err)
	}
	if sk == nil {
		return nil // no skims captured yet
	}
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("Skim events (%dd)", windowDays), Value: strconv.FormatInt(sk.Skims, 10), Hint: "caller-initiated claims of pool balance above recorded reserves (rare; not trades)"},
		BespokeKPI{Label: fmt.Sprintf("Skimmed token0 (%dd)", windowDays), Value: sk.Amount0.String(), Unit: "token-units", Hint: "summed token0 excess skimmed (base units)"},
		BespokeKPI{Label: fmt.Sprintf("Skimmed token1 (%dd)", windowDays), Value: sk.Amount1.String(), Unit: "token-units", Hint: "summed token1 excess skimmed (base units)"},
		BespokeKPI{Label: fmt.Sprintf("Pairs skimmed (%dd)", windowDays), Value: strconv.FormatInt(sk.Pairs, 10)},
	)
	blk.Notes = append(blk.Notes,
		"Skim figures are summed soroswap_skim_events amounts (migration 0043) in native token base units — the excess pool balance a caller claimed above recorded reserves. Skim is not a trade and never feeds VWAP.",
	)
	return nil
}

// bespokeLending builds the Blend lending bespoke block: per-asset net
// supplied (supply + supply_collateral − withdraw − withdraw_collateral) and
// net borrowed (borrow − repay) from blend_positions, plus auction and
// backstop activity. blend_positions amounts are unsigned magnitudes — the
// sign is the event_kind, not the value — so net positions are signed sums of
// the gross magnitudes. Confirmed on r1: event_kind ∈ {supply, withdraw,
// supply_collateral, withdraw_collateral, borrow, repay, flash_loan};
// token_amount + b_or_d_amount are both ≥ 0.
func (s *Store) bespokeLending(ctx context.Context, source string, windowDays int) (*BespokeBlock, error) {
	// sorocredit is a lending-category source with its own event surface
	// (credit_positions / credit_statements / credit_settlements /
	// credit_events, migration 0090) — not the Blend money-market tables —
	// so it gets a dedicated builder rather than the blend_* path below.
	if source == "sorocredit" {
		return s.bespokeCredit(ctx, windowDays)
	}
	if source != "blend" {
		return nil, nil
	}
	since := fmt.Sprintf("%d days", windowDays)
	blk := &BespokeBlock{
		Category: "lending",
		Notes: []string{
			"Net supplied / net borrowed are signed running sums of unsigned blend_positions.token_amount over the window — supply/supply_collateral add, withdraw/withdraw_collateral subtract for supplied; borrow adds, repay subtracts for borrowed. They are WINDOW deltas, not all-time TVL (the served tier is retention-scoped); flash_loan is excluded.",
			"Asset is a Soroban token contract id, shown shortened; amounts are in the token's base units (per-asset decimals).",
			"Per-pool 'Util %' is the window borrow/supply ratio — a coarse proxy, not on-chain utilisation (which is current reserve borrowed/supplied). Real current-state TVL + supply/borrow APYs need the Soroban pool-storage reader (reserve b_rate/d_rate + totals from contract storage); this block is event-derived and window-scoped until that ships.",
		},
	}

	if err := s.lendingPositionBlocks(ctx, blk, since, windowDays); err != nil {
		return nil, err
	}
	if err := s.lendingAuctionBlocks(ctx, blk, since, windowDays); err != nil {
		return nil, err
	}
	if err := s.lendingBackstopKPIs(ctx, blk, since, windowDays); err != nil {
		return nil, err
	}
	if err := s.lendingEmissionKPIs(ctx, blk, windowDays); err != nil {
		return nil, err
	}
	return blk, nil
}

// lendingEmissionKPIs augments the Blend lending block with emission /
// credit-risk KPIs from blend_emissions (migration 0045) — claimed-emission
// volume + claim/gulp counts, and a credit-risk (bad_debt + defaulted_debt)
// event count surfaced honestly. Claim amounts are token base units, not
// USD. Empty-safe: a no-op when no emission event was captured.
func (s *Store) lendingEmissionKPIs(ctx context.Context, blk *BespokeBlock, windowDays int) error {
	em, err := s.BlendEmissionWindowStats(ctx, windowDays)
	if err != nil {
		return fmt.Errorf("timescale: bespokeLending emissions: %w", err)
	}
	if em == nil {
		return nil // no emission activity captured yet
	}
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("Emissions claimed (%dd)", windowDays), Value: em.ClaimVolume.String(), Unit: "token-units", Hint: "summed blend_emissions claim amount (base units)"},
		BespokeKPI{Label: fmt.Sprintf("Claim events (%dd)", windowDays), Value: strconv.FormatInt(em.Claims, 10)},
		BespokeKPI{Label: fmt.Sprintf("Emission gulps (%dd)", windowDays), Value: strconv.FormatInt(em.Gulps, 10), Hint: "gulp + gulp_emissions accounting events in the window"},
		BespokeKPI{Label: fmt.Sprintf("Credit-risk events (%dd)", windowDays), Value: strconv.FormatInt(em.CreditRisk, 10), Hint: "bad_debt + defaulted_debt events — a genuine risk signal (unlike sorocredit's scheduled settlements)"},
	)
	blk.Notes = append(blk.Notes,
		"Emission figures are summed blend_emissions amounts (migration 0045) in token base units, not USD. Credit-risk events count bad_debt + defaulted_debt (a genuine risk signal); emissions claimed is a WINDOW sum, not an all-time total.",
	)
	return nil
}

// lendingPositionBlocks fills the net-supplied / net-borrowed KPIs, the
// per-asset net-position table, and the daily position-event series.
func (s *Store) lendingPositionBlocks(ctx context.Context, blk *BespokeBlock, since string, windowDays int) error {
	var netSupplied, netBorrowed, users, flashLoans string
	err := s.db.QueryRowContext(ctx, `
		SELECT
		  COALESCE(sum(CASE
		    WHEN event_kind IN ('supply','supply_collateral')    THEN token_amount
		    WHEN event_kind IN ('withdraw','withdraw_collateral') THEN -token_amount
		    ELSE 0 END),0)::text,
		  COALESCE(sum(CASE
		    WHEN event_kind = 'borrow' THEN token_amount
		    WHEN event_kind = 'repay'  THEN -token_amount
		    ELSE 0 END),0)::text,
		  count(DISTINCT user_address)::text,
		  count(*) FILTER (WHERE event_kind = 'flash_loan')::text
		FROM blend_positions
		WHERE ledger_close_time > now() - $1::interval`, since).
		Scan(&netSupplied, &netBorrowed, &users, &flashLoans)
	if err != nil {
		return fmt.Errorf("timescale: bespokeLending position KPIs: %w", err)
	}
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("Net supplied (%dd)", windowDays), Value: netSupplied, Unit: "token-units", Hint: "supply+collateral minus withdrawals, summed across assets (base units)"},
		BespokeKPI{Label: fmt.Sprintf("Net borrowed (%dd)", windowDays), Value: netBorrowed, Unit: "token-units", Hint: "borrow minus repay, summed across assets (base units)"},
		BespokeKPI{Label: fmt.Sprintf("Active users (%dd)", windowDays), Value: users},
		BespokeKPI{Label: fmt.Sprintf("Flash loans (%dd)", windowDays), Value: flashLoans},
	)

	tbl, err := s.scanTable(ctx,
		BespokeTable{Title: "Net position by asset", Columns: []string{"Asset", "Net supplied", "Net borrowed", "Events"}},
		`SELECT asset,
		   COALESCE(sum(CASE
		     WHEN event_kind IN ('supply','supply_collateral')    THEN token_amount
		     WHEN event_kind IN ('withdraw','withdraw_collateral') THEN -token_amount
		     ELSE 0 END),0)::text,
		   COALESCE(sum(CASE
		     WHEN event_kind = 'borrow' THEN token_amount
		     WHEN event_kind = 'repay'  THEN -token_amount
		     ELSE 0 END),0)::text,
		   count(*)::text
		 FROM blend_positions
		 WHERE ledger_close_time > now() - $1::interval
		 GROUP BY asset
		 ORDER BY count(*) DESC LIMIT 25`, since)
	if err != nil {
		return err
	}
	if len(tbl.Rows) > 0 {
		blk.Tables = append(blk.Tables, tbl)
	}

	poolTbl, err := s.scanTable(ctx,
		BespokeTable{Title: "Net position by pool", Columns: []string{"Pool", "Net supplied", "Net borrowed", "Util %", "Users", "Events"}},
		`SELECT pool,
		   COALESCE(sum(CASE
		     WHEN event_kind IN ('supply','supply_collateral')    THEN token_amount
		     WHEN event_kind IN ('withdraw','withdraw_collateral') THEN -token_amount
		     ELSE 0 END),0)::text,
		   COALESCE(sum(CASE
		     WHEN event_kind = 'borrow' THEN token_amount
		     WHEN event_kind = 'repay'  THEN -token_amount
		     ELSE 0 END),0)::text,
		   CASE WHEN COALESCE(sum(CASE
		         WHEN event_kind IN ('supply','supply_collateral')    THEN token_amount
		         WHEN event_kind IN ('withdraw','withdraw_collateral') THEN -token_amount
		         ELSE 0 END),0) > 0
		     THEN round(100.0 * COALESCE(sum(CASE
		         WHEN event_kind = 'borrow' THEN token_amount
		         WHEN event_kind = 'repay'  THEN -token_amount
		         ELSE 0 END),0) / sum(CASE
		         WHEN event_kind IN ('supply','supply_collateral')    THEN token_amount
		         WHEN event_kind IN ('withdraw','withdraw_collateral') THEN -token_amount
		         ELSE 0 END), 2)::text
		     ELSE '—' END,
		   count(DISTINCT user_address)::text,
		   count(*)::text
		 FROM blend_positions
		 WHERE ledger_close_time > now() - $1::interval
		 GROUP BY pool
		 ORDER BY count(*) DESC LIMIT 25`, since)
	if err != nil {
		return err
	}
	if len(poolTbl.Rows) > 0 {
		blk.Tables = append(blk.Tables, poolTbl)
	}

	series, err := s.scanDailySeries(ctx, `
		SELECT to_char(date_trunc('day', ledger_close_time), 'YYYY-MM-DD'), count(*)::text
		FROM blend_positions WHERE ledger_close_time > now() - $1::interval
		GROUP BY 1 ORDER BY 1 ASC`, since)
	if err != nil {
		return err
	}
	if len(series) > 0 {
		blk.Series = append(blk.Series, BespokeSeries{Name: "Daily position events", Unit: "events", Points: series})
	}
	return nil
}

// lendingAuctionBlocks fills the auction-count KPIs and the recent-auctions
// table from blend_auctions.
func (s *Store) lendingAuctionBlocks(ctx context.Context, blk *BespokeBlock, since string, windowDays int) error {
	var newAuctions, fills string
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE event_kind = 'new')::text,
		       count(*) FILTER (WHERE event_kind = 'fill')::text
		FROM blend_auctions WHERE ts > now() - $1::interval`, since).
		Scan(&newAuctions, &fills); err != nil {
		return fmt.Errorf("timescale: bespokeLending auction KPIs: %w", err)
	}
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("Auctions started (%dd)", windowDays), Value: newAuctions, Hint: "blend_auctions new events (type 0 user-liquidation, 1 bad-debt, 2 interest)"},
		BespokeKPI{Label: fmt.Sprintf("Auctions filled (%dd)", windowDays), Value: fills},
	)

	atbl, err := s.scanTable(ctx,
		BespokeTable{Title: "Recent auctions", Columns: []string{"When", "Type", "Kind", "User", "Fill %"}},
		`SELECT to_char(ts, 'YYYY-MM-DD HH24:MI'),
		        CASE auction_type WHEN 0 THEN 'user-liquidation' WHEN 1 THEN 'bad-debt' WHEN 2 THEN 'interest' ELSE auction_type::text END,
		        event_kind,
		        user_address,
		        COALESCE(fill_percent::text,'—')
		   FROM blend_auctions WHERE ts > now() - $1::interval
		  ORDER BY ts DESC LIMIT 25`, since)
	if err != nil {
		return err
	}
	if len(atbl.Rows) > 0 {
		blk.Tables = append(blk.Tables, atbl)
	}
	return nil
}

// lendingBackstopKPIs fills the backstop deposit/withdraw volume KPIs (the
// table is sparse — degrades to 0).
func (s *Store) lendingBackstopKPIs(ctx context.Context, blk *BespokeBlock, since string, windowDays int) error {
	var backstopVol, backstopCount string
	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(sum(amount),0)::text, count(*)::text
		FROM blend_backstop_events WHERE ledger_close_time > now() - $1::interval`, since).
		Scan(&backstopVol, &backstopCount); err != nil {
		return fmt.Errorf("timescale: bespokeLending backstop KPIs: %w", err)
	}
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("Backstop volume (%dd)", windowDays), Value: backstopVol, Unit: "token-units", Hint: "summed blend_backstop_events amount (base units)"},
		BespokeKPI{Label: fmt.Sprintf("Backstop events (%dd)", windowDays), Value: backstopCount},
	)
	return nil
}

// bespokeCredit builds the sorocredit (consumer-USDC credit / CDP protocol)
// lending bespoke block from the four credit_* hypertables (migration 0090):
// positions opened + a window-scoped open-position proxy, statements
// published, SCHEDULED settlements (volume + count), withdrawals, and a
// recent-settlements table.
//
// CRITICAL semantic: credit_settlements rows are the on-wire "Liquidation"
// event but are recurring SCHEDULED keeper settlements of published
// statements — NOT distressed liquidations. Every settlement label + the
// appended Note says so; a "liquidations" risk signal must never be surfaced
// from this data (migration 0090 header + internal/sources/sorocredit).
//
// Empty-safe: returns nil (not an error) when no credit_* row exists in the
// window, so /v1/protocols/sorocredit degrades to its generic analytics —
// r1's credit_* tables are empty until the sorocredit projector-replay runs
// post-deploy. Amounts are USDC / token base units (per-asset decimals),
// never USD (sorocredit has no published price).
func (s *Store) bespokeCredit(ctx context.Context, windowDays int) (*BespokeBlock, error) {
	a, err := s.CreditWindowAnalytics(ctx, windowDays)
	if err != nil {
		return nil, fmt.Errorf("timescale: bespokeCredit analytics: %w", err)
	}
	if a == nil {
		return nil, nil // no activity in the window — omit the panel
	}

	since := fmt.Sprintf("%d days", windowDays)
	blk := &BespokeBlock{
		Category: "lending",
		Notes: []string{
			"Settlements are SCHEDULED settlements decoded from the on-wire \"Liquidation\" event — a single keeper settles published statements on a recurring schedule (~1:1 with statements). These are NOT distressed liquidations; do not read them as a risk/liquidation signal.",
			"Open positions is a WINDOW-SCOPED proxy: positions opened in the window whose collateral child has no withdrawal (cash-out) observed in the window. It is not an all-time live-position count (the served tier is retention-scoped).",
			"Amounts are in token base units (USDC settlements/withdrawals at 6-decimal USDC base units; statement amounts at the protocol's i128 scale) — NOT USD. sorocredit has no published price and never contributes to VWAP.",
		},
	}

	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("Positions opened (%dd)", windowDays), Value: strconv.FormatInt(a.PositionsOpened, 10), Hint: "NewCollateralContract events (one child position per open) in the window"},
		BespokeKPI{Label: fmt.Sprintf("Open positions (%dd)", windowDays), Value: strconv.FormatInt(a.OpenPositions, 10), Hint: "opened in the window without an observed withdrawal in the window (window-scoped proxy, not all-time)"},
		BespokeKPI{Label: fmt.Sprintf("Unique users (%dd)", windowDays), Value: strconv.FormatInt(a.UniqueUsers, 10), Hint: "distinct position owners (G-addresses)"},
		BespokeKPI{Label: fmt.Sprintf("Statements published (%dd)", windowDays), Value: strconv.FormatInt(a.Statements, 10), Hint: "StatementPublished events (periodic per-position charge statements)"},
		BespokeKPI{Label: fmt.Sprintf("Scheduled settlements (%dd)", windowDays), Value: strconv.FormatInt(a.Settlements, 10), Hint: "recurring keeper settlements of published statements — NOT distressed liquidations"},
		BespokeKPI{Label: fmt.Sprintf("Settlement volume (%dd)", windowDays), Value: a.SettlementVolume.String(), Unit: "USDC-units", Hint: "summed scheduled-settlement amount in 6-decimal USDC base units (NOT a liquidation/risk signal)"},
		BespokeKPI{Label: fmt.Sprintf("Withdrawals (%dd)", windowDays), Value: strconv.FormatInt(a.Withdrawals, 10), Hint: "position cash-out events in the window"},
	)
	if !a.LatestActivity.IsZero() {
		blk.KPIs = append(blk.KPIs, BespokeKPI{
			Label: "Latest activity",
			Value: a.LatestActivity.UTC().Format("2006-01-02 15:04"),
			Unit:  "UTC",
			Hint:  "most recent credit event across positions/statements/settlements/withdrawals",
		})
	}

	// Daily scheduled-settlement volume (the protocol's dominant recurring
	// flow). Native USDC base units, not USD.
	series, err := s.scanDailySeries(ctx, `
		SELECT to_char(date_trunc('day', ledger_close_time), 'YYYY-MM-DD'), COALESCE(sum(settled_amount),0)::text
		FROM credit_settlements WHERE ledger_close_time > now() - $1::interval
		GROUP BY 1 ORDER BY 1 ASC`, since)
	if err != nil {
		return nil, err
	}
	if len(series) > 0 {
		blk.Series = append(blk.Series, BespokeSeries{Name: "Daily settlement volume", Unit: "USDC-units", Points: series})
	}

	// Recent scheduled settlements. Ordered newest-first; settled_amount is
	// rendered as text so the i128 NUMERIC never round-trips through int64.
	tbl, err := s.scanTable(ctx,
		BespokeTable{Title: "Recent scheduled settlements", Columns: []string{"When", "Position", "Debt asset", "Settled amount", "Settler"}},
		`SELECT to_char(ledger_close_time, 'YYYY-MM-DD HH24:MI'),
		        position_uuid,
		        COALESCE(debt_asset, '—'),
		        COALESCE(settled_amount::text, '—'),
		        settler_account
		   FROM credit_settlements WHERE ledger_close_time > now() - $1::interval
		  ORDER BY ledger_close_time DESC LIMIT 25`, since)
	if err != nil {
		return nil, err
	}
	if len(tbl.Rows) > 0 {
		blk.Tables = append(blk.Tables, tbl)
	}

	return blk, nil
}

// bespokeYield builds the DeFindex vault bespoke block from defindex_flows:
// windowed gross deposit / withdraw flow volume (by direction), per-vault net
// flow (deposit − withdraw, an AUM proxy), and unique actors. Confirmed on r1:
// direction ∈ {deposit, withdraw}; layer ∈ {strategy, vault}. The series and
// per-vault net scope to the vault layer to avoid double-counting a deposit
// that fans out into strategies.
func (s *Store) bespokeYield(ctx context.Context, source string, windowDays int) (*BespokeBlock, error) {
	if source != "defindex" {
		return nil, nil
	}
	since := fmt.Sprintf("%d days", windowDays)
	blk := &BespokeBlock{
		Category: "yield",
		Notes: []string{
			"Flow volume is gross summed defindex_flows.amount by direction over the window from the STRATEGY layer — the vault layer records who/when but carries the amount as a per-strategy vector (amounts_vec), so its scalar amount is NULL; the strategy-layer scalars are the actual capital deployed.",
			"Net flow (deposit − withdraw) is a window AUM proxy, not all-time TVL (the served tier is retention-scoped). Contract is a Soroban strategy contract id; amounts are summed token base units (per-asset decimals) across strategies.",
		},
	}

	var depositVol, withdrawVol, actors string
	err := s.db.QueryRowContext(ctx, `
		SELECT
		  COALESCE(sum(amount) FILTER (WHERE direction = 'deposit'  AND layer = 'strategy'),0)::text,
		  COALESCE(sum(amount) FILTER (WHERE direction = 'withdraw' AND layer = 'strategy'),0)::text,
		  count(DISTINCT actor)::text
		FROM defindex_flows WHERE ledger_close_time > now() - $1::interval`, since).
		Scan(&depositVol, &withdrawVol, &actors)
	if err != nil {
		return nil, fmt.Errorf("timescale: bespokeYield KPIs: %w", err)
	}
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("Deposit volume (%dd)", windowDays), Value: depositVol, Unit: "token-units", Hint: "gross strategy-layer deposit amount (base units)"},
		BespokeKPI{Label: fmt.Sprintf("Withdraw volume (%dd)", windowDays), Value: withdrawVol, Unit: "token-units", Hint: "gross strategy-layer withdraw amount (base units)"},
		BespokeKPI{Label: fmt.Sprintf("Unique actors (%dd)", windowDays), Value: actors},
	)

	series, err := s.scanDailySeries(ctx, `
		SELECT to_char(date_trunc('day', ledger_close_time), 'YYYY-MM-DD'), COALESCE(sum(amount),0)::text
		FROM defindex_flows
		WHERE ledger_close_time > now() - $1::interval AND layer = 'strategy'
		GROUP BY 1 ORDER BY 1 ASC`, since)
	if err != nil {
		return nil, err
	}
	if len(series) > 0 {
		blk.Series = append(blk.Series, BespokeSeries{Name: "Daily strategy flow volume", Unit: "token-units", Points: series})
	}

	tbl, err := s.scanTable(ctx,
		BespokeTable{Title: "Net flow by strategy", Columns: []string{"Strategy", "Deposits", "Withdrawals", "Net flow", "Flows"}},
		`SELECT contract_id,
		   COALESCE(sum(amount) FILTER (WHERE direction = 'deposit'  AND layer = 'strategy'),0)::text,
		   COALESCE(sum(amount) FILTER (WHERE direction = 'withdraw' AND layer = 'strategy'),0)::text,
		   COALESCE(sum(CASE WHEN layer = 'strategy' AND direction = 'deposit'  THEN amount
		                     WHEN layer = 'strategy' AND direction = 'withdraw' THEN -amount
		                     ELSE 0 END),0)::text,
		   count(*) FILTER (WHERE layer = 'strategy')::text
		 FROM defindex_flows
		 WHERE ledger_close_time > now() - $1::interval AND layer = 'strategy'
		 GROUP BY contract_id
		 ORDER BY count(*) DESC LIMIT 25`, since)
	if err != nil {
		return nil, err
	}
	if len(tbl.Rows) > 0 {
		blk.Tables = append(blk.Tables, tbl)
	}
	return blk, nil
}

// bespokeOracle builds the oracle bespoke block (reflector-dex/cex/fx, band,
// redstone) from oracle_updates scoped by source: feed count (distinct
// asset/quote), update cadence (updates/day), and a latest-prices table.
// price is a NUMERIC at the row's `decimals` scale — shown raw with the scale
// column, not rescaled (decimals can vary per feed).
func (s *Store) bespokeOracle(ctx context.Context, source string, windowDays int) (*BespokeBlock, error) {
	since := fmt.Sprintf("%d days", windowDays)
	blk := &BespokeBlock{
		Category: "oracle",
		Notes: []string{
			"Scoped to oracle_updates.source = the protocol's feed contract. price is the raw on-chain integer at the row's `decimals` scale (not rescaled — decimals can differ per feed). asset/quote are the feed pair; asset is a token contract id shown shortened, quote is often `native`.",
		},
	}

	var updates, feeds, perDay string
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*)::text,
		       count(DISTINCT (asset, quote))::text,
		       CASE WHEN $2 > 0 THEN round(count(*)::numeric / $2, 1)::text ELSE '0' END
		FROM oracle_updates
		WHERE source = $1 AND ts > now() - $3::interval`, source, windowDays, since).
		Scan(&updates, &feeds, &perDay)
	if err != nil {
		return nil, fmt.Errorf("timescale: bespokeOracle KPIs: %w", err)
	}
	if updates == "0" {
		return nil, nil // no updates for this feed in the window — omit, don't show zeros
	}
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("Updates (%dd)", windowDays), Value: updates},
		BespokeKPI{Label: "Distinct feeds", Value: feeds, Hint: "distinct asset/quote pairs seen in the window"},
		BespokeKPI{Label: "Updates / day", Value: perDay, Hint: "mean update cadence over the window"},
	)

	series, err := s.scanDailySeries(ctx, `
		SELECT to_char(date_trunc('day', ts), 'YYYY-MM-DD'), count(*)::text
		FROM oracle_updates WHERE source = $1 AND ts > now() - $2::interval
		GROUP BY 1 ORDER BY 1 ASC`, source, since)
	if err != nil {
		return nil, err
	}
	if len(series) > 0 {
		blk.Series = append(blk.Series, BespokeSeries{Name: "Daily updates", Unit: "updates", Points: series})
	}

	tbl, err := s.scanTable(ctx,
		BespokeTable{Title: "Latest feed prices", Columns: []string{"Asset", "Quote", "Price (raw)", "Decimals", "Updated"}},
		`SELECT asset,
		        quote,
		        COALESCE(price::text,'—'),
		        COALESCE(decimals::text,'—'),
		        to_char(ts, 'YYYY-MM-DD HH24:MI')
		   FROM (
		     SELECT DISTINCT ON (asset, quote) asset, quote, price, decimals, ts
		       FROM oracle_updates
		      WHERE source = $1 AND ts > now() - $2::interval
		      ORDER BY asset, quote, ts DESC
		   ) latest
		  ORDER BY ts DESC LIMIT 50`, source, since)
	if err != nil {
		return nil, err
	}
	if len(tbl.Rows) > 0 {
		blk.Tables = append(blk.Tables, tbl)
	}
	return blk, nil
}
