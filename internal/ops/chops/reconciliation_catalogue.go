package chops

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sourcenet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/accounts"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/sources/band"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	blend_backstop "github.com/Stellar-Index/StellarIndex/internal/sources/blend_backstop"
	blend_emitter "github.com/Stellar-Index/StellarIndex/internal/sources/blend_emitter"
	"github.com/Stellar-Index/StellarIndex/internal/sources/cctp"
	"github.com/Stellar-Index/StellarIndex/internal/sources/claimable_balances"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/defindex"
	"github.com/Stellar-Index/StellarIndex/internal/sources/liquidity_pools"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
	"github.com/Stellar-Index/StellarIndex/internal/sources/redstone"
	"github.com/Stellar-Index/StellarIndex/internal/sources/reflector"
	"github.com/Stellar-Index/StellarIndex/internal/sources/rozo"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sac_balances"
	sep41supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	sep41transfers "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_transfers"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorocredit"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	soroswap_router "github.com/Stellar-Index/StellarIndex/internal/sources/soroswap_router"
	"github.com/Stellar-Index/StellarIndex/internal/sources/spectra"
	sushiswap_v3 "github.com/Stellar-Index/StellarIndex/internal/sources/sushiswap_v3"
	"github.com/Stellar-Index/StellarIndex/internal/sources/trustlines"
	"github.com/Stellar-Index/StellarIndex/internal/sources/upshift"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// reconTarget is one protocol table a source writes, plus the
// EventKinds that route to it. Re-derive counts ONLY these kinds for
// this table (a multi-table source like soroswap/phoenix/comet/blend
// routes different kinds to different tables; counting all outputs
// would overcount any single table).
type reconTarget struct {
	table       string
	whereFilter string   // "" = whole table belongs to this source
	kinds       []string // EventKind() values routing here; nil for census (sdex)
}

// sdexTradesFilter is the sdex target's whereFilter. It is also the persisted
// completeness_target_floors key (timescale.TargetFloorKey), so it never changes.
const sdexTradesFilter = "source = 'sdex'"

// sdexPriceableFilter scopes the sdex served COUNT to the rows the census
// counts: the census still excludes one-side-zero fills, because ledgers
// written before they were admitted hold none. Once a full-history
// ch-rebuild -sdex lands them, this filter goes with the census one.
const sdexPriceableFilter = "base_amount > 0 AND quote_amount > 0"

// countFilter is the predicate for the served-side row COUNT. It equals
// whereFilter except for the sdex target, where it adds sdexPriceableFilter.
// Use it only for CountRowsByLedger; floor identity (TargetFloorKey, MinLedger,
// the floor upsert) stays on whereFilter.
func (t reconTarget) countFilter() string {
	if t.table == "trades" && t.whereFilter == sdexTradesFilter {
		return t.whereFilter + " AND " + sdexPriceableFilter
	}
	return t.whereFilter
}

// reconSource is one source's reconciliation spec (ADR-0033 Claim 2b).
type reconSource struct {
	name        string
	dec         completeness.Decoder // nil for census-only sources (sdex)
	contractIDs []string             // SQL prefilter (oracles); empty = match-by-topic
	// firehoseTopics marks a contract-scoped source whose decoder consumes a
	// clickhouse.FirehoseExcludeSyms topic, so ch-rebuild must read it by
	// contractIDs with no topic exclusion (as the projector does).
	firehoseTopics bool
	topic0Syms     []string
	targets        []reconTarget
	census         bool   // sdex: expected = decoder re-derive over the lake's SDEX ops
	genesis        uint32 // first-possible-data ledger; mirrors DefaultGapDetectorTargets (WASM-audit sourced)

	// servedWindowReason, when non-empty, lets the projection reconcile floor
	// at each target's served MIN(ledger) instead of genesis: the served tier
	// deliberately holds only a window of this source, so a prefix the lake
	// has but Postgres lacks is not a gap. Empty (every source by default)
	// means the served tier claims genesis-to-tip and an unprojected prefix
	// fails the projection axis. TestCatalogue_ServedWindowExemptionsReviewed
	// pins the allow-list.
	servedWindowReason string

	// Factory-anchored gating (ADR-0035): when factories is non-empty, dec
	// gates Matches() on a registry of factory-deployed children, so the
	// re-derive must seed that registry before counting. A re-derive that
	// starts at `genesis` self-seeds in-stream (the factories' creation
	// events precede every child's events and dec.Decode registers them);
	// a re-derive over a custom sub-range does NOT, so the caller pre-walks
	// every factory's creation events via preseedFactoryChildren. creationSym
	// is the topic_0_sym of the creation event (e.g. blend "deploy"). A
	// protocol can have several factories (Blend was redeployed).
	factories   []string
	creationSym string

	// Event-less ContractCall sources (band, soroswap-router): no
	// soroban_events landing zone, so the projection census is re-derived by
	// streaming InvokeContract ops from the lake (filtered on callContract's
	// bytes in body_xdr) and running callDec over each. callDec != nil selects
	// the ContractCall census path. callContract is the C-strkey of the
	// invoked contract (strkey-decoded to the body_xdr substring filter).
	callDec      dispatcher.ContractCallDecoder
	callContract string

	// needsOpArgs marks the one decoder class that consumes
	// events.Event.OpArgs (redstone zips write_prices feed_ids from the op
	// args). The -ch projection reconcile trims the WIDE op_args_xdr column
	// from the lake read for every other source; reading it across the
	// sep41/CAP-67 firehose is one of the loads that run
	// compute-completeness out of memory.
	needsOpArgs bool

	// needsStateWriteKeys marks the decoder class that consumes
	// events.Event.StateWriteKeys — the operation's written contract-data
	// keys, resolved from the lake's ledger_entry_changes (redstone's
	// exact accepted-feed subset attribution). Opt-in like needsOpArgs:
	// the -ch reconcile skips the batched key lookups for every other
	// source.
	needsStateWriteKeys bool

	// aggregate, when non-nil, makes the -ch projection reconcile compare
	// WINDOW TOTALS (the netting compare) at ledgers <= its boundary
	// and strict per-ledger above it. Per-ledger is the default: totals let a
	// real drop in ledger L net against a phantom elsewhere and report
	// complete=true. Only a source whose served `ledger` keying differs from
	// the re-derive's event ledger over a FIXED historical span may opt out.
	aggregate *aggregateWaiver

	// waived names the served rows this source writes that the projection
	// reconcile deliberately does NOT count, with why. It is the runtime
	// record of what "reconciled" excludes for this source.
	waived []waivedTable

	// newGatedDec, when non-nil, opts a factory-anchored IDENTITY-gated
	// source (aquarius, phoenix) into the -ch re-derive contract-id
	// PREFILTER: the lake read is scoped to the source's gated contract set
	// (factory ∪ children) instead of streaming the whole ~6B-event lake.
	// Correct ONLY for sources whose Matches() keys purely on contract
	// identity — a contractIDs prefilter would BREAK any source that
	// correlates events ACROSS contracts (defindex's same-tx vault↔strategy
	// correlation), which is why it is an explicit per-source opt-in, not a
	// property inferred from `factories`. It builds a THROWAWAY decoder used
	// to enumerate the gate from the certified lake without disturbing dec's
	// in-stream self-seeding (see gatedPrefilter). Returning the concrete
	// gatedDecoder keeps the enumeration type-safe. See ADR-0035 gating.
	newGatedDec func() gatedDecoder

	// reproofOutlastsPass, when non-empty, says why this source's from-genesis
	// re-proof cannot fit the daily -pass: the pass never forces it on expired
	// evidence and runs it after the other from-genesis sources, as it does
	// for the census. A failing prior still re-verifies (bounded by
	// -source-timeout); a `-source <name>` run re-proves it in full.
	reproofOutlastsPass string
}

