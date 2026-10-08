package clickhouse

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// AccountCohortMinCreated is the creator-side coverage floor: a creator with fewer accounts
// is not rolled up (every sponsor is); the per-account pages serve that better and the API
// says "not covered" rather than "holds nothing".
const AccountCohortMinCreated = 10

// Cohort relations, the `rel` column's vocabulary and the API's
// ?relation= values.
const (
	CohortRelationCreated   = "created"
	CohortRelationSponsored = "sponsored"
)

// cohortScanSettings is the board rollups' budget with more threads: the cycle reads the whole
// movements archive daily. Measured on a 1M-ledger window (657M rows): the boards' argMax
// de-dup exceeded 8 GiB at 2 and 6 threads; FINAL at 6 threads took 151 s at 2.7 GiB. So the
// walk uses FINAL per window and every join spills rather than sizing the ~25M-row membership.
const cohortScanSettings = "SETTINGS max_threads = 6, max_memory_usage = 8589934592, " +
	"max_bytes_before_external_group_by = 4294967296, max_bytes_before_external_sort = 4294967296, " +
	"join_algorithm = 'grace_hash', grace_hash_join_initial_buckets = 16, " +
	"do_not_merge_across_partitions_select_final = 1"

// cohortWalkSettings bounds each window of the movements walk.
const cohortWalkSettings = cohortScanSettings + ", max_execution_time = 1800"

// cohortJoinSettings is for the one-shot joins against
// ledger_entries_current and account_activity: the same budget with more
// time, since they are not windowed.
const cohortJoinSettings = cohortScanSettings + ", max_execution_time = 3600"

// cohortFoldSettings bounds the folds over the cycle's own working
// tables.
const cohortFoldSettings = boundedScanSettings + ", max_execution_time = 1800"

// cohortStagingTables are the served tables the cycle rebuilds: truncated at the start,
// exchanged at the end, in this order.
var cohortStagingTables = []string{
	"account_cohort_roots",
	"account_cohort_holdings",
	"account_cohort_flows",
	"account_cohort_contracts",
	"account_cohort_activity",
	"account_cohort_positions",
	"defi_position_holders",
	"asset_month_usd_prices",
}

// cohortLoadedTables are the staging twins RunCohortRollup fills by batch insert before the
// statements run; their truncation is the loader's.
var cohortLoadedTables = map[string]bool{
	"defi_position_holders":  true,
	"asset_month_usd_prices": true,
}

