package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sourcenet"
)

// GapDetectorTarget identifies one (source, table) pair the gap detector scans for ledger-coverage gaps; the table label disambiguates a multi-hypertable source and LedgerColumn is normally "ledger".
type GapDetectorTarget struct {
	Source       string
	Table        string
	LedgerColumn string

	// CanonicalSource is the [sourcenet] source name for targets keyed by a per-TABLE name ("aquarius-liquidity" -> "aquarius"). Empty
	// means Source is already canonical. The detector asks sourcenet whether the source exists on this network;
	// [TestGapDetectorTargetsResolveToKnownSources] fails when a target resolves to a name sourcenet does not know, which on a test net
	// would silently mean EXCLUDED.
	CanonicalSource string

	// Genesis is the first ledger at which this source could have data: the contract deploy ledger for Soroban sources, 2 for SDEX.
	// Density is distinct / (tip - Genesis + 1). Literal because storage may not import internal/sources (lint-imports
	// L/storage-below-compute); TestCatalogueGenesisLocksStepWithGapDetectorTargets pins each floor to the catalogue and to any exported
	// package constant.
	Genesis int64

	// WhereFilter is an optional predicate ANDed into the gap query (without the leading "AND", e.g. `source = 'sdex'`) to scan one
	// source's slice of a shared table. SAFETY: interpolated like Table and LedgerColumn, so it must come only from
	// [DefaultGapDetectorTargets] (ADR-0030).
	WhereFilter string

	// MinGapSizeOverride overrides [GapDetectorMinGapSize] (default 1000 ledgers) for this target; sparse sources (Blend, CCTP/Rozo) would
	// otherwise page on every quiet stretch. It is alerting cadence only and can mask a gap just under the threshold, so it is not a
	// coverage signal; the completeness watermark (completeness_snapshots) proves quiet vs gap.
	MinGapSizeOverride int64

	// ScanCadence overrides [GapDetectorInterval] (default 30 min) for huge tables where the LAG-DISTINCT scan is slow, trading signal
	// freshness for not saturating postgres. Each cycle stays bounded by a 15 min Go timeout and the SQL `SET LOCAL statement_timeout`
	// backstop ([gapDetectorStatementTimeoutMS], 13 min).
	ScanCadence time.Duration

	// DistinctLedgerCountSQL optionally replaces the generic COUNT(DISTINCT <LedgerColumn>) that [Store.CountDistinctLedgers] runs. When
	// set it MUST be a complete statement binding $1/$2 as the inclusive ledger range and returning one bigint, because the generic query
	// full-scans a table with no leading-`ledger` index. Interpolated verbatim, so the compile-time-only rule of Table / WhereFilter
	// applies.
	DistinctLedgerCountSQL string

	// CloseTimeColumn names the close-time partition column of a hypertable partitioned by time rather than ledger. The gap scan also
	// bounds it by the close times ledger_ingest_log records around [from, to]; a bare `ledger BETWEEN` excludes no chunk and decompresses
	// every compressed one. Same ADR-0030 discipline as Table.
	CloseTimeColumn string
}

// sorobanEventsDistinctLedgerCountSQL counts ledgers RECORDED as carrying events via ledger_ingest_log (PK range scan), not rows present, so
// density reads HIGH during a writer halt and 0 for a range repaired without `census-backfill`. Ledgers below the log floor read 0.
const sorobanEventsDistinctLedgerCountSQL = `SELECT COUNT(*) FROM ledger_ingest_log WHERE ledger_seq BETWEEN $1 AND $2 AND soroban_event_count > 0`

// SourceNetKey is the [sourcenet] name for this target — its [GapDetectorTarget.CanonicalSource] when set, else its Source.
func (t GapDetectorTarget) SourceNetKey() string {
	if t.CanonicalSource != "" {
		return t.CanonicalSource
	}
	return t.Source
}

