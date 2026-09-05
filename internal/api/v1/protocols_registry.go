package v1

import (
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	blend_backstop "github.com/Stellar-Index/StellarIndex/internal/sources/blend_backstop"
	blend_emitter "github.com/Stellar-Index/StellarIndex/internal/sources/blend_emitter"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorocredit"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	sushiswap_v3 "github.com/Stellar-Index/StellarIndex/internal/sources/sushiswap_v3"
)

// ProtocolMeta is the hand-curated static identity of one indexed
// protocol — everything about a protocol that is true regardless of
// what the database currently holds. The dynamic halves (contract
// registry, 24h event counts, completeness verdict) are joined onto
// this at request time by the /v1/protocols handlers.
//
// Deliberately boring: a flat struct in a package-level slice. Genesis
// ledgers mirror cmd/stellarindex-ops/reconciliation_catalogue.go (the
// WASM-audit-sourced first-possible-data ledgers); factory sets come
// from the per-source packages so a factory amendment there (e.g. a
// Blend factory redeploy) propagates here without a second edit.
type ProtocolMeta struct {
	// Name is the canonical source name — the same identifier used by
	// completeness_snapshots, protocol_contracts and /v1/coverage.
	Name string
	// Category is one of: dex | amm | lending | yield | bridge | oracle | token.
	Category string
	// Description is a single human sentence for the directory card.
	Description string
	// GenesisLedger is the first ledger this protocol could have data
	// at (first factory/contract deploy; 2 for the protocol-native SDEX).
	GenesisLedger uint32
	// Factories lists the verified factory / trust-root contract IDs
	// (C-strkeys) the decoder anchors on (ADR-0035). Empty when the
	// source has no factory model (oracles, sdex, bridges).
	Factories []string
	// EventKinds lists the EventKind() discriminators this source's
	// decoder emits — the wire vocabulary of its decoded output.
	EventKinds []string
	// VerificationPage is the repo-relative path of the protocol's
	// public verification/coverage write-up, "" when none exists yet.
	// Derived from Name by withVerificationPages below — never set in a
	// row literal, which is why it carries no value in this file.
	VerificationPage string
	// ExtraContracts folds a sub-module source's own contracts into
	// this protocol's roster + analytics scope — for a source that is
	// logically part of this protocol but emits on its own contract
	// address(es) and lands in its own table (e.g. the Blend Backstop
	// insurance module: distinct contracts + a separate blend_backstop
	// source, but part of the Blend protocol page). Tagged Kind="module"
	// in the roster; included in the lake-analytics contract-id scope so
	// their events show in the breakdown/activity. Empty for the common
	// case (a protocol that owns every contract it reports).
	ExtraContracts []string
	// ExtraEventSources lists additional logical source names whose
	// trailing-24h event count is summed into this protocol's events_24h
	// — the count-side companion to ExtraContracts (the Backstop's
	// blend_backstop_events land under the 'blend_backstop' census key,
	// which folds into 'blend' here). Empty for a self-contained source.
	ExtraEventSources []string
}

