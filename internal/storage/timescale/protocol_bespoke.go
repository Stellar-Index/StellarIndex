package timescale

import (
	"context"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// BespokeBlock is the served-tier per-category analytics for a protocol page, mapped 1:1 to api/v1.ProtocolBespoke (timescale cannot
// import v1); numeric values are pre-formatted strings so amounts above 2^53 stay exact.
type BespokeBlock struct {
	Category   string
	KPIs       []BespokeKPI
	Series     []BespokeSeries
	Breakdowns []BespokeBreakdown
	Tables     []BespokeTable
	Notes      []string
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

// BespokeBreakdown is a named composition dataset: value-weighted (label, value, count) rows, window-scoped and sorted descending by
// the store, usable by any category.
type BespokeBreakdown struct {
	Title string
	Unit  string
	Rows  []BespokeBreakdownRow
}

// BespokeBreakdownRow is one composition slice; Value is a numeric string and Count the number of contributing events.
type BespokeBreakdownRow struct {
	Label string
	Value string
	Count int64
}

// BespokeTable is a named top-N table — column headers + string rows.
type BespokeTable struct {
	Title   string
	Columns []string
	Rows    [][]string
}

// BuildProtocolBespoke assembles the bespoke block for source over a trailing windowDays; nil (not an error) for a category with no
// bespoke metrics, and windowDays bounds every query to the ts-indexed recent window.
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

// bespokeBridge builds the bridge block: directional USDC flow volumes, daily series and per-domain tables, as decimal strings via
// exact NUMERIC division. Scales differ: cctp_events.amount is canonical 6-decimal USDC, except mint_and_forward (local 7-decimal
// restatement, excluded from flow sums, see protocol_bespoke_cctp.go); rozo_events.amount is 7-decimal SAC stroops.
func (s *Store) bespokeBridge(ctx context.Context, source string, windowDays int) (*BespokeBlock, error) {
	since := fmt.Sprintf("%d days", windowDays)
	switch source {
	case "cctp":
		return s.bespokeBridgeCCTP(ctx, since, windowDays)
	case "rozo":
		return s.bespokeBridgeRozo(ctx, since, windowDays)
	}
	return nil, nil
}

// bridgeSeriesGrain returns the date_trunc grain and to_char format; windowDays == 1 buckets by hour (spelled out to tell it from the
// daily shape). In to_char, double-quoted text is literal, so "T" renders as the ISO separator.
func bridgeSeriesGrain(windowDays int) (trunc, format string) {
	if windowDays == 1 {
		return "hour", `YYYY-MM-DD"T"HH24:00`
	}
	return "day", "YYYY-MM-DD"
}

// completeDayCutoffSQL is the exclusive bound for [completeDaysOnly], in the session timezone date_trunc('day', ...) buckets in.
const completeDayCutoffSQL = `date_trunc('day', now())`

// completeDaysOnly returns a WHERE fragment (leading " AND ") excluding today's partial daily bucket, which would read as a collapse;
// empty at 24h. col is code-owned, never request input. Apply to the point-producing scan only; top-N ranking CTEs may keep the live
// day.
func completeDaysOnly(windowDays int, col string) string {
	if windowDays == 1 {
		return ""
	}
	return " AND " + col + " < " + completeDayCutoffSQL
}

// rozoSettledSeriesQuery builds the settled-volume series at the window's grain; exact NUMERIC division (ADR-0003) over 7-decimal SAC
// stroops.
func rozoSettledSeriesQuery(windowDays int) string {
	trunc, format := bridgeSeriesGrain(windowDays)
	return `
		SELECT to_char(date_trunc('` + trunc + `', ts), '` + format + `'), (COALESCE(sum(amount),0) / 10000000::numeric)::numeric(24,7)::text
		FROM rozo_events WHERE ts > now() - $1::interval AND amount IS NOT NULL AND event_type = 'payment'` +
		completeDaysOnly(windowDays, "ts") + `
		GROUP BY 1 ORDER BY 1 ASC`
}

// bespokeBridgeRozo reports settlement volume from payment_event rows; flush_event rows are admin sweeps of already-counted funds and
// are excluded.
func (s *Store) bespokeBridgeRozo(ctx context.Context, since string, windowDays int) (*BespokeBlock, error) {
	blk := &BespokeBlock{
		Category: "bridge",
		Notes: []string{
			"Volume is USDC settled through Rozo payment events (7-decimal SAC amounts, verified against the SAC transfer leg on-chain). Admin flush sweeps are excluded — they re-move already-counted funds.",
		},
	}
	var vol, cnt string
	err := s.db.QueryRowContext(ctx, `
		SELECT (COALESCE(sum(amount),0) / 10000000::numeric)::numeric(24,7)::text, count(*)::text
		FROM rozo_events WHERE ts > now() - $1::interval AND amount IS NOT NULL AND event_type = 'payment'`, since).
		Scan(&vol, &cnt)
	if err != nil {
		return nil, fmt.Errorf("timescale: bespokeBridge rozo KPIs: %w", err)
	}
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("Settled volume (%dd)", windowDays), Value: vol, Unit: "USDC", Hint: "summed payment_event amounts settled on Stellar"},
		BespokeKPI{Label: fmt.Sprintf("Payments (%dd)", windowDays), Value: cnt},
	)
	// Name is window-stable ("Settled volume (USDC)") — see the cctp series
	// comment; the grain (hourly at 24h, daily otherwise) is in the points.
	series, err := s.scanDailySeries(ctx, rozoSettledSeriesQuery(windowDays), since)
	if err != nil {
		return nil, err
	}
	if len(series) > 0 {
		blk.Series = append(blk.Series, BespokeSeries{Name: "Settled volume (USDC)", Unit: "USDC", Points: series})
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

func (s *Store) dexSourceAugments(ctx context.Context, blk *BespokeBlock, source string, windowDays int) error {
	switch source {
	case "aquarius":
		if err := s.aquariusReserveBlocks(ctx, blk, windowDays); err != nil {
			return err
		}
		return s.aquariusRewardsBlocks(ctx, blk, windowDays)
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

// aquariusReserveBlocks adds pool liquidity depth from aquarius_reserves in native base units; no USD TVL (pools have no published price).
// A no-op when no reserves were captured.
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

// aquariusRewardsBlocks adds the rewards-gauge/governance surface. The claim_reward drill-down is fixed at 30 days; the per-kind
// breakdown and governance total are lifetime; the series follows windowDays. A no-op when neither kind was captured; each part
// degrades independently.
func (s *Store) aquariusRewardsBlocks(ctx context.Context, blk *BespokeBlock, windowDays int) error {
	byKind, err := s.AquariusRewardsLifetimeByKind(ctx)
	if err != nil {
		return fmt.Errorf("timescale: bespokeDEX aquarius rewards lifetime: %w", err)
	}
	var rewardsLifetime int64
	for _, k := range byKind {
		rewardsLifetime += k.Events
	}

	adminLifetime, err := s.AquariusAdminLifetimeTotal(ctx)
	if err != nil {
		return fmt.Errorf("timescale: bespokeDEX aquarius admin lifetime: %w", err)
	}

	if rewardsLifetime == 0 && adminLifetime == 0 {
		return nil // nothing backfilled/captured yet — leave the rest of the block as is
	}

	claims, err := s.AquariusRewardsClaimWindow(ctx, 30)
	if err != nil {
		return fmt.Errorf("timescale: bespokeDEX aquarius rewards claims: %w", err)
	}

	if rewardsLifetime > 0 {
		blk.KPIs = append(blk.KPIs, BespokeKPI{
			Label: "Rewards-gauge events (lifetime)",
			Value: strconv.FormatInt(rewardsLifetime, 10),
			Hint:  "sum of all 12 rewards-gauge event kinds, all-time (aquarius_rewards_events, migration 0099)",
		})
	}
	if claims != nil {
		blk.KPIs = append(blk.KPIs, aquariusClaimKPIs(claims)...)
	}
	if adminLifetime > 0 {
		blk.KPIs = append(blk.KPIs, BespokeKPI{
			Label: "Governance events (lifetime)",
			Value: strconv.FormatInt(adminLifetime, 10),
			Hint:  "sum of all 8 governance/upgrade kinds, all-time (aquarius_admin, migration 0100)",
		})
	}

	if tbl := aquariusRewardsKindTable(byKind); len(tbl.Rows) > 0 {
		blk.Tables = append(blk.Tables, tbl)
	}

	if claims != nil {
		blk.Tables = append(blk.Tables, aquariusClaimVolumeTable(claims))
	}

	if err := s.aquariusGovernanceTable(ctx, blk); err != nil {
		return err
	}

	series, err := s.AquariusRewardsDailyClaimSeries(ctx, windowDays)
	if err != nil {
		return fmt.Errorf("timescale: bespokeDEX aquarius rewards series: %w", err)
	}
	if len(series) > 0 {
		blk.Series = append(blk.Series, BespokeSeries{Name: "Daily reward claims", Unit: "claims", Points: series})
	}

	blk.Notes = append(blk.Notes,
		"Rewards-gauge + governance figures are from the v0.12 full-history backfill (aquarius_rewards_events, migration 0099; aquarius_admin, migration 0100). Reward volume is reported per reward token, in that token's base units (per-asset decimals), and never summed across tokens — Aquarius has no published price for reward tokens at this layer, so these are never USD and never feed VWAP. Fields marked '(lifetime)' are unwindowed all-time totals; the claim_reward drill-down is a fixed trailing-30-day window regardless of the page's overall analytics window; the daily series follows the page's overall window.",
	)
	return nil
}

// aquariusRewardsKindTable lists the lifetime rewards-gauge event count per
// kind, omitting kinds that never fired.
func aquariusRewardsKindTable(byKind []AquariusRewardsKindCount) BespokeTable {
	tbl := BespokeTable{Title: "Rewards events by kind (lifetime)", Columns: []string{"Kind", "Events"}}
	for _, k := range byKind {
		if k.Events == 0 {
			continue
		}
		tbl.Rows = append(tbl.Rows, []string{string(k.Kind), strconv.FormatInt(k.Events, 10)})
	}
	return tbl
}

// aquariusClaimKPIs renders the 30d claim KPIs; "Reward volume (30d)" is one figure only when every claim paid the same token,
// since base units of different tokens do not add.
func aquariusClaimKPIs(claims *AquariusClaimRewardWindow) []BespokeKPI {
	kpis := []BespokeKPI{{Label: "Reward claims (30d)", Value: strconv.FormatInt(claims.Events, 10), Hint: "claim_reward events in the trailing 30 days"}}
	if len(claims.ByToken) == 1 {
		t := claims.ByToken[0]
		kpis = append(kpis, BespokeKPI{
			Label: "Reward volume (30d)", Value: t.Amount.String(), Unit: "token-units",
			Hint: "summed claim_reward amount over 30d in base units of the only reward token claimed (" + aquariusRewardTokenLabel(t.RewardToken) + ") — Aquarius reward tokens have no published price at this layer, so this is NOT USD",
		})
	}
	return append(kpis, BespokeKPI{Label: "Distinct claimants (30d)", Value: strconv.FormatInt(claims.DistinctClaimants, 10), Hint: "distinct user addresses claiming a reward in the trailing 30 days"})
}

// aquariusClaimVolumeTable renders the 30d claim_reward volume per reward
// token, busiest token first.
func aquariusClaimVolumeTable(claims *AquariusClaimRewardWindow) BespokeTable {
	tbl := BespokeTable{Title: "Reward volume by token (30d)", Columns: []string{"Reward token", "Claims", "Amount (token base units)"}}
	for _, t := range claims.ByToken {
		tbl.Rows = append(tbl.Rows, []string{aquariusRewardTokenLabel(t.RewardToken), strconv.FormatInt(t.Events, 10), t.Amount.String()})
	}
	return tbl
}

func aquariusRewardTokenLabel(token string) string {
	if token == "" {
		return "unrecorded"
	}
	return token
}

// aquariusGovernanceTable adds "Recent governance events" from aquarius_admin; split out to keep aquariusRewardsBlocks' complexity down.
func (s *Store) aquariusGovernanceTable(ctx context.Context, blk *BespokeBlock) error {
	events, err := s.LatestAquariusAdminEvents(ctx, 25)
	if err != nil {
		return fmt.Errorf("timescale: bespokeDEX aquarius admin recent: %w", err)
	}
	if len(events) == 0 {
		return nil
	}
	tbl := BespokeTable{Title: "Recent governance events", Columns: []string{"When", "Kind", "Contract", "Admin", "Target", "Ledger"}}
	for _, e := range events {
		admin, target := e.Admin, e.Target
		if admin == "" {
			admin = "—"
		}
		if target == "" {
			target = "—"
		}
		tbl.Rows = append(tbl.Rows, []string{
			e.LedgerCloseTime.Format("2006-01-02 15:04"), string(e.Kind), e.ContractID, admin, target,
			strconv.FormatUint(uint64(e.Ledger), 10),
		})
	}
	blk.Tables = append(blk.Tables, tbl)
	return nil
}

// cometLiquidityBlocks adds LP-activity KPIs and a per-(pool, token) net-flow table from comet_liquidity, in native base units. Depth
// is window net flow, NOT a reserve or USD TVL (no post-state snapshot, no price). Gated by comet.MainnetGatedSet; rows from before the
// gate (topic-only decoder) are not re-verified. Both caveats are Notes.
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

// phoenixLiquidityBlocks adds LP-activity KPIs and a per-pool net-flow table from phoenix_liquidity, in native base units. Depth is
// window net flow (provide - withdraw), NOT a reserve or USD TVL: events carry moved amounts and Phoenix has no price. Contract-gated,
// so no pre-gate caveat.
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

// phoenixStakeKPIs adds bonded/unbonded/net-staked amounts and unique stakers from phoenix_stake_events, in LP-share base units, not USD.
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

// soroswapSkimKPIs adds per-pair skim KPIs from soroswap_skim_events in native base units, not USD; skim is not a trade and never feeds
// VWAP.
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
		BespokeKPI{Label: fmt.Sprintf("Pairs skimmed (%dd)", windowDays), Value: strconv.FormatInt(sk.Pairs, 10)},
	)
	blk.Tables = append(blk.Tables, soroswapSkimPairTable(sk.ByPair))
	blk.Notes = append(blk.Notes,
		"Skim amounts are summed soroswap_skim_events amounts (migration 0043) per pair, in that pair's native token base units — the excess pool balance a caller claimed above recorded reserves. They are never summed across pairs (each pair's token0/token1 is a different token). Skim is not a trade and never feeds VWAP.",
	)
	return nil
}

// soroswapSkimPairTableLimit caps the rendered per-pair skim table.
const soroswapSkimPairTableLimit = 25

// soroswapSkimPairTable renders the per-pair skim totals, most-skimmed first.
func soroswapSkimPairTable(pairs []SoroswapSkimPairTotal) BespokeTable {
	tbl := BespokeTable{
		Title:   "Skims by pair",
		Columns: []string{"Pair", "Skims", "Token0", "Token0 skimmed", "Token1", "Token1 skimmed"},
	}
	orDash := func(v string) string {
		if v == "" {
			return "—"
		}
		return v
	}
	for i, p := range pairs {
		if i == soroswapSkimPairTableLimit {
			break
		}
		tbl.Rows = append(tbl.Rows, []string{
			p.ContractID, strconv.FormatInt(p.Skims, 10),
			orDash(p.Token0), p.Amount0.String(),
			orDash(p.Token1), p.Amount1.String(),
		})
	}
	return tbl
}

func (s *Store) lendingEmissionKPIs(ctx context.Context, blk *BespokeBlock, windowDays int) error {
	em, err := s.BlendEmissionWindowStats(ctx, windowDays)
	if err != nil {
		return fmt.Errorf("timescale: bespokeLending emissions: %w", err)
	}
	if em == nil {
		return nil // no emission activity captured yet
	}
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("Emissions claimed (%dd)", windowDays), Value: blndUnits(em.ClaimVolume), Unit: "BLND", Hint: "summed blend_emissions claim amount (BLND, 7 decimals)"},
		BespokeKPI{Label: fmt.Sprintf("Claim events (%dd)", windowDays), Value: strconv.FormatInt(em.Claims, 10)},
		BespokeKPI{Label: fmt.Sprintf("Emission gulps (%dd)", windowDays), Value: strconv.FormatInt(em.Gulps, 10), Hint: "gulp + gulp_emissions accounting events in the window"},
		BespokeKPI{Label: fmt.Sprintf("Credit-risk events (%dd)", windowDays), Value: strconv.FormatInt(em.CreditRisk, 10), Hint: "bad_debt + defaulted_debt events — a genuine risk signal (unlike sorocredit's scheduled settlements)"},
	)
	blk.Notes = append(blk.Notes,
		"Emission figures are summed blend_emissions amounts (migration 0045) in BLND, not USD. Credit-risk events count bad_debt + defaulted_debt (a genuine risk signal); emissions claimed is a WINDOW sum, not an all-time total.",
	)
	return nil
}

