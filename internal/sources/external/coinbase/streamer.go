package coinbase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/wsclient"
)

// Streamer implements external.Streamer for Coinbase Exchange.
// Single subscription (with an array of product_ids) covers every
// configured pair on one connection.
type Streamer struct {
	PairMap        map[string]canonical.Pair
	Logger         *slog.Logger
	Endpoint       string
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

// NewStreamer constructs a Streamer with sensible defaults.
//
// Backoff defaults (F-0029, ported G10-03): InitialBackoff 5 s,
// MaxBackoff 60 s, plus the healthy-connection reset in the shared
// wsclient.Loop.
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

type subscribeReq struct {
	Type     string             `json:"type"`
	Channels []subscribeChannel `json:"channels"`
}

type subscribeChannel struct {
	Name       string   `json:"name"`
	ProductIDs []string `json:"product_ids"`
}

// Start implements external.Streamer.
func (s *Streamer) Start(ctx context.Context, pairs []canonical.Pair) (<-chan canonical.Trade, error) {
	if len(pairs) == 0 {
		return nil, errors.New("coinbase: pairs required")
	}
	products, err := s.productsFor(pairs)
	if err != nil {
		return nil, err
	}

	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	// Touched only from the Loop's connection goroutine (Subscribe and
	// HandleFrame never run concurrently).
	active := products
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
		Subscribe: func(ctx context.Context, conn *websocket.Conn) error {
			sub := subscribeReq{
				Type: "subscribe",
				Channels: []subscribeChannel{
					{Name: ChannelName, ProductIDs: active},
				},
			}
			bs, err := json.Marshal(sub)
			if err != nil {
				return fmt.Errorf("marshal subscribe: %w", err)
			}
			if err := conn.Write(ctx, websocket.MessageText, bs); err != nil {
				return fmt.Errorf("write subscribe: %w", err)
			}
			return nil
		},
		HandleFrame: func(data []byte) ([]canonical.Trade, error) {
			trade, isTrade, err := parseFrame(data, s.PairMap)
			if err != nil {
				if errors.Is(err, ErrSubscriptionRejected) {
					active = dropAndLog(logger, active, err)
				}
				return nil, err
			}
			if !isTrade {
				return nil, nil
			}
			return []canonical.Trade{trade}, nil
		},
		// A rejected subscription must drop the connection (it flows
		// to the classifier below) instead of being skipped as a
		// decode error.
		FatalFrameErr: func(err error) bool {
			return errors.Is(err, ErrSubscriptionRejected)
		},
		Classify: classifyDisconnect,
		// Subscription rejection is usually a config bug — log
		// loudly but still reconnect (operator may have fixed the
		// config mid-flight).
		OnDisconnect: func(logger *slog.Logger, err error, reason string) (handled, resetBackoff bool) {
			if errors.Is(err, ErrSubscriptionRejected) {
				logger.Error("coinbase subscription rejected — check product_ids in DefaultPairs",
					"source", SourceName, "err", err, "reason", reason)
				return true, false
			}
			return false, false
		},
	}
	go loop.Run(ctx, out)
	return out, nil
}

// classifyDisconnect handles Coinbase's venue-specific
// ErrSubscriptionRejected label — so operators can tell a config-reject
// loop apart from transient wire drops — then delegates the wire-level
// cases to wsclient.ClassifyDisconnect.
func classifyDisconnect(err error) string {
	if errors.Is(err, ErrSubscriptionRejected) {
		return "subscription_rejected"
	}
	return wsclient.ClassifyDisconnect(err)
}

func dropAndLog(logger *slog.Logger, active []string, err error) []string {
	remaining, dropped := dropRejectedProducts(active, err.Error())
	if len(dropped) > 0 {
		logger.Error("coinbase rejected products — dropping them and resubscribing without",
			"source", SourceName, "products", dropped, "remaining", len(remaining))
	}
	return remaining
}

// dropRejectedProducts removes every product the rejection text names, so
// one delisted product cannot starve the whole venue. It never empties the
// list: with nothing left to subscribe the rejection stays a loud config error.
func dropRejectedProducts(active []string, text string) (remaining, dropped []string) {
	named := make(map[string]bool)
	for _, tok := range strings.FieldsFunc(strings.ToUpper(text), func(r rune) bool {
		return (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-'
	}) {
		named[tok] = true
	}
	remaining = make([]string, 0, len(active))
	for _, id := range active {
		if named[strings.ToUpper(id)] {
			dropped = append(dropped, id)
		} else {
			remaining = append(remaining, id)
		}
	}
	if len(remaining) == 0 {
		return active, nil
	}
	return remaining, dropped
}

func (s *Streamer) productsFor(pairs []canonical.Pair) ([]string, error) {
	inverse := make(map[string]string, len(s.PairMap))
	for sym, p := range s.PairMap {
		inverse[p.String()] = sym
	}
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		sym, ok := inverse[p.String()]
		if !ok {
			return nil, fmt.Errorf("coinbase: pair %s not in configured PairMap", p.String())
		}
		out = append(out, sym)
	}
	return out, nil
}
