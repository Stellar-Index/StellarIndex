package binance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/wsclient"
)

// Streamer implements external.Streamer for Binance's combined
// aggTrade feed. One instance per indexer process — serialises all
// subscribed pairs onto a single WebSocket.
type Streamer struct {
	// PairMap maps Binance symbol (e.g. "XLMUSDT") to the canonical
	// Pair to stamp on emitted trades. Required at construction
	// time; Start() rejects pairs not present here rather than
	// subscribing blind.
	PairMap map[string]canonical.Pair

	// Logger receives structured reconnect / error messages. If
	// nil, slog.Default() is used.
	Logger *slog.Logger

	// Endpoint overrides the wss:// URL. Default is [WSEndpoint];
	// integration tests use this to point at an httptest WS server.
	Endpoint string

	// InitialBackoff is the first reconnect delay after a dropped
	// connection. Each subsequent failure doubles it (with jitter)
	// up to MaxBackoff. Defaults to 5 s (F-0029).
	InitialBackoff time.Duration

	// MaxBackoff caps the exponential growth. Defaults to 60 s.
	MaxBackoff time.Duration
}

// NewStreamer constructs a Streamer with the supplied pair map and
// sensible defaults for the rest. Logger defaults to slog.Default().
//
// Backoff defaults (F-0029, audit-2026-05-27): InitialBackoff 5 s,
// MaxBackoff 60 s. Combined with the healthy-connection reset in the
// shared wsclient.Loop (a connection that stays alive ≥
// wsclient.DefaultHealthyConnectionThreshold rewinds backoff to
// InitialBackoff on its next failure), the effect is bounded 5-60 s
// reconnect windows instead of the 60 s blanket observed pre-fix on r1.
func NewStreamer(pairMap map[string]canonical.Pair) *Streamer {
	return &Streamer{
		PairMap:        pairMap,
		Endpoint:       WSEndpoint,
		InitialBackoff: 5 * time.Second,
		MaxBackoff:     60 * time.Second,
	}
}

// Name implements external.Connector.
func (s *Streamer) Name() string { return SourceName }

// Class implements external.Connector.
func (s *Streamer) Class() external.Class { return external.ClassExchange }

// Start implements external.Streamer. Connects to the combined
// stream for the requested pairs, parses frames, and emits
// canonical.Trade values until ctx is cancelled. Reconnects with
// bounded exponential backoff on dropped connections; only persistent
// configuration errors (empty pair list, URL that doesn't parse)
// return through Start itself.
//
// Empty `pairs` is rejected — Binance requires explicit subscription.
// Auto-enumeration of all listed symbols is a future capability; for
// v1 the operator configures the pair set explicitly via the indexer
// config.
func (s *Streamer) Start(ctx context.Context, pairs []canonical.Pair) (<-chan canonical.Trade, error) {
	if len(pairs) == 0 {
		return nil, errors.New("binance: pairs required — auto-enumeration not yet supported")
	}
	symbols, err := s.symbolsFor(pairs)
	if err != nil {
		return nil, err
	}
	streamURL, err := s.buildStreamURL(symbols)
	if err != nil {
		return nil, err
	}

	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}

	out := make(chan canonical.Trade, 128)

	// Binance disconnects clients after 24h of connection life —
	// deliberate server-side policy. The shared loop handles that
	// like any other disconnect; bounded backoff reconnects.
	loop := &wsclient.Loop{
		Source:         SourceName,
		URL:            streamURL,
		Logger:         logger,
		InitialBackoff: s.InitialBackoff,
		MaxBackoff:     s.MaxBackoff,
		// Subscription rides the combined-stream URL — no
		// post-dial subscribe frame.
		HandleFrame: func(data []byte) ([]canonical.Trade, error) {
			trade, err := parseAggTradeFrame(data, s.PairMap)
			if err != nil {
				return nil, err
			}
			return []canonical.Trade{trade}, nil
		},
	}
	go loop.Run(ctx, out)

	return out, nil
}

// symbolsFor resolves canonical.Pair → Binance symbol by inverting
// s.PairMap. Unknown pairs are rejected — we never subscribe to
// a symbol we can't decode on the way back.
func (s *Streamer) symbolsFor(pairs []canonical.Pair) ([]string, error) {
	// Build inverse map once; O(pairs × map) is fine for small N.
	inverse := make(map[string]string, len(s.PairMap))
	for sym, p := range s.PairMap {
		inverse[p.String()] = sym
	}
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		sym, ok := inverse[p.String()]
		if !ok {
			return nil, fmt.Errorf("binance: pair %s not in configured PairMap — add mapping before subscribing", p.String())
		}
		out = append(out, sym)
	}
	return out, nil
}

// buildStreamURL turns a list of Binance symbols into the combined-
// stream URL. Format:
//
//	wss://stream.binance.com:9443/stream?streams=xlmusdt@aggTrade/btcusdt@aggTrade
//
// Symbols are lowercased per Binance convention for the URL (the
// wire-side Symbol field arrives uppercase).
func (s *Streamer) buildStreamURL(symbols []string) (string, error) {
	if s.Endpoint == "" {
		s.Endpoint = WSEndpoint
	}
	u, err := url.Parse(s.Endpoint)
	if err != nil {
		return "", fmt.Errorf("endpoint parse: %w", err)
	}
	streams := make([]string, len(symbols))
	for i, sym := range symbols {
		streams[i] = strings.ToLower(sym) + "@aggTrade"
	}
	q := u.Query()
	q.Set("streams", strings.Join(streams, "/"))
	u.RawQuery = q.Encode()
	return u.String(), nil
}
