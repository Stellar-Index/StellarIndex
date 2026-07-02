package bitstamp

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

// Streamer implements external.Streamer for Bitstamp. One
// connection per process; sends N subscribe frames on connect
// (Bitstamp does not accept a symbol array like Kraken).
// Honours `bts:request_reconnect` by closing + reconnecting via the
// normal backoff path.
type Streamer struct {
	// PairMap: Bitstamp symbol ("xlmusd") → canonical.Pair. See
	// pairs.go:DefaultPairs.
	PairMap map[string]canonical.Pair

	Logger   *slog.Logger
	Endpoint string

	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

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

// subscribeReq is the JSON shape Bitstamp expects for channel
// subscriptions — `bts:subscribe` with a `channel` name.
type subscribeReq struct {
	Event string           `json:"event"`
	Data  subscribeReqData `json:"data"`
}

type subscribeReqData struct {
	Channel string `json:"channel"`
}

// Start implements external.Streamer.
func (s *Streamer) Start(ctx context.Context, pairs []canonical.Pair) (<-chan canonical.Trade, error) {
	if len(pairs) == 0 {
		return nil, errors.New("bitstamp: pairs required")
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
		// One subscribe frame per symbol — Bitstamp serialises them
		// (no symbol-array subscription like Kraken).
		Subscribe: func(ctx context.Context, conn *websocket.Conn) error {
			for _, sym := range symbols {
				req := subscribeReq{
					Event: "bts:subscribe",
					Data:  subscribeReqData{Channel: ChannelPrefix + sym},
				}
				bs, err := json.Marshal(req)
				if err != nil {
					return fmt.Errorf("marshal subscribe: %w", err)
				}
				if err := conn.Write(ctx, websocket.MessageText, bs); err != nil {
					return fmt.Errorf("write subscribe %s: %w", sym, err)
				}
			}
			return nil
		},
		HandleFrame: func(data []byte) ([]canonical.Trade, error) {
			trade, isTrade, err := parseFrame(data, s.PairMap)
			if err != nil {
				return nil, err
			}
			if !isTrade {
				return nil, nil
			}
			return []canonical.Trade{trade}, nil
		},
		// `bts:request_reconnect` must drop the connection and flow
		// to the classifier / disconnect hook, not be skipped as a
		// decode error.
		FatalFrameErr: func(err error) bool {
			return errors.Is(err, ErrRequestedReconnect)
		},
		Classify: classifyDisconnect,
		// Server-initiated reconnect is benign — log at info, use
		// initial backoff (don't grow the backoff window for a
		// normal rebalance request).
		OnDisconnect: func(logger *slog.Logger, err error, _ string) (handled, resetBackoff bool) {
			if errors.Is(err, ErrRequestedReconnect) {
				logger.Info("bitstamp reconnecting per server request",
					"source", SourceName)
				return true, true
			}
			return false, false
		},
	}
	go loop.Run(ctx, out)
	return out, nil
}

// classifyDisconnect handles Bitstamp's venue-specific
// ErrRequestedReconnect label — a benign server-initiated reconnect — then
// delegates the wire-level cases to wsclient.ClassifyDisconnect.
func classifyDisconnect(err error) string {
	if errors.Is(err, ErrRequestedReconnect) {
		return "server_requested"
	}
	return wsclient.ClassifyDisconnect(err)
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
			return nil, fmt.Errorf("bitstamp: pair %s not in configured PairMap", p.String())
		}
		out = append(out, sym)
	}
	return out, nil
}