// aggregateWaiver is a source's opt-out from strict per-ledger reconcile.
// boundary is the highest ledger of the keying vintage that justifies the
// netting; projectionDelta reconciles a waiver with boundary 0 strict, so a
// netting exception without its bound cannot mark a source complete.
type aggregateWaiver struct {
	reason   string
	boundary uint32
}

// waivedTable is one served table (or the whereFilter slice of it) a source
// writes but the projection reconcile does not count.
type waivedTable struct {
	table       string
	whereFilter string // "" = the whole table
	reason      string
}

// fanoutWaiver: the aquarius reserve/liquidity sinks fan ONE decoder event
// out to N per-token-position rows (token_index is a PK component), so an
// event-count vs served-row-count reconcile false-flags nearly every ledger
// (lake-measured: aquarius_reserves 843705 rows / 421793 events,
// aquarius_liquidity 12043/6021).
const fanoutWaiver = "fan-out: one decoder event → N per-token-position rows " +
	"(token_index PK component), so event-count vs served-row-count would false-flag; " +
	"density gap-detector covers it pending a fan-out-aware reconcile"

// blendEmitterDropWaiver: one DropEvent carries N recipients and the sink
// writes one blend_emitter_events row per recipient (r1-measured: ledger
// 51,499,914 = 13 rows / 1 event; 57,467,292 = 3 / 1). Only the drop rows
// are waived; distribute/swap_config stay reconciled per-ledger.
const blendEmitterDropWaiver = "fan-out: one drop event → N recipient rows " +
	"(recipient_index PK component), so event-count vs served-row-count false-flags " +
	"the drop ledgers; the blend_emitter_events reconTarget excludes drop rows " +
	"(whereFilter event_kind <> 'drop') and omits the drop kind so the 1:1 " +
	"distribute/swap_config rows still reconcile per-ledger; density gap-detector covers drop"

// targetLabel names one served-table slice in verdict text.
func targetLabel(table, filter string) string {
	if filter == "" {
		return table
	}
	return table + "[" + filter + "]"
}

