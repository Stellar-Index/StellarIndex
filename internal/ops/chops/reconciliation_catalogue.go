package chops

import (
	"fmt"
	"sort"
	"strings"

	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/sources/band"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	blend_backstop "github.com/Stellar-Index/StellarIndex/internal/sources/blend_backstop"
	blend_emitter "github.com/Stellar-Index/StellarIndex/internal/sources/blend_emitter"
	"github.com/Stellar-Index/StellarIndex/internal/sources/cctp"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/defindex"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
	"github.com/Stellar-Index/StellarIndex/internal/sources/redstone"
	"github.com/Stellar-Index/StellarIndex/internal/sources/reflector"
	"github.com/Stellar-Index/StellarIndex/internal/sources/rozo"
	sep41supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	sep41transfers "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_transfers"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorocredit"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	soroswap_router "github.com/Stellar-Index/StellarIndex/internal/sources/soroswap_router"
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

// reconSource is one source's reconciliation spec (ADR-0033 Claim 2b).
type reconSource struct {
	name        string
	dec         completeness.Decoder // nil for census-only sources (sdex)
	contractIDs []string             // SQL prefilter (oracles); empty = match-by-topic
	topic0Syms  []string
	targets     []reconTarget
	census      bool   // sdex: expected = decoder re-derive over the lake's SDEX ops
	genesis     uint32 // first-possible-data ledger; mirrors DefaultGapDetectorTargets (WASM-audit sourced)

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
	// args — PR 166). The -ch projection reconcile trims the WIDE
	// op_args_xdr column from the lake read for every other source; reading
	// it across the sep41/CAP-67 firehose was one leg of the 2026-07-08
	// compute-completeness OOMs.
	needsOpArgs bool

	// needsStateWriteKeys marks the decoder class that consumes
	// events.Event.StateWriteKeys — the operation's written contract-data
	// keys, resolved from the lake's ledger_entry_changes (redstone's
	// exact accepted-feed subset attribution). Opt-in like needsOpArgs:
	// the -ch reconcile skips the batched key lookups for every other
	// source.
	needsStateWriteKeys bool

	// aggregateReconcile, when non-empty, makes the -ch projection
	// reconcile compare WINDOW TOTALS instead of strict per-ledger
	// counts, and documents why. Per-ledger is the default (CS-084:
	// totals let a real drop in ledger L net against a phantom
	// elsewhere and report complete=true); only sources whose served
	// `ledger` keying can legitimately differ from the re-derive's
	// event ledger may opt out, and they accept the netting residual
	// the reason string acknowledges.
	aggregateReconcile string

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
}

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
// sep41_supply (see [buildSEP41ReconSources]) — promoted into the default
// catalogue as of the 2026-07-11 full-history truncate+re-derive
// (`ch-rebuild -sep41 -write`, windows 50.0M→63.42M, rc=0), which purged
// every pre-migration-0057 collapsed row. Before that re-derive, counting
// them here would have produced false projection deltas: the historical
// table rows predated the event_index PK discriminator (migration 0057),
// so multiple same-op events sat COLLAPSED on disk, and a re-derive (which
// counts each event) would have flagged every such historical ledger as
// "missing rows". That risk is gone now that the affected history has been
// rebuilt clean.
//
// Promoting them HERE — rather than each caller special-casing the append,
// as compute-completeness alone used to — means verify-reconciliation and
// ch-reproject see them too; ch_rebuild.go's -sep41 flag doc says exactly
// this: "promote them into buildReconciliationCatalogue".
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

	// genesis values mirror internal/api/v1/protocols_registry.go (the
	// WASM-audit / lake-derived exact-first-event authority; checked
	// against it 2026-07-31 — cctp + rozo were corrected here after the
	// registry's 07-30 lake-derived fix). DefaultGapDetectorTargets
	// (timescale/per_source_gaps.go) still carries the old cctp/rozo
	// 62_403_000 floors — a supporting signal only, but drift to fix when
	// that file's owner touches it next.
	cat := []reconSource{
		{name: "soroswap", genesis: 50_746_266, dec: soroswapDec, targets: []reconTarget{
			{"trades", "source = 'soroswap'", []string{"soroswap.trade"}},
			{"soroswap_skim_events", "", []string{"soroswap.skim"}},
			// soroswap.liquidity → soroswap_liquidity (persistSoroswapLiquidity
			// is a single INSERT: one decoder LiquidityEvent → one row).
			// Lake-validated 2026-08-17: 54/54 full-history rows == distinct
			// event identities, so the per-ledger count reconciles 1:1. Closes
			// the "emitted (soroswap.liquidity), persisted, never reconciled"
			// blind spot the density detector alone was covering — the exact
			// omission the catalogue-completeness invariant now guards.
			{"soroswap_liquidity", "", []string{"soroswap.liquidity"}},
		}},
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
			// whole ~6B-event lake — the fix for the -pass 120-min-deadline
			// timeout on aquarius's dirty-window [51M,tip] re-derive. Matches()
			// gates purely on pool identity, so the prefilter is
			// counts-identical. gatedPrefilter walks the router's add_pool
			// events on THIS throwaway to capture in-window pools too.
			newGatedDec: func() gatedDecoder { return aquarius.NewDecoder() },
			targets: []reconTarget{
				{"trades", "source = 'aquarius'", []string{"aquarius.trade"}},
				// 1:1 protocol tables — each of these Go event types has a
				// DISTINCT coarse EventKind() that lands in exactly ONE table,
				// and each sink persist func is a single INSERT (one decoder
				// event → one row). Lake-validated 2026-08-17 (rows == distinct
				// event identity, i.e. no fan-out): rewards 777004/777004,
				// protocol_fee 409/409, admin 12/12, kill 17/17. So the
				// per-ledger count reconciles. Closes the aquarius blind spots
				// the density detector alone was covering (~777k rewards rows).
				{"aquarius_rewards_events", "", []string{"aquarius.rewards"}},
				{"aquarius_admin", "", []string{"aquarius.admin"}},
				{"aquarius_protocol_fee", "", []string{"aquarius.fee"}},
				{"aquarius_kill_switches", "", []string{"aquarius.kill"}},
				// DELIBERATELY NOT reconciled here (declared in the
				// catalogue-completeness invariant's noReconcile waiver):
				// aquarius_reserves / aquarius_reserves_sync / aquarius_liquidity
				// each fan ONE decoder event out to N per-token-position rows
				// (token_index is a PK component), so the projection axis's
				// event-count-vs-served-row-count reconcile would false-flag
				// nearly every ledger — lake-measured ~2.0 served rows per
				// decoder event (aquarius_reserves 843705/421793,
				// aquarius_liquidity 12043/6021 over 62.8M–63.2M). Worse,
				// aquarius_reserves and aquarius_reserves_sync SHARE the single
				// coarse EventKind() "aquarius.reserves" (the sink routes on the
				// runtime ReservesEvent.Kind field, which the reconcile's
				// by-EventKind expected side cannot see), so no kinds split can
				// attribute a per-ledger count to one table vs the other. These
				// three stay on the density gap-detector until a fan-out-aware
				// (per-event-identity) reconcile lands — surfaced as a real
				// follow-up finding, not silently claimed complete.
			},
		},
		{
			// Mechanism-1 fix (2026-08-18 phoenix projection-completeness):
			// the pre-upgrade pool WASM (ledgers ~51,019,036–53,134,167)
			// emits swaps as 7 field-events (a RawSwap needs 8), so a group
			// is flushed only when a LATER event ages it out of the
			// correlation buffer (sweep-emit, dispatcher_adapter.go
			// decodeSwapEvent). The emitted trade keeps its OWN first-field
			// ledger, but the reconcile re-derive counts it at the later
			// sweep-triggering event's ledger — expected[realLedger]=0 vs
			// served=1: a per-ledger MISATTRIBUTION with the window total
			// preserved (net count unchanged, just shifted; proven at the
			// first mismatch ledger 51,573,544 = min phoenix trade ledger).
			// Window-total (aggregate) netting absorbs the shift. CS-084
			// caveat: aggregate also lets a genuine per-ledger drop net
			// against a phantom elsewhere, so this opt-out is justified ONLY
			// for the pre-upgrade sweep shift and does NOT substitute for the
			// curated-set seed fix (phoenix.MainnetPools /
			// MainnetStakeContracts, extended 2026-08-18) that makes the
			// re-derive reproduce the liquidity/stake rows so THOSE targets
			// reconcile by identity, not by netting.
			name: "phoenix", genesis: 51_572_016, dec: phoenix.NewDecoder(),
			// -ch re-derive prefilter (identity-gated): scope the lake read to
			// the curated pool/stake set instead of the whole lake — latent
			// timeout risk on phoenix's [51.5M,tip] re-derive, pre-empted the
			// same way as aquarius. Matches() gates purely on contract
			// identity and the correlation buffer only groups a SINGLE pool's
			// events, so the prefilter is counts-identical. The gate is static
			// (factory creation events predate the lake), so gatedPrefilter's
			// walk is a no-op here — the throwaway just enumerates the seed.
			newGatedDec:        func() gatedDecoder { return phoenix.NewDecoder() },
			aggregateReconcile: "pre-upgrade (~51.02M–53.13M) 7-field swaps flush at sweep only when a later event ages the group out of the correlation buffer; the trade keeps its first-field ledger but the re-derive counts it at the sweep-trigger ledger — a per-ledger shift with the window total preserved. Aggregate absorbs the shift and accepts the CS-084 netting residual on this source; it does NOT replace the curated-set seed fix.",
			targets: []reconTarget{
				{"trades", "source = 'phoenix'", []string{"phoenix.trade"}},
				{"phoenix_liquidity", "", []string{"phoenix.liquidity"}},
				{"phoenix_stake_events", "", []string{"phoenix.stake"}},
				// 1:1 protocol tables — self-contained decoders (no correlation
				// buffer), one event → one row (persistPhoenixInitialize /
				// persistPhoenixAdmin are single INSERTs). Each has a distinct
				// coarse EventKind() landing in exactly one table. Lake-validated
				// 2026-08-17: phoenix_initialize 24/24 rows == events;
				// phoenix_admin_events currently 0 rows (no mainnet admin rotation
				// yet) — reconciles clean at expected==served==0 and counts 1:1 the
				// first time one occurs, instead of the density detector's coarse
				// window. Closes the phoenix_initialize / phoenix_admin_events blind
				// spots.
				{"phoenix_initialize", "", []string{"phoenix.initialize"}},
				{"phoenix_admin_events", "", []string{"phoenix.admin"}},
			},
		},
		{name: "comet", genesis: 51_499_546, dec: comet.NewDecoder(), targets: []reconTarget{
			{"trades", "source = 'comet'", []string{"comet.trade"}},
			{"comet_liquidity", "", []string{"comet.liquidity"}},
		}},
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
				// ledger — r1-measured 2026-08-18: ledger 51,499,914 = 13 rows / 1
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
		},
		{
			// Lake-derived exact genesis (2026-07-30, mirrors
			// internal/api/v1/protocols_registry.go): the
			// MessageTransmitter's first on-chain event. The old
			// 62_403_000 was the ingestion-config floor, ~256k ledgers
			// late — it left 410 real served rows permanently BELOW the
			// verify floor, structurally out of every verdict
			// (density-genesis precision rule).
			name: "cctp", genesis: 62_146_641, dec: cctp.NewDecoder(),
			// contractIDs pins recognition attribution (board #31):
			// without it an unhandled cctp topic (mint_and_forward
			// was one until 2026-07-02) fell into the system-wide
			// recognition bucket instead of capping THIS source.
			contractIDs: cctp.MainnetContracts(),
			targets: []reconTarget{
				{"cctp_events", "", []string{"cctp.event"}},
			},
		},
		// Lake-derived exact genesis (2026-07-30, mirrors
		// protocols_registry.go): first event across all four Rozo
		// contracts; rozo_events is projected to exactly here. The old
		// 62_403_000 ingestion-config floor sat ~1.57M ledgers late.
		{name: "rozo", genesis: 60_829_397, dec: rozo.NewDecoder(), targets: []reconTarget{
			{"rozo_events", "", []string{"rozo.event"}},
		}},
		{
			// sorocredit — ADR-0035 contract-gated on a SINGLE trust-root
			// main contract. The bare NewDecoder() hard-codes that trust
			// root as its only "factory" and the main contract emits ALL
			// events (children emit nothing), so IsFactory(main) matches
			// everything without a factories/creationSym preseed. contractIDs
			// pins the re-derive to the one emitter (fast + recognition
			// attribution). One Go Event type fans out to four tables by the
			// dynamic EventKind() — hence a target per table. NOTE: the
			// "settlement" kind is the on-wire "Liquidation" event (scheduled
			// settlement, NOT distress).
			name: sorocredit.SourceName, genesis: sorocredit.GenesisLedger,
			dec: sorocredit.NewDecoder(), contractIDs: []string{sorocredit.MainnetContract},
			targets: []reconTarget{
				{"credit_positions", "", []string{"sorocredit.new_collateral_contract"}},
				{"credit_statements", "", []string{"sorocredit.statement_published"}},
				{"credit_settlements", "", []string{"sorocredit.settlement"}},
				{"credit_events", "", []string{
					"sorocredit.withdrawal", "sorocredit.beacon_updated",
					"sorocredit.supported_asset_added", "sorocredit.collateral_hash_updated",
				}},
			},
		},
		{name: blend_backstop.SourceName, genesis: blend_backstop.BackstopGenesisLedger, dec: blend_backstop.NewDecoder(), targets: []reconTarget{
			{"blend_backstop_events", "", []string{"blend_backstop.event"}},
		}},
		{name: "defindex", genesis: 57_056_338, dec: defindex.NewDecoder(), targets: []reconTarget{
			// ADR-0035/0040 contract-gated (curated set): the bare
			// NewDecoder() carries the in-code evidence-verified seed
			// (defindex.MainnetGatedSet), which is the trust root — the
			// factory create event does not announce the child address,
			// so there is no factories/creationSym preseed to run
			// (phoenix-style; a vault verified after the snapshot needs
			// the seed extended before its history reconciles).
			// Computed kinds: "defindex.strategy.{deposit,withdraw,harvest}"
			// + "defindex.vault.{deposit,withdraw}" (defindex.Event /
			// VaultEvent EventKind()). Both layers land in defindex_flows
			// (layer discriminator column). strategy.harvest MUST be listed:
			// the decoder emits it (audit 2026-08-04 finding 4 — strategy
			// yield realised into the vault, direction=harvest, admitted by
			// migration 0138) and the sink persists it to defindex_flows, so
			// omitting the kind here undercounts the EXPECTED side and
			// false-flags every genuine-harvest ledger as a projection gap
			// (the 974-mismatched-ledger verdict whose Σ|Δ| equalled the
			// served harvest-row count exactly).
			{"defindex_flows", "", []string{
				"defindex.strategy.deposit", "defindex.strategy.withdraw",
				"defindex.strategy.harvest",
				"defindex.vault.deposit", "defindex.vault.withdraw",
			}},
		}},
		{
			name: "blend", genesis: blend.FactoryGenesisLedger, dec: blend.NewDecoder(),
			factories: blend.MainnetPoolFactories, creationSym: blend.EventDeploy,
			targets: []reconTarget{
				{"blend_auctions", "", []string{blend.NewAuctionEventKind, blend.FillAuctionEventKind, blend.DeleteAuctionEventKind}},
				{"blend_positions", "", []string{blend.PositionEventKind}},
				{"blend_emissions", "", []string{blend.EmissionEventKind}},
				{"blend_admin", "", []string{blend.AdminEventKind}},
			},
		},
		{name: "sdex", genesis: 2, census: true, targets: []reconTarget{
			{"trades", "source = 'sdex'", nil},
		}},
	}

	// Oracle sources: decoder needs a real contract address; include only
	// when configured. The contract prefilter also makes the re-derive
	// fast (uses the soroban_events contract index).
	if a := cfg.Oracle.Reflector.DEXContract; a != "" {
		cat = append(cat, reconSource{
			name:               "reflector-dex",
			aggregateReconcile: "oracle_updates ledger keying differs across write vintages (legacy backfills keyed by oracle-timestamp ledger; live keys by event ledger) — strict per-ledger would false-flag the vintage boundary; aggregate accepts the CS-084 netting residual on this source", genesis: 50_644_229, dec: reflector.NewDecoder(reflector.VariantDEX, a), contractIDs: []string{a},
			targets: []reconTarget{{"oracle_updates", "source = 'reflector-dex'", []string{"reflector.update"}}},
		})
	}
	if a := cfg.Oracle.Reflector.CEXContract; a != "" {
		cat = append(cat, reconSource{
			name:               "reflector-cex",
			aggregateReconcile: "oracle_updates ledger keying differs across write vintages (legacy backfills keyed by oracle-timestamp ledger; live keys by event ledger) — strict per-ledger would false-flag the vintage boundary; aggregate accepts the CS-084 netting residual on this source", genesis: 50_644_239, dec: reflector.NewDecoder(reflector.VariantCEX, a), contractIDs: []string{a},
			targets: []reconTarget{{"oracle_updates", "source = 'reflector-cex'", []string{"reflector.update"}}},
		})
	}
	if a := cfg.Oracle.Reflector.FXContract; a != "" {
		cat = append(cat, reconSource{
			name:               "reflector-fx",
			aggregateReconcile: "oracle_updates ledger keying differs across write vintages (legacy backfills keyed by oracle-timestamp ledger; live keys by event ledger) — strict per-ledger would false-flag the vintage boundary; aggregate accepts the CS-084 netting residual on this source", genesis: 56_733_481, dec: reflector.NewDecoder(reflector.VariantFX, a), contractIDs: []string{a},
			targets: []reconTarget{{"oracle_updates", "source = 'reflector-fx'", []string{"reflector.update"}}},
		})
	}
	if a := cfg.Oracle.Redstone.AdapterContract; a != "" {
		cat = append(cat, reconSource{
			name:               "redstone",
			aggregateReconcile: "oracle_updates ledger keying differs across write vintages (legacy backfills keyed by oracle-timestamp ledger; live keys by event ledger) — strict per-ledger would false-flag the vintage boundary; aggregate accepts the CS-084 netting residual on this source", genesis: 58_758_722, dec: redstone.NewDecoder(a), contractIDs: []string{a},
			needsOpArgs:         true, // redstone reads feed_ids from the write_prices op args (events.Event.OpArgs, PR 166)
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
			name: "band", genesis: 60_000_000, callContract: a, callDec: band.NewDecoder(a),
			targets: []reconTarget{{"oracle_updates", "source = 'band'", nil}},
		})
	}
	cat = append(cat, reconSource{
		name: "soroswap-router", genesis: 50_746_272,
		callContract: soroswap_router.MainnetRouter,
		callDec:      soroswap_router.NewDecoder(soroswap_router.MainnetRouter),
		targets:      []reconTarget{{"soroswap_router_swaps", "", nil}},
	})

	// sep41 promotion (2026-07-11, post-full-history-re-derive) — see the
	// doc comment above. Gated the same way buildSEP41ReconSources's own
	// EmptyWatchedSetErrors precondition expects: only attempt it when a
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

	return cat, soroswapDec, nil
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
// [buildReconciliationCatalogue] (promoted into the default catalogue as
// of the 2026-07-11 full-history truncate+re-derive, gated on the watched
// set being non-empty so an unconfigured deployment gets silence instead
// of this function's error).
//
// Errors when the watched set is empty: called directly (ch-rebuild
// -sep41), an operator who passed -sep41 with no `[supply]
// watched_sep41_contracts` asked for an impossible rebuild, and silence
// would read as "nothing to recover". buildReconciliationCatalogue avoids
// ever hitting this by checking non-emptiness itself first.
func buildSEP41ReconSources(cfg config.Config) ([]reconSource, error) {
	watched := cfg.Supply.WatchedSEP41Contracts
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
	// Pre-fix both carried "" (whole-table ownership) while the EXPECTED
	// side was gated on the watched set through `dec`/`contractIDs`. The
	// two axes therefore measured different populations: any row written
	// for a contract that is not in TODAY'S watched set — history from a
	// contract since removed from `[supply] watched_sep41_contracts`, or
	// from a wider set used during an earlier backfill — counted on the
	// served side and could not counted on the expected side. That is a
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
	// Without it the sep41_supply re-derive streamed the watched contracts'
	// ENTIRE event firehose (KALE transfers dominate at ~99.95% of rows)
	// and discarded non-supply events one-by-one in a single goroutine:
	// ~35 of the full verify's ~37 minutes (measured 2026-07-27).
	return []reconSource{
		{
			name: sep41transfers.SourceName, genesis: sorobanEraGenesis,
			dec: tdec, contractIDs: watched,
			topic0Syms: []string{
				sep41transfers.SymbolTransfer,
				sep41transfers.SymbolApprove,
				sep41transfers.SymbolSetAdmin,
				sep41transfers.SymbolSetAuthorized,
			},
			targets: []reconTarget{{"sep41_transfers", filter, []string{sep41transfers.EventKind}}},
		},
		{
			name: sep41supply.SourceName, genesis: sorobanEraGenesis,
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
