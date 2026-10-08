// Package external is the connector framework for off-chain sources: centralised exchanges, institutional
// FX feeds, third-party aggregators and sovereign daily anchors. Unlike the on-chain dispatcher, they speak
// HTTPS/WebSocket to vendor APIs on their own cadence, outside the ledger loop, but converge on the same
// canonical types and hypertables.
//
// A venue implements whichever capabilities it supports:
//
//   - [Streamer]   — live WebSocket trade feed (exchange class)
//   - [Poller]     — periodic REST fetch (aggregator / FX / sovereign)
//   - [Backfiller] — historical OHLC candles, synthesised to canonical.Trade per bucket (optional; depth
//     varies, Kraken caps at 720 intervals)
//
// [Registry] holds source class metadata, read by the aggregator at VWAP time to decide contribution.
package external

import (
	"context"
	"errors"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// Class is how a source participates in aggregation. Mixing classes in a VWAP is wrong (averaging
// aggregates with raw trades double-counts upstream markets) or imposes another publisher's methodology
// (oracles), so only [ClassExchange] contributes; the rest are reported for transparency and divergence.
type Class string

const (
	// ClassExchange publishes real executed trades (Binance, Kraken, Coinbase, SDEX, Soroswap) and
	// contributes to VWAP. Off-chain FX providers (Massive's daily interbank close, ExchangeRatesApi's computed
	// rate) are first-party FX references, so also exchange class, not ClassAggregator.
	ClassExchange Class = "exchange"

	// ClassAggregator — a third-party service that publishes
	// already-aggregated prices across many markets (CoinGecko,
	// CoinMarketCap, CryptoCompare). Useful as a divergence signal
	// against our own VWAP, but including in our VWAP would
	// double-count the upstream markets they derive from.
	ClassAggregator Class = "aggregator"

	// ClassOracle — on-chain, signed, governance-backed price
	// publishers (Reflector, Redstone, Band). Like aggregators
	// they publish derived prices, not raw trades; we report them
	// alongside but exclude from VWAP to avoid methodology
	// inheritance. Operator may opt one in per-source via config.
	ClassOracle Class = "oracle"

	// ClassAuthoritySanity — sovereign / central-bank daily
	// reference rates (ECB reference rates, Fed H.10, BOE rates).
	// Cadence too slow for aggregation but authoritative as a
	// sanity anchor — the daily-close divergence check that flags
	// if our live rate drifts more than N bps from the central
	// bank's public close.
	ClassAuthoritySanity Class = "authority_sanity"

	// ClassLending is on-chain lending (Blend): auction stress-prices, position metrics and bad-debt flags
	// are decisions taken on other oracles' prices, not new observations, so never VWAP; reported as a
	// secondary validation surface.
	ClassLending Class = "lending"

	// ClassRouter is Soroban DEX routers and aggregator vaults (Soroswap Router, DeFindex). They invoke
	// contracts that trade rather than trading, so are excluded from VWAP; their value is per-tx attribution
	// and intent (path requested vs realised). The `routers` table lists the contracts each one watches.
	ClassRouter Class = "router"

	// ClassBridge is cross-chain transfer/intent protocols (Circle CCTP, Rozo): a Stellar deposit_for_burn
	// plus an Ethereum mint_and_withdraw is one USDC transfer, not a two-leg trade, so no price and no VWAP.
	// Reported for cross-chain flow attribution and USDC supply accounting (the cross-chain side of
	// Algorithm 3). Schemas: docs/protocols/cctp.md, docs/protocols/rozo.md.
	ClassBridge Class = "bridge"
)

// Subclass partitions a [Class] for the confidence diversity factor (ADR-0019): a CEX and a DEX under
// ClassExchange are economically distinct. Empty means no partition, usual outside ClassExchange.
type Subclass string

const (
	SubclassCEX Subclass = "cex" // centralised exchange, off-chain
	SubclassDEX Subclass = "dex" // decentralised exchange, on-chain
	SubclassFX  Subclass = "fx"  // institutional FX feed
)

// Metadata is the source-registry record. Static at startup — not
// mutated during ingest. Operators may override DefaultWeight and
// IncludeInVWAP via config, but Class, Subclass and Paid are facts
// about the venue that don't change per deployment.
type Metadata struct {
	Class         Class
	Subclass      Subclass
	DefaultWeight int
	IncludeInVWAP bool
	// Paid indicates the source requires a commercial license or API
	// key to function. Exposed in /v1/sources so operators know
	// which connectors need credential setup.
	Paid bool
	// BackfillAvailable reports whether the source implements the
	// Backfiller interface with useful depth. Kraken would be false
	// here (30-day cap at 1h is too shallow to matter); Binance and
	// Bitstamp would be true.
	BackfillAvailable bool
	// Backfill is the source's replay policy for historical ranges
	// (backfill, projector-replay, ch-rebuild -write, projected-rebuild).
	// Soroban contracts upgrade in place, so a current decoder is trusted
	// on a range only when every WASM version active in it is audited. The
	// zero value refuses.
	Backfill BackfillPolicy

	// AmountDecimals is the smallest-unit scale of the amounts this
	// source stamps on canonical.Trade (Quote/BaseAmount): 8 for the
	// CEX/aggregator 1e8 convention, 6 for the FX pollers' 1e6
	// (DefaultDecimals). 0 means "unset → treat as 8" via
	// AmountScaleDecimals(). Read this instead of assuming 1e8:
	// the USD-volume gate mis-scales FX ~100× otherwise.
	AmountDecimals int

	// OracleResolution is the cadence at which the timestamps on this
	// source's canonical.OracleUpdate rows advance upstream — the base of
	// its stellarindex_oracle_stale budget. Nonzero exactly for the
	// sources that write oracle_updates; config.OracleSourceNames mirrors
	// that set (pinned by pipeline's lockstep test).
	OracleResolution time.Duration
}

// AmountScaleDecimals returns the source's amount scale, defaulting to 8
// (the CEX 1e8 convention) when AmountDecimals is unset.
func (m Metadata) AmountScaleDecimals() int {
	if m.AmountDecimals > 0 {
		return m.AmountDecimals
	}
	return 8
}

// Connector is the common root interface. Every venue package
// exposes a concrete type that implements Connector plus at least one
// of [Streamer] / [Poller] / [Backfiller].
type Connector interface {
	// Name returns the canonical source identifier. Must match the
	// key in [Registry] and the value stamped on emitted
	// canonical.Trade.Source / canonical.OracleUpdate.Source.
	Name() string
	// Class returns the source's aggregation class. Short-hand for
	// Registry[Name()].Class — concrete types return it directly so
	// callers don't need to import the registry.
	Class() Class
}

// Streamer is implemented by venues that push live trades via
// WebSocket or similar persistent connection. Connectors handle
// reconnect, heartbeats, and rate limiting internally — callers just
// read from the returned channel.
type Streamer interface {
	Connector
	// Start subscribes to pairs (empty = all, where the venue enumerates symbols on connect; otherwise an
	// error) and returns a trade channel. Transient faults (a dropped frame, a reconnect) are handled
	// inside and surface as metrics; the channel closes only on ctx cancel or a fatal state (bad
	// credential, outage past the backoff ceiling).
	Start(ctx context.Context, pairs []canonical.Pair) (<-chan canonical.Trade, error)
}

// Poller is implemented by venues with REST endpoints that serve the
// current quote board. Called on a fixed cadence by the framework's
// runner; the connector itself is stateless between calls.
type Poller interface {
	Connector
	// PollOnce hits the venue once and returns whatever it serves.
	// Exchange-class pollers (rare — most exchanges stream) return
	// Trades. Aggregator / oracle / sovereign pollers return
	// OracleUpdates (derived prices with no executed-trade context).
	// A connector returns only one of the two; the unused slice is
	// nil.
	PollOnce(ctx context.Context, pairs []canonical.Pair) (trades []canonical.Trade, updates []canonical.OracleUpdate, err error)
	// PollInterval is the minimum gap between PollOnce calls.
	// Framework enforces — connector just declares its cadence.
	PollInterval() time.Duration
}

// Backfiller is implemented by venues whose REST APIs expose historical OHLC candles, emitted as one
// synthesised canonical.Trade per candle at its VWAP (or close) with its volume; open/high/low are dropped.
// Depth varies per venue (Kraken caps at 720 intervals), so check Metadata.BackfillAvailable.
type Backfiller interface {
	Connector
	Backfill(ctx context.Context, pair canonical.Pair, from, to time.Time, granularity time.Duration) ([]canonical.Trade, error)
}

// TradeEvent wraps external trades as a consumer.Event, mirroring the on-chain venues' TradeEvent so the
// indexer's sink type-switch stays uniform. One wrapper suffices: all external trades take the same
// InsertTrade path and canonical.Trade.Source names the venue.
type TradeEvent struct {
	Trade canonical.Trade
}

// EventKind implements [consumer.Event].
func (TradeEvent) EventKind() string { return "external.trade" }

// Source implements [consumer.Event]. Delegates to the embedded
// Trade's Source so metrics label by venue, not by event kind.
func (e TradeEvent) Source() string { return e.Trade.Source }

// UpdateEvent wraps OracleUpdate-shaped output from aggregator /
// oracle / sovereign pollers. Parallel to reflector.UpdateEvent /
// redstone.UpdateEvent / band.UpdateEvent.
type UpdateEvent struct {
	Update canonical.OracleUpdate
}

// EventKind implements [consumer.Event].
func (UpdateEvent) EventKind() string { return "external.update" }

// Source implements [consumer.Event].
func (e UpdateEvent) Source() string { return e.Update.Source }

// ErrNoApplicablePairs is returned by a poller whose configured pairs map to
// nothing it can request. The runner scores it "idle", never success.
var ErrNoApplicablePairs = errors.New("external: no configured pair applies to this poller")

// Compile-time checks.
var (
	_ consumer.Event = TradeEvent{}
	_ consumer.Event = UpdateEvent{}
)

// BackfillPolicy is how a re-derive path decides a source may decode history.
type BackfillPolicy uint8

const (
	// BackfillUnsafe refuses every replay (zero value, fail-closed).
	BackfillUnsafe BackfillPolicy = iota
	// BackfillNoWASM: no Soroban WASM behind the decoder (off-chain
	// vendors, SDEX), so history decodes like live.
	BackfillNoWASM
	// BackfillPerWASM: a replay is admitted only when every WASM hash its
	// contracts ran in the range is in internal/wasmaudit/audited_wasm.json.
	BackfillPerWASM
)

// BackfillSafe reports whether any replay can be admitted. PerWASM sources
// are further gated per range by internal/wasmaudit.
func (m Metadata) BackfillSafe() bool { return m.Backfill != BackfillUnsafe }
