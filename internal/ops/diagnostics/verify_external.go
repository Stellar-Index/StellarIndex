package diagnostics

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	externalbinance "github.com/Stellar-Index/StellarIndex/internal/sources/external/binance"
	externalbitstamp "github.com/Stellar-Index/StellarIndex/internal/sources/external/bitstamp"
	externalcoinbase "github.com/Stellar-Index/StellarIndex/internal/sources/external/coinbase"
	externalcoingecko "github.com/Stellar-Index/StellarIndex/internal/sources/external/coingecko"
	externalcoinmarketcap "github.com/Stellar-Index/StellarIndex/internal/sources/external/coinmarketcap"
	externalcryptocompare "github.com/Stellar-Index/StellarIndex/internal/sources/external/cryptocompare"
	externalecb "github.com/Stellar-Index/StellarIndex/internal/sources/external/ecb"
	externalexchangerates "github.com/Stellar-Index/StellarIndex/internal/sources/external/exchangeratesapi"
	externalkraken "github.com/Stellar-Index/StellarIndex/internal/sources/external/kraken"
)

// verifyExternal starts every enabled off-chain connector, drains the sink for up to -timeout and prints a per-venue table.
// Paid venues need a non-empty API key after env resolution. Writes nothing to Timescale or Redis.
func verifyExternal(args []string) error { //nolint:funlen,gocognit,gocyclo // dispatch-heavy; splitting would reduce linearity
	fs := flag.NewFlagSet("verify-external", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	timeout := fs.Duration("timeout", 60*time.Second, "Max time to wait for every enabled venue to emit")
	failOnSilent := fs.Bool("fail-on-silent", false,
		"exit non-zero when ANY enabled venue emitted nothing within -timeout. Off by default because slow pollers (ECB, exchangeratesapi) can be legitimately silent in a short window; every venue silent always fails")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return fmt.Errorf("-config is required")
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}

	streamers, pollers, enabled, err := buildVerifyExternal(cfg.External)
	if err != nil {
		return err
	}
	if len(enabled) == 0 {
		return fmt.Errorf("no external connectors enabled — set [external.<venue>].enabled = true " +
			"and, for paid venues, provide the API key env var")
	}

	fmt.Fprintf(os.Stderr, "verify-external: enabled %d venues: %s\n",
		len(enabled), strings.Join(enabled, ", "))
	fmt.Fprintf(os.Stderr, "verify-external: waiting up to %s for first output from each\n\n", *timeout)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	logger := slog.Default()
	sink := make(chan consumer.Event, 256)

	wait, err := external.Run(ctx, streamers, pollers, sink, logger)
	if err != nil {
		return fmt.Errorf("external.Run: %w", err)
	}

	type perVenueStat struct {
		outputs int
		first   string // one-line summary
	}
	stats := make(map[string]*perVenueStat)

	allSeen := func() bool {
		for _, name := range enabled {
			if stats[name] == nil {
				return false
			}
		}
		return true
	}

DRAIN:
	for {
		select {
		case <-ctx.Done():
			break DRAIN
		case ev, ok := <-sink:
			if !ok {
				break DRAIN
			}
			src := ev.Source()
			s, ok := stats[src]
			if !ok {
				s = &perVenueStat{}
				stats[src] = s
			}
			s.outputs++
			if s.first == "" {
				s.first = summariseExternalEvent(ev)
			}
			if allSeen() {
				break DRAIN
			}
		}
	}

	cancel()
	wait()

	// Print table.
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "VENUE\tCLASS\tOUTPUTS\tFIRST SAMPLE")
	sort.Strings(enabled)
	silent := 0
	for _, name := range enabled {
		entry := external.Lookup(name)
		cls := string(entry.Class)
		s := stats[name]
		if s == nil {
			_, _ = fmt.Fprintf(w, "%s\t%s\t0\t(none — poll interval too long or connection failed)\n", name, cls)
			silent++
			continue
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", name, cls, s.outputs, s.first)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if silent > 0 {
		fmt.Fprintf(os.Stderr, "\nverify-external: %d/%d venues silent — ECB polls daily, "+
			"exchangeratesapi minute; raise -timeout or inspect logs.\n",
			silent, len(enabled))
	}
	return silentVerdict("verify-external", "enabled venues", silent, len(enabled), *failOnSilent)
}

