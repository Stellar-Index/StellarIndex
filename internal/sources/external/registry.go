package external

import (
	"sort"
	"time"
)

// Registry is the metadata table for every known source; a miss falls back to ClassExchange with IncludeInVWAP=false, fail-closed out of VWAP, and on-chain Soroban sources are BackfillPerWASM because `update_contract` can change event schemas.
var Registry = map[string]Metadata{
	// ─── On-chain exchanges (dispatcher-path; listed here so the
	// aggregator has a single lookup table) ──────────────────────
	"soroswap": {Class: ClassExchange, Subclass: SubclassDEX, AmountDecimals: 7, DefaultWeight: 100, IncludeInVWAP: true, Paid: false, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; see docs/operations/wasm-audits/soroswap.md */},
	"aquarius": {Class: ClassExchange, Subclass: SubclassDEX, AmountDecimals: 7, DefaultWeight: 100, IncludeInVWAP: true, Paid: false, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; 313 pools, 3 unique WASMs, shared-emitter topology. See docs/operations/wasm-audits/aquarius.md */},
	"phoenix":  {Class: ClassExchange, Subclass: SubclassDEX, AmountDecimals: 7, DefaultWeight: 100, IncludeInVWAP: true, Paid: false, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; the pre-upgrade pool WASM emitted only 7 of 8 swap fields (see phoenix/decode.go RawSwap.Decodable). See docs/operations/wasm-audits/phoenix.md */},
	"comet":    {Class: ClassExchange, Subclass: SubclassDEX, AmountDecimals: 7, DefaultWeight: 100, IncludeInVWAP: true, Paid: false, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; only known mainnet pool is Blend backstop CAS3FL6T..., WASM 8abc2891... verified. See docs/operations/wasm-audits/comet.md */},
	"sdex":     {Class: ClassExchange, Subclass: SubclassDEX, AmountDecimals: 7, DefaultWeight: 100, IncludeInVWAP: true, Paid: false, BackfillAvailable: true, Backfill: BackfillNoWASM},
	// SushiSwap V3 pools were upgraded twice by their factory; the per-WASM gate admits a range
	// only where every version is audited (docs/operations/wasm-audits/sushiswap_v3.md).
	"sushiswap_v3": {Class: ClassExchange, Subclass: SubclassDEX, AmountDecimals: 7, DefaultWeight: 100, IncludeInVWAP: true, Paid: false, BackfillAvailable: true, Backfill: BackfillPerWASM},

	// Upshift vaults are ClassRouter like DeFindex: a vault allocates deposits across other
	// protocols and publishes no price. AmountDecimals is unset because `assets` and `shares`
	// are on different scales. Backfill stays Unsafe until a WASM audit page exists.
	"upshift": {Class: ClassRouter, DefaultWeight: 0, IncludeInVWAP: false, Paid: false, BackfillAvailable: true, Backfill: BackfillUnsafe},

	// Spectra is ClassRouter: PT/YT swaps and wraps derive from the underlying IBT, so they
	// never feed a price (docs/operations/wasm-audits/spectra.md).
	"spectra": {Class: ClassRouter, DefaultWeight: 0, IncludeInVWAP: false, Paid: false, BackfillAvailable: true, Backfill: BackfillPerWASM},

	// ─── On-chain oracles ────────────────────────────────────────
	// Excluded from VWAP by default: they publish already-aggregated prices with their own
	// methodology. An operator can opt one in per source via config.
	"reflector-dex": {Class: ClassOracle, DefaultWeight: 100, IncludeInVWAP: false, Paid: false, OracleResolution: 5 * time.Minute, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; v2 disassembly confirms compat. See docs/operations/wasm-audits/reflector.md */},
	"reflector-cex": {Class: ClassOracle, DefaultWeight: 100, IncludeInVWAP: false, Paid: false, OracleResolution: 5 * time.Minute, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; v2 disassembly confirms compat. See docs/operations/wasm-audits/reflector.md */},
	"reflector-fx":  {Class: ClassOracle, DefaultWeight: 100, IncludeInVWAP: false, Paid: false, OracleResolution: 5 * time.Minute, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; see docs/operations/wasm-audits/reflector.md */},
	"redstone":      {Class: ClassOracle, DefaultWeight: 100, IncludeInVWAP: false, Paid: false, OracleResolution: 24 * time.Hour, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; see docs/operations/wasm-audits/redstone.md */},
	"band":          {Class: ClassOracle, DefaultWeight: 100, IncludeInVWAP: false, Paid: false, OracleResolution: time.Hour, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; see docs/operations/wasm-audits/band.md */},

	// ─── On-chain lending protocols ─────────────────────────────
	// Auction events surface stress prices during liquidations; they are reported as a secondary
	// validation surface and DO NOT contribute to VWAP.
	"blend": {Class: ClassLending, DefaultWeight: 100, IncludeInVWAP: false, Paid: false, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; 11 contracts, 3 unique WASMs, no mid-life upgrades observed. See docs/operations/wasm-audits/blend.md §"Phase 2 results". */},
	// blend_emitter is BLND emissions plumbing: no price, never VWAP. Audited against the lake
	// (docs/operations/wasm-audits/blend_emitter.md).
	"blend_emitter": {Class: ClassLending, DefaultWeight: 0, IncludeInVWAP: false, Paid: false, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; see docs/operations/wasm-audits/blend_emitter.md */},
	// sorocredit is a consumer-USDC credit protocol with no published price, never VWAP. Its
	// "Liquidation" events are scheduled settlements, not distress.
	"sorocredit": {Class: ClassLending, DefaultWeight: 0, IncludeInVWAP: false, Paid: false, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited lake-direct: one instance WASM since deploy, all 7 event types keep one on-wire schema; safe from genesis 61,620,822. See docs/operations/wasm-audits/sorocredit.md */},

	// ─── On-chain routers + aggregator vaults ────────────────────
	// Excluded from VWAP: they invoke contracts that trade. Captured for per-tx attribution and
	// intent (docs/architecture/explorer-data-inventory.md §9.9); vault exposure is not persisted.
	"soroswap-router": {Class: ClassRouter, DefaultWeight: 0, IncludeInVWAP: false, Paid: false, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; single WASM hash for the contract's whole life; decoded by ContractCallDecoder because the router emits no events. See docs/operations/wasm-audits/soroswap-router.md */},
	"defindex":        {Class: ClassRouter, DefaultWeight: 0, IncludeInVWAP: false, Paid: false, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; decoder follows the real on-chain schema ("BlendStrategy",deposit|withdraw){from,amount}, topic-dispatched across all emitters. See docs/operations/wasm-audits/defindex.md */},

	// ─── Cross-chain bridges (flow coverage; excluded from VWAP) ─
	// Bridges publish no prices and emit no trades. CCTP burns and mints are USDC supply exits
	// and entries beyond the classic trustline channel (docs/operations/wasm-audits/cctp.md).
	"cctp": {Class: ClassBridge, DefaultWeight: 0, IncludeInVWAP: false, Paid: false, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited across all 3 mainnet contracts; no WASM upgrades observed. See docs/operations/wasm-audits/cctp.md. */},
	// Rozo v1 intent-bridge: same bridge semantics, audited alongside CCTP.
	"rozo": {Class: ClassBridge, DefaultWeight: 0, IncludeInVWAP: false, Paid: false, BackfillAvailable: true, Backfill: BackfillPerWASM /* audited; no WASM upgrades, one hash shared by every payment contract in rozo.MainnetPaymentContracts. See docs/operations/wasm-audits/rozo.md. */},

	// ─── Off-chain centralised exchanges (this package's scope) ─
	"binance":  {Class: ClassExchange, Subclass: SubclassCEX, DefaultWeight: 100, IncludeInVWAP: true, Paid: false, BackfillAvailable: true, Backfill: BackfillNoWASM},
	"kraken":   {Class: ClassExchange, Subclass: SubclassCEX, DefaultWeight: 100, IncludeInVWAP: true, Paid: false, BackfillAvailable: true /* implemented, but 720-interval cap: ~30d at 1h */, Backfill: BackfillNoWASM},
	"bitstamp": {Class: ClassExchange, Subclass: SubclassCEX, DefaultWeight: 100, IncludeInVWAP: true, Paid: false, BackfillAvailable: true, Backfill: BackfillNoWASM},
	// Poloniex XLM/BTC crossed with a USD BTC leg: history-only daily bars from before any USD
	// venue listed XLM. A derived close is not a fill, so never VWAP.
	"poloniex_via_btc": {Class: ClassExchange, Subclass: SubclassCEX, DefaultWeight: 0, IncludeInVWAP: false, Paid: false, BackfillAvailable: true, Backfill: BackfillNoWASM},
	"coinbase":         {Class: ClassExchange, Subclass: SubclassCEX, DefaultWeight: 100, IncludeInVWAP: true, Paid: false, BackfillAvailable: true, Backfill: BackfillNoWASM},

	// ─── Institutional FX feeds ──────────────────────────────────
	// `massive` is the active fiat-FX feed; there is deliberately NO `polygon-forex` entry, since
	// it names the same upstream and would double-count in VWAP. fx_quotes holds one row per
	// ticker per UTC day: the USD anchor behind usd_volume and fiat pricing.
	// `exchangeratesapi` (disabled) emits only OracleUpdates, so it is not a forex-snap fallback;
	// the only fallback is the ECB standby, and any new FX provider needs a row here or the
	// FX-snap class check refuses it. FX pollers stamp 1e6, not the CEX 1e8. OracleResolution is
	// a trading day because FX holds through the ~48h weekend close.
	"massive":          {Class: ClassExchange, Subclass: SubclassFX, DefaultWeight: 100, IncludeInVWAP: true, Paid: true, Credit: "Massive", BackfillAvailable: true, Backfill: BackfillNoWASM, AmountDecimals: 6},
	"exchangeratesapi": {Class: ClassExchange, Subclass: SubclassFX, DefaultWeight: 100, IncludeInVWAP: true, Paid: true, Credit: "exchangeratesapi.io", BackfillAvailable: true, Backfill: BackfillNoWASM, AmountDecimals: 6, OracleResolution: 24 * time.Hour},

	// ─── Aggregators (divergence signal; excluded from VWAP) ─────
	"coingecko":     {Class: ClassAggregator, DefaultWeight: 100, IncludeInVWAP: false, Paid: false, Credit: "CoinGecko", BackfillAvailable: true, Backfill: BackfillNoWASM, OracleResolution: 5 * time.Minute},
	"coinmarketcap": {Class: ClassAggregator, DefaultWeight: 100, IncludeInVWAP: false, Paid: true, Credit: "CoinMarketCap", BackfillAvailable: true, Backfill: BackfillNoWASM, OracleResolution: time.Minute},
	"cryptocompare": {Class: ClassAggregator, DefaultWeight: 100, IncludeInVWAP: false, Paid: true, Credit: "CCData (CryptoCompare)", BackfillAvailable: true, Backfill: BackfillNoWASM, OracleResolution: time.Minute},

	// ─── Sovereign daily anchors (sanity check only) ─────────────
	// ECB publishes once per TARGET business day (24 h resolution). SubclassFX because the ECB
	// standby writes fx_quotes as "ecb"; without it the FX-snap class check refuses every row.
	"ecb": {Class: ClassAuthoritySanity, Subclass: SubclassFX, DefaultWeight: 100, IncludeInVWAP: false, Paid: false, Credit: "European Central Bank euro foreign exchange reference rates", BackfillAvailable: true, Backfill: BackfillNoWASM, AmountDecimals: 6, OracleResolution: 24 * time.Hour},

	// ─── Off-chain oracles (Chainlink via EVM RPC) ───────────────
	// Chainlink is read over Ethereum JSON-RPC: a price publisher, so ClassOracle, and NoWASM.
	// OracleResolution is 24h: Timestamp is the round's updatedAt, and the slowest (FX) feeds
	// heartbeat daily and pause on weekends.
	"chainlink": {Class: ClassOracle, DefaultWeight: 100, IncludeInVWAP: false, Paid: false /* Alchemy free tier covers 516-feed scale */, BackfillAvailable: true, Backfill: BackfillNoWASM, OracleResolution: 24 * time.Hour},

	// Tiingo publishes funds' daily NAV as `raw:<TICKER>` rows, read only by the RWA reference
	// surface; never a VWAP input or a pair leg, hence weight 0. One NAV per business day.
	"tiingo": {Class: ClassOracle, DefaultWeight: 0, IncludeInVWAP: false, Paid: false, Credit: "Tiingo", BackfillAvailable: false, Backfill: BackfillNoWASM, AmountDecimals: 6, OracleResolution: 24 * time.Hour},
}

// Lookup returns metadata for a source; unknown names fall back to class=exchange with included_in_vwap=false, so a typo cannot inject unvetted data into aggregation.
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

// Registered reports whether source has an explicit Registry entry, which Lookup cannot tell from an entry with AmountDecimals unset (both read AmountScaleDecimals()==8).
func Registered(source string) bool {
	_, ok := Registry[source]
	return ok
}

// IncludeInVWAP is true only when the source is registered AND its IncludeInVWAP flag is true.
func IncludeInVWAP(source string) bool {
	return Lookup(source).IncludeInVWAP
}

// BackfillSafe reports whether the source's policy admits any replay; a BackfillPerWASM source is further gated per range by internal/wasmaudit.
func BackfillSafe(source string) bool {
	return Lookup(source).BackfillSafe()
}

// replayAuditCoveredBy maps a projected source with no Registry row to the entry whose WASM audit covers its decoder (Blend's audit
// covered the backstop). String literals, not SourceName constants: those packages import this one; ops tests pin them.
var replayAuditCoveredBy = map[string]string{
	"blend_backstop": "blend",
}

// replayStandardSchemaSources read a standard-fixed schema (SEP-41 / CAP-67) across arbitrary token contracts, so the WASM audit has no
// single subject; their sanctioned re-derives (docs/operations/sep41-mint-recovery.md) must not be stranded by this gate.
var replayStandardSchemaSources = map[string]struct{}{
	"sep41_transfers": {},
	"sep41_supply":    {},
}

// ReplayBackfillSafe is BackfillSafe for the re-derive paths, additionally accepting projector source names; any other name gets BackfillSafe's fail-closed answer.
func ReplayBackfillSafe(source string) bool {
	if ReplayExempt(source) {
		return true
	}
	return BackfillSafe(ReplayAuditSubject(source))
}

// ReplayExempt reports a standard-schema source (the sep41 pair) that the per-protocol WASM audit does not apply to.
func ReplayExempt(source string) bool {
	_, ok := replayStandardSchemaSources[source]
	return ok
}

// ReplayAuditSubject is the Registry source whose WASM audit covers a projector source name (blend_backstop -> blend; other names map to themselves).
func ReplayAuditSubject(source string) string {
	if coveredBy, ok := replayAuditCoveredBy[source]; ok {
		return coveredBy
	}
	return source
}

// UnsafeReplaySources filters `sources` to those ReplayBackfillSafe refuses, preserving order, so a re-derive command can name every offender.
func UnsafeReplaySources(sources []string) []string {
	var out []string
	for _, s := range sources {
		if !ReplayBackfillSafe(s) {
			out = append(out, s)
		}
	}
	return out
}

// FXSources returns the registered SubclassFX source names, sorted so the forex-snap tiebreak is deterministic.
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
func IsFXSource(source string) bool {
	return Lookup(source).Subclass == SubclassFX
}

// IsOnChain reports whether a source observes Stellar directly rather than an off-chain vendor API; unknown names fall to on-chain, which only matters for typos.
func IsOnChain(source string) bool {
	m := Lookup(source)
	//exhaustive:ignore
	switch m.Subclass {
	case SubclassCEX, SubclassFX:
		return false
	}
	// Reference-pricing classes are off-chain; the rest are Soroban on-chain.
	//exhaustive:ignore
	switch m.Class {
	case ClassAggregator, ClassAuthoritySanity:
		return false
	}
	// Chainlink and Tiingo are off-chain ClassOracle sources; class alone can't separate them.
	if source == "chainlink" || source == "tiingo" {
		return false
	}
	return true
}

// AggregatorSources returns every ClassAggregator source, sorted; disabled aggregators stay listed.
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

// Credit returns the vendor credit for source, "" when unregistered or on-chain.
func Credit(source string) string { return Registry[source].Credit }
