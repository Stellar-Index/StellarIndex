package main

import (
	"fmt"

	"github.com/StellarIndex/stellar-index/internal/completeness"
	"github.com/StellarIndex/stellar-index/internal/config"
	"github.com/StellarIndex/stellar-index/internal/dispatcher"
	"github.com/StellarIndex/stellar-index/internal/sources/aquarius"
	"github.com/StellarIndex/stellar-index/internal/sources/band"
	"github.com/StellarIndex/stellar-index/internal/sources/blend"
	blend_backstop "github.com/StellarIndex/stellar-index/internal/sources/blend_backstop"
	"github.com/StellarIndex/stellar-index/internal/sources/cctp"
	"github.com/StellarIndex/stellar-index/internal/sources/comet"
	"github.com/StellarIndex/stellar-index/internal/sources/defindex"
	"github.com/StellarIndex/stellar-index/internal/sources/phoenix"
	"github.com/StellarIndex/stellar-index/internal/sources/redstone"
	"github.com/StellarIndex/stellar-index/internal/sources/reflector"
	"github.com/StellarIndex/stellar-index/internal/sources/rozo"
	sep41supply "github.com/StellarIndex/stellar-index/internal/sources/sep41_supply"
	sep41transfers "github.com/StellarIndex/stellar-index/internal/sources/sep41_transfers"
	"github.com/StellarIndex/stellar-index/internal/sources/soroswap"
	soroswap_router "github.com/StellarIndex/stellar-index/internal/sources/soroswap_router"
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

	// aggregateReconcile, when non-empty, makes the -ch projection
	// reconcile compare WINDOW TOTALS instead of strict per-ledger
	// counts, and documents why. Per-ledger is the default (CS-084:
	// totals let a real drop in ledger L net against a phantom
	// elsewhere and report complete=true); only sources whose served
	// `ledger` keying can legitimately differ from the re-derive's
	// event ledger may opt out, and they accept the netting residual
	// the reason string acknowledges.
	aggregateReconcile string
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
// bytes (stellar.operations has no contract_id column). EXCLUDED by default:
//   - sep41-transfers / sep41-supply: included ONLY behind ch-rebuild's
//     -sep41 opt-in (see [buildSEP41ReconSources]); run only as part of the
//     truncate+re-derive procedure (see the flag help). The decoders gate
//     Matches() on cfg.Supply.WatchedSEP41Contracts (the same watched set
//     the dispatcher uses), with the watched contracts doubling as the
//     contractIDs prefilter (kinds: "sep41_transfers.event" /
//     "sep41_supply.event"; genesis 50_457_424 = sorobanEraGenesis). They
//     stay OUT of the default catalogue for a CONCRETE correctness reason:
//     the historical table rows predate the event_index PK discriminator
//     (migration 0057), so multiple same-op events are still COLLAPSED on
//     disk — a re-derive (which counts each event) would flag every such
//     historical ledger as "missing rows", a false delta. Until the operator
//     truncate + `ch-rebuild -sep41 -write` re-derive has run, they're
//     covered by the data-derived gap detector, not the ADR-0033 projection
//     reconcile; AFTER it has run they become eligible for the reconcile
//     (promote them into this default set then).
func buildReconciliationCatalogue(cfg config.Config) ([]reconSource, *soroswap.Decoder) {
	soroswapDec := soroswap.NewDecoder()

	// genesis values mirror DefaultGapDetectorTargets (WASM-audit sourced).
	cat := []reconSource{
		{name: "soroswap", genesis: 50_746_266, dec: soroswapDec, targets: []reconTarget{
			{"trades", "source = 'soroswap'", []string{"soroswap.trade"}},
			{"soroswap_skim_events", "", []string{"soroswap.skim"}},
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
			targets: []reconTarget{
				{"trades", "source = 'aquarius'", []string{"aquarius.trade"}},
			},
		},
		{name: "phoenix", genesis: 51_572_016, dec: phoenix.NewDecoder(), targets: []reconTarget{
			{"trades", "source = 'phoenix'", []string{"phoenix.trade"}},
			{"phoenix_liquidity", "", []string{"phoenix.liquidity"}},
			{"phoenix_stake_events", "", []string{"phoenix.stake"}},
		}},
		{name: "comet", genesis: 51_499_546, dec: comet.NewDecoder(), targets: []reconTarget{
			{"trades", "source = 'comet'", []string{"comet.trade"}},
			{"comet_liquidity", "", []string{"comet.liquidity"}},
		}},
		{
			name: "cctp", genesis: 62_403_000, dec: cctp.NewDecoder(),
			// contractIDs pins recognition attribution (board #31):
			// without it an unhandled cctp topic (mint_and_forward
			// was one until 2026-07-02) fell into the system-wide
			// recognition bucket instead of capping THIS source.
			contractIDs: cctp.MainnetContracts(),
			targets: []reconTarget{
				{"cctp_events", "", []string{"cctp.event"}},
			},
		},
		{name: "rozo", genesis: 62_403_000, dec: rozo.NewDecoder(), targets: []reconTarget{
			{"rozo_events", "", []string{"rozo.event"}},
		}},
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
			// Computed kinds: "defindex.{strategy,vault}.{deposit,withdraw}"
			// (defindex.Event / VaultEvent EventKind()). Both layers land
			// in defindex_flows (layer discriminator column).
			{"defindex_flows", "", []string{
				"defindex.strategy.deposit", "defindex.strategy.withdraw",
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
			targets: []reconTarget{{"oracle_updates", "source = 'redstone'", []string{"redstone.update"}}},
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
	return cat, soroswapDec
}

// buildSEP41ReconSources builds the two SEP-41 reconSources that
// [buildReconciliationCatalogue] deliberately excludes (see its doc
// comment): sep41_transfers + sep41_supply, watched-set-gated exactly
// like the production dispatcher (pipeline.RegisterSupplyEventDecoders
// constructs the SAME decoders from the SAME config field, so a
// re-derive reproduces precisely what the dispatcher would have
// written). The watched contracts double as the contractIDs prefilter,
// so the lake read is a contract-indexed scan, not a firehose walk —
// mandatory here because the SEP-41 topics ARE the CAP-67 classic-token
// firehose the DEX/lending passes exclude (ClassicTokenTopic0Syms).
//
// Consumers: ch-rebuild's -sep41 flag (the re-derive) and — since the
// 2026-07-06 full-history truncate+re-derive purged the
// pre-migration-0057 collapsed rows — compute-completeness's default
// catalogue (watched-set-gated), which gives the two sources ADR-0033
// verdicts like every other projected source.
//
// Errors when the watched set is empty: an operator who passed -sep41
// with no `[supply] watched_sep41_contracts` asked for an impossible
// rebuild, and silence would read as "nothing to recover".
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
	return []reconSource{
		{
			name: sep41transfers.SourceName, genesis: sorobanEraGenesis,
			dec: tdec, contractIDs: watched,
			targets: []reconTarget{{"sep41_transfers", "", []string{sep41transfers.EventKind}}},
		},
		{
			name: sep41supply.SourceName, genesis: sorobanEraGenesis,
			dec: sdec, contractIDs: watched,
			targets: []reconTarget{{"sep41_supply_events", "", []string{sep41supply.EventKind}}},
		},
	}, nil
}