// EffectiveMinGapSize returns the threshold this target should use, preferring [MinGapSizeOverride] if non-zero.
func (t GapDetectorTarget) EffectiveMinGapSize() int64 {
	if t.MinGapSizeOverride > 0 {
		return t.MinGapSizeOverride
	}
	return GapDetectorMinGapSize
}

// EffectiveScanCadence returns the per-target scan cadence, preferring [ScanCadence] if non-zero.
func (t GapDetectorTarget) EffectiveScanCadence() time.Duration {
	if t.ScanCadence > 0 {
		return t.ScanCadence
	}
	return GapDetectorInterval
}

// DefaultGapDetectorTargets is the set of per-source hypertables scanned every cycle; a migration creating a per-source hypertable MUST add a target (TestGapDetectorTargetsCoverMigrations fails CI otherwise).
var DefaultGapDetectorTargets = []GapDetectorTarget{
	// SEP-41 tokens have no single deploy ledger, so the floor is the Soroban era start (pubnet
	// L50,457,424); nothing earlier can emit SEP-41 events.
	{Source: "sep41-transfers", CanonicalSource: "sep41_transfers", Table: "sep41_transfers", LedgerColumn: "ledger", Genesis: 50_457_424},
	// SEP-41 supply events fire only on mint/burn/clawback — much rarer than
	// transfers.
	{Source: "sep41-supply", CanonicalSource: "sep41_supply", Table: "sep41_supply_events", LedgerColumn: "ledger", Genesis: 50_457_424, MinGapSizeOverride: 100000},
	// CCTP / Rozo are sparse bridges whose natural quiet windows exceed 100K ledgers, so 200K sits
	// above the envelope. This detector sees missing ledgers only, not same-ledger row loss.
	{Source: "cctp", Table: "cctp_events", LedgerColumn: "ledger", Genesis: 62_146_641, MinGapSizeOverride: 200000},
	{Source: "rozo", Table: "rozo_events", LedgerColumn: "ledger", Genesis: 60_829_397, MinGapSizeOverride: 100000},
	// comet_liquidity: pool events are sparse (max natural gap 7826 ledgers, ~11h); 50K threshold.
	{Source: "comet-liquidity", CanonicalSource: "comet", Table: "comet_liquidity", LedgerColumn: "ledger", Genesis: 51_499_546, MinGapSizeOverride: 50000},
	// blend-emitter: emissions plumbing (distribute / drop / q_swap / swap), mostly one-off admin
	// actions. 200K sits above the observed envelope, matching cctp's similarly sparse table.
	{Source: "blend-emitter", CanonicalSource: "blend_emitter", Table: "blend_emitter_events", LedgerColumn: "ledger", Genesis: 51_499_914, MinGapSizeOverride: 200000},
	// Completeness shows every soroswap skim is captured, so the ~564K-ledger quiet stretches are
	// genuine rarity, not a decoder gap; 700K sits above the observed envelope.
	{Source: "soroswap-skim", CanonicalSource: "soroswap", Table: "soroswap_skim_events", LedgerColumn: "ledger", Genesis: 50_746_266, MinGapSizeOverride: 700000},
	// soroswap-liquidity: LP add/remove is sparse (single digits per day, like skim), so a large
	// override keeps a quiet stretch from reading as a decoder outage.
	{Source: "soroswap-liquidity", CanonicalSource: "soroswap", Table: "soroswap_liquidity", LedgerColumn: "ledger", Genesis: 50_746_266, MinGapSizeOverride: 700000},
	// soroswap-router: the router emits no Soroban events, so invocations arrive via
	// dispatcher.ContractCallDecoder and the gap signal lives on soroswap_router_swaps.
	{Source: "soroswap-router", Table: "soroswap_router_swaps", LedgerColumn: "ledger", Genesis: 50_746_272, MinGapSizeOverride: 100000},
	// defindex: strategy + vault layers both emit on capital movement, but the protocol is young
	// and user-triggered, so multi-hour quiet windows happen. 100k (~5.8 days) matches
	// soroswap-router: past natural sparsity, well below "writer wedged".
	{Source: "defindex", Table: "defindex_flows", LedgerColumn: "ledger", Genesis: 55_484_403, MinGapSizeOverride: 100000},
	// defindex-fees: dfees distributions fire only when a vault has fees pending, so quiet
	// stretches are normal; 700K matches the other sparse fee tables. Genesis is the first dfees
	// event, NOT the source genesis: the topic never fired before 60,903,337, and a 57M floor
	// would flag ~3.8M permanently empty ledgers.
	{Source: "defindex-fees", CanonicalSource: "defindex", Table: "defindex_fees", LedgerColumn: "ledger", Genesis: 60_903_337, MinGapSizeOverride: 700000},
	// defindex-admin-events: rescues, pause toggles and role rotations are so rare that gaps are
	// the norm; max override like phoenix-admin-events.
	{Source: "defindex-admin-events", CanonicalSource: "defindex", Table: "defindex_admin_events", LedgerColumn: "ledger", Genesis: 57_056_338, MinGapSizeOverride: 100000000},
	// phoenix-liquidity / phoenix-stake: events are user-triggered, so quiet windows are normal
	// behaviour, not loss. Natural max gaps reached ~144K ledgers with completeness verified;
	// 200K sits above that envelope.
	{Source: "phoenix-liquidity", CanonicalSource: "phoenix", Table: "phoenix_liquidity", LedgerColumn: "ledger", Genesis: 51_572_016, MinGapSizeOverride: 200000},
	// phoenix-initialize: per-pool-deploy announcements are VERY sparse, so long quiet stretches
	// are normal; huge override.
	{Source: "phoenix-initialize", CanonicalSource: "phoenix", Table: "phoenix_initialize", LedgerColumn: "ledger", Genesis: 51_572_016, MinGapSizeOverride: 2000000},
	// phoenix-admin-events: admin rotations are defensive and may never occur. Max override so an
	// empty table never alerts; "no rows yet" is not an outage.
	{Source: "phoenix-admin-events", CanonicalSource: "phoenix", Table: "phoenix_admin_events", LedgerColumn: "ledger", Genesis: 51_572_016, MinGapSizeOverride: 100000000},
	{Source: "phoenix-stake", CanonicalSource: "phoenix", Table: "phoenix_stake_events", LedgerColumn: "ledger", Genesis: 51_572_016, MinGapSizeOverride: 200000},
	// aquarius-liquidity: user-triggered and sparse, so a wide override matches phoenix-liquidity.
	// Genesis is the Aquarius pool deploy ledger, as for the aquarius trades target.
	{Source: "aquarius-liquidity", CanonicalSource: "aquarius", Table: "aquarius_liquidity", LedgerColumn: "ledger", Genesis: 52_728_375, MinGapSizeOverride: 200000},
	// aquarius-reserves: update_reserves fires on EVERY state-changing pool op, so it is as dense
	// as aquarius trades and going quiet is a strong writer-halt tripwire; 100K matches trades.
	{Source: "aquarius-reserves", CanonicalSource: "aquarius", Table: "aquarius_reserves", LedgerColumn: "ledger", Genesis: 52_728_375, MinGapSizeOverride: 100000},
	// aquarius-reserves-sync: reserves_sync is sparse, so a large override keeps a quiet stretch
	// from reading as a decoder outage.
	{Source: "aquarius-reserves-sync", CanonicalSource: "aquarius", Table: "aquarius_reserves_sync", LedgerColumn: "ledger", Genesis: 52_728_375, MinGapSizeOverride: 700000},
	// aquarius-protocol-fee: claims are bursty and set_protocol_fee is very rare, so a quiet
	// stretch is normal; large override.
	{Source: "aquarius-protocol-fee", CanonicalSource: "aquarius", Table: "aquarius_protocol_fee", LedgerColumn: "ledger", Genesis: 52_728_375, MinGapSizeOverride: 700000},
	// aquarius-kill-switches: circuit-breaker toggles are VERY rare, so a multi-month gap is
	// normal, not an outage; huge override.
	{Source: "aquarius-kill-switches", CanonicalSource: "aquarius", Table: "aquarius_kill_switches", LedgerColumn: "ledger", Genesis: 52_728_375, MinGapSizeOverride: 2000000},
	// aquarius-rewards: dominated by pool_state, one of the densest Aquarius topics, though some
	// admin-triggered kinds are sparse. 100K matches aquarius-reserves; pool_state's density means
	// a real writer halt trips well before a natural quiet stretch would.
	{Source: "aquarius-rewards", CanonicalSource: "aquarius", Table: "aquarius_rewards_events", LedgerColumn: "ledger", Genesis: 52_728_375, MinGapSizeOverride: 100000},
	// aquarius-admin: router-scoped, operator-triggered governance actions (upgrades, ownership
	// transfers, emergency mode) are rare by design; wide override matches blend-admin.
	{Source: "aquarius-admin", CanonicalSource: "aquarius", Table: "aquarius_admin", LedgerColumn: "ledger", Genesis: 52_728_375, MinGapSizeOverride: 1300000},
	// The observed max natural gap (53515) sat just over 50K and paged, so the override is 100K.
	{Source: "blend-auctions", CanonicalSource: "blend", Table: "blend_auctions", LedgerColumn: "ledger", Genesis: 51_499_546, MinGapSizeOverride: 100000},
	// blend_positions: live ingest started late, so the max gap is pre-history boundary plus
	// natural sparsity; 50K threshold.
	{Source: "blend-positions", CanonicalSource: "blend", Table: "blend_positions", LedgerColumn: "ledger", Genesis: 51_499_546, MinGapSizeOverride: 50000},
	// Completeness shows blend complete, so the long gaps are genuine rarity; the 1.3M override
	// sits above them, and real loss is caught by completeness_snapshots, not this threshold.
	{Source: "blend-emissions", CanonicalSource: "blend", Table: "blend_emissions", LedgerColumn: "ledger", Genesis: 51_499_546, MinGapSizeOverride: 100000},
	{Source: "blend-admin", CanonicalSource: "blend", Table: "blend_admin", LedgerColumn: "ledger", Genesis: 51_499_546, MinGapSizeOverride: 1300000},
	// blend-backstop: events are episodic, so a wide override avoids paging on natural quiet.
	// Genesis is blend_backstop.BackstopGenesisLedger; V1 events start at 51_499_926, so a later
	// floor would hide that era.
	{Source: "blend-backstop", CanonicalSource: "blend_backstop", Table: "blend_backstop_events", LedgerColumn: "ledger", Genesis: 51_499_546, MinGapSizeOverride: 100000},
	// sorocredit: four tables, Genesis is the main contract's first event. Wide override because
	// the historical back-window fills only via projector-replay, so a large threshold avoids
	// paging on the empty pre-fill range. See internal/sources/sorocredit/README.md.
	{Source: "sorocredit-positions", CanonicalSource: "sorocredit", Table: "credit_positions", LedgerColumn: "ledger", Genesis: 61_620_822, MinGapSizeOverride: 200000},
	{Source: "sorocredit-statements", CanonicalSource: "sorocredit", Table: "credit_statements", LedgerColumn: "ledger", Genesis: 61_620_822, MinGapSizeOverride: 200000},
	{Source: "sorocredit-settlements", CanonicalSource: "sorocredit", Table: "credit_settlements", LedgerColumn: "ledger", Genesis: 61_620_822, MinGapSizeOverride: 200000},
	{Source: "sorocredit-events", CanonicalSource: "sorocredit", Table: "credit_events", LedgerColumn: "ledger", Genesis: 61_620_822, MinGapSizeOverride: 200000},
	// soroban-events spans the whole Soroban era. The long ScanCadence is because a 50M+ row scan
	// holds postgres for 5+ min and would starve trade inserts at 30 min; it is chunk-pruned via
	// CloseTimeColumn.
	{Source: "soroban-events", CanonicalSource: "soroban_events", Table: "soroban_events", LedgerColumn: "ledger", Genesis: 50_457_424, ScanCadence: 6 * time.Hour, MinGapSizeOverride: 100000, DistinctLedgerCountSQL: sorobanEventsDistinctLedgerCountSQL, CloseTimeColumn: "ledger_close_time"},
	// SDEX does not flow through soroban_events; WhereFilter slices it out of the shared `trades`
	// hypertable. Natural gaps reach ~575K ledgers (hence 1M), and tens of millions of rows need
	// the long ScanCadence.
	{Source: "sdex", Table: "trades", LedgerColumn: "ledger", WhereFilter: "source = 'sdex'", Genesis: 2, MinGapSizeOverride: 1000000, ScanCadence: 6 * time.Hour},
	// No target for sdex_offer_events: nothing writes it, so it would report a permanent
	// genesis-to-tip gap. Soroban-DEX sources also write into `trades`; each needs a source-tagged
	// WhereFilter target or `backfill_coverage` reports 0% for it.
	{Source: "aquarius", Table: "trades", LedgerColumn: "ledger", WhereFilter: "source = 'aquarius'", Genesis: 52_728_375, MinGapSizeOverride: 100000},
	{Source: "soroswap", Table: "trades", LedgerColumn: "ledger", WhereFilter: "source = 'soroswap'", Genesis: 50_746_266, MinGapSizeOverride: 100000},
	{Source: "phoenix", Table: "trades", LedgerColumn: "ledger", WhereFilter: "source = 'phoenix'", Genesis: 51_572_016, MinGapSizeOverride: 100000},
	// comet: Balancer-v1 pool swaps are sparse.
	{Source: "comet", Table: "trades", LedgerColumn: "ledger", WhereFilter: "source = 'comet'", Genesis: 51_499_546, MinGapSizeOverride: 200000},
	// sushiswap_v3: 100k matches the other DEXs. The widest natural gap is 32,512 ledgers (at
	// launch); every later one is under 9,400, so 100k leaves 3x headroom.
	{Source: "sushiswap_v3", Table: "trades", LedgerColumn: "ledger", WhereFilter: "source = 'sushiswap_v3'", Genesis: 61_487_379, MinGapSizeOverride: 100000},
	// sushiswap_v3-positions: mint/burn/collect are sparse, so a large override keeps a quiet
	// stretch from reading as a decoder outage.
	{Source: "sushiswap_v3-positions", CanonicalSource: "sushiswap_v3", Table: "sushiswap_v3_position_events", LedgerColumn: "ledger", Genesis: 61_487_379, MinGapSizeOverride: 700000},
	// upshift: vaults publish no price, so they use their own hypertable, not `trades`. Genesis is
	// the first event (an `admin_set`, which produces no row); the first ROW lands 15,341 ledgers
	// later, far inside the threshold.
	{Source: "upshift", Table: "upshift_vault_events", LedgerColumn: "ledger", Genesis: 62_623_313, MinGapSizeOverride: 600000},
	// spectra: own hypertable, no price. Markets are few and weeks apart;
	// 600k (~40 days) is a starting bound to revisit after the backfill.
	{Source: "spectra", Table: "spectra_events", LedgerColumn: "ledger", Genesis: 63_778_088, MinGapSizeOverride: 600000},
	// Oracle sources (reflector, band, redstone) share `oracle_updates`, sliced per source by
	// WhereFilter like the trades targets. Genesis is each oracle's pubnet deploy ledger.
	{Source: "band", Table: "oracle_updates", LedgerColumn: "ledger", WhereFilter: "source = 'band'", Genesis: 50_842_736, MinGapSizeOverride: 100000},
	{Source: "chainlink-stellar", Table: "oracle_updates", LedgerColumn: "ledger", WhereFilter: "source = 'chainlink-stellar'", Genesis: 64_442_968, MinGapSizeOverride: 100000},
	{Source: "redstone", Table: "oracle_updates", LedgerColumn: "ledger", WhereFilter: "source = 'redstone'", Genesis: 58_758_722, MinGapSizeOverride: 100000},
	{Source: "reflector-dex", Table: "oracle_updates", LedgerColumn: "ledger", WhereFilter: "source = 'reflector-dex'", Genesis: 50_644_229, MinGapSizeOverride: 100000},
	{Source: "reflector-cex", Table: "oracle_updates", LedgerColumn: "ledger", WhereFilter: "source = 'reflector-cex'", Genesis: 50_644_239, MinGapSizeOverride: 100000},
	{Source: "reflector-fx", Table: "oracle_updates", LedgerColumn: "ledger", WhereFilter: "source = 'reflector-fx'", Genesis: 56_733_481, MinGapSizeOverride: 100000},
	// defindex + soroswap-router are intentionally NOT registered here: as log-only sinks they bump
	// source_entry_counts without ledger-keyed rows, which the LAG scan needs, so coverage % stays
	// n/a on the API listing and the entry count is shown instead.
}