// projectionScope renders which served tables the projection reconcile counted
// and which it waived, for the verdict detail. scopes is parallel to s.targets;
// a target whose scope is empty was skipped by reconcileTarget, so it is named
// as not reconciled. The text carries no "; ": detail is joined on that separator.
func (s reconSource) projectionScope(scopes []projectionScope) string {
	rec := make([]string, 0, len(s.targets))
	var uncounted []string
	for i, t := range s.targets {
		if i < len(scopes) && !scopes[i].empty() {
			rec = append(rec, targetLabel(t.table, t.whereFilter))
		} else {
			uncounted = append(uncounted, targetLabel(t.table, t.whereFilter))
		}
	}
	sort.Strings(rec)
	sort.Strings(uncounted)
	out := fmt.Sprintf("scope: reconciled %d table(s) [%s]", len(rec), strings.Join(rec, ", "))
	if s.aggregate != nil {
		out += fmt.Sprintf(", window totals at ledgers <= %d", s.aggregate.boundary)
	}
	if len(s.waived) == 0 && len(uncounted) == 0 {
		return out
	}
	byReason := map[string][]string{}
	for _, w := range s.waived {
		r := strings.TrimSpace(strings.SplitN(w.reason, ":", 2)[0])
		byReason[r] = append(byReason[r], targetLabel(w.table, w.whereFilter))
	}
	reasons := make([]string, 0, len(byReason))
	for r := range byReason {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	parts := make([]string, 0, len(reasons))
	for _, r := range reasons {
		sort.Strings(byReason[r])
		parts = append(parts, strings.Join(byReason[r], ", ")+" ("+r+")")
	}
	if len(uncounted) > 0 {
		parts = append(parts, strings.Join(uncounted, ", ")+" (empty scope this run)")
	}
	return out + ", not reconciled: " + strings.Join(parts, ", ")
}

// outlastsPass reports whether the daily -pass must not force this source's
// from-genesis re-proof (the SDEX census, or a named heavy source).
func (s reconSource) outlastsPass() bool { return s.census || s.reproofOutlastsPass != "" }

// gatedDecoder is a decoder that can enumerate its gated contract set — the
// factory trust roots ∪ registered children. Both aquarius.Decoder and
// phoenix.Decoder satisfy it. Used by gatedPrefilter to build the -ch
// re-derive contract-id prefilter.
type gatedDecoder interface {
	completeness.Decoder
	GatedContractSet() []string
}

// buildReconciliationCatalogue assembles the per-source reconciliation
// set and returns the soroswap decoder separately so the caller can
// seed its pair registry (its swap event omits token identities).
//
// Scope: every source whose decoder matches by TOPIC (so a soroban_events
// re-derive reproduces it) or by a REAL contract address (oracles); sdex via
// the LCM op census; and the event-less ContractCall sources (band,
// soroswap-router) via the InvokeContract-op census (callDec path) — their
// calls are re-derived from the lake by filtering body_xdr on the contract
// bytes (stellar.operations has no contract_id column). PLUS, when
// cfg.Supply.WatchedSEP41Contracts is configured, sep41_transfers +
// sep41_supply (see [buildSEP41ReconSources]). Counting them is sound only
// because the full-history truncate+re-derive (`ch-rebuild -sep41 -write`,
// windows 50.0M→63.42M) purged every row written before the event_index PK
// discriminator (migration 0057): those rows collapsed multiple same-op
// events on disk, and a re-derive (which counts each event) flags every
// such ledger as "missing rows".
//
// Promoting them HERE, rather than each caller special-casing the append,
// means compute-completeness, verify-reconciliation and ch-reproject all
// see them; ch_rebuild.go's -sep41 flag doc says exactly this: "promote
// them into buildReconciliationCatalogue".
//
// Errors only when a configured watched contract fails to build its
// decoder (a malformed C-strkey). An EMPTY watched set is NOT an error
// here — unlike calling [buildSEP41ReconSources] directly for ch-rebuild's
// explicit -sep41 opt-in — a deployment that doesn't watch any SEP-41
// contract simply gets no sep41 entries, mirroring the dispatcher's own
// non-opted-in behavior.
//
//nolint:funlen // linear per-source catalogue; one entry per projected source, splitting scatters the reconcile spec.
func buildReconciliationCatalogue(cfg config.Config) ([]reconSource, *soroswap.Decoder, error) {
	soroswapDec := soroswap.NewDecoder()

	// genesis: prefer the source package's exported constant; literals
	// mirror internal/api/v1/protocols_registry.go and must equal the
	// DefaultGapDetectorTargets floor
	// (TestCatalogueGenesisLocksStepWithGapDetectorTargets).
	cat := []reconSource{
		{
			// Identity-gated (factories ∪ registered pairs), so the -ch
			// re-derive opts into the gated prefilter like aquarius below.
			// Without it a re-floor at genesis streams every contract
			// event from genesis to tip, unfiltered, as the pass's FIRST
			// source. factories/creationSym also let the preseed register
			// pairs announced before a sub-range's lo.
			name: "soroswap", genesis: 50_746_266, dec: soroswapDec,
			factories: soroswap.MainnetFactories, creationSym: soroswap.PrefixFactory,
			newGatedDec: func() gatedDecoder { return soroswap.NewDecoder() },
			targets: []reconTarget{
				{"trades", "source = 'soroswap'", []string{"soroswap.trade"}},
				{"soroswap_skim_events", "", []string{"soroswap.skim"}},
				// soroswap.liquidity → soroswap_liquidity (persistSoroswapLiquidity
				// is a single INSERT: one decoder LiquidityEvent → one row).
				// Lake-validated: 54/54 full-history rows == distinct event
				// identities, so the per-ledger count reconciles 1:1. Without it
				// soroswap.liquidity is emitted and persisted but never reconciled,
				// covered only by the density detector — the omission the
				// catalogue-completeness invariant guards.
				{"soroswap_liquidity", "", []string{"soroswap.liquidity"}},
			},
		},
		{
			// ADR-0035/0040 contract-gated (router-anchored). The bare
			// NewDecoder() already carries the curated in-code pool seed
			// (aquarius.MainnetPools), so sub-range re-derives work; the
			// factories/creationSym pair additionally lets the preseed
			// register pools announced AFTER the in-code snapshot from
			// the router's add_pool events before counting.
			name: "aquarius", genesis: 52_728_375, dec: aquarius.NewDecoder(),
			factories: []string{aquarius.MainnetRouter}, creationSym: aquarius.EventAddPool,
			// -ch re-derive prefilter (identity-gated, factory-anchored):
			// scope the lake read to the router ∪ its pools instead of the
			// whole ~6B-event lake, which otherwise runs aquarius's dirty-window
			// [51M,tip] re-derive past the -pass 120-min deadline. Matches()
			// gates purely on pool identity, so the prefilter is
			// counts-identical. gatedPrefilter walks the router's add_pool
			// events on THIS throwaway to capture in-window pools too.
			newGatedDec: func() gatedDecoder { return aquarius.NewDecoder() },
			targets: []reconTarget{
				{"trades", "source = 'aquarius'", []string{"aquarius.trade"}},
				// 1:1 protocol tables — each of these Go event types has a
				// DISTINCT coarse EventKind() that lands in exactly ONE table,
				// and each sink persist func is a single INSERT (one decoder
				// event → one row). Lake-validated (rows == distinct event
				// identity, i.e. no fan-out): rewards 777004/777004,
				// protocol_fee 409/409, admin 12/12, kill 17/17. So the
				// per-ledger count reconciles, rather than leaving these tables
				// (~777k rewards rows) to the density detector alone.
				{"aquarius_rewards_events", "", []string{"aquarius.rewards"}},
				{"aquarius_admin", "", []string{"aquarius.admin"}},
				{"aquarius_protocol_fee", "", []string{"aquarius.fee"}},
				{"aquarius_kill_switches", "", []string{"aquarius.kill"}},
				// DELIBERATELY NOT reconciled here (the `waived` list below):
				// aquarius_reserves / aquarius_reserves_sync / aquarius_liquidity
				// each fan ONE decoder event out to N per-token-position rows
				// (token_index is a PK component), so the projection axis's
				// event-count-vs-served-row-count reconcile would false-flag
				// nearly every ledger — lake-measured ~2.0 served rows per
				// decoder event (aquarius_reserves 843705/421793,
				// aquarius_liquidity 12043/6021 over 62.8M–63.2M).
				// aquarius_reserves and aquarius_reserves_sync carry
				// distinct EventKind()s ("aquarius.reserves" /
				// "aquarius.reserves_sync" — ReservesEvent.EventKind()
				// branches on the runtime Kind field), so a kinds split CAN
				// attribute a per-ledger count to one table vs the other; the
				// fan-out ratio is the only remaining blocker. These three
				// stay on the density gap-detector until a fan-out-aware
				// (per-event-identity) reconcile lands — surfaced as a real
				// follow-up finding, not silently claimed complete.
			},
			waived: []waivedTable{
				{"aquarius_reserves", "", fanoutWaiver},
				{"aquarius_reserves_sync", "", fanoutWaiver},
				{"aquarius_liquidity", "", fanoutWaiver},
			},
		},
		{
			// The pre-upgrade pool WASM (ledgers ~51,019,036–53,134,167)
			// emits swaps as 7 field-events (a RawSwap needs 8), so a group
			// is flushed only when a LATER event ages it out of the
			// correlation buffer (sweep-emit, dispatcher_adapter.go
			// decodeSwapEvent). The emitted trade keeps its OWN first-field
			// ledger, and the re-derive must count it there too (see the
			// eventLedgerCarrier note below). The curated set
			// (phoenix.MainnetPools / MainnetStakeContracts) is what makes
			// the re-derive reproduce the liquidity/stake rows, so those
			// targets reconcile by identity.
			name: "phoenix", genesis: 51_572_016, dec: phoenix.NewDecoder(),
			// -ch re-derive prefilter (identity-gated): scope the lake read to
			// the curated pool/stake set instead of the whole lake — latent
			// timeout risk on phoenix's [51.5M,tip] re-derive, pre-empted the
			// same way as aquarius. Matches() gates purely on contract
			// identity and the correlation buffer only groups a SINGLE pool's
			// events, so the prefilter is counts-identical. factories +
			// creationSym are set so gatedPrefilter's walk actually runs: the
			// factory's ("create","liquidity_pool") events ARE in the lake
			// from ledger 51,572,026, and the decoder admits the pools they
			// announce, so a pool created after the curated seed was last
			// hand-edited is picked up by the walk instead of being missed.
			factories: []string{phoenix.MainnetFactory}, creationSym: phoenix.EventActionCreate,
			newGatedDec: func() gatedDecoder { return phoenix.NewDecoder() },
			// The eventLedgerCarrier own-ledger attribution
			// (completeness.countLedger) counts each sweep-rescued
			// 7-field-era trade at its OWN first-field ledger — exactly where
			// the served row lives — so phoenix reconciles strictly per-ledger
			// over its FULL range, with no window-total netting that would let
			// a real drop net against a phantom elsewhere. Measured: per-ledger
			// expectation vs served over [51,573,544, 64,055,537] = ZERO
			// mismatched ledgers, totals 246,725 == 246,725.
			targets: []reconTarget{
				{"trades", "source = 'phoenix'", []string{"phoenix.trade"}},
				{"phoenix_liquidity", "", []string{"phoenix.liquidity"}},
				{"phoenix_stake_events", "", []string{"phoenix.stake"}},
				// 1:1 protocol tables — self-contained decoders (no correlation
				// buffer), one event → one row (persistPhoenixInitialize /
				// persistPhoenixAdmin are single INSERTs). Each has a distinct
				// coarse EventKind() landing in exactly one table. Lake-validated:
				// phoenix_initialize 24/24 rows == events; phoenix_admin_events
				// had 0 rows (no mainnet admin rotation) — reconciles clean at
				// expected==served==0 and counts 1:1 the first time one occurs,
				// instead of the density detector's coarse window.
				{"phoenix_initialize", "", []string{"phoenix.initialize"}},
				{"phoenix_admin_events", "", []string{"phoenix.admin"}},
			},
		},
		// contractIDs scopes the lake read to the curated pool (regateSource unions
		// the protocol_contracts registry in); without it the re-derive streamed
		// every POOL-topic-candidate event in [floor, tip] and timed out the pass.
		{name: "comet", genesis: 51_499_546, dec: comet.NewDecoder(), contractIDs: comet.MainnetGatedSet(), targets: []reconTarget{
			{"trades", "source = 'comet'", []string{"comet.trade"}},
			{"comet_liquidity", "", []string{"comet.liquidity"}},
		}},
		{
			// ADR-0035 factory-anchored. The bare NewDecoder() already
			// carries the curated in-code pool seed (MainnetPools, all 58
			// pools the factory has created), so a sub-range re-derive
			// resolves tokens without first replaying creation events; the
			// factory/creationSym pair lets preseedFactoryChildren admit a
			// pool created after that table was frozen.
			//
			// newGatedDec (and NOT a static contractIDs list) is what scopes
			// the -ch re-derive. It is not an optimisation here, it is what
			// makes the re-derive finish: the source's own `mint` and `burn`
			// symbols are 33% and 12% of ALL pubnet contract events, so an
			// unscoped read over [61.5M, tip] streams the CAP-67 firehose. A
			// STATIC contractIDs list would also be a hard filter on
			// ch-rebuild / ch-reproject and would silently drop a pool created
			// after the curated table was frozen; gatedPrefilter re-walks the
			// factory creation events from the lake instead, so the set is a
			// guaranteed superset at every point in the window.
			//
			// Strict per-ledger, no netting: a V3 swap body is
			// self-contained, so a decoded trade is always attributed to the
			// ledger its own event closed in and the served row keys 1:1
			// with the re-derive.
			name:        "sushiswap_v3",
			genesis:     sushiswap_v3.FactoryGenesisLedger,
			dec:         sushiswap_v3.NewDecoder(),
			factories:   sushiswap_v3.MainnetFactories,
			creationSym: sushiswap_v3.EventPoolCreated,
			newGatedDec: func() gatedDecoder { return sushiswap_v3.NewDecoder() },
			targets: []reconTarget{
				{"trades", "source = 'sushiswap_v3'", []string{"sushiswap_v3.trade"}},
				// One decoded mint/burn/collect → one row, so the per-ledger
				// count reconciles 1:1 (persistSushiswapV3Position is a single INSERT).
				{"sushiswap_v3_position_events", "", []string{"sushiswap_v3.position"}},
			},
		},
		{
			// upshift — ADR-0035/0040 contract-gated (curated two-vault
			// set; no factory namespace exists, so there is nothing to
			// fan out from and no newGatedDec).
			//
			// contractIDs is not an optimisation here, it is what makes
			// the re-derive finish AND what keeps the recognition axis
			// honest. The source's own symbols are `deposit`, `withdraw`
			// and `transfer` — `transfer` alone is ~88% of all pubnet
			// contract events under the archive's uniform V4 meta — so an
			// unscoped read over [62.6M, tip] streams the CAP-67 firehose,
			// and every unrecognised stranger's `deposit` would cap THIS
			// source's recognition instead of the system-wide bucket.
			//
			// Strict per-ledger, no netting and no fan-out: each decoded
			// event produces exactly one row keyed on its own
			// (ledger, tx, op, event_index), so the served row keys 1:1
			// with the re-derive. The eight recognised-but-unserved kinds
			// (custody / governance / allowance) decode to zero rows, which
			// is what lets their ledgers count as expected-zero rather than
			// blind.
			name:           "upshift",
			genesis:        upshift.GenesisLedger,
			dec:            upshift.NewDecoder(),
			contractIDs:    upshift.MainnetGatedSet(),
			firehoseTopics: true, // share `transfer`
			targets: []reconTarget{
				{"upshift_vault_events", "", []string{upshift.EventKind}},
			},
		},
		{
			// spectra — ADR-0035 factory-anchored (pt_deployed admits a PT,
			// the PT's yt_deployed its YT) plus the hand-kept set. The static
			// contractIDs (regateSource unions the protocol_contracts
			// children in) let ch-rebuild read PT/YT `transfer` rows, a
			// firehose topic, by contract; factories/creationSym preseed a
			// sub-range re-derive. One decoded event is one row.
			name:           spectra.SourceName,
			genesis:        spectra.GenesisLedger,
			dec:            spectra.NewDecoder(),
			contractIDs:    append(spectra.MainnetGatedSet(), spectra.MainnetInfrastructure...),
			firehoseTopics: true,
			factories:      []string{spectra.MainnetFactory},
			creationSym:    spectra.EventPTDeployed,
			targets: []reconTarget{
				{"spectra_events", "", []string{spectra.EventKind}},
			},
		},
		{
			// blend_emitter — ADR-0035/0040 contract-gated (curated
			// one-contract set, same shape as comet/cctp: no factory
			// namespace exists). contractIDs pins recognition
			// attribution the same way cctp's does — without it an
			// unrecognised blend_emitter topic would fall into the
			// system-wide recognition bucket instead of capping this
			// source.
			name: "blend_emitter", genesis: 51_499_914, dec: blend_emitter.NewDecoder(),
			contractIDs: blend_emitter.MainnetGatedSet(),
			targets: []reconTarget{
				// The `drop` kind FANS OUT: one decoder DropEvent carries N
				// recipients and the sink writes one blend_emitter_events row per
				// recipient (recipient_index is a PK component), so a per-ledger
				// event-count-vs-served-row-count reconcile false-flags every drop
				// ledger — r1-measured: ledger 51,499,914 = 13 rows / 1
				// event identity, ledger 57,467,292 = 3 / 1, Σ|Δ|=14, data CORRECT.
				// It is the same fan-out class aquarius_reserves/liquidity are
				// waived for. BUT — unlike those all-fan-out tables —
				// blend_emitter_events is MIXED: `distribute` (465 events) and
				// `q_swap`/`swap` (2) are strictly 1:1 (one event → one row). So
				// rather than waive the whole table and lose that 1:1 coverage, we
				// carve ONLY the fan-out `drop` rows out of the served side
				// (whereFilter) and omit "blend_emitter.drop" from the re-derived
				// kinds: the 467/469 1:1 events keep exact per-ledger reconciliation
				// and the 2 drop ledgers are covered by the density gap-detector
				// (per_source_gaps.go). DropEvent→blend_emitter_events is the
				// declared noReconcile waiver in the catalogue-completeness
				// invariant (blendEmitterDropWaiver).
				{"blend_emitter_events", "event_kind <> 'drop'", []string{
					"blend_emitter.distribute", "blend_emitter.swap_config",
				}},
			},
			waived: []waivedTable{{"blend_emitter_events", "event_kind = 'drop'", blendEmitterDropWaiver}},
		},
		{
			// Lake-derived exact genesis (mirrors
			// internal/api/v1/protocols_registry.go): the
			// MessageTransmitter's first on-chain event. The 62_403_000
			// ingestion-config floor is ~256k ledgers late and would leave
			// 410 real served rows permanently BELOW the verify floor,
			// structurally out of every verdict (density-genesis precision
			// rule).
			name: "cctp", genesis: 62_146_641, dec: cctp.NewDecoder(),
			// contractIDs pins recognition attribution: without it an
			// unhandled cctp topic falls into the system-wide recognition
			// bucket instead of capping THIS source.
			contractIDs: cctp.MainnetContracts(),
			targets: []reconTarget{
				{"cctp_events", "", []string{"cctp.event"}},
			},
		},
		// Lake-derived exact genesis (mirrors protocols_registry.go):
		// first event across all four Rozo contracts; rozo_events is
		// projected to exactly here. The 62_403_000 ingestion-config floor
		// sits ~1.57M ledgers late.
		{
			name: "rozo", genesis: 60_829_397, dec: rozo.NewDecoder(),
			// contractIDs pins recognition attribution, exactly as cctp's
			// does above. rozo is not a gated-registry source, so the
			// protocol_contracts fold (loadRegistryOwners) never names its
			// contracts either: without this pin NOTHING puts a Rozo
			// contract in ownerOf, an unhandled topic on one falls into the
			// system-wide bucket, and rozo's own recognition_ok cannot go
			// false. The
			// decoder gates Matches() on this same set (rozoContracts is
			// built from MainnetPaymentContracts), so the pin is
			// counts-identical as a re-derive prefilter. Copied so the
			// catalogue never aliases the package's slice.
			contractIDs: append([]string(nil), rozo.MainnetPaymentContracts...),
			targets: []reconTarget{
				{"rozo_events", "", []string{"rozo.event"}},
			},
		},
		{
			// sorocredit — ADR-0035 identity-gated on its main contract (the
			// trust root) ∪ the Collateral children it announces via
			// NewCollateralContract. Matches admits a registered child's
			// events, so a static root-only contractIDs pin would drop them
			// in ch-rebuild / ch-reproject and the lake prefilter while the
			// live projector serves them. factories/creationSym preseed the
			// children for sub-range re-derives; newGatedDec scopes the lake
			// read to root ∪ children, as blend does. One Go Event type fans
			// out to four tables by the dynamic EventKind() — hence a target
			// per table. NOTE: the "settlement" kind is the on-wire
			// "Liquidation" event (scheduled settlement, NOT distress).
			name: sorocredit.SourceName, genesis: sorocredit.GenesisLedger,
			dec:       sorocredit.NewDecoder(),
			factories: []string{sorocredit.MainnetContract}, creationSym: sorocredit.TopicNewCollateralContract,
			newGatedDec: func() gatedDecoder { return sorocredit.NewDecoder() },
			targets: []reconTarget{
				{"credit_positions", "", []string{"sorocredit.new_collateral_contract"}},
				{"credit_statements", "", []string{"sorocredit.statement_published"}},
				{"credit_settlements", "", []string{"sorocredit.settlement"}},
				{"credit_events", "", []string{
					"sorocredit.withdrawal", "sorocredit.beacon_updated",
					"sorocredit.supported_asset_added", "sorocredit.collateral_hash_updated",
					"sorocredit.treasury_updated",
				}},
			},
		},
		{
			name: blend_backstop.SourceName, genesis: blend_backstop.BackstopGenesisLedger, dec: blend_backstop.NewDecoder(),
			// contractIDs pins recognition attribution, same reason
			// as rozo above: the backstop is not a gated-registry source,
			// so no other path names its contracts in ownerOf. Both
			// deployments are pinned because the decoder claims both
			// (IsBackstopContract: the V2 singleton and the V1 it
			// replaced); TestCatalogue_RecognitionPinsMatchDecoderIdentity
			// holds the pin and the decoder's identity check in step.
			contractIDs: []string{blend_backstop.MainnetBackstopV2, blend_backstop.MainnetBackstopV1},
			targets: []reconTarget{
				{"blend_backstop_events", "", []string{"blend_backstop.event"}},
			},
		},
		// contractIDs scopes the lake read to the curated vault ∪ strategy set
		// (regateSource unions the protocol_contracts registry in). Safe as a
		// static list: Decode is per-event and a factory `create` never
		// registers a child, so Matches can accept nothing outside it. Unscoped,
		// the re-derive streamed the whole lake and hit the per-source deadline.
		{name: "defindex", genesis: defindex.GenesisLedger, dec: defindex.NewDecoder(), contractIDs: defindex.MainnetGatedSet(), targets: []reconTarget{
			// ADR-0035/0040 contract-gated (curated set): the bare
			// NewDecoder() carries the in-code evidence-verified seed
			// (defindex.MainnetGatedSet), which is the trust root — the
			// factory create event does not announce the child address,
			// so there is no factories/creationSym preseed to run
			// (phoenix-style; a vault verified after the snapshot needs
			// the seed extended before its history reconciles).
			// Computed kinds: "defindex.strategy.{deposit,withdraw,harvest}"
			// + "defindex.vault.{deposit,withdraw}" (defindex.Event /
			// VaultEvent EventKind()) + "defindex.vault.dfees"
			// (DFeesEvent, second target below). Both flow layers land
			// in defindex_flows
			// (layer discriminator column). strategy.harvest MUST be listed:
			// the decoder emits it (strategy yield realised into the vault,
			// direction=harvest, admitted by migration 0138) and the sink
			// persists it to defindex_flows, so omitting the kind here
			// undercounts the EXPECTED side and false-flags every
			// genuine-harvest ledger as a projection gap (a
			// 974-mismatched-ledger verdict whose Σ|Δ| equalled the served
			// harvest-row count exactly).
			{"defindex_flows", "", []string{
				"defindex.strategy.deposit", "defindex.strategy.withdraw",
				"defindex.strategy.harvest",
				"defindex.vault.deposit", "defindex.vault.withdraw",
			}},
			// dfees: vault-layer per-asset protocol-fee
			// distribution into its own table (migration 0146; fires in
			// the same op as the vault flow, fans out per
			// distributed_fees entry with fee_index a PK component). The
			// decoder emits ONE DFeesEvent PER Vec entry — deliberately,
			// so expected event-count == served row-count stays a strict
			// 1:1 per-ledger reconcile (NOT the blend_emitter-drop /
			// aquarius sink-side fan-out class that needs a waiver). An
			// empty distributed_fees Vec (real, observed) emits zero
			// events and zero rows — count-consistent by construction.
			{"defindex_fees", "", []string{"defindex.vault.dfees"}},
			// Vault admin topics (rescue / pause toggles / role rotations):
			// one AdminEvent per on-chain event, one row each.
			{"defindex_admin_events", "", []string{"defindex.vault.admin"}},
		}},
		{
			name: "blend", genesis: blend.FactoryGenesisLedger, dec: blend.NewDecoder(),
			factories: blend.MainnetPoolFactories, creationSym: blend.EventDeploy,
			// Identity-gated (factories ∪ registered pools): scope the lake read to
			// that set, walked from the factories' deploy events through tip, as
			// aquarius does. A static contractIDs list would miss pools deployed
			// after the snapshot; the unscoped read hit the per-source deadline.
			newGatedDec: func() gatedDecoder { return blend.NewDecoder() },
			targets: []reconTarget{
				{"blend_auctions", "", []string{blend.NewAuctionEventKind, blend.FillAuctionEventKind, blend.DeleteAuctionEventKind}},
				{"blend_positions", "", []string{blend.PositionEventKind}},
				{"blend_emissions", "", []string{blend.EmissionEventKind}},
				{"blend_admin", "", []string{blend.AdminEventKind}},
			},
		},
		{
			name: "sdex", genesis: 2, census: true,
			servedWindowReason: "served sdex trades are the ADR-0034 working set: the pre-window classic history is lake-only " +
				"(ch-rebuild -sdex backfills it), and a census re-derive from ledger 2 would outlast every pass",
			targets: []reconTarget{{"trades", sdexTradesFilter, nil}},
		},
	}

	// Oracle sources: decoder needs a real contract address; include only
	// when configured. The contract prefilter also makes the re-derive
	// fast (uses the soroban_events contract index).
	//
	// Strict per-ledger, no aggregate waiver: the legacy backfill vintage that
	// keyed oracle_updates.ledger by the oracle timestamp is gone from the
	// served tier. Measured on r1: every served (source, ledger, tx_hash) was
	// joined to stellar.contract_events (ledger_seq, tx_hash) for its contract;
	// all 680,108 pairs in [58,849,942, 64,782,017] sit on their event's ledger
	// (reflector-dex 59,775, -cex 59,188, -fx 59,869, redstone 501,276).
	if a := cfg.Oracle.Reflector.DEXContract; a != "" {
		cat = append(cat, reconSource{
			name: "reflector-dex", genesis: 50_644_229, dec: reflector.NewDecoder(reflector.VariantDEX, a, reflector.WithDecoderDecimals(cfg.Oracle.Reflector.DEXDecimals)), contractIDs: []string{a},
			targets: []reconTarget{{"oracle_updates", "source = 'reflector-dex'", []string{"reflector.update"}}},
		})
	}
	if a := cfg.Oracle.Reflector.CEXContract; a != "" {
		cat = append(cat, reconSource{
			name: "reflector-cex", genesis: 50_644_239, dec: reflector.NewDecoder(reflector.VariantCEX, a, reflector.WithDecoderDecimals(cfg.Oracle.Reflector.CEXDecimals)), contractIDs: []string{a},
			targets: []reconTarget{{"oracle_updates", "source = 'reflector-cex'", []string{"reflector.update"}}},
		})
	}
	if a := cfg.Oracle.Reflector.FXContract; a != "" {
		cat = append(cat, reconSource{
			name: "reflector-fx", genesis: 56_733_481, dec: reflector.NewDecoder(reflector.VariantFX, a, reflector.WithDecoderDecimals(cfg.Oracle.Reflector.FXDecimals)), contractIDs: []string{a},
			targets: []reconTarget{{"oracle_updates", "source = 'reflector-fx'", []string{"reflector.update"}}},
		})
	}
	if a := cfg.Oracle.Redstone.AdapterContract; a != "" {
		cat = append(cat, reconSource{
			name:    "redstone",
			genesis: 58_758_722, dec: redstone.NewDecoder(a), contractIDs: []string{a},
			needsOpArgs:         true, // reads feed_ids from write_prices op args (Event.OpArgs)
			needsStateWriteKeys: true, // exact subset attribution from the op's written per-feed contract-data keys
			targets:             []reconTarget{{"oracle_updates", "source = 'redstone'", []string{"redstone.update"}}},
		})
	}

	// Event-less ContractCall sources — census re-derived from the lake's
	// InvokeContract ops (callDec path). band is gated on its configured
	// StandardReference contract; soroswap-router uses the mainnet router
	// const (matching how the indexer wires both decoders). genesis bounds the
	// verify range; the empty pre-first-call prefix reconciles to zero.
	if a := cfg.Oracle.Band.StandardReferenceContract; a != "" {
		cat = append(cat, reconSource{
			// 50,842,736 is Band's first on-chain write, not 60,000,000.
			// It cannot be found the usual way: Band's Soroban contract emits ZERO
			// events, so a contract_events probe returns 0 rows and reads as absence.
			// The number comes from contract_instance_changes (min ledger 50,842,736,
			// against that table's own floor of 50,457,429, so it is not a coverage
			// artifact) and is corroborated by 4,210 contract_data writes in
			// ledger_entry_changes from the same ledger. It matches the WASM audit
			// and the two other constants that already carried it.
			//
			// Expect this to turn band RED until a catch-up runs: oracle_updates
			// starts at 60,000,414, so the 9.16M ledgers between the true genesis
			// and the first projected row are a real gap. The reconcile floors at
			// genesis (no servedWindowReason), so that gap fails projection_ok.
			name: "band", genesis: 50_842_736, callContract: a, callDec: band.NewDecoder(a),
			targets: []reconTarget{{"oracle_updates", "source = 'band'", nil}},
		})
	}
	cat = append(cat, reconSource{
		name: "soroswap-router", genesis: 50_746_272,
		callContract: soroswap_router.MainnetRouter,
		callDec:      soroswap_router.NewDecoder(soroswap_router.MainnetRouter),
		targets:      []reconTarget{{"soroswap_router_swaps", "", nil}},
	})

	// sep41 promotion — see the doc comment above. Gated the same way
	// buildSEP41ReconSources's own EmptyWatchedSetErrors precondition expects: only attempt it when a
	// watched set is actually configured, so a deployment that never opted
	// into SEP-41 supply/transfer capture gets an empty (not an error)
	// promotion — matching the dispatcher's own non-opted-in behavior.
	if len(cfg.Supply.WatchedSEP41Contracts) > 0 {
		sepCat, err := buildSEP41ReconSources(cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("sep41 reconciliation sources: %w", err)
		}
		cat = append(cat, sepCat...)
	}

	cat, dropped := filterCatalogueByNetwork(cat, cfg.Stellar.Network)
	if len(dropped) > 0 {
		fmt.Fprintf(os.Stderr, "compute-completeness: network=%s — %d pubnet-only source(s) not applicable, excluded from the catalogue: %s\n",
			cfg.Stellar.Network, len(dropped), strings.Join(dropped, ","))
	}
	return cat, soroswapDec, nil
}

// filterCatalogueByNetwork drops every catalogue source that does not
// exist on network: protocol decoders are anchored to PUBNET
// contract identities (ADR-0035), so on testnet / futurenet they match
// nothing and their pubnet genesis floors (soroswap 50,746,266) sit above
// the network's tip — every such source would read "incomplete" by
// construction and the test-net verdict could never go green. Returns the surviving
// catalogue (input order) and the dropped names (source-sorted) so the
// caller can log them and delete their stale snapshots. On pubnet this is
// the identity.
func filterCatalogueByNetwork(cat []reconSource, network string) (kept []reconSource, dropped []string) {
	names := make([]string, 0, len(cat))
	for _, src := range cat {
		names = append(names, src.name)
	}
	_, excluded := sourcenet.Filter(names, network)
	if len(excluded) == 0 {
		return cat, nil
	}
	drop := make(map[string]struct{}, len(excluded))
	for _, e := range excluded {
		drop[e.Source] = struct{}{}
		dropped = append(dropped, e.Source)
	}
	kept = make([]reconSource, 0, len(cat)-len(excluded))
	for _, src := range cat {
		if _, ok := drop[src.name]; ok {
			continue
		}
		kept = append(kept, src)
	}
	return kept, dropped
}

// warmCatalogueGates gives every contract-gated catalogue source the SAME
// gate the live indexer runs with: the source's in-code curated set UNION
// the children recorded in protocol_contracts.
//
// buildReconciliationCatalogue takes only a config, so it can only build
// each gated decoder bare — in-code seed and nothing else. The live
// indexer's decoders are built from pipeline.GatedRegistryOptions, which
// also warms them from protocol_contracts: the documented operator seam
// for admitting a pool or vault without a redeploy. Without this warm, a
// contract admitted through that seam is decoded live and then invisible to
// every re-derive: its served rows read as phantoms against an expected side
// that cannot produce them, and a truncate + `ch-rebuild -write` rebuilds the
// table WITHOUT them. preseedFactoryChildren does not cover it — it walks
// creation events, and the seam exists precisely for contracts that have
// none (and it is a no-op for every source that declares no factories).
//
// Read-only by construction: withHook=false, so nothing here can write to
// protocol_contracts. A re-derive that registered contracts while
// re-deriving them would be manufacturing its own evidence.
//
// Call it on the freshly built catalogue, BEFORE preseedFactoryChildren or
// any stream touches src.dec: the gated decoders are rebuilt, so anything
// seeded into the old instances would be lost.
func warmCatalogueGates(ctx context.Context, store *timescale.Store, logger *slog.Logger, cat []reconSource) ([]reconSource, error) {
	gated, err := pipeline.GatedRegistryOptions(ctx, store, logger, ctx, false)
	if err != nil {
		return nil, fmt.Errorf("gated registry warm: %w", err)
	}
	return applyGatedOptions(cat, gated)
}

// applyGatedOptions is the pure half of [warmCatalogueGates]: it rebuilds
// each gated source's decoder (and its throwaway newGatedDec, and its
// static contractIDs filter) with gated[source] applied, and returns a new
// catalogue. Non-gated sources pass through untouched.
//
// Constructors come from pipeline.GatedMetaFor — the one registry the
// indexer's BuildDispatcher / BuildRegistry also construct from — so the
// re-derive cannot drift onto a different decoder than the one it audits.
// That is asserted, not assumed: a catalogue entry whose decoder type
// differs from the registry's fails closed here.
//
// FAIL CLOSED on a gated source with no entry in `gated`:
// GatedRegistryOptions returns one for every gated source, so a missing
// key is a wiring bug, and quietly keeping the bare decoder would restore
// exactly the defect this exists to remove.
func applyGatedOptions(cat []reconSource, gated map[string][]contractid.Option) ([]reconSource, error) {
	out := make([]reconSource, len(cat))
	copy(out, cat)
	for i := range out {
		src := &out[i]
		meta, ok := pipeline.GatedMetaFor(src.name)
		if !ok {
			continue
		}
		opts, have := gated[src.name]
		if !have {
			return nil, fmt.Errorf("gated catalogue source %q has no warmed registry options — "+
				"refusing to re-derive it on the bare in-code seed", src.name)
		}
		if err := regateSource(src, meta, opts); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// regateSource rebuilds one gated catalogue source in place with opts.
func regateSource(src *reconSource, meta pipeline.GatedMeta, opts []contractid.Option) error {
	dec := meta.NewDecoder(opts...)
	if src.dec != nil && reflect.TypeOf(src.dec) != reflect.TypeOf(dec) {
		return fmt.Errorf("gated catalogue source %q decodes with %T but the gated registry builds %T — "+
			"the re-derive would audit a different decoder than the indexer runs", src.name, src.dec, dec)
	}
	src.dec = dec

	if src.newGatedDec != nil {
		if _, ok := dec.(gatedDecoder); !ok {
			return fmt.Errorf("gated catalogue source %q opts into the gated prefilter but %T cannot enumerate its gate", src.name, dec)
		}
		newDecoder := meta.NewDecoder
		src.newGatedDec = func() gatedDecoder {
			// Same constructor, same options as the instance asserted
			// just above, so the concrete type — and the assertion's
			// outcome — is the same.
			g, _ := newDecoder(opts...).(gatedDecoder)
			return g
		}
	}

	// A static contractIDs list is a HARD per-event filter in ch-rebuild /
	// ch-reproject and the lake prefilter of the re-derive, so a decoder
	// that admits a registry-only contract is not enough on its own: the
	// filter has to admit it too. The curated order is kept as-is and the
	// registry-only extras are appended sorted, so an empty registry leaves
	// the list byte-identical to the in-code one.
	if len(src.contractIDs) > 0 {
		src.contractIDs = unionContractIDs(src.contractIDs, contractid.New(opts...).Children())
	}
	return nil
}

// unionContractIDs returns base followed by the members of extra that base
// does not already hold, those extras sorted. Never aliases base.
func unionContractIDs(base, extra []string) []string {
	seen := make(map[string]struct{}, len(base)+len(extra))
	out := make([]string, 0, len(base)+len(extra))
	for _, c := range base {
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	added := make([]string, 0, len(extra))
	for _, c := range extra {
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		added = append(added, c)
	}
	sort.Strings(added)
	return append(out, added...)
}

// validateSourceFilter fails CLOSED when a -source filter names no source in
// the catalogue actually built for this config. Both compute-completeness and
// verify-reconciliation filter their per-source loop with
// `if only != "" && src.name != only { continue }`; without this check a typo'd
// -source silently skips EVERY source and the run reports SUCCESS / "no gaps"
// having verified nothing (F7 fail-open). The catalogue is config-dependent
// (sep41 sources are promoted only when configured), so the valid set is
// exactly what buildReconciliationCatalogue returned. only == "" (all sources)
// is always valid.
// entryDecoderSourceNames lists RegisterSupplyEntryDecoders' five sources
// (internal/pipeline/dispatcher.go). They're real, config-driven ingest
// sources — they bump stellarindex_source_decode_errors_total like every
// other source, and the generic stellarindex_ingestion_decode_error alert
// (deploy/monitoring/rules/ingestion.yml) fires on them with no source
// exception — but they read LedgerEntry changes, not soroban_events, so
// they can never be a reconSource: completeness.Decoder's re-derive is
// soroban_events-specific (reconcile.go), a different data model entirely.
// Named here so validateSourceFilter can tell an operator "known but not on
// this axis" instead of the misleading "unknown source" a typo would also
// produce.
var entryDecoderSourceNames = map[string]bool{
	accounts.SourceName:           true,
	trustlines.SourceName:         true,
	claimable_balances.SourceName: true,
	liquidity_pools.SourceName:    true,
	sac_balances.SourceName:       true,
}

func validateSourceFilter(only string, cat []reconSource) error {
	if only == "" {
		return nil
	}
	names := make([]string, 0, len(cat))
	for _, src := range cat {
		if src.name == only {
			return nil
		}
		names = append(names, src.name)
	}
	if entryDecoderSourceNames[only] {
		return fmt.Errorf("-source %q is a LedgerEntry-based supply observer, not reconcilable on the soroban_events axis this tool covers (see internal/pipeline.RegisterSupplyEntryDecoders); known reconciliation sources: %s", only, strings.Join(names, ", "))
	}
	return fmt.Errorf("-source %q matches no reconciliation source for this config; known sources: %s", only, strings.Join(names, ", "))
}

// buildSEP41ReconSources builds the two SEP-41 reconSources —
// sep41_transfers + sep41_supply, watched-set-gated exactly like the
// production dispatcher (pipeline.RegisterSupplyEventDecoders constructs
// the SAME decoders from the SAME config field, so a re-derive reproduces
// precisely what the dispatcher would have written). The watched contracts
// double as the contractIDs prefilter, so the lake read is a
// contract-indexed scan, not a firehose walk — mandatory here because the
// SEP-41 topics ARE the CAP-67 classic-token firehose the DEX/lending
// passes exclude (ClassicTokenTopic0Syms).
//
// Consumers: ch-rebuild's -sep41 flag (the re-derive; called directly,
// unconditionally erroring on an empty watched set — see below) and
// [buildReconciliationCatalogue] (promoted into the default catalogue,
// gated on the watched set being non-empty so an unconfigured deployment gets silence instead
// of this function's error).
//
// Errors when the watched set is empty: called directly (ch-rebuild
// -sep41), an operator who passed -sep41 with no `[supply]
// watched_sep41_contracts` asked for an impossible rebuild, and silence
// would read as "nothing to recover". buildReconciliationCatalogue avoids
// ever hitting this by checking non-emptiness itself first.
func buildSEP41ReconSources(cfg config.Config) ([]reconSource, error) {
	watched := cfg.Supply.WatchedSEP41Contracts
	floor := sorobanEraFloor(cfg)
	tdec, err := sep41transfers.NewDecoder(watched)
	if err != nil {
		return nil, fmt.Errorf("sep41_transfers decoder: %w", err)
	}
	sdec, err := sep41supply.NewDecoder(watched)
	if err != nil {
		return nil, fmt.Errorf("sep41_supply decoder: %w", err)
	}
	// Both sep41 targets are WATCHED-SET SLICES of their table, not whole
	// tables — so they need a whereFilter, exactly like `trades` needs
	// `source = '...'`.
	//
	// With "" (whole-table ownership) the two axes would measure different
	// populations, since the EXPECTED side is gated on the watched set
	// through `dec`/`contractIDs`: any row written for a contract that is
	// not in TODAY'S watched set — history from a contract since removed
	// from `[supply] watched_sep41_contracts`, or from a wider set used
	// during an earlier backfill — counts on the served side and cannot
	// count on the expected side. That is a
	// PERMANENT surplus: it never closes, because the re-derive can never
	// produce a row for a contract it is configured not to decode, and
	// the served rows are real history nobody is going to delete.
	filter, err := contractIDFilter(watched)
	if err != nil {
		return nil, err
	}
	// topic0Syms mirrors the live projector's SQL prefilter for the same
	// sources (projector/registry.go sep41TransferSyms / sep41SupplySyms) —
	// the re-derive must stream the same population the live writer sees.
	// Without it the sep41_supply re-derive streams the watched contracts'
	// ENTIRE event firehose (KALE transfers dominate at ~99.95% of rows)
	// and discards non-supply events one-by-one in a single goroutine:
	// measured at ~35 of the full verify's ~37 minutes.
	return []reconSource{
		{
			name: sep41transfers.SourceName, genesis: floor,
			dec: tdec, contractIDs: watched,
			topic0Syms: []string{
				sep41transfers.SymbolTransfer,
				sep41transfers.SymbolApprove,
				sep41transfers.SymbolSetAdmin,
				sep41transfers.SymbolSetAuthorized,
			},
			targets: []reconTarget{{"sep41_transfers", filter, []string{sep41transfers.EventKind}}},
			reproofOutlastsPass: "the watched set includes the KALE SAC, whose CAP-67 transfers sit in " +
				"nearly every contract_events granule, so the contract_id bloom index skips almost nothing and " +
				"a from-genesis re-derive reads the whole Soroban-era lake; it exceeded -source-timeout on r1 (2026-10-04)",
		},
		{
			name: sep41supply.SourceName, genesis: floor,
			dec: sdec, contractIDs: watched,
			topic0Syms: []string{
				sep41supply.SymbolMint,
				sep41supply.SymbolBurn,
				sep41supply.SymbolClawback,
			},
			targets: []reconTarget{{"sep41_supply_events", filter, []string{sep41supply.EventKind}}},
		},
	}, nil
}

// contractIDFilter renders a watched set as the served-side SQL predicate
// that scopes a target to the same contracts the expected-side decoder is
// gated on. Both sep41 tables carry `contract_id` and index it
// (sep41_supply_events_contract_ledger_idx, sep41_transfers_contract_*_idx).
//
// whereFilter is INTERPOLATED into SQL by timescale.Store's row-count
// helpers, and unlike every other filter in this catalogue — all in-code
// literals — this one is built from operator config, which validates only
// that entries are non-empty (config.SupplyConfig.Validate). So each id is
// strkey-decoded as a contract address before it is quoted: a valid
// C-strkey is base32 [A-Z2-7]{56} and cannot carry a quote, so the
// rendered predicate is safe by construction rather than by escaping.
// Sorted for a stable string — the filter is part of the durable
// completeness_target_floors key (timescale.TargetFloorKey), so map/slice
// order must not churn it between runs.
func contractIDFilter(watched []string) (string, error) {
	ids := make([]string, 0, len(watched))
	for i, c := range watched {
		if _, err := strkey.Decode(strkey.VersionByteContract, c); err != nil {
			return "", fmt.Errorf(
				"sep41 reconcile filter: watched_sep41_contracts[%d] = %q is not a contract C-strkey: %w", i, c, err)
		}
		ids = append(ids, "'"+c+"'")
	}
	sort.Strings(ids)
	return "contract_id IN (" + strings.Join(ids, ", ") + ")", nil
}
