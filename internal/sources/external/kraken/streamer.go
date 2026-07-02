package kraken

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/coder/websocket"

	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/sources/external"
	"github.com/StellarIndex/stellar-index/internal/sources/external/wsclient"
)

// Streamer implements external.Streamer for Kraken's v2 WebSocket
// trade channel. Single connection per process, reconnects with
// bounded exponential backoff + jitter — same lifecycle as Binance.
type Streamer struct {
	// PairMap: Kraken symbol ("XLM/USD") → canonical.Pair. See
	// pairs.go:DefaultPairs.
	PairMap map[string]canonical.Pair

	Logger   *slog.Logger
	Endpoint string

	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

// NewStreamer constructs a Streamer with sensible defaults.
//
// Backoff defaults (F-0029, ported G10-03): InitialBackoff 5 s,
// MaxBackoff 60 s. Combined with the healthy-connection reset in the
// shared wsclient.Loop (a connection that stays alive ≥
// wsclient.DefaultHealthyConnectionThreshold rewinds backoff to
// InitialBackoff on its next failure), the effect is bounded 5-60 s
// reconnect windows instead of a 60 s blanket.
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

// subscribeReq is the JSON envelope we send post-connect to
// register the trade channel for a symbol list. Kraken v2 accepts
// an array of symbols in a single method call; no need to send N
// separate subscriptions.
type subscribeReq struct {
	Method string         `json:"method"`
	Params subscribeParam `json:"params"`
}

type subscribeParam struct {
	Channel string   `json:"channel"`
	Symbol  []string `json:"symbol"`
}

// Start implements external.Streamer. Connects to v2, subscribes to
// the trade channel for the supplied pairs, spawns the read loop,
// returns a channel that emits canonical.Trade values until ctx
// cancel or unrecoverable error.
func (s *Streamer) Start(ctx context.Context, pairs []canonical.Pair) (<-chan canonical.Trade, error) {
	if len(pairs) == 0 {
		return nil, errors.New("kraken: pairs required")
	}
	symbols, err := s.symbolsFor(pairs)
	if err != nil {
		return nil, err
	}

	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}

	if s.Endpoint == "" {
		s.Endpoint = WSEndpoint
	}

	out := make(chan canonical.Trade, 128)
	loop := &wsclient.Loop{
		Source:         SourceName,
		URL:            s.Endpoint,
		Logger:         logger,
		InitialBackoff: s.InitialBackoff,
		MaxBackoff:     s.MaxBackoff,
		// Send subscribe AFTER the status frame arrives on real
		// Kraken. Doing so upfront works too — v2 queues the
		// subscription until the session is ready. We send
		// immediately for simplicity.
		Subscribe: func(ctx context.Context, conn *websocket.Conn) error {
			sub := subscribeReq{
				Method: "subscribe",
				Params: subscribeParam{
					Channel: ChannelTrade,
					Symbol:  symbols,
				},
			}
			subBytes, err := json.Marshal(sub)
			if err != nil {
				return fmt.Errorf("marshal subscribe: %w", err)
			}
			if err := conn.Write(ctx, websocket.MessageText, subBytes); err != nil {
				return fmt.Errorf("write subscribe: %w", err)
			}
			return nil
		},
		HandleFrame: func(data []byte) ([]canonical.Trade, error) {
			return parseFrame(data, s.PairMap)
		},
	}
	go loop.Run(ctx, out)
	return out, nil
}

func (s *Streamer) symbolsFor(pairs []canonical.Pair) ([]string, error) {
	inverse := make(map[string]string, len(s.PairMap))
	for sym, p := range s.PairMap {
		inverse[p.String()] = sym
	}
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		sym, ok := inverse[p.String()]
		if !ok {
			return nil, fmt.Errorf("kraken: pair %s not in configured PairMap", p.String())
		}
		out = append(out, sym)
	}
	return out, nil
}