// ApplicableGapDetectorTargets drops targets whose source does not exist on this network and returns the rest in scan order, logging the exclusion set once.
func ApplicableGapDetectorTargets(targets []GapDetectorTarget, network string, logger *slog.Logger) []GapDetectorTarget {
	out := make([]GapDetectorTarget, 0, len(targets))
	var excluded []string
	for _, t := range targets {
		if ok, _ := sourcenet.Applicable(t.SourceNetKey(), network); ok {
			out = append(out, t)
			continue
		}
		excluded = append(excluded, t.Source)
	}
	if len(excluded) > 0 && logger != nil {
		logger.Info("gap-detector: sources not applicable on this network are not scanned",
			"network", network,
			"excluded", strings.Join(excluded, ","),
			"scanned", len(out),
			"registered", len(targets))
	}
	return out
}

// perSourceLedgerGapsQuery builds the LAG()-over-DISTINCT gap query. The `seeded` CTE adds a synthetic row at $4 (last ledger the previous
// cycle saw) when positive and below $1, else a gap spanning a window boundary is invisible. timeBound comes from [closeTimeBoundPredicate].
func perSourceLedgerGapsQuery(target GapDetectorTarget, timeBound string) string {
	filter := timeBound
	if target.WhereFilter != "" {
		filter += " AND (" + target.WhereFilter + ")"
	}
	//nolint:gosec // G201: identifiers from compile-time const list per ADR-0030
	return fmt.Sprintf(`
		WITH ledgers AS (
		    SELECT DISTINCT %[1]s AS ledger
		    FROM %[2]s
		    WHERE %[1]s BETWEEN $1 AND $2%[3]s
		),
		seeded AS (
		    SELECT ledger FROM ledgers
		    UNION ALL
		    SELECT $4::bigint WHERE $4::bigint > 0 AND $4::bigint < $1
		),
		ordered AS (
		    SELECT ledger, LAG(ledger) OVER (ORDER BY ledger) AS prev_l
		    FROM seeded
		)
		SELECT prev_l + 1 AS gap_start,
		       ledger - 1 AS gap_end,
		       ledger - prev_l - 1 AS gap_size
		FROM ordered
		WHERE prev_l IS NOT NULL
		  AND ledger - prev_l - 1 >= $3
		ORDER BY gap_size DESC
	`, target.LedgerColumn, target.Table, filter)
}