// protocolRegistry is the static protocol directory served by
// GET /v1/protocols. Ordering here is the wire ordering.
var protocolRegistry = withVerificationPages([]ProtocolMeta{
	{
		Name:          "sdex",
		Category:      "dex",
		Description:   "Stellar's protocol-native central-limit order book, traded via classic manage-offer and path-payment operations.",
		GenesisLedger: 2,
		EventKinds:    []string{"sdex.trade"},
	},
	{
		Name:          "soroswap",
		Category:      "amm",
		Description:   "Soroswap AMM — constant-product Soroban pairs deployed from the Soroswap factory.",
		GenesisLedger: 50_746_266,
		Factories:     soroswap.MainnetFactories,
		EventKinds:    []string{"soroswap.trade", "soroswap.skim"},
	},
	{
		Name:          "aquarius",
		Category:      "amm",
		Description:   "Aquarius AMM — incentivised constant-product and stableswap pools anchored on the Aquarius router.",
		GenesisLedger: 52_728_375,
		Factories:     []string{aquarius.MainnetRouter},
		EventKinds:    []string{"aquarius.trade", "aquarius.reserves", "aquarius.liquidity", "aquarius.rewards", "aquarius.admin"},
	},
	{
		Name:          "phoenix",
		Category:      "amm",
		Description:   "Phoenix AMM — Soroban constant-product pools with liquidity provision and stake events.",
		GenesisLedger: 51_572_016,
		EventKinds:    []string{"phoenix.trade", "phoenix.liquidity", "phoenix.stake"},
	},
	{
		Name:          "comet",
		Category:      "amm",
		Description:   "Comet — Balancer-v1-style weighted pools on Soroban (home of the BLND/USDC pool).",
		GenesisLedger: 51_499_546,
		EventKinds:    []string{"comet.trade", "comet.liquidity"},
	},
	{
		Name:          "sushiswap_v3",
		Category:      "amm",
		Description:   "SushiSwap V3 — concentrated-liquidity pools on Soroban, deployed from a single pool factory. Prices from a tick range and a Q64.96 sqrt price, not from constant-product reserves.",
		GenesisLedger: sushiswap_v3.FactoryGenesisLedger,
		Factories:     sushiswap_v3.MainnetFactories,
		EventKinds:    []string{"sushiswap_v3.trade"},
	},
	{
		Name:          "blend",
		Category:      "lending",
		Description:   "Blend — isolated lending pools on Soroban, deployed from the Blend pool factories, plus the shared Backstop insurance module.",
		GenesisLedger: blend.FactoryGenesisLedger,
		Factories:     blend.MainnetPoolFactories,
		EventKinds: []string{
			blend.PositionEventKind, blend.EmissionEventKind, blend.AdminEventKind,
			blend.NewAuctionEventKind, blend.FillAuctionEventKind, blend.DeleteAuctionEventKind,
			// The Backstop insurance module (blend_backstop source) folds into
			// the Blend protocol page: its deposit/withdraw/draw/claim/… events
			// land under blend_backstop.event, on the Backstop contracts below.
			blend_backstop.Event{}.EventKind(),
			// The Emitter (blend_emitter source) also folds in — protocol-
			// emissions plumbing, its own contract + own distribute/drop/
			// q_swap/swap vocabulary, but logically part of Blend.
			"blend_emitter.distribute", "blend_emitter.drop", "blend_emitter.swap_config",
		},
		// The two mainnet Backstop deployments plus the single Emitter
		// deployment — separate event surfaces (own contracts, own event
		// vocabularies) but part of the Blend protocol. Folding them here
		// surfaces both on /v1/protocols/blend (roster + lake analytics +
		// event count) instead of being invisible.
		ExtraContracts:    []string{blend_backstop.MainnetBackstopV1, blend_backstop.MainnetBackstopV2, blend_emitter.MainnetEmitter},
		ExtraEventSources: []string{blend_backstop.SourceName, blend_emitter.SourceName},
	},
	{
		Name:          "sorocredit",
		Category:      "lending",
		Description:   "SoroCredit — an on-chain consumer USDC credit / CDP protocol on Soroban: per-user collateral-position child contracts, recurring published statements, and scheduled settlements (the on-wire \"Liquidation\" event is a scheduled settlement, not distress).",
		GenesisLedger: sorocredit.GenesisLedger,
		Factories:     []string{sorocredit.MainnetContract},
		EventKinds: []string{
			sorocredit.SourceName + ".new_collateral_contract",
			sorocredit.SourceName + ".statement_published",
			sorocredit.SourceName + ".settlement",
			sorocredit.SourceName + ".withdrawal",
			sorocredit.SourceName + ".beacon_updated",
			sorocredit.SourceName + ".supported_asset_added",
			sorocredit.SourceName + ".collateral_hash_updated",
		},
	},
	{
		Name:          "defindex",
		Category:      "yield",
		Description:   "DeFindex — yield vaults and strategies allocating deposits across Soroban DeFi.",
		GenesisLedger: 57_056_338,
		EventKinds: []string{
			"defindex.strategy.deposit", "defindex.strategy.withdraw",
			// strategy.harvest — strategy yield realised into the vault
			// (audit 2026-08-04 finding 4; direction=harvest, migration
			// 0138). The decoder emits it and the sink persists it to
			// defindex_flows, so it is part of this source's decoded
			// vocabulary and must be listed here for the API to report it.
			"defindex.strategy.harvest",
			"defindex.vault.deposit", "defindex.vault.withdraw",
			// vault.dfees — per-asset protocol-fee distributions (W5.2;
			// body shape proven from real lake blobs 2026-08). One
			// decoded event per distributed_fees entry, persisted to
			// defindex_fees (migration 0146).
			"defindex.vault.dfees",
		},
	},
	{
		Name:        "cctp",
		Category:    "bridge",
		Description: "Circle CCTP v2 — canonical burn-and-mint USDC bridging between Stellar and other chains.",
		// Lake-derived exact genesis (2026-07-30): the MessageTransmitter's
		// first on-chain event. The old 62_403_000 was the ingestion-config
		// floor, ~121k ledgers late — the head has since been re-projected
		// from here (density-genesis precision rule).
		GenesisLedger: 62_146_641,
		EventKinds:    []string{"cctp.event"},
	},
	{
		Name:        "rozo",
		Category:    "bridge",
		Description: "Rozo — intent-bridge payment settlement on Stellar (v1 Payment contract).",
		// Lake-derived exact genesis (2026-07-30): first event across all
		// four Rozo contracts; rozo_events is projected to exactly here.
		GenesisLedger: 60_829_397,
		EventKinds:    []string{"rozo.event"},
	},
	{
		Name:          "soroswap-router",
		Category:      "amm",
		Description:   "Soroswap router — aggregated multi-hop swap intents observed from router invocations (event-less; ContractCall-derived).",
		GenesisLedger: 50_746_272,
		EventKinds:    []string{"soroswap-router.swap"},
	},
	{
		Name:        "band",
		Category:    "oracle",
		Description: "Band Protocol oracle — reference rates observed from relay()/force_relay() invocations (the contract emits no events).",
		// Band's first on-chain write. Not discoverable from contract_events —
		// Band emits no events — so this comes from contract_instance_changes
		// and the WASM audit; see reconciliation_catalogue.go (#361/#363).
		GenesisLedger: 50_842_736,
		EventKinds:    []string{"band.update"},
	},
	{
		Name:          "reflector-dex",
		Category:      "oracle",
		Description:   "Reflector oracle, DEX feed — on-chain price updates from the Reflector Stellar-DEX contract.",
		GenesisLedger: 50_644_229,
		EventKinds:    []string{"reflector.update"},
	},
	{
		Name:          "reflector-cex",
		Category:      "oracle",
		Description:   "Reflector oracle, CEX feed — centralized-exchange price updates from the Reflector CEX contract.",
		GenesisLedger: 50_644_239,
		EventKinds:    []string{"reflector.update"},
	},
	{
		Name:          "reflector-fx",
		Category:      "oracle",
		Description:   "Reflector oracle, FX feed — fiat exchange-rate updates from the Reflector FX contract.",
		GenesisLedger: 56_733_481,
		EventKinds:    []string{"reflector.update"},
	},
	{
		Name:          "redstone",
		Category:      "oracle",
		Description:   "RedStone oracle — batched multi-feed price pushes to the RedStone adapter contract.",
		GenesisLedger: 58_758_722,
		EventKinds:    []string{"redstone.update"},
	},
})

