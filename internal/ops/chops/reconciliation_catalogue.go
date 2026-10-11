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

// reconTarget is one protocol table a source writes, plus the EventKinds that route to it. The
// re-derive counts only these kinds, since counting all outputs would overcount a table in a
// multi-table source.
type reconTarget struct {
	table       string
	whereFilter string   // "" = whole table belongs to this source
	kinds       []string // EventKind() values routing here; nil for census (sdex)
}

// sdexTradesFilter is the sdex target's whereFilter. It is also the persisted
// completeness_target_floors key (timescale.TargetFloorKey), so it never changes.
const sdexTradesFilter = "source = 'sdex'"

// sdexPriceableFilter scopes the sdex served count to the rows the census counts. The census
// excludes one-side-zero fills because older ledgers hold none.
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
	// symbolTopic0 marks a decoder that matches topic[0] only as an ScvSymbol,
	// so the lake read skips the ScvString arm and its wide topics_xdr read.
	symbolTopic0 bool
	targets      []reconTarget
	census       bool   // sdex: expected = decoder re-derive over the lake's SDEX ops
	genesis      uint32 // first-possible-data ledger; mirrors DefaultGapDetectorTargets (WASM-audit sourced)

	// servedWindowReason, when non-empty, floors the projection reconcile at each target's served
	// MIN(ledger) instead of genesis, because the served tier deliberately holds only a window.
	// Empty means genesis-to-tip, so an unprojected prefix fails the axis.
	// TestCatalogue_ServedWindowExemptionsReviewed pins the allow-list.
	servedWindowReason string

	// Factory-anchored gating (ADR-0035): dec gates Matches() on a registry of factory-deployed
	// children. A re-derive from genesis self-seeds in-stream, but a sub-range does not, so the
	// caller pre-walks creation events via preseedFactoryChildren. creationSym is the creation
	// event's topic_0_sym; a protocol can have several factories.
	factories   []string
	creationSym string

	// Event-less ContractCall sources: the census streams InvokeContract ops from the lake,
	// filtered on callContract's bytes in body_xdr, and runs callDec over each. callDec != nil
	// selects this path; callContract is the invoked contract's C-strkey.
	callDec      dispatcher.ContractCallDecoder
	callContract string

	// needsOpArgs marks the decoder class that consumes events.Event.OpArgs (redstone). The
	// reconcile trims the wide op_args_xdr column for every other source, since reading it across
	// the sep41 firehose can run compute-completeness out of memory.
	needsOpArgs bool

	// needsStateWriteKeys marks the decoder class that consumes events.Event.StateWriteKeys, the
	// operation's written contract-data keys from ledger_entry_changes (redstone). Opt-in like
	// needsOpArgs, so other sources skip the batched key lookups.
	needsStateWriteKeys bool

	// aggregate, when non-nil, compares window totals at ledgers <= its boundary and strict per
	// ledger above it. Per-ledger is the default because totals let a real drop net against a
	// phantom. Only a source whose served ledger keying differs from the event ledger over a
	// fixed historical span may opt out.
	aggregate *aggregateWaiver

	// waived names the served rows this source writes that the projection
	// reconcile deliberately does NOT count, with why. It is the runtime
	// record of what "reconciled" excludes for this source.
	waived []waivedTable

	// newGatedDec, when non-nil, opts an identity-gated source into the contract-id prefilter, so
	// the lake read is scoped to its gated set (factory plus children). That is correct only when
	// Matches() keys purely on contract identity; a source correlating events across contracts
	// (defindex) would break, hence the explicit opt-in. It builds a throwaway decoder to
	// enumerate the gate without disturbing dec's self-seeding (see gatedPrefilter, ADR-0035).
	newGatedDec func() gatedDecoder

	// reproofOutlastsPass, when non-empty, says why this source's from-genesis re-proof cannot fit
	// the daily -pass: the pass never forces it on expired evidence and runs it last, as for the
	// census. A failing prior still re-verifies; `-source <name>` re-proves it in full.
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

// fanoutWaiver: the aquarius reserve and liquidity sinks fan one decoder event out to one row per
// token position, so an event-count reconcile would false-flag nearly every ledger.
const fanoutWaiver = "fan-out: one decoder event → N per-token-position rows " +
	"(token_index PK component), so event-count vs served-row-count would false-flag; " +
	"density gap-detector covers it pending a fan-out-aware reconcile"