// cohortRollupStatements is one cycle: membership, the joins needing no walk, the movements
// walk and its folds, the DeFi positions join (reads defi_position_holders_staging, loaded
// before these run), then the swap.
var cohortRollupStatements = []rollupStep{
	{sql: `TRUNCATE TABLE stellar.account_cohort_members`},
	{sql: `INSERT INTO stellar.account_cohort_members (rel, root, member)
	 SELECT '` + CohortRelationSponsored + `', sponsor, sponsored
	 FROM stellar.account_sponsor_edges
	 UNION ALL
	 SELECT '` + CohortRelationCreated + `', creator, created
	 FROM stellar.account_creator_edges
	 WHERE creator IN (
	     SELECT creator FROM stellar.account_creators_rollup
	     WHERE accounts_created >= ` + itoa(AccountCohortMinCreated) + `
	 )
	 ` + cohortFoldSettings},

	{sql: `TRUNCATE TABLE stellar.account_cohort_roots_staging`},
	{sql: `TRUNCATE TABLE stellar.account_cohort_holdings_staging`},
	{sql: `TRUNCATE TABLE stellar.account_cohort_flows_staging`},
	{sql: `TRUNCATE TABLE stellar.account_cohort_contracts_staging`},
	{sql: `TRUNCATE TABLE stellar.account_cohort_activity_staging`},
	{sql: `TRUNCATE TABLE stellar.account_cohort_positions_staging`},
	{sql: `TRUNCATE TABLE stellar.account_cohort_parts_staging`},

	// Current holdings: every live account + trustline entry of each member, re-keyed by root.
	// A pool-share trustline (asset 'pool:<hex>') rides along as a classic-side DeFi position.
	{sql: `INSERT INTO stellar.account_cohort_holdings_staging (rel, root, asset, holders, balance)
	 SELECT c.rel, c.root, e.asset,
	        toUInt64(count()) AS holders,
	        sum(toInt128(e.balance)) AS balance
	 FROM (
	     SELECT account_id, if(entry_type = 'account', 'native', asset) AS asset, balance
	     FROM stellar.ledger_entries_current FINAL
	     WHERE entry_type IN ('account', 'trustline') AND change_type != 'removed'
	 ) AS e
	 INNER JOIN stellar.account_cohort_members AS c ON e.account_id = c.member
	 GROUP BY c.rel, c.root, e.asset
	 ` + cohortJoinSettings},

	// Roots: one row per covered (rel, root). live_accounts is the 'native' holdings row (an
	// account entry exists iff the account is live).
	{sql: `INSERT INTO stellar.account_cohort_roots_staging
	     (rel, root, cohort_accounts, live_accounts, computed_at, tip_ledger)
	 SELECT m.rel, m.root, m.cohort_accounts, toUInt64(h.holders), now('UTC'),
	        (SELECT toUInt32(max(ledger_seq)) FROM stellar.ledgers)
	 FROM (
	     SELECT rel, root, toUInt64(uniqExact(member)) AS cohort_accounts
	     FROM stellar.account_cohort_members
	     GROUP BY rel, root
	 ) AS m
	 LEFT JOIN (
	     SELECT rel, root, holders FROM stellar.account_cohort_holdings_staging WHERE asset = 'native'
	 ) AS h ON m.rel = h.rel AND m.root = h.root
	 ` + cohortFoldSettings},

	// Recent activity from account_activity's last-seen watermark.
	{sql: `INSERT INTO stellar.account_cohort_activity_staging (rel, root, active_30d, active_90d, active_365d)
	 SELECT c.rel, c.root,
	        toUInt64(uniqExactIf(c.member, a.last_seen >= now('UTC') - INTERVAL 30 DAY)),
	        toUInt64(uniqExactIf(c.member, a.last_seen >= now('UTC') - INTERVAL 90 DAY)),
	        toUInt64(uniqExactIf(c.member, a.last_seen >= now('UTC') - INTERVAL 365 DAY))
	 FROM stellar.account_cohort_members AS c
	 INNER JOIN (
	     SELECT account_id, max(last_seen) AS last_seen
	     FROM stellar.account_activity
	     GROUP BY account_id
	 ) AS a ON c.member = a.account_id
	 GROUP BY c.rel, c.root
	 ` + cohortJoinSettings},

	// The movements walk. Each window reads the archive with FINAL (ReplacingMergeTree dedup per
	// partition; a 1M-ledger window is one partition), joins to membership, and writes one partial
	// row per (cohort, month, asset, C... counterparty) with a mergeable distinct-member state.
	// A month spans windows, so the fold below produces its figure. Not argMax: see
	// cohortScanSettings.
	{walk: true, windowBinds: 1, sql: `INSERT INTO stellar.account_cohort_parts_staging
	     (rel, root, month, asset, contract, inflow, outflow, movements, actives, first_at, last_at)
	 SELECT c.rel, c.root,
	        toStartOfMonth(m.closed_at) AS month,
	        m.asset,
	        if(startsWith(m.counterparty, 'C'), m.counterparty, '') AS contract,
	        sumIf(toInt128(m.amount), m.direction = '` + string(AccountMovementReceived) + `') AS inflow,
	        sumIf(toInt128(m.amount), m.direction = '` + string(AccountMovementSent) + `') AS outflow,
	        toUInt64(count()) AS movements,
	        uniqCombinedState(m.address) AS actives,
	        min(m.closed_at) AS first_at,
	        max(m.closed_at) AS last_at
	 FROM (
	     SELECT address, asset, amount, counterparty, direction,
	            toDateTime(ledger_close_time, 'UTC') AS closed_at
	     FROM stellar.account_movements FINAL
	     WHERE ledger BETWEEN ? AND ?
	 ) AS m
	 INNER JOIN stellar.account_cohort_members AS c ON m.address = c.member
	 GROUP BY c.rel, c.root, month, m.asset, contract
	 ` + cohortWalkSettings},

	// Fold: per-asset monthly flows …
	{sql: `INSERT INTO stellar.account_cohort_flows_staging
	     (rel, root, month, asset, inflow, outflow, movements, active_accounts)
	 SELECT rel, root, month, asset,
	        sum(toInt128(inflow)), sum(toInt128(outflow)),
	        toUInt64(sum(movements)), toUInt64(uniqCombinedMerge(actives))
	 FROM stellar.account_cohort_parts_staging
	 GROUP BY rel, root, month, asset
	 ` + cohortFoldSettings},
	// … plus one all-assets row per month (asset '*'): amounts are not summable across
	// units so they are zero; movements and distinct active members are the point.
	{sql: `INSERT INTO stellar.account_cohort_flows_staging
	     (rel, root, month, asset, inflow, outflow, movements, active_accounts)
	 SELECT rel, root, month, '` + CohortAllAssets + `',
	        toInt128(0), toInt128(0),
	        toUInt64(sum(movements)), toUInt64(uniqCombinedMerge(actives))
	 FROM stellar.account_cohort_parts_staging
	 GROUP BY rel, root, month
	 ` + cohortFoldSettings},
	// Fold: contracts the cohort moved value through.
	{sql: `INSERT INTO stellar.account_cohort_contracts_staging
	     (rel, root, contract_id, movements, active_accounts, first_at, last_at)
	 SELECT rel, root, contract,
	        toUInt64(sum(movements)), toUInt64(uniqCombinedMerge(actives)),
	        min(first_at), max(last_at)
	 FROM stellar.account_cohort_parts_staging
	 WHERE contract != ''
	 GROUP BY rel, root, contract
	 ` + cohortFoldSettings},

	// DeFi positions: the served tier's snapshot, joined to membership.
	{sql: `INSERT INTO stellar.account_cohort_positions_staging
	     (rel, root, protocol, position_kind, venue, asset, holders, amount)
	 SELECT c.rel, c.root, p.protocol, p.position_kind, p.venue, p.asset,
	        toUInt64(uniqExact(p.user)), sum(toInt256(p.amount))
	 FROM stellar.defi_position_holders_staging AS p
	 INNER JOIN stellar.account_cohort_members AS c ON p.user = c.member
	 GROUP BY c.rel, c.root, p.protocol, p.position_kind, p.venue, p.asset
	 ` + cohortFoldSettings},

	{sql: cohortExchangeSQL()},
}

