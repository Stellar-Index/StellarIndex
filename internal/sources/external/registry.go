package external

import (
	"sort"
	"time"
)

// Registry is the metadata table for every source the aggregator knows, external and on-chain, so it can
// read Registry[trade.Source].Class without importing every source package. A miss falls back to
// ClassExchange with IncludeInVWAP=false: visible in /v1/sources, fail-closed out of VWAP. Venues toggle via
// `enabled` in config.ExternalConfig; Class and Paid are venue facts, and per-venue weight/VWAP overrides
// are not wired.
//
// Backfill is BackfillPerWASM for on-chain Soroban sources: a replay is admitted only over a range whose
// every active WASM hash is in internal/wasmaudit/audited_wasm.json (`update_contract` can change event
// schemas). Off-chain sources and SDEX are BackfillNoWASM; BackfillUnsafe refuses outright.
var Registry = map[string]Metadata{
	// ─── On-chain exchanges (dispatcher-path; listed here so the
	// aggregator has a single lookup table) ──────────────────────
	"soroswap": {Class: ClassExchange, Subclass: SubclassDEX, AmountDecimals: 7, DefaultWeight: 100, IncludeInVWAP: true, Paid: false, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; see docs/operations/wasm-audits/soroswap.md */},
	"aquarius": {Class: ClassExchange, Subclass: SubclassDEX, AmountDecimals: 7, DefaultWeight: 100, IncludeInVWAP: true, Paid: false, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; 313 pools, 3 unique WASMs, shared-emitter topology. See docs/operations/wasm-audits/aquarius.md */},
	"phoenix":  {Class: ClassExchange, Subclass: SubclassDEX, AmountDecimals: 7, DefaultWeight: 100, IncludeInVWAP: true, Paid: false, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; 11 pools, 2 unique WASM hashes, both contain 8 expected swap-field strings. Byte presence isn't runtime uniformity: the pre-upgrade pool WASM only actually emitted 7 of the 8 fields (no ActualReceived) for ledgers 51,019,036-53,134,167 — see phoenix/decode.go's RawSwap.Decodable doc for the reduced-field recovery path. See docs/operations/wasm-audits/phoenix.md */},
	"comet":    {Class: ClassExchange, Subclass: SubclassDEX, AmountDecimals: 7, DefaultWeight: 100, IncludeInVWAP: true, Paid: false, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; only known mainnet pool is Blend backstop CAS3FL6T..., WASM 8abc2891... verified. See docs/operations/wasm-audits/comet.md */},
	"sdex":     {Class: ClassExchange, Subclass: SubclassDEX, AmountDecimals: 7, DefaultWeight: 100, IncludeInVWAP: true, Paid: false, BackfillAvailable: true, Backfill: BackfillNoWASM},
	// SushiSwap V3 — concentrated liquidity. The pools have been through
	// two factory-driven upgrades (ledgers 61,594,973 and 62,898,378); the
	// per-WASM gate admits a range only where every version is audited
	// (docs/operations/wasm-audits/sushiswap_v3.md).
	"sushiswap_v3": {Class: ClassExchange, Subclass: SubclassDEX, AmountDecimals: 7, DefaultWeight: 100, IncludeInVWAP: true, Paid: false, BackfillAvailable: true, Backfill: BackfillPerWASM},

	// Upshift tokenized vaults (earnUSDC, earnXLM): ClassRouter like DeFindex, since a vault allocates deposits
	// across other protocols and publishes no price (RedStone's earnUSDC price arrives on the oracle path).
	// AmountDecimals is unset: vault events carry `assets` and `shares` on different scales. Backfill stays
	// Unsafe until a WASM audit page exists.
	"upshift": {Class: ClassRouter, DefaultWeight: 0, IncludeInVWAP: false, Paid: false, BackfillAvailable: true, Backfill: BackfillUnsafe},

	// Spectra yield-tokenisation markets: ClassRouter because PT/YT swaps and
	// wraps are derivative of the underlying IBT, so they never feed a price.
	// All eight WASM hashes audited (docs/operations/wasm-audits/spectra.md).
	"spectra": {Class: ClassRouter, DefaultWeight: 0, IncludeInVWAP: false, Paid: false, BackfillAvailable: true, Backfill: BackfillPerWASM},

	// ─── On-chain oracles ────────────────────────────────────────
	// Excluded from VWAP by default — they publish already-aggregated
	// derived prices with their own governance and methodology. Reported
	// alongside for transparency. Operator opts one in per-source via
	// config if they want oracle-inclusive aggregation.
	"reflector-dex": {Class: ClassOracle, DefaultWeight: 100, IncludeInVWAP: false, Paid: false, OracleResolution: 5 * time.Minute, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; v2 disassembly confirms compat. See docs/operations/wasm-audits/reflector.md */},
	"reflector-cex": {Class: ClassOracle, DefaultWeight: 100, IncludeInVWAP: false, Paid: false, OracleResolution: 5 * time.Minute, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; v2 disassembly confirms compat. See docs/operations/wasm-audits/reflector.md */},
	"reflector-fx":  {Class: ClassOracle, DefaultWeight: 100, IncludeInVWAP: false, Paid: false, OracleResolution: 5 * time.Minute, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; see docs/operations/wasm-audits/reflector.md */},
	"redstone":      {Class: ClassOracle, DefaultWeight: 100, IncludeInVWAP: false, Paid: false, OracleResolution: 24 * time.Hour, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; see docs/operations/wasm-audits/redstone.md */},
	"band":          {Class: ClassOracle, DefaultWeight: 100, IncludeInVWAP: false, Paid: false, OracleResolution: time.Hour, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; see docs/operations/wasm-audits/band.md */},

	// ─── On-chain lending protocols ─────────────────────────────
	// Auction events surface stress-prices during liquidations; we
	// report them alongside as a secondary validation surface but
	// they DO NOT contribute to VWAP. See the blend source package
	// README for the full extraction scope.
	"blend": {Class: ClassLending, DefaultWeight: 100, IncludeInVWAP: false, Paid: false, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; 11 contracts (9 pools + backstop + factory), 3 unique WASMs, no mid-life upgrades observed in 5h4m walk over [50457424, 62249727]. See docs/operations/wasm-audits/blend.md §"Phase 2 results". */},
	// blend_emitter: BLND emissions plumbing in the `blend` family; no price, never VWAP. Audited against the
	// ClickHouse lake rather than a wasm-history walk: all 469 lifetime events decode to the expected shape and
	// the sole WASM hash (438a5528…) is SHA256-verified (docs/operations/wasm-audits/blend_emitter.md).
	"blend_emitter": {Class: ClassLending, DefaultWeight: 0, IncludeInVWAP: false, Paid: false, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; see docs/operations/wasm-audits/blend_emitter.md */},
	// sorocredit — an unbranded consumer-USDC credit / CDP protocol
	// (single main contract CCG5EWFY…). Credit positions / statements /
	// scheduled-settlements — no published price, never VWAP. Its
	// "Liquidation" events are SCHEDULED SETTLEMENTS, not distress — see
	// internal/sources/sorocredit/README.md.
	"sorocredit": {Class: ClassLending, DefaultWeight: 0, IncludeInVWAP: false, Paid: false, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited lake-direct (ADR-0034): single instance WASM 84a88013…810ea set at deploy (ledger 61,620,824), zero executable changes in the dense-coverage window [62.0M→tip]; all 7 event types have one invariant on-wire schema across the contract's whole life (NewCollateralContract structurally identical 61,624,053→63,363,505, spanning the sparse early window). Safe from genesis 61,620,822. See docs/operations/wasm-audits/sorocredit.md */},

	// ─── On-chain routers + aggregator vaults ────────────────────
	// Excluded from VWAP: they emit no independent trades, they invoke contracts that do. Captured for per-tx
	// attribution and user intent (path requested vs realised; docs/architecture/explorer-data-inventory.md
	// §9.9). Vault exposure to underlying protocols is not persisted.
	"soroswap-router": {Class: ClassRouter, DefaultWeight: 0, IncludeInVWAP: false, Paid: false, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; r1 wasm-history walk: single hash 4c3db3eb...07 over the contract's entire life [50746272→tip], zero mid-life upgrades; both swap_exact_tokens_for_tokens + swap_tokens_for_exact_tokens exports verified present; ContractCallDecoder (router emits no events). See docs/operations/wasm-audits/soroswap-router.md */},
	"defindex":        {Class: ClassRouter, DefaultWeight: 0, IncludeInVWAP: false, Paid: false, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; decoder re-derived to the real on-chain schema ("BlendStrategy",deposit|withdraw){from,amount}, topic-dispatched across all emitters (the tag-1.0.0 "DeFindexVault" schema was fiction; deployed WASM 11329c24...988 is Blend strategy code). Live-verified after deploy: indexer emitted `defindex strategy flow` log lines against real traffic (9 in 90min sample). wasm2wat data-section scan of the deployed bytes confirmed all required symbols present (BlendStrategy/deposit/withdraw/from/amount). See docs/operations/wasm-audits/defindex.md */},

	// ─── Cross-chain bridges (flow coverage; excluded from VWAP) ─
	// Bridges publish no prices and emit no trades. CCTP's deposit_for_burn / mint_and_withdraw are USDC supply
	// exits and entries beyond the classic trustline mint/burn channel. WASM audit:
	// docs/operations/wasm-audits/cctp.md; protocol: docs/protocols/cctp.md.
	"cctp": {Class: ClassBridge, DefaultWeight: 0, IncludeInVWAP: false, Paid: false, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited via wasm-history walk [60M, 62.64M] across all 3 mainnet contracts (TokenMessengerMinter, MessageTransmitter, CctpForwarder): zero WASM upgrades observed, ranges=null. Single deploy confirmed via stellar.expert. See docs/operations/wasm-audits/cctp.md. */},
	// Rozo v1 intent-bridge — same bridge semantics. payment / flush
	// events from the three live v1 Payment contracts. Audited alongside
	// CCTP — same walk.
	"rozo": {Class: ClassBridge, DefaultWeight: 0, IncludeInVWAP: false, Paid: false, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited via wasm-history walk [60M, 62.64M] across the original 3 mainnet payment contracts: zero WASM upgrades observed, ranges=null. Single WASM hash b56aedeaf80c3d4b... shared across all three contracts per stellar.expert. A 4th contract (internal/sources/rozo.MainnetPaymentContracts) carries the SAME wasm hash per direct lake lookup, so it's covered by this finding without a separate walk. See docs/operations/wasm-audits/rozo.md. */},

	// ─── Off-chain centralised exchanges (this package's scope) ─
	"binance":  {Class: ClassExchange, Subclass: SubclassCEX, DefaultWeight: 100, IncludeInVWAP: true, Paid: false, BackfillAvailable: true, Backfill: BackfillNoWASM},
	"kraken":   {Class: ClassExchange, Subclass: SubclassCEX, DefaultWeight: 100, IncludeInVWAP: true, Paid: false, BackfillAvailable: true /* implemented, but 720-interval cap: ~30d at 1h */, Backfill: BackfillNoWASM},
	"bitstamp": {Class: ClassExchange, Subclass: SubclassCEX, DefaultWeight: 100, IncludeInVWAP: true, Paid: false, BackfillAvailable: true, Backfill: BackfillNoWASM},
	// Poloniex XLM/BTC crossed with a USD-quoted BTC leg: history-only daily
	// bars for the span before any USD venue listed XLM. A derived close is
	// not a fill, so the live aggregator never counts it toward VWAP.
	"poloniex_via_btc": {Class: ClassExchange, Subclass: SubclassCEX, DefaultWeight: 0, IncludeInVWAP: false, Paid: false, BackfillAvailable: true, Backfill: BackfillNoWASM},
	"coinbase":         {Class: ClassExchange, Subclass: SubclassCEX, DefaultWeight: 100, IncludeInVWAP: true, Paid: false, BackfillAvailable: true, Backfill: BackfillNoWASM},

	// ─── Institutional FX feeds ──────────────────────────────────
	// `massive` is the active fiat-FX feed (massive.com, the vendor once branded polygon.io). There is
	// deliberately NO `polygon-forex` entry: it named the same upstream with IncludeInVWAP:true and would
	// double-count. Do not re-add it. The forex worker (API binary) polls hourly but writes fx_quotes at
	// one row per ticker per UTC day (every write buckets to Truncate(24 * time.Hour)): the USD anchor
	// behind per-trade usd_volume, /v1/assets fiat pricing and the /v1/chart fiat series. Registered so /v1/sources classifies it off-chain FX, not the unknown fallback.
	//
	// `exchangeratesapi` (disabled) is NOT a forex-snap fallback: it emits only OracleUpdates, so
	// FXQuoteAtOrBefore's `trades` arm finds nothing. The only fallback is the forex worker's ECB standby
	// (forex.ECBProvider, fx_quotes source "ecb"). forex.OpenExchangeRatesProvider has no row; wiring it must
	// add one or the FX-snap class check refuses it. FX pollers stamp 1e6, not the CEX 1e8 (AmountDecimals:6,
	// for the USD-volume gate). OracleResolution is a trading day: FX holds through the ~48h weekend close.
	"massive":          {Class: ClassExchange, Subclass: SubclassFX, DefaultWeight: 100, IncludeInVWAP: true, Paid: true, BackfillAvailable: true, Backfill: BackfillNoWASM, AmountDecimals: 6},
	"exchangeratesapi": {Class: ClassExchange, Subclass: SubclassFX, DefaultWeight: 100, IncludeInVWAP: true, Paid: true, BackfillAvailable: true, Backfill: BackfillNoWASM, AmountDecimals: 6, OracleResolution: 24 * time.Hour},

	// ─── Aggregators (divergence signal; excluded from VWAP) ─────
	"coingecko":     {Class: ClassAggregator, DefaultWeight: 100, IncludeInVWAP: false, Paid: false, BackfillAvailable: true, Backfill: BackfillNoWASM, OracleResolution: 5 * time.Minute},
	"coinmarketcap": {Class: ClassAggregator, DefaultWeight: 100, IncludeInVWAP: false, Paid: true, BackfillAvailable: true, Backfill: BackfillNoWASM, OracleResolution: time.Minute},
	"cryptocompare": {Class: ClassAggregator, DefaultWeight: 100, IncludeInVWAP: false, Paid: true, BackfillAvailable: true, Backfill: BackfillNoWASM, OracleResolution: time.Minute},

	// ─── Sovereign daily anchors (sanity check only) ─────────────
	// ECB publishes once per TARGET business day, hence a 24 h resolution.
	// SubclassFX because the forex worker's ECB standby writes fx_quotes
	// as "ecb"; without it the FX-snap class check refuses every standby row.
	"ecb": {Class: ClassAuthoritySanity, Subclass: SubclassFX, DefaultWeight: 100, IncludeInVWAP: false, Paid: false, BackfillAvailable: true, Backfill: BackfillNoWASM, AmountDecimals: 6, OracleResolution: 24 * time.Hour},

	// ─── Off-chain oracles (Chainlink via EVM RPC) ───────────────
	// Chainlink lives on Ethereum mainnet, read over JSON-RPC from AggregatorV3 contracts (backfill walks
	// AnswerUpdated via eth_getLogs): a price publisher, so ClassOracle, and NoWASM. OracleResolution is 24h:
	// Timestamp is the round's updatedAt, and the slowest (FX) feeds heartbeat daily and pause on weekends.
	"chainlink": {Class: ClassOracle, DefaultWeight: 100, IncludeInVWAP: false, Paid: false /* Alchemy free tier covers 516-feed scale */, BackfillAvailable: true, Backfill: BackfillNoWASM, OracleResolution: 24 * time.Hour},

	// Tiingo publishes registered funds' daily NAV. Rows are `raw:<TICKER>`,
	// read only by the RWA reference surface through the curated fund
	// bindings in internal/rwa — never a VWAP input or a pair leg, hence
	// weight 0. OracleResolution is 24 h: one NAV per business day.
	"tiingo": {Class: ClassOracle, DefaultWeight: 0, IncludeInVWAP: false, Paid: false, BackfillAvailable: false, Backfill: BackfillNoWASM, AmountDecimals: 6, OracleResolution: 24 * time.Hour},
}

// Lookup returns metadata for a source, with a safe fallback for
// unknown names. The fallback intentionally excludes-from-VWAP so a
// typo or renamed source can't quietly inject unauthorised data into
// aggregation — it shows up in /v1/sources as `class=exchange,
// included_in_vwap=false` and ops fixes the registry entry.
func Lookup(source string) Metadata {
	if m, ok := Registry[source]; ok {
		return m
	}
	return Metadata{
		Class:         ClassExchange,
		DefaultWeight: 100,
		IncludeInVWAP: false,          // fail-closed — see doc above
		Backfill:      BackfillUnsafe, // fail-closed — unknown sources cannot backfill
	}
}

// Registered reports whether source has an explicit [Registry] entry.
// Lookup collapses "unregistered" and "registered with AmountDecimals
// unset" into the same zero-value-derived answer (both read as
// AmountScaleDecimals()==8), which is safe for Lookup's own VWAP-
// inclusion fallback but wrong for a caller that needs to tell "no
// entry" apart from "entry, CEX-default scale" — see
// commonAmountScaleDecimals in internal/api/v1/ohlc.go.
func Registered(source string) bool {
	_, ok := Registry[source]
	return ok
}

// IncludeInVWAP is a convenience wrapper for the most-common
// aggregator-side question. Returns true only when the source is
// registered AND its IncludeInVWAP flag is true.
func IncludeInVWAP(source string) bool {
	return Lookup(source).IncludeInVWAP
}

// BackfillSafe reports whether the source's policy admits any replay.
// A BackfillPerWASM source is further gated per range by internal/wasmaudit.
func BackfillSafe(source string) bool {
	return Lookup(source).BackfillSafe()
}

// replayAuditCoveredBy maps a projected source with no Registry row to the entry whose WASM audit covers its
// decoder. blend_backstop is a lending surface, not a venue, but Blend's wasm-history walk covered the
// backstop contract (docs/operations/wasm-audits/blend.md), so its replay safety stands or falls with
// `blend`'s. String literals, not SourceName constants: those packages import this one; ops tests pin them.
var replayAuditCoveredBy = map[string]string{
	"blend_backstop": "blend",
}

// replayStandardSchemaSources read a schema fixed by a standard (SEP-41 / CAP-67) across curated arbitrary
// token contracts, so the per-protocol WASM audit has no single subject. Their sanctioned re-derives,
// `projector-replay -source sep41_supply` and `ch-rebuild -sep41` (docs/operations/sep41-mint-recovery.md),
// must not be stranded by this gate.
var replayStandardSchemaSources = map[string]struct{}{
	"sep41_transfers": {},
	"sep41_supply":    {},
}

// ReplayBackfillSafe is [BackfillSafe] for the re-derive paths (projector-replay, ch-rebuild), which run a
// current decoder over historical events with the same old-WASM hazard as `backfill`. It differs only in
// accepting projector source names (see replayAuditCoveredBy, replayStandardSchemaSources); any other name,
// registered or not, gets BackfillSafe's fail-closed answer.
func ReplayBackfillSafe(source string) bool {
	if ReplayExempt(source) {
		return true
	}
	return BackfillSafe(ReplayAuditSubject(source))
}

// ReplayExempt reports a standard-schema source (the sep41 pair) that the
// per-protocol WASM audit does not apply to.
func ReplayExempt(source string) bool {
	_, ok := replayStandardSchemaSources[source]
	return ok
}

// ReplayAuditSubject is the Registry source whose WASM audit covers a
// projector source name (blend_backstop -> blend; every other name maps to itself).
func ReplayAuditSubject(source string) string {
	if coveredBy, ok := replayAuditCoveredBy[source]; ok {
		return coveredBy
	}
	return source
}

// UnsafeReplaySources filters `sources` to those [ReplayBackfillSafe]
// refuses, preserving order, so a re-derive command can name every
// offender in one refusal.
func UnsafeReplaySources(sources []string) []string {
	var out []string
	for _, s := range sources {
		if !ReplayBackfillSafe(s) {
			out = append(out, s)
		}
	}
	return out
}

// FXSources returns the registered source names whose Subclass is
// SubclassFX, in deterministic lexicographic order. Used by the
// forex-snap rule (FXQuoteAtOrBefore): `massive` in the list
// admits the fx_quotes-first read (the active feed's table), and the
// full list scopes the trades-hypertable fallback; the stable
// order makes the across-region tiebreak deterministic when two FX
// sources publish the same observed_at.
func FXSources() []string {
	out := make([]string, 0, 2)
	for name, m := range Registry {
		if m.Subclass == SubclassFX {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// IsFXSource reports whether the named source has Subclass=SubclassFX.
// Convenience wrapper for the snap-rule's per-leg classification.
func IsFXSource(source string) bool {
	return Lookup(source).Subclass == SubclassFX
}

// IsOnChain reports whether a source observes Stellar directly rather than an off-chain vendor API.
// Off-chain: CEX and FX venues, aggregators, sovereign anchors, and the two non-Stellar ClassOracle sources
// (Chainlink, Tiingo). The explorer's network surfaces filter on it so reference-pricing feeds don't pose as
// on-chain activity. Unknown names fall to on-chain, which only matters for typos: the registry is closed.
func IsOnChain(source string) bool {
	m := Lookup(source)
	// Off-chain subclasses short-circuit to false; every other subclass
	// (incl. on-chain DEX) falls through to the class check + on-chain default.
	//exhaustive:ignore
	switch m.Subclass {
	case SubclassCEX, SubclassFX:
		return false
	}
	// Reference-pricing classes are off-chain; the remaining classes
	// (Exchange DEX, Oracle, Lending, Router, Bridge) are Soroban on-chain
	// and fall through to `return true`.
	//exhaustive:ignore
	switch m.Class {
	case ClassAggregator, ClassAuthoritySanity:
		return false
	}
	// Chainlink (Ethereum mainnet via JSON-RPC) and Tiingo (a vendor REST
	// API) are the off-chain ClassOracle sources; class alone can't separate them.
	if source == "chainlink" || source == "tiingo" {
		return false
	}
	return true
}

// AggregatorSources returns every ClassAggregator source, sorted. The `aggregator_avg` tier of the
// global-price fallback passes it to Store.LatestAggregatorPricesForPair, keeping class policy out of
// storage. Disabled aggregators (e.g. CMC without a key) stay listed; they simply have no rows.
func AggregatorSources() []string {
	out := make([]string, 0, 4)
	for name, m := range Registry {
		if m.Class == ClassAggregator {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
