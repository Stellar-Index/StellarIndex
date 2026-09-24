package kraken

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/wsclient"
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
		HandleFrame: newFrameHandler(s.PairMap, logger).handle,
	}
	go loop.Run(ctx, out)
	return out, nil
}

// skipLogInterval bounds the skip WARN log to one line per reason per
// interval; the counter carries the full rate.
const skipLogInterval = time.Minute

// unknownSymbolLabel stands in for a rejected symbol outside PairMap so
// the venue cannot mint label values.
const unknownSymbolLabel = "unknown"

// frameHandler turns parsed frames into trades and surfaces what the
// parser reports beside them: skipped entries and subscribe verdicts.
type frameHandler struct {
	pairMap map[string]canonical.Pair
	logger  *slog.Logger
	now     func() time.Time

	mu          sync.Mutex
	lastSkipLog map[string]time.Time
}

func newFrameHandler(pairMap map[string]canonical.Pair, logger *slog.Logger) *frameHandler {
	return &frameHandler{
		pairMap:     pairMap,
		logger:      logger,
		now:         time.Now,
		lastSkipLog: map[string]time.Time{},
	}
}

func (h *frameHandler) handle(data []byte) ([]canonical.Trade, error) {
	res, err := parseFrame(data, h.pairMap)
	if err != nil {
		return nil, err
	}
	if res.Ack != nil {
		h.recordAck(*res.Ack)
	}
	for _, sk := range res.Skips {
		h.recordSkip(sk)
	}
	return res.Trades, nil
}

// recordAck flags a rejected symbol rather than dropping the
// connection: Kraken answers per symbol, so the other pairs on this
// socket are still live and a reconnect would only interrupt them.
func (h *frameHandler) recordAck(ack subscribeAck) {
	label := strings.ToUpper(ack.Symbol)
	if _, ok := h.pairMap[label]; !ok {
		label = unknownSymbolLabel
	}
	if ack.Accepted {
		if label != unknownSymbolLabel {
			obs.CEXStreamSubscriptionRejected.WithLabelValues(SourceName, label).Set(0)
		}
		return
	}
	obs.CEXStreamSubscriptionRejected.WithLabelValues(SourceName, label).Set(1)
	h.logger.Error("kraken rejected the trade subscription; the pair will deliver no trades",
		"source", SourceName, "symbol", ack.Symbol, "venue_error", ack.Message)
}

func (h *frameHandler) recordSkip(sk entrySkip) {
	obs.CEXStreamEntrySkipsTotal.WithLabelValues(SourceName, sk.Reason).Inc()
	now := h.now()
	h.mu.Lock()
	last, seen := h.lastSkipLog[sk.Reason]
	due := !seen || now.Sub(last) >= skipLogInterval
	if due {
		h.lastSkipLog[sk.Reason] = now
	}
	h.mu.Unlock()
	if due {
		h.logger.Warn("kraken trade entry skipped",
			"source", SourceName, "reason", sk.Reason, "err", sk.Err)
	}
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