// CohortAllAssets is the flows row standing for every asset: movements and distinct active
// members, amounts zero (not summable across units).
const CohortAllAssets = "*"

func itoa(n int) string { return strconv.Itoa(n) }

// isNoRows is the keyed-read miss: a root the cycle did not cover.
func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// cohortExchangeSQL swaps every staging twin live in one statement.
func cohortExchangeSQL() string {
	out := "EXCHANGE TABLES"
	for i, t := range cohortStagingTables {
		if i > 0 {
			out += ","
		}
		out += fmt.Sprintf(" stellar.%s_staging AND stellar.%s", t, t)
	}
	return out
}

// RunCohortRollup runs one cycle: loads the DeFi position snapshot and monthly USD prices
// into ClickHouse, then the statements above. Empty holders or prices swap in empty, which is
// the truth on a network with none.
func RunCohortRollup(ctx context.Context, addr string, holders []timescale.DeFiPositionHolder, prices []timescale.MonthlyUSDVWAP, logf func(format string, args ...any)) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	tip, err := lakeTipLedger(ctx, conn)
	if err != nil {
		return fmt.Errorf("clickhouse: cohort rollup: %w", err)
	}
	if err := loadDeFiPositionHolders(ctx, conn, holders); err != nil {
		return err
	}
	logf("defi position snapshot: %d rows staged", len(holders))
	if err := loadAssetMonthUSDPrices(ctx, conn, prices); err != nil {
		return err
	}
	logf("monthly usd prices: %d rows staged", len(prices))
	return runRollupSteps(ctx, conn, tip, "cohort rollup", cohortRollupStatements, logf)
}