// closeTimeBoundPredicate appends the open-ended close-time bounds lo/hi (an invalid side stays unbounded) to args and returns the
// predicate.
func closeTimeBoundPredicate(column string, lo, hi sql.NullTime, args []any) (string, []any) {
	pred := ""
	if lo.Valid {
		args = append(args, lo.Time)
		pred += fmt.Sprintf(" AND %s >= $%d", column, len(args))
	}
	if hi.Valid {
		args = append(args, hi.Time)
		pred += fmt.Sprintf(" AND %s <= $%d", column, len(args))
	}
	return pred, args
}

// maxLedgerInWindowQuery builds the highest-present-ledger query that persists the seed [perSourceLedgerGapsQuery] reads next cycle. Targets
// with a DistinctLedgerCountSQL override opted out of the generic scan, so [Store.MaxLedgerInWindow] skips them.
func maxLedgerInWindowQuery(target GapDetectorTarget) string {
	filter := ""
	if target.WhereFilter != "" {
		filter = " AND (" + target.WhereFilter + ")"
	}
	//nolint:gosec // G201: identifiers from compile-time const list per ADR-0030
	return fmt.Sprintf(`SELECT MAX(%[1]s) FROM %[2]s WHERE %[1]s BETWEEN $1 AND $2%[3]s`,
		target.LedgerColumn, target.Table, filter)
}

