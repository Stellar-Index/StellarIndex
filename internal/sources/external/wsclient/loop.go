// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package wsclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/coder/websocket"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// DefaultHealthyConnectionThreshold: a connection that lived this long resets
// backoff to InitialBackoff, or routine venue recycles pin it at MaxBackoff and
// lose that much data per cycle.
const DefaultHealthyConnectionThreshold = 5 * time.Minute

// DefaultPingInterval / DefaultPingTimeout bound how long a half-open socket
// goes unnoticed; conn.Read on one blocks forever. An active ping, not a read
// deadline, because a quiet pair is indistinguishable from a wedged socket by
// reads alone and control frames never unblock conn.Read. Worst case ≈ 40 s.
const (
	DefaultPingInterval = 30 * time.Second
	DefaultPingTimeout  = 10 * time.Second
)

// DefaultDialTimeout bounds one whole dial including the HTTP upgrade, which
// the transport timeouts and the ping watchdog do not cover.
const DefaultDialTimeout = 30 * time.Second

// DefaultReadLimit raises coder/websocket's 32 KiB message limit: one Kraken
// v2 sweep batches ~175+ fills into a frame past it, dropping the connection.
const DefaultReadLimit = 4 * 1024 * 1024

// Loop is the connect → subscribe → read → reconnect lifecycle shared by the
// CEX streamers: capped exponential backoff with [Jitter] (5-60 s), the
// healthy-lifetime reset, and disconnect/decode metrics. Venues supply only
// subscribe frames and a frame parser.
type Loop struct {
	// Source is the venue name stamped on metric labels + log fields
	// (e.g. "binance").
	Source string

	// URL is the fully-built wss:// endpoint to dial. Built once by
	// the venue's Start (query-string subscriptions like Binance's
	// combined stream ride here).
	URL string

	// Logger receives structured reconnect / error messages. If nil,
	// slog.Default() is used.
	Logger *slog.Logger

	// InitialBackoff is the first reconnect delay after a dropped
	// connection. Each subsequent failure doubles it (with jitter) up
	// to MaxBackoff. <=0 defaults to 5 s.
	InitialBackoff time.Duration

	// MaxBackoff caps the exponential growth. <=0 defaults to 60 s.
	MaxBackoff time.Duration

	// HealthyThreshold is the connection lifetime past which the next
	// disconnect resets backoff to InitialBackoff. <=0 defaults to
	// [DefaultHealthyConnectionThreshold].
	HealthyThreshold time.Duration

	// PingInterval is how often the live connection is actively probed
	// with a WebSocket ping. <=0 defaults to [DefaultPingInterval].
	PingInterval time.Duration

	// PingTimeout bounds each ping's pong wait; exceeding it drops the
	// connection with [ErrStreamStalled] (metric reason "stall"). <=0
	// defaults to [DefaultPingTimeout].
	PingTimeout time.Duration

	// DialTimeout bounds each dial attempt including the upgrade
	// handshake; exceeding it returns a "dial" disconnect and the loop
	// reconnects with backoff. <=0 defaults to [DefaultDialTimeout].
	DialTimeout time.Duration

	// ReadLimit caps a single WebSocket message in bytes, set on the
	// connection right after dial. <=0 defaults to [DefaultReadLimit].
	// coder/websocket's own default (32 KiB) is too small for a
	// venue that batches many fills into one frame.
	ReadLimit int64

	// Subscribe, if non-nil, registers channels once per connection; an
	// error drops the connection. Binance subscribes via the URL.
	Subscribe func(ctx context.Context, conn *websocket.Conn) error

	// HandleFrame parses one frame into trades. An error counts a decode
	// error and skips the frame, unless FatalFrameErr says to drop the
	// connection (coinbase's ErrSubscriptionRejected, for example).
	HandleFrame func(data []byte) ([]canonical.Trade, error)

	// FatalFrameErr, if non-nil, reports whether a HandleFrame error
	// must drop the connection instead of being skipped as a decode
	// error. nil means no frame error is fatal.
	FatalFrameErr func(err error) bool

	// Classify maps a disconnect error to a stable metric label. nil
	// defaults to [ClassifyDisconnect]; venues with bespoke sentinel
	// labels wrap it (coinbase / bitstamp).
	Classify func(err error) string

	// OnDisconnect, if non-nil, intercepts a disconnect before the default
	// warn log: handled suppresses the log, resetBackoff rewinds backoff.
	OnDisconnect func(logger *slog.Logger, err error, reason string) (handled, resetBackoff bool)
}