// positionHolderAmount is h.Amount as a canonical base-10 integer, or an error: the fold sums
// exactly, so a fractional or empty amount fails the cycle (prior snapshot stays live) rather
// than being rounded or dropped from an exact total.
func positionHolderAmount(h timescale.DeFiPositionHolder) (string, error) {
	a, err := canonical.FromString(h.Amount)
	if err != nil || h.Amount == "" {
		return "", fmt.Errorf("clickhouse: cohort rollup: %s %s position of %s in %s: amount %q is not an integer: %w",
			h.Protocol, h.PositionKind, h.User, h.Venue, h.Amount, canonical.ErrInvalidAmount)
	}
	return a.String(), nil
}

// loadDeFiPositionHolders truncates and refills defi_position_holders_staging.
func loadDeFiPositionHolders(ctx context.Context, conn driver.Conn, holders []timescale.DeFiPositionHolder) error {
	if err := conn.Exec(ctx, `TRUNCATE TABLE stellar.defi_position_holders_staging`); err != nil {
		return fmt.Errorf("clickhouse: cohort rollup: truncate position snapshot: %w", err)
	}
	if len(holders) == 0 {
		return nil
	}
	b, err := conn.PrepareBatch(ctx, `INSERT INTO stellar.defi_position_holders_staging
		(protocol, position_kind, venue, asset, user, amount, last_ledger, snapshot_at)`)
	if err != nil {
		return fmt.Errorf("clickhouse: cohort rollup: prepare position snapshot: %w", err)
	}
	now := time.Now().UTC()
	for _, h := range holders {
		amount, err := positionHolderAmount(h)
		if err != nil {
			return err
		}
		if err := b.Append(h.Protocol, h.PositionKind, h.Venue, h.Asset, h.User, amount, h.LastLedger, now); err != nil {
			return fmt.Errorf("clickhouse: cohort rollup: append position snapshot: %w", err)
		}
	}
	if err := b.Send(); err != nil {
		return fmt.Errorf("clickhouse: cohort rollup: send position snapshot: %w", err)
	}
	return nil
}

// loadAssetMonthUSDPrices truncates and refills asset_month_usd_prices_staging with per-month
// USD VWAPs (timescale.Store.MonthlyUSDVWAPs), which readCohortFlows joins to price a month.
func loadAssetMonthUSDPrices(ctx context.Context, conn driver.Conn, prices []timescale.MonthlyUSDVWAP) error {
	if err := conn.Exec(ctx, `TRUNCATE TABLE stellar.asset_month_usd_prices_staging`); err != nil {
		return fmt.Errorf("clickhouse: cohort rollup: truncate month prices: %w", err)
	}
	if len(prices) == 0 {
		return nil
	}
	b, err := conn.PrepareBatch(ctx, `INSERT INTO stellar.asset_month_usd_prices_staging
		(asset, month, vwap_usd, volume_usd)`)
	if err != nil {
		return fmt.Errorf("clickhouse: cohort rollup: prepare month prices: %w", err)
	}
	for _, p := range prices {
		if err := b.Append(p.Asset, p.Month.UTC(), p.VWAPUSD, p.VolumeUSD); err != nil {
			return fmt.Errorf("clickhouse: cohort rollup: append month prices: %w", err)
		}
	}
	if err := b.Send(); err != nil {
		return fmt.Errorf("clickhouse: cohort rollup: send month prices: %w", err)
	}
	return nil
}