// verificationPageDocs names the protocols whose write-up does not live
// at docs/protocols/<name>.md: the three Reflector feeds are three
// source names sharing one page, and the router is a view of Soroswap's
// swaps, documented on the Soroswap page. A name mapped to "" declares a
// protocol that has no page yet — the API serves no link for it and
// TestProtocolVerificationPages stops requiring one.
var verificationPageDocs = map[string]string{
	"reflector-dex":   "reflector",
	"reflector-cex":   "reflector",
	"reflector-fx":    "reflector",
	"soroswap-router": "soroswap",
}

// withVerificationPages stamps each entry's verification-page path from
// its name, so writing docs/protocols/<name>.md is all it takes to link
// a protocol's write-up. Deriving it is the fix for a second, silent
// edit site: the pages for ten protocols — sdex, comet, cctp, rozo,
// band, redstone, the three Reflector feeds and the router — existed in
// the tree while /v1/protocols reported verification_page: null, because
// per-row literals here were only ever added for six.
func withVerificationPages(reg []ProtocolMeta) []ProtocolMeta {
	for i := range reg {
		doc, ok := verificationPageDocs[reg[i].Name]
		if !ok {
			doc = reg[i].Name
		}
		if doc == "" {
			continue
		}
		reg[i].VerificationPage = "docs/protocols/" + doc + ".md"
	}
	return reg
}

// protocolByName returns the registry entry for name, false when the
// name isn't a known protocol.
func protocolByName(name string) (ProtocolMeta, bool) {
	for _, p := range protocolRegistry {
		if p.Name == name {
			return p, true
		}
	}
	return ProtocolMeta{}, false
}