// blendEmitterDropWaiver: one DropEvent carries N recipients and the sink writes one row per
// recipient. Only the drop rows are waived; distribute and swap_config stay reconciled.
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

// buildReconciliationCatalogue assembles the per-source reconciliation set and returns the
// soroswap decoder separately so the caller can seed its pair registry (its swap event omits
// token identities). It covers topic- or contract-matched sources, the sdex op census, the
// event-less ContractCall sources, and the sep41 sources when cfg.Supply.WatchedSEP41Contracts is
// set (see buildSEP41ReconSources). Building it here gives compute-completeness,
// verify-reconciliation and ch-reproject one set. It errors only on a malformed watched
// C-strkey.
//
//nolint:funlen // linear per-source catalogue; one entry per projected source, splitting scatters the reconcile spec.
func buildReconciliationCatalogue(cfg config.Config) ([]reconSource, *soroswap.Decoder, error) {
	soroswapDec := soroswap.NewDecoder()

	// genesis literals mirror internal/api/v1/protocols_registry.go and must equal the
	// DefaultGapDetectorTargets floor (TestCatalogueGenesisLocksStepWithGapDetectorTargets).
	cat := []reconSource{
		{
			// Identity-gated, so the re-derive uses the gated prefilter like aquarius; without it a
			// genesis re-floor streams every contract event as the pass's first source.
			// factories/creationSym let the preseed register pairs announced before a sub-range.
			name: "soroswap", genesis: 50_746_266, dec: soroswapDec,
			factories: soroswap.MainnetFactories, creationSym: soroswap.PrefixFactory,
			newGatedDec: func() gatedDecoder { return soroswap.NewDecoder() },
			targets: []reconTarget{
				{"trades", "source = 'soroswap'", []string{"soroswap.trade"}},
				{"soroswap_skim_events", "", []string{"soroswap.skim"}},
				// One decoder LiquidityEvent writes one row, so the per-ledger count reconciles 1:1;
				// omitting it would leave the table to the density detector alone.
				{"soroswap_liquidity", "", []string{"soroswap.liquidity"}},
			},
		},
		{
			// ADR-0035/0040 contract-gated, router-anchored. NewDecoder() carries the in-code pool
			// seed so sub-range re-derives work; factories/creationSym let the preseed register
			// pools announced after that snapshot from the router's add_pool events.
			name: "aquarius", genesis: 52_728_375, dec: aquarius.NewDecoder(),
			factories: []string{aquarius.MainnetRouter}, creationSym: aquarius.EventAddPool,
			// Scope the re-derive read to the router and its pools; the whole lake would run the
			// dirty-window re-derive past the -pass deadline. Matches() gates purely on pool
			// identity, so counts are identical.
			newGatedDec: func() gatedDecoder { return aquarius.NewDecoder() },
			targets: []reconTarget{
				{"trades", "source = 'aquarius'", []string{"aquarius.trade"}},
				// 1:1 tables: each event type has a distinct EventKind landing in one table via a
				// single INSERT, so the per-ledger count reconciles.
				{"aquarius_rewards_events", "", []string{"aquarius.rewards"}},
				{"aquarius_admin", "", []string{"aquarius.admin"}},
				{"aquarius_protocol_fee", "", []string{"aquarius.fee"}},
				{"aquarius_kill_switches", "", []string{"aquarius.kill"}},
				// Deliberately not reconciled (see waived below): reserves, reserves_sync and
				// liquidity fan one event out to one row per token position, so an event-count
				// reconcile would false-flag nearly every ledger. They stay on the density
				// gap-detector until a fan-out-aware reconcile exists, not claimed complete.
			},
			waived: []waivedTable{
				{"aquarius_reserves", "", fanoutWaiver},
				{"aquarius_reserves_sync", "", fanoutWaiver},
				{"aquarius_liquidity", "", fanoutWaiver},
			},
		},
		{
			// The pre-upgrade pool WASM emits swaps as 7 field-events (a RawSwap needs 8), so a group
			// flushes only when a later event ages it out of the correlation buffer (sweep-emit,
			// decodeSwapEvent). The trade keeps its own first-field ledger and the re-derive counts
			// it there (see eventLedgerCarrier below). The curated pool and stake set makes the
			// liquidity and stake targets reconcile by identity.
			name: "phoenix", genesis: 51_572_016, dec: phoenix.NewDecoder(),
			// Re-derive prefilter, identity-gated as for aquarius: scope the lake read to the curated
			// pool and stake set. Matches() gates purely on contract identity and the correlation
			// buffer groups one pool's events, so counts are identical. factories/creationSym make
			// gatedPrefilter's walk run, so pools created after the curated seed are still picked up.
			factories: []string{phoenix.MainnetFactory}, creationSym: phoenix.EventActionCreate,
			newGatedDec: func() gatedDecoder { return phoenix.NewDecoder() },
			// eventLedgerCarrier attribution (completeness.countLedger) counts each sweep-rescued
			// 7-field-era trade at its own first-field ledger, where the served row lives. Phoenix
			// therefore reconciles strictly per-ledger over its full range, with no netting.
			targets: []reconTarget{
				{"trades", "source = 'phoenix'", []string{"phoenix.trade"}},
				{"phoenix_liquidity", "", []string{"phoenix.liquidity"}},
				{"phoenix_stake_events", "", []string{"phoenix.stake"}},
				// 1:1 tables: self-contained decoders, one event to one row, each with a distinct
				// EventKind. Zero admin rows reconcile clean at expected == served == 0.
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
			// ADR-0035 factory-anchored. NewDecoder() carries the in-code pool seed, so a sub-range
			// re-derive resolves tokens; factories/creationSym let preseedFactoryChildren admit
			// pools created after that seed was frozen.
			//
			// newGatedDec, not a static contractIDs list, scopes the re-derive: mint and burn are a
			// large share of all contract events, and a static list would drop later pools from
			// ch-rebuild and ch-reproject. gatedPrefilter re-walks creation events instead.
			//
			// Strict per-ledger: a V3 swap body is self-contained, so the served row keys 1:1.
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
			// upshift: ADR-0035/0040 contract-gated on a curated two-vault set, with no factory and
			// so no newGatedDec.
			//
			// contractIDs lets the re-derive finish and keeps recognition honest: `transfer` is most
			// contract events, and a stranger's `deposit` would cap this source instead of the
			// system-wide bucket.
			//
			// Strict per-ledger, 1:1: each event is one row. The recognised-but-unserved kinds decode
			// to zero rows, so their ledgers count as expected-zero rather than blind.
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
			// spectra: ADR-0035 factory-anchored (pt_deployed admits a PT, its yt_deployed a YT) plus
			// a hand-kept set. The static contractIDs let ch-rebuild read PT/YT `transfer` rows, a
			// firehose topic, by contract; factories/creationSym preseed sub-ranges. One event, one row.
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
			// blend_emitter: ADR-0035/0040 contract-gated on one curated contract, with no factory.
			// contractIDs pins recognition attribution, so an unrecognised topic caps this source
			// rather than the system-wide bucket.
			name: "blend_emitter", genesis: 51_499_914, dec: blend_emitter.NewDecoder(),
			contractIDs: blend_emitter.MainnetGatedSet(),
			targets: []reconTarget{
				// The `drop` kind fans out (one row per recipient), so it would false-flag every drop
				// ledger, like the aquarius fan-out tables. This table is mixed, though: distribute and
				// swap are strictly 1:1. So only the drop rows are carved out of the served side and
				// the drop kind is omitted from the re-derive; drop ledgers fall to the density
				// gap-detector (per_source_gaps.go). See blendEmitterDropWaiver.
				{"blend_emitter_events", "event_kind <> 'drop'", []string{
					"blend_emitter.distribute", "blend_emitter.swap_config",
				}},
			},
			waived: []waivedTable{{"blend_emitter_events", "event_kind = 'drop'", blendEmitterDropWaiver}},
		},
		{
			// Lake-derived exact genesis (mirrors protocols_registry.go): the MessageTransmitter's
			// first event. The later ingestion-config floor would leave real served rows below the
			// verify floor, outside every verdict.
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
			// contractIDs pins recognition attribution, as for cctp. Rozo is not a gated-registry
			// source, so without the pin nothing puts its contracts in ownerOf and its own
			// recognition_ok could never go false. The decoder gates on this same set, so the pin
			// is counts-identical as a prefilter. Copied so the catalogue never aliases the slice.
			contractIDs: append([]string(nil), rozo.MainnetPaymentContracts...),
			targets: []reconTarget{
				{"rozo_events", "", []string{"rozo.event"}},
			},
		},
		{
			// sorocredit: ADR-0035 identity-gated on its main contract plus the Collateral children
			// it announces via NewCollateralContract. A static root-only contractIDs pin would drop
			// child events in ch-rebuild and ch-reproject while the live projector serves them, so
			// factories/creationSym preseed children and newGatedDec scopes the read, as for blend.
			// One Event type fans out to four tables by EventKind(), hence a target per table. The
			// "settlement" kind is the on-wire "Liquidation" event (scheduled, not distress).
			name: sorocredit.SourceName, genesis: sorocredit.GenesisLedger,
			dec:       sorocredit.NewDecoder(),
			factories: []string{sorocredit.MainnetContract}, creationSym: sorocredit.TopicNewCollateralContract,
			newGatedDec: func() gatedDecoder { return sorocredit.NewDecoder() },
			// Its 139k+ children overflow the contract prefilter; these exact
			// topics (none in the firehose) keep the unscoped read narrow.
			topic0Syms: sorocredit.EventSymbols(),
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
			// contractIDs pins recognition attribution, as for rozo. Both the V2 singleton and the V1
			// it replaced are pinned because the decoder claims both (IsBackstopContract);
			// TestCatalogue_RecognitionPinsMatchDecoderIdentity keeps the two in step.
			contractIDs: []string{blend_backstop.MainnetBackstopV2, blend_backstop.MainnetBackstopV1},
			targets: []reconTarget{
				{"blend_backstop_events", "", []string{"blend_backstop.event"}},
			},
		},
		// contractIDs scopes the lake read to the curated vault and strategy set. A static list is
		// safe because Decode is per-event and a factory `create` never registers a child.
		{name: "defindex", genesis: defindex.GenesisLedger, dec: defindex.NewDecoder(), contractIDs: defindex.MainnetGatedSet(), targets: []reconTarget{
			// ADR-0035/0040 contract-gated on a curated set. The in-code seed (MainnetGatedSet) is the
			// trust root because the factory create event does not announce the child address, so
			// there is no preseed; a vault verified later needs the seed extended.
			// Both flow layers land in defindex_flows. strategy.harvest must be listed: omitting it
			// undercounts the expected side and false-flags every harvest ledger.
			{"defindex_flows", "", []string{
				"defindex.strategy.deposit", "defindex.strategy.withdraw",
				"defindex.strategy.harvest",
				"defindex.vault.deposit", "defindex.vault.withdraw",
			}},
			// dfees: per-asset protocol-fee distribution. The decoder emits one DFeesEvent per Vec
			// entry on purpose, so the count stays a strict 1:1 per-ledger reconcile with no
			// waiver. An empty distributed_fees Vec emits zero events and zero rows.
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

	// Oracle sources: the decoder needs a real contract address, so include one only when
	// configured. The contract prefilter also speeds the re-derive.
	//
	// Strict per-ledger, no aggregate waiver: the legacy backfill that keyed
	// oracle_updates.ledger by the oracle timestamp is gone from the served tier.
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

	// Event-less ContractCall sources: the census is re-derived from the lake's InvokeContract
	// ops (callDec path). band uses its configured StandardReference contract and soroswap-router
	// the mainnet router constant, as the indexer does. The empty prefix reconciles to zero.
	if a := cfg.Oracle.Band.StandardReferenceContract; a != "" {
		cat = append(cat, reconSource{
			// 50,842,736 is Band's first on-chain write. The contract emits no events, so a
			// contract_events probe reads as absence; the ledger comes from contract_instance_changes
			// and is corroborated by contract_data writes in ledger_entry_changes.
			//
			// Band stays red until a catch-up runs: oracle_updates starts much later, and with no
			// servedWindowReason the reconcile floors at genesis, so that gap fails projection_ok.
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

	// sep41 promotion: attempted only when a watched set is configured, so a deployment that never
	// opted into SEP-41 capture gets an empty result rather than an error, as the dispatcher does.
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

// filterCatalogueByNetwork drops catalogue sources that do not exist on network. Decoders are
// anchored to pubnet contract identities (ADR-0035), so on test networks they match nothing and
// their genesis floors sit above the tip, which would read incomplete forever. It returns the
// survivors and the dropped names so the caller can log them and delete stale snapshots.
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

// warmCatalogueGates gives every contract-gated source the live indexer's gate: its curated set
// plus the children recorded in protocol_contracts. Without it, a contract admitted through that
// seam is decoded live but invisible to re-derives, so its served rows read as phantoms and a
// truncate plus `ch-rebuild -write` drops them. preseedFactoryChildren does not cover contracts
// with no creation event.
//
// Read-only (withHook=false), since a re-derive that registered contracts would manufacture its
// own evidence. Call it before preseedFactoryChildren or any stream touches src.dec, because the
// gated decoders are rebuilt.
func warmCatalogueGates(ctx context.Context, store *timescale.Store, logger *slog.Logger, cat []reconSource) ([]reconSource, error) {
	gated, err := pipeline.GatedRegistryOptions(ctx, store, logger, ctx, false)
	if err != nil {
		return nil, fmt.Errorf("gated registry warm: %w", err)
	}
	return applyGatedOptions(cat, gated)
}

// applyGatedOptions is the pure half of warmCatalogueGates: it rebuilds each gated source's
// decoder, newGatedDec and contractIDs with gated[source] applied, using the registry the indexer
// builds from (pipeline.GatedMetaFor). A decoder type mismatch or a gated source missing from
// gated fails closed.
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

	// A static contractIDs list is a hard per-event filter in ch-rebuild, ch-reproject and the
	// re-derive prefilter, so it must admit registry-only contracts too. The curated order is kept
	// and extras are appended sorted, so an empty registry leaves the in-code list unchanged.
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

// entryDecoderSourceNames lists RegisterSupplyEntryDecoders' five sources.
// They are real ingest sources but read LedgerEntry changes, not
// soroban_events, so they can never be a reconSource; naming them lets
// validateSourceFilter say "known but not on this axis" instead of "unknown
// source".
var entryDecoderSourceNames = map[string]bool{
	accounts.SourceName:           true,
	trustlines.SourceName:         true,
	claimable_balances.SourceName: true,
	liquidity_pools.SourceName:    true,
	sac_balances.SourceName:       true,
}

// validateSourceFilter fails CLOSED when a -source filter names no source in
// the catalogue built for this config; otherwise a typo'd -source skips every
// source and the run reports "no gaps" having verified nothing. The catalogue
// is config-dependent, so the valid set is exactly what
// buildReconciliationCatalogue returned. only == "" is always valid.
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

// buildSEP41ReconSources builds sep41_transfers and sep41_supply, gated on the watched set like
// pipeline.RegisterSupplyEventDecoders so a re-derive reproduces what the dispatcher wrote. The
// watched contracts double as the contractIDs prefilter, since the SEP-41 topics are the CAP-67
// firehose. It errors on an empty watched set, because silence would read as nothing to recover;
// buildReconciliationCatalogue checks non-emptiness first.
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
	// Both sep41 targets are watched-set slices of their table, so they need a whereFilter like
	// `trades` needs `source = '...'`. With "" the axes would measure different populations:
	// rows for a contract no longer in today's watched set count on the served side but never on
	// the expected side, a permanent surplus the re-derive can never close.
	filter, err := contractIDFilter(watched)
	if err != nil {
		return nil, err
	}
	// topic0Syms mirrors the live projector's SQL prefilter (projector/registry.go), so the
	// re-derive streams the population the live writer sees. Without it sep41_supply streams the
	// watched contracts' whole firehose and discards non-supply events one by one.
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
			symbolTopic0: true,
			targets:      []reconTarget{{"sep41_supply_events", filter, []string{sep41supply.EventKind}}},
		},
	}, nil
}

// contractIDFilter renders a watched set as the served-side SQL predicate
// scoping a target to the contracts the expected-side decoder is gated on.
// The predicate is interpolated into SQL and, unlike the catalogue's other
// filters, comes from operator config, so each id is strkey-decoded first: a
// valid C-strkey is base32 and cannot carry a quote. Sorted because the filter
// is part of the completeness_target_floors key and must not churn.
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