// ── reads ──────────────────────────────────────────────────────────────

// AccountCohort is one root's cohort as the last cycle left it.
type AccountCohort struct {
	Relation string
	Root     string
	// Covered is false when the root is not in the rollup (creator under the floor, or no edges
	// in this relation); other fields are then empty, Cycle still set once a cycle completed.
	Covered bool
	Cycle   AccountCohortCycle

	CohortAccounts uint64
	LiveAccounts   uint64
	Activity       AccountCohortActivity
	Holdings       []AccountCohortHolding
	Flows          []AccountCohortFlow // ascending by month, then asset
	Contracts      []AccountCohortContract
	Positions      []AccountCohortPosition
	// FlowPricesUnavailable is true when Flows was served without then-prices because the
	// OPTIONAL asset_month_usd_prices table is absent; PriceUSDThen is then nil on every row.
	FlowPricesUnavailable bool
}

// AccountCohortCycle dates the snapshot every figure comes from.
type AccountCohortCycle struct {
	ComputedAt time.Time
	TipLedger  uint32
}

// AccountCohortActivity is how many members were seen recently.
type AccountCohortActivity struct {
	Active30d  uint64
	Active90d  uint64
	Active365d uint64
}

// AccountCohortHolding is the cohort's current position in one asset.
type AccountCohortHolding struct {
	Asset   string   // canonical id: native | CODE-ISSUER | pool:<hex>
	Holders uint64   // members holding it
	Balance *big.Int // stroops
}

// AccountCohortFlow is one month of movement in one asset. Asset
// CohortAllAssets is the month's all-assets row (amounts zero).
type AccountCohortFlow struct {
	Month          time.Time
	Asset          string
	Inflow         *big.Int
	Outflow        *big.Int
	Movements      uint64
	ActiveAccounts uint64
	// PriceUSDThen is the asset's volume-weighted USD price for this month on the index's own
	// markets (joined at read time); nil where unpriced. Never zero.
	PriceUSDThen *string
}

// AccountCohortContract is one C… counterparty the cohort moved value
// through.
type AccountCohortContract struct {
	ContractID     string
	Movements      uint64
	ActiveAccounts uint64
	FirstAt        time.Time
	LastAt         time.Time
}

// AccountCohortPosition is the cohort's aggregate in one DeFi venue.
type AccountCohortPosition struct {
	Protocol     string
	PositionKind string
	Venue        string
	Asset        string
	Holders      uint64
	Amount       *big.Int // exact sum in the fold's own unit
}

// Read caps: an exchange's sponsored set touches thousands of assets; the caps keep one read
// one screen without hiding that a cap applied (Truncated* on the view).
// CohortHoldingsLimit is exported so the API can say a cap applied.
const CohortHoldingsLimit = 400

// CohortFlowAssetsLimit is exported so the API can say a cap applied.
const CohortFlowAssetsLimit = 12

const (
	cohortContractsLimit = 60
	cohortPositionsLimit = 120
)