// Run reconnects until ctx is cancelled, then closes `out`; transient errors
// leave a timestamp gap, not a closed stream. The healthy-lifetime reset is
// what stops Binance's 6-12 min recycles pinning backoff at 60 s.
//
//nolint:gocognit // the reconnect lifecycle (backoff, jitter, healthy-reset, ctx) was extracted VERBATIM from four streamer copies — splitting it re-fragments the exact logic the extraction unified
func (l *Loop) Run(ctx context.Context, out chan<- canonical.Trade) {
	defer close(out)

	logger := l.Logger
	if logger == nil {
		logger = slog.Default()
	}
	initialBackoff := l.InitialBackoff
	if initialBackoff <= 0 {
		initialBackoff = 5 * time.Second
	}
	maxBackoff := l.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = 60 * time.Second
	}
	healthyThreshold := l.HealthyThreshold
	if healthyThreshold <= 0 {
		healthyThreshold = DefaultHealthyConnectionThreshold
	}
	classify := l.Classify
	if classify == nil {
		classify = ClassifyDisconnect
	}
	backoff := initialBackoff

	for {
		if ctx.Err() != nil {
			return
		}
		connectedAt := time.Now()
		err := l.runOnce(ctx, out)
		if ctx.Err() != nil {
			return
		}
		lifetime := time.Since(connectedAt)
		reason := classify(err)
		obs.CEXStreamDisconnectTotal.WithLabelValues(l.Source, reason).Inc()

		// Healthy-lifetime reset: a long-lived connection that
		// finally dropped is NOT evidence of a wedged venue — reset the
		// backoff so the next cycle isn't penalised for prior failures.
		if lifetime >= healthyThreshold {
			backoff = initialBackoff
		}
		handled := false
		if l.OnDisconnect != nil {
			var reset bool
			handled, reset = l.OnDisconnect(logger, err, reason)
			if reset {
				backoff = initialBackoff
			}
		}
		if !handled {
			// Transient — log, backoff, retry.
			logger.Warn(l.Source+" stream disconnected, reconnecting",
				"source", l.Source, "err", err,
				"lifetime", lifetime, "backoff", backoff, "reason", reason)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(Jitter(backoff)):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// runOnce runs one connect-and-read cycle: nil on clean close, an error on
// disconnect so Run backs off.
//
//nolint:gocognit // dial + subscribe + read-loop with per-venue hooks; linear despite the branch count
func (l *Loop) runOnce(ctx context.Context, out chan<- canonical.Trade) error {
	conn, err := l.dial(ctx)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close(websocket.StatusNormalClosure, "client shutdown")
	}()

	// Connection-scoped context: the stall watchdog cancels it (with the
	// stall as the cause) so the blocked conn.Read below unblocks and the
	// reconnect path runs. Cancelled unconditionally on return so the
	// watchdog goroutine cannot outlive the connection.
	connCtx, cancelConn := context.WithCancelCause(ctx)
	defer cancelConn(nil)
	go l.pingWatchdog(connCtx, conn, cancelConn)

	if l.Subscribe != nil {
		if err := l.Subscribe(connCtx, conn); err != nil {
			return err
		}
	}

	for {
		if ctx.Err() != nil {
			return nil
		}
		_, data, err := conn.Read(connCtx)
		if err != nil {
			// A watchdog-declared stall is the real disconnect reason;
			// the Read error is just "context canceled" noise.
			if cause := context.Cause(connCtx); cause != nil && errors.Is(cause, ErrStreamStalled) {
				return cause
			}
			return fmt.Errorf("read: %w", err)
		}
		trades, err := l.HandleFrame(data)
		if err != nil {
			if l.FatalFrameErr != nil && l.FatalFrameErr(err) {
				return err
			}
			// A single bad frame (often an unmapped new symbol) is counted,
			// not fatal; the decode-error runbook depends on the counter.
			obs.SourceDecodeErrorsTotal.WithLabelValues(l.Source).Inc()
			continue
		}
		for _, t := range trades {
			select {
			case <-ctx.Done():
				return nil
			case out <- t:
			}
		}
	}
}

// dial performs one upgrade dial under a per-attempt deadline so a venue
// that stalls anywhere in the handshake surfaces as a "dial" disconnect.
// The deadline covers only the handshake: the returned conn is not tied to
// dialCtx, so the read loop keeps running on the caller's ctx.
func (l *Loop) dial(ctx context.Context) (*websocket.Conn, error) {
	timeout := l.DialTimeout
	if timeout <= 0 {
		timeout = DefaultDialTimeout
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, resp, err := websocket.Dial(dialCtx, l.URL, &websocket.DialOptions{
		HTTPClient: KeepAliveHTTPClient(),
	})
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	readLimit := l.ReadLimit
	if readLimit <= 0 {
		readLimit = DefaultReadLimit
	}
	conn.SetReadLimit(readLimit)
	return conn, nil
}

// pingWatchdog pings every PingInterval and cancels connCtx with
// [ErrStreamStalled] when a pong misses PingTimeout. It must run beside the read
// loop, which dispatches pongs; it returns once connCtx is done.
func (l *Loop) pingWatchdog(connCtx context.Context, conn *websocket.Conn, fail context.CancelCauseFunc) {
	interval := l.PingInterval
	if interval <= 0 {
		interval = DefaultPingInterval
	}
	timeout := l.PingTimeout
	if timeout <= 0 {
		timeout = DefaultPingTimeout
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-connCtx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(connCtx, timeout)
			err := conn.Ping(pingCtx)
			cancel()
			if err == nil {
				continue
			}
			if connCtx.Err() != nil {
				// Shutdown or an unrelated disconnect won the race —
				// not a stall.
				return
			}
			fail(fmt.Errorf("%w (after %s): %w", ErrStreamStalled, timeout, err))
			return
		}
	}
}