// buildVerifyExternal mirrors the indexer's startExternalConnectors without its logger and sink wiring.
func buildVerifyExternal(cfg config.ExternalConfig) ([]external.StreamerSpec, []external.PollerSpec, []string, error) { //nolint:funlen,gocognit,gocyclo // dispatch-heavy; splitting would reduce linearity
	var streamers []external.StreamerSpec
	var pollers []external.PollerSpec
	var enabled []string

	if cfg.Binance.Enabled {
		pairMap, err := externalbinance.DefaultPairs()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("binance: %w", err)
		}
		pairs, err := externalbinance.DefaultPairList()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("binance: %w", err)
		}
		streamers = append(streamers, external.StreamerSpec{
			Streamer: externalbinance.NewStreamer(pairMap),
			Pairs:    pairs,
		})
		enabled = append(enabled, externalbinance.SourceName)
	}
	if cfg.Kraken.Enabled {
		pairMap, err := externalkraken.DefaultPairs()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("kraken: %w", err)
		}
		pairs, err := externalkraken.DefaultPairList()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("kraken: %w", err)
		}
		streamers = append(streamers, external.StreamerSpec{
			Streamer: externalkraken.NewStreamer(pairMap),
			Pairs:    pairs,
		})
		enabled = append(enabled, externalkraken.SourceName)
	}
	if cfg.Bitstamp.Enabled {
		pairMap, err := externalbitstamp.DefaultPairs()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("bitstamp: %w", err)
		}
		pairs, err := externalbitstamp.DefaultPairList()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("bitstamp: %w", err)
		}
		streamers = append(streamers, external.StreamerSpec{
			Streamer: externalbitstamp.NewStreamer(pairMap),
			Pairs:    pairs,
		})
		enabled = append(enabled, externalbitstamp.SourceName)
	}
	if cfg.Coinbase.Enabled {
		pairMap, err := externalcoinbase.DefaultPairs()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("coinbase: %w", err)
		}
		pairs, err := externalcoinbase.DefaultPairList()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("coinbase: %w", err)
		}
		streamers = append(streamers, external.StreamerSpec{
			Streamer: externalcoinbase.NewStreamer(pairMap),
			Pairs:    pairs,
		})
		enabled = append(enabled, externalcoinbase.SourceName)
	}

	// Pollers. Pair lists mirror the indexer's defaults. ECB and FX
	// venues take fiat cross pairs; aggregators take a fixed crypto-
	// vs-G3 fiat set.
	fxPairs := verifyDefaultFXPairs("USD")
	aggPairs := verifyDefaultAggregatorPairs()

	if cfg.ExchangeRatesApi.Enabled {
		p, err := externalexchangerates.NewPoller(cfg.ExchangeRatesApi.APIKey)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("exchangeratesapi: %w", err)
		}
		if cfg.ExchangeRatesApi.Base != "" {
			p.Base = cfg.ExchangeRatesApi.Base
		}
		pollers = append(pollers, external.PollerSpec{Poller: p, Pairs: verifyDefaultFXPairs(p.Base)})
		enabled = append(enabled, externalexchangerates.SourceName)
	}
	if cfg.CoinGecko.Enabled {
		pollers = append(pollers, external.PollerSpec{
			Poller: externalcoingecko.NewPoller(),
			Pairs:  aggPairs,
		})
		enabled = append(enabled, externalcoingecko.SourceName)
	}
	if cfg.CoinMarketCap.Enabled {
		p, err := externalcoinmarketcap.NewPoller(cfg.CoinMarketCap.APIKey)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("coinmarketcap: %w", err)
		}
		// Bind the catalogue's CMC IDs so the poller queries by id, not the ambiguous symbol, matching the indexer.
		if cat, catErr := currency.LoadEmbedded(); catErr == nil {
			p.CMCIDs = cat.CoinMarketCapIDs()
		}
		pollers = append(pollers, external.PollerSpec{Poller: p, Pairs: aggPairs})
		enabled = append(enabled, externalcoinmarketcap.SourceName)
	}
	if cfg.CryptoCompare.Enabled {
		p, err := externalcryptocompare.NewPoller(cfg.CryptoCompare.APIKey)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("cryptocompare: %w", err)
		}
		pollers = append(pollers, external.PollerSpec{Poller: p, Pairs: aggPairs})
		enabled = append(enabled, externalcryptocompare.SourceName)
	}
	if cfg.ECB.Enabled {
		pollers = append(pollers, external.PollerSpec{
			Poller: externalecb.NewPoller(),
			Pairs:  fxPairs,
		})
		enabled = append(enabled, externalecb.SourceName)
	}

	return streamers, pollers, enabled, nil
}

// verifyDefaultFXPairs mirrors the indexer's defaultFXPairs; both
// delegate to external.DefaultFXPairs so the two can't drift apart.
func verifyDefaultFXPairs(base string) []canonical.Pair {
	return external.DefaultFXPairs(base)
}

// verifyDefaultAggregatorPairs mirrors the indexer's
// defaultAggregatorPairs.
func verifyDefaultAggregatorPairs() []canonical.Pair {
	cryptos := []string{"XLM", "BTC", "ETH"}
	fiats := []string{"USD", "EUR", "GBP"}
	out := make([]canonical.Pair, 0, len(cryptos)*len(fiats))
	for _, c := range cryptos {
		ca, err := canonical.NewCryptoAsset(c)
		if err != nil {
			continue
		}
		for _, f := range fiats {
			fa, err := canonical.NewFiatAsset(f)
			if err != nil {
				continue
			}
			p, err := canonical.NewPair(ca, fa)
			if err != nil {
				continue
			}
			out = append(out, p)
		}
	}
	return out
}

// summariseExternalEvent renders one external-connector event as a
// one-line human summary for the verify-external table.
func summariseExternalEvent(ev consumer.Event) string {
	switch e := any(ev).(type) {
	case external.TradeEvent:
		return fmt.Sprintf("trade %s pair=%s base=%s quote=%s",
			e.Trade.Timestamp.Format(time.RFC3339),
			e.Trade.Pair.String(),
			e.Trade.BaseAmount.String(),
			e.Trade.QuoteAmount.String())
	case external.UpdateEvent:
		return fmt.Sprintf("update %s asset=%s price=%s",
			e.Update.Timestamp.Format(time.RFC3339),
			e.Update.Asset.String(),
			e.Update.Price.String())
	default:
		return fmt.Sprintf("event kind=%s", ev.EventKind())
	}
}

// ─── verify-archive ─────────────────────────────────────────────