// AccountCohort reads one root's cohort. ok=false (not an error): no cycle has completed yet;
// a completed cycle not covering the root gives ok=true, Covered=false.
func (r *ExplorerReader) AccountCohort(ctx context.Context, account, relation string) (AccountCohort, bool, error) {
	out := AccountCohort{Relation: relation, Root: account}
	var computed time.Time
	var tip uint32
	if err := r.conn.QueryRow(ctx, `
		SELECT max(computed_at), toUInt32(max(tip_ledger)) FROM stellar.account_cohort_roots`).
		Scan(&computed, &tip); err != nil {
		return out, false, fmt.Errorf("clickhouse: account cohort cycle: %w", err)
	}
	if computed.IsZero() || computed.Unix() <= 0 {
		return out, false, nil
	}
	out.Cycle = AccountCohortCycle{ComputedAt: computed.UTC(), TipLedger: tip}

	row := r.conn.QueryRow(ctx, `
		SELECT cohort_accounts, live_accounts, computed_at, tip_ledger
		FROM stellar.account_cohort_roots
		WHERE rel = ? AND root = ?`, relation, account)
	var rootComputed time.Time
	var rootTip uint32
	if err := row.Scan(&out.CohortAccounts, &out.LiveAccounts, &rootComputed, &rootTip); err != nil {
		if isNoRows(err) {
			return out, true, nil
		}
		return out, false, fmt.Errorf("clickhouse: account cohort root: %w", err)
	}
	out.Covered = true
	out.Cycle = AccountCohortCycle{ComputedAt: rootComputed.UTC(), TipLedger: rootTip}

	for _, step := range []func(context.Context, *AccountCohort) error{
		r.readCohortActivity, r.readCohortHoldings, r.readCohortFlows, r.readCohortContracts, r.readCohortPositions,
	} {
		if err := step(ctx, &out); err != nil {
			return out, false, err
		}
	}
	return out, true, nil
}

func (r *ExplorerReader) readCohortActivity(ctx context.Context, out *AccountCohort) error {
	err := r.conn.QueryRow(ctx, `
		SELECT active_30d, active_90d, active_365d
		FROM stellar.account_cohort_activity
		WHERE rel = ? AND root = ?`, out.Relation, out.Root).
		Scan(&out.Activity.Active30d, &out.Activity.Active90d, &out.Activity.Active365d)
	if err != nil && !isNoRows(err) {
		return fmt.Errorf("clickhouse: account cohort activity: %w", err)
	}
	return nil
}