// lendingPositionBlocks fills position KPIs, the per-asset net-position and per-pool tables, and the daily series. Count-first:
// blend_positions mixes tokens at per-asset decimals with no USD valuation, so cross-asset sums or a "Util %" would be meaningless;
// amount sums are scoped to a single asset.
func (s *Store) lendingPositionBlocks(ctx context.Context, blk *BespokeBlock, since string, windowDays int) error {
	var users, flashLoans string
	err := s.db.QueryRowContext(ctx, `
		SELECT
		  count(DISTINCT user_address)::text,
		  count(*) FILTER (WHERE event_kind = 'flash_loan')::text
		FROM blend_positions
		WHERE ledger_close_time > now() - $1::interval`, since).
		Scan(&users, &flashLoans)
	if err != nil {
		return fmt.Errorf("timescale: bespokeLending position KPIs: %w", err)
	}
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("Active users (%dd)", windowDays), Value: users, Hint: "distinct user addresses with at least one position event in the window"},
		BespokeKPI{Label: fmt.Sprintf("Flash loans (%dd)", windowDays), Value: flashLoans},
	)

	tbl, err := s.scanTable(ctx,
		BespokeTable{Title: "Net position by asset", Columns: []string{"Asset", "Net supplied", "Net borrowed", "Events"}},
		`SELECT asset,
		   COALESCE(sum(`+blendSupplyNetExpr+`),0)::text,
		   COALESCE(sum(`+blendBorrowNetExpr+`),0)::text,
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

	// Per-pool table is COUNT-only: a pool holds several assets, so a per-pool amount sum (or a
	// Util% ratio of two) mixes decimals across tokens. Side counts use the kind partition of
	// lendingSupplySideKinds / lendingBorrowSideKinds in bespoke_lending.go.
	poolTbl, err := s.scanTable(ctx,
		BespokeTable{Title: "Activity by pool", Columns: []string{"Pool", "Supply-side events", "Borrow-side events", "Users", "Events"}},
		`SELECT pool,
		   count(*) FILTER (WHERE event_kind IN (`+lendingSupplySideKinds+`))::text,
		   count(*) FILTER (WHERE event_kind IN (`+lendingBorrowSideKinds+`))::text,
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
		FROM blend_positions WHERE ledger_close_time > now() - $1::interval`+
		completeDaysOnly(windowDays, "ledger_close_time")+`
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
	// The `amount` column carries different units per event_kind: deposit/withdraw are underlying
	// tokens, queue_withdrawal/dequeue_withdrawal are backstop-LP shares, and
	// distribute/gulp_emissions/claim/donate are BLND emissions. Summing across kinds yields a
	// mixed-unit figure dominated by the most frequent kind, so this token-volume KPI sums only the
	// two token-denominated kinds. The event count stays unfiltered: it is a coherent tally.
	var backstopVol canonical.Amount
	var backstopCount string
	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(sum(amount) FILTER (WHERE event_kind IN ('deposit','withdraw')),0),
		       count(*)::text
		FROM blend_backstop_events WHERE ledger_close_time > now() - $1::interval`, since).
		Scan(&backstopVol, &backstopCount); err != nil {
		return fmt.Errorf("timescale: bespokeLending backstop KPIs: %w", err)
	}
	blk.KPIs = append(blk.KPIs,
		BespokeKPI{Label: fmt.Sprintf("Backstop volume (%dd)", windowDays), Value: blndUnits(backstopVol), Unit: "BLND:USDC LP", Hint: "summed deposit+withdraw backstop-token (Comet BLND:USDC LP, 7 decimals) amounts; excludes share/emission kinds; not USD"},
		BespokeKPI{Label: fmt.Sprintf("Backstop events (%dd)", windowDays), Value: backstopCount},
	)
	return nil
}

// blndUnits renders a 7-decimal token base-unit amount (BLND, Comet LP) as an exact decimal string.
func blndUnits(a canonical.Amount) string {
	return new(big.Rat).SetFrac(a.BigInt(), big.NewInt(10_000_000)).FloatString(7)
}
