// Package divergence cross-checks our VWAP against independent external
// references: the last line of defence when every layer above it agrees
// on a wrong price (ADR-0019 Layer 5).
//
// Every reference implements [Reference]: [CoinGeckoReference] (on by
// default), [ChainlinkReference] (opt-in via FeedMap) and
// [OracleReference], which reads the on-chain oracles we ingested from
// `oracle_updates` via [OracleReader]. Oracle rows never feed the VWAP
// itself (their ingest class is excluded); here they show what an
// on-chain consumer actually sees.
//
// [Compare] queries every reference in parallel, takes the median of the
// successes and reports our deviation from it; failures land in
// [Result.Failures]. The aggregator sets `flags.divergence_warning` only
// when [Result.SuccessCount] reaches [ServiceOptions.MinSourcesForWarning]
// (`[divergence].min_sources_for_warning`, default 2).
//
// [Reference] implementations must be safe for concurrent LookupQuote
// calls; every exported type is safe for concurrent use.
package divergence