func (r *ExplorerReader) readCohortHoldings(ctx context.Context, out *AccountCohort) error {
	rows, err := r.conn.Query(ctx, `
		SELECT asset, holders, balance
		FROM stellar.account_cohort_holdings
		WHERE rel = ? AND root = ?
		ORDER BY holders DESC, asset
		LIMIT ?`, out.Relation, out.Root, CohortHoldingsLimit)
	if err != nil {
		return fmt.Errorf("clickhouse: account cohort holdings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var h AccountCohortHolding
		var bal big.Int
		if err := rows.Scan(&h.Asset, &h.Holders, &bal); err != nil {
			return fmt.Errorf("clickhouse: scan account cohort holding: %w", err)
		}
		h.Balance = &bal
		out.Holdings = append(out.Holdings, h)
	}
	return rows.Err()
}

// cohortFlowsSQL and cohortFlowsFallbackSQL differ only in price_usd_then: the fallback drops
// the LEFT JOIN and literals it empty (read as nil) so one Scan serves both. Used when the
// OPTIONAL asset_month_usd_prices is absent, which must not take the flows read down.
const cohortFlowsSQL = `
		SELECT f.month, f.asset, f.inflow, f.outflow, f.movements, f.active_accounts,
		       ifNull(p.vwap_usd, '') AS price_usd_then
		FROM stellar.account_cohort_flows AS f
		LEFT JOIN stellar.asset_month_usd_prices AS p ON p.asset = f.asset AND p.month = f.month
		WHERE f.rel = ? AND f.root = ?
		  AND (f.asset = ? OR f.asset IN (
		      SELECT asset FROM stellar.account_cohort_flows
		      WHERE rel = ? AND root = ? AND asset != ?
		      GROUP BY asset
		      ORDER BY sum(movements) DESC, asset
		      LIMIT ?
		  ))
		ORDER BY f.month, f.asset`

const cohortFlowsFallbackSQL = `
		SELECT f.month, f.asset, f.inflow, f.outflow, f.movements, f.active_accounts,
		       '' AS price_usd_then
		FROM stellar.account_cohort_flows AS f
		WHERE f.rel = ? AND f.root = ?
		  AND (f.asset = ? OR f.asset IN (
		      SELECT asset FROM stellar.account_cohort_flows
		      WHERE rel = ? AND root = ? AND asset != ?
		      GROUP BY asset
		      ORDER BY sum(movements) DESC, asset
		      LIMIT ?
		  ))
		ORDER BY f.month, f.asset`

// readCohortFlows serves every month for the CohortFlowAssetsLimit assets moved most, plus the
// all-assets row. Assets past the cap are not summed into "other" (units differ); the view
// says so (assets_truncated). Each row carries the month's USD price via a LEFT JOIN on
// (asset, month); the empty string is the miss, read as nil.
func (r *ExplorerReader) readCohortFlows(ctx context.Context, out *AccountCohort) error {
	rows, err := r.conn.Query(ctx, cohortFlowsSQL,
		out.Relation, out.Root, CohortAllAssets, out.Relation, out.Root, CohortAllAssets, CohortFlowAssetsLimit)
	if err != nil && isSchemaAbsent(err) {
		out.FlowPricesUnavailable = true
		rows, err = r.conn.Query(ctx, cohortFlowsFallbackSQL,
			out.Relation, out.Root, CohortAllAssets, out.Relation, out.Root, CohortAllAssets, CohortFlowAssetsLimit)
	}
	if err != nil {
		return fmt.Errorf("clickhouse: account cohort flows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var f AccountCohortFlow
		var in, outAmt big.Int
		var priceThen string
		if err := rows.Scan(&f.Month, &f.Asset, &in, &outAmt, &f.Movements, &f.ActiveAccounts, &priceThen); err != nil {
			return fmt.Errorf("clickhouse: scan account cohort flow: %w", err)
		}
		f.Month = f.Month.UTC()
		f.Inflow, f.Outflow = &in, &outAmt
		if priceThen != "" {
			f.PriceUSDThen = &priceThen
		}
		out.Flows = append(out.Flows, f)
	}
	return rows.Err()
}

func (r *ExplorerReader) readCohortContracts(ctx context.Context, out *AccountCohort) error {
	rows, err := r.conn.Query(ctx, `
		SELECT contract_id, movements, active_accounts, first_at, last_at
		FROM stellar.account_cohort_contracts
		WHERE rel = ? AND root = ?
		ORDER BY active_accounts DESC, movements DESC, contract_id
		LIMIT ?`, out.Relation, out.Root, cohortContractsLimit)
	if err != nil {
		return fmt.Errorf("clickhouse: account cohort contracts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var c AccountCohortContract
		if err := rows.Scan(&c.ContractID, &c.Movements, &c.ActiveAccounts, &c.FirstAt, &c.LastAt); err != nil {
			return fmt.Errorf("clickhouse: scan account cohort contract: %w", err)
		}
		c.FirstAt, c.LastAt = c.FirstAt.UTC(), c.LastAt.UTC()
		out.Contracts = append(out.Contracts, c)
	}
	return rows.Err()
}

func (r *ExplorerReader) readCohortPositions(ctx context.Context, out *AccountCohort) error {
	rows, err := r.conn.Query(ctx, `
		SELECT protocol, position_kind, venue, asset, holders, amount
		FROM stellar.account_cohort_positions
		WHERE rel = ? AND root = ?
		ORDER BY holders DESC, protocol, venue, asset, position_kind
		LIMIT ?`, out.Relation, out.Root, cohortPositionsLimit)
	if err != nil {
		return fmt.Errorf("clickhouse: account cohort positions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p AccountCohortPosition
		var amount big.Int
		if err := rows.Scan(&p.Protocol, &p.PositionKind, &p.Venue, &p.Asset, &p.Holders, &amount); err != nil {
			return fmt.Errorf("clickhouse: scan account cohort position: %w", err)
		}
		p.Amount = &amount
		out.Positions = append(out.Positions, p)
	}
	return rows.Err()
}