// MaxLedgerInWindow returns the highest ledger present in [from,to] for target, and ok=false when the window has no rows at all (NULL max).
func (s *Store) MaxLedgerInWindow(ctx context.Context, target GapDetectorTarget, from, to int64) (int64, bool, error) {
	if target.DistinctLedgerCountSQL != "" {
		return 0, false, nil
	}
	if from < 0 || to < from || to == 0 {
		return 0, false, nil
	}
	var ledger sql.NullInt64
	if err := s.db.QueryRowContext(ctx, maxLedgerInWindowQuery(target), from, to).Scan(&ledger); err != nil {
		return 0, false, fmt.Errorf("timescale: MaxLedgerInWindow %s [%d,%d]: %w", target.Table, from, to, err)
	}
	if !ledger.Valid {
		return 0, false, nil
	}
	return ledger.Int64, true, nil
}

// FindPerSourceLedgerGaps finds ledger gaps >= minGapSize in the target's hypertable within [from, to], seeding a synthetic prior row from `seedLedger`; target.Table and LedgerColumn are interpolated unvalidated, so MUST come from [DefaultGapDetectorTargets].
func (s *Store) FindPerSourceLedgerGaps(ctx context.Context, target GapDetectorTarget, from, to, minGapSize, seedLedger int64) ([]LedgerGap, error) {
	if from < 0 || to < from {
		return nil, fmt.Errorf("timescale: FindPerSourceLedgerGaps invalid range [%d,%d]", from, to)
	}
	if to == 0 {
		// Defensive: caller passed an unresolved tip. Return empty
		// rather than scanning the whole table.
		return nil, nil
	}

	// ctx cancellation reaches PG only as a best-effort async cancel and leaked scans starve
	// trade inserts, so a statement_timeout makes PG itself abort.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("timescale: FindPerSourceLedgerGaps begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%d'", gapDetectorStatementTimeoutMS)); err != nil {
		return nil, fmt.Errorf("timescale: FindPerSourceLedgerGaps SET: %w", err)
	}
	args := []any{from, to, minGapSize, seedLedger}
	timeBound := ""
	if target.CloseTimeColumn != "" {
		var lo, hi sql.NullTime
		if err := tx.QueryRowContext(ctx, enclosingCloseTimeQuery, from, to).Scan(&lo, &hi); err != nil {
			return nil, fmt.Errorf("timescale: FindPerSourceLedgerGaps close-time bound [%d,%d]: %w", from, to, err)
		}
		timeBound, args = closeTimeBoundPredicate(target.CloseTimeColumn, lo, hi, args)
	}
	rows, err := tx.QueryContext(ctx, perSourceLedgerGapsQuery(target, timeBound), args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: FindPerSourceLedgerGaps %s [%d,%d, min %d]: %w",
			target.Table, from, to, minGapSize, err)
	}
	defer func() { _ = rows.Close() }()

	var out []LedgerGap
	for rows.Next() {
		var g LedgerGap
		if err := rows.Scan(&g.Start, &g.End, &g.Size); err != nil {
			return nil, fmt.Errorf("timescale: FindPerSourceLedgerGaps scan: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: FindPerSourceLedgerGaps rows.Err: %w", err)
	}
	return out, nil
}
