// Package external is the connector framework for off-chain sources: exchanges, institutional FX
// feeds, third-party aggregators and sovereign daily anchors. They run on their own cadence outside
// the ledger loop but converge on the same canonical types and hypertables.
//
// A venue implements whichever capabilities it supports:
//   - [Streamer]   — live WebSocket trade feed (exchange class)
//   - [Poller]     — periodic REST fetch (aggregator / FX / sovereign)
//   - [Backfiller] — historical OHLC candles, one canonical.Trade per bucket (optional)
//
// [Registry] holds source class metadata, read by the aggregator at VWAP time to decide
// contribution.
package external

import (
	"context"
	"errors"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// Class is how a source participates in aggregation: only ClassExchange contributes to VWAP, since averaging aggregates or oracles with raw trades double-counts upstream markets.
type Class string

const (
	// ClassExchange publishes real executed trades and contributes to VWAP. Off-chain FX
	// providers are first-party FX references, so also exchange class, not ClassAggregator.
	ClassExchange Class = "exchange"

	// ClassAggregator publishes already-aggregated prices (CoinGecko, CoinMarketCap): a
	// divergence signal, but including it would double-count the markets it derives from.
	ClassAggregator Class = "aggregator"

	// ClassOracle is an on-chain signed price publisher (Reflector, Redstone, Band). Excluded from
	// VWAP to avoid inheriting its methodology unless an operator opts one in via config.
	ClassOracle Class = "oracle"

	// ClassAuthoritySanity is a central-bank daily reference rate (ECB, Fed H.10, BOE): too slow
	// to aggregate, used only to flag our live rate drifting from the daily close.
	ClassAuthoritySanity Class = "authority_sanity"

	// ClassLending is on-chain lending (Blend). Auction stress-prices and bad-debt flags are
	// decisions taken on other oracles' prices, not observations, so never VWAP.
	ClassLending Class = "lending"

	// ClassRouter is DEX routers and aggregator vaults. They invoke contracts that trade rather
	// than trading, so are excluded from VWAP; their value is per-tx attribution and intent.
	ClassRouter Class = "router"

	// ClassBridge is cross-chain transfer protocols (CCTP, Rozo): a burn plus a remote mint is
	// one transfer, not a two-leg trade, so no price and no VWAP.
	ClassBridge Class = "bridge"
)

// Subclass partitions a Class for the confidence diversity factor (ADR-0019); empty means no partition.
type Subclass string

const (
	SubclassCEX Subclass = "cex" // centralised exchange, off-chain
	SubclassDEX Subclass = "dex" // decentralised exchange, on-chain
	SubclassFX  Subclass = "fx"  // institutional FX feed
)

// Metadata is the static source-registry record; operators may override DefaultWeight and IncludeInVWAP, while Class, Subclass and Paid are facts about the venue.
type Metadata struct {
	Class         Class
	Subclass      Subclass
	DefaultWeight int
	IncludeInVWAP bool
	// Retired marks a source nothing writes any more; it stays registered so its history keeps its class.
	Retired bool
	// Paid indicates the source needs a commercial license or API key (shown in /v1/sources).
	Paid bool
	// Credit names the vendor to credit wherever this source's value is served on its own (a
	// composite with provenance is allowed, a raw vendor proxy is not). Empty for on-chain sources.
	Credit string
	// BackfillAvailable reports whether the source implements Backfiller with useful depth.
	BackfillAvailable bool
	// Backfill is the replay policy for historical ranges. Soroban contracts upgrade in place,
	// so a current decoder is trusted on a range only when every WASM version active in it is
	// audited. The zero value refuses.
	Backfill BackfillPolicy

	// AmountDecimals is the smallest-unit scale of the amounts this source stamps on
	// canonical.Trade: 8 for the CEX/aggregator convention, 6 for FX pollers. 0 means unset,
	// read as 8 via AmountScaleDecimals(). Never assume 1e8: the USD-volume gate mis-scales FX
	// ~100x otherwise.
	AmountDecimals int

	// OracleResolution is the cadence at which this source's OracleUpdate timestamps advance
	// upstream, the base of its stale budget. Nonzero exactly for sources that write
	// oracle_updates; config.OracleSourceNames mirrors that set (pipeline lockstep test).
	OracleResolution time.Duration
}

// AmountScaleDecimals returns the amount scale, defaulting to 8 when AmountDecimals is unset.
func (m Metadata) AmountScaleDecimals() int {
	if m.AmountDecimals > 0 {
		return m.AmountDecimals
	}
	return 8
}

// Connector is the common root interface; every venue implements it plus at least one of Streamer, Poller or Backfiller.
type Connector interface {
	// Name returns the source identifier; it must match the [Registry] key and the Source
	// stamped on emitted trades and updates.
	Name() string
	// Class returns the aggregation class, shorthand for Registry[Name()].Class.
	Class() Class
}

// Streamer is implemented by venues that push live trades over a persistent connection, handling reconnect, heartbeats and rate limiting internally.
type Streamer interface {
	Connector
	// Start subscribes to pairs (empty = all, where the venue enumerates symbols on connect; otherwise an
	// error) and returns a trade channel. Transient faults (a dropped frame, a reconnect) are handled
	// inside and surface as metrics; the channel closes only on ctx cancel or a fatal state (bad
	// credential, outage past the backoff ceiling).
	Start(ctx context.Context, pairs []canonical.Pair) (<-chan canonical.Trade, error)
}

// Poller is implemented by venues with REST endpoints serving the current quote board; the runner calls it on a fixed cadence and the connector is stateless between calls.
type Poller interface {
	Connector
	// PollOnce hits the venue once. Exchange-class pollers return Trades; aggregator, oracle
	// and sovereign pollers return OracleUpdates (derived prices). Only one slice is non-nil.
	PollOnce(ctx context.Context, pairs []canonical.Pair) (trades []canonical.Trade, updates []canonical.OracleUpdate, err error)
	// PollInterval is the minimum gap between PollOnce calls, enforced by the framework.
	PollInterval() time.Duration
}

// Backfiller is implemented by venues with historical OHLC candles, emitted as one synthesised canonical.Trade per candle at its VWAP (or close) with its volume; depth varies per venue, so check Metadata.BackfillAvailable.
type Backfiller interface {
	Connector
	Backfill(ctx context.Context, pair canonical.Pair, from, to time.Time, granularity time.Duration) ([]canonical.Trade, error)
}

// TradeEvent wraps external trades as a consumer.Event, mirroring the on-chain TradeEvent so the sink type-switch stays uniform; Trade.Source names the venue.
type TradeEvent struct {
	Trade canonical.Trade
}

// EventKind implements [consumer.Event].
func (TradeEvent) EventKind() string { return "external.trade" }

// Source implements [consumer.Event]; metrics label by venue, not event kind.
func (e TradeEvent) Source() string { return e.Trade.Source }

// UpdateEvent wraps OracleUpdate output from aggregator / oracle / sovereign pollers.
type UpdateEvent struct {
	Update canonical.OracleUpdate
}

// EventKind implements [consumer.Event].
func (UpdateEvent) EventKind() string { return "external.update" }

// Source implements [consumer.Event].
func (e UpdateEvent) Source() string { return e.Update.Source }

// ErrNoApplicablePairs is returned by a poller whose configured pairs map to nothing it can request; the runner scores it "idle", never success.
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
	// BackfillNoWASM: no Soroban WASM behind the decoder, so history decodes like live.
	BackfillNoWASM
	// BackfillPerWASM: a replay is admitted only when every WASM hash its contracts ran in the
	// range is in internal/wasmaudit/audited_wasm.json.
	BackfillPerWASM
)

// BackfillSafe reports whether any replay can be admitted; PerWASM sources are further gated per range by internal/wasmaudit.
func (m Metadata) BackfillSafe() bool { return m.Backfill != BackfillUnsafe }
