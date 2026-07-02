// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package wsclient

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/coder/websocket"

	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/obs"
)

// DefaultHealthyConnectionThreshold — if a connection survived at least
// this long before disconnecting, treat the next reconnect as a fresh
// start and reset backoff to InitialBackoff. Without this, an indefinite
// stream of healthy multi-minute venue cycles (e.g. Binance's 24h policy
// plus PING-driven recycles) eventually pins backoff at MaxBackoff
// forever — losing ~MaxBackoff of data per cycle instead of the expected
// InitialBackoff (F-0029, ported G10-03).
const DefaultHealthyConnectionThreshold = 5 * time.Minute

// Loop is the shared connect → subscribe → read → reconnect lifecycle
// used by every external WS streamer (binance / kraken / coinbase /
// bitstamp). It owns the dial (via [KeepAliveHTTPClient]), the read
// loop, ctx cancellation, the capped exponential backoff with [Jitter],
// the healthy-lifetime backoff reset (F-0029), and the per-source
// disconnect / decode-error metrics. Venues supply only their
// subscribe frame(s) and frame parser.
//
// Backoff defaults (F-0029, audit-2026-05-27): InitialBackoff 5 s,
// MaxBackoff 60 s. Combined with the healthy-connection reset (a
// connection that stays alive ≥ HealthyThreshold rewinds backoff to
// InitialBackoff on its next failure), the effect is bounded 5-60 s
// reconnect windows instead of a 60 s blanket.
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
	// to MaxBackoff. <=0 defaults to 5 s (F-0029).
	InitialBackoff time.Duration

	// MaxBackoff caps the exponential growth. <=0 defaults to 60 s.
	MaxBackoff time.Duration

	// HealthyThreshold is the connection lifetime past which the next
	// disconnect resets backoff to InitialBackoff. <=0 defaults to
	// [DefaultHealthyConnectionThreshold].
	HealthyThreshold time.Duration

	// Subscribe, if non-nil, is called once per connection immediately
	// after a successful dial to register channels (venues whose
	// subscription rides the URL — Binance — leave it nil). A returned
	// error drops the connection and flows to the disconnect
	// classifier.
	Subscribe func(ctx context.Context, conn *websocket.Conn) error

	// HandleFrame parses one wire frame into zero or more trades.
	// A returned error counts a decode error (SourceDecodeErrorsTotal,
	// F-1235) and skips the frame — UNLESS FatalFrameErr reports it
	// fatal, in which case the connection is dropped and the error
	// reaches the disconnect classifier (e.g. coinbase's
	// ErrSubscriptionRejected, bitstamp's ErrRequestedReconnect).
	HandleFrame func(data []byte) ([]canonical.Trade, error)

	// FatalFrameErr, if non-nil, reports whether a HandleFrame error
	// must drop the connection instead of being skipped as a decode
	// error. nil means no frame error is fatal.
	FatalFrameErr func(err error) bool

	// Classify maps a disconnect error to a stable metric label. nil
	// defaults to [ClassifyDisconnect]; venues with bespoke sentinel
	// labels wrap it (coinbase / bitstamp).
	Classify func(err error) string

	// OnDisconnect, if non-nil, lets a venue intercept specific
	// disconnect errors before the default warn log (e.g. bitstamp's
	// benign server-requested reconnect, coinbase's loud
	// config-rejection log). Return handled=true to suppress the
	// default warn log; resetBackoff=true to rewind backoff to
	// InitialBackoff for the next cycle.
	OnDisconnect func(logger *slog.Logger, err error, reason string) (handled, resetBackoff bool)
}

// Run is the reconnect-forever loop. It blocks until ctx cancellation
// and closes `out` on return; transient errors cause exponential-backoff
// reconnect without closing the channel (downstream consumers see a gap
// in timestamps but no stream termination).
//
// F-0029 (audit-2026-05-27): backoff resets to InitialBackoff on any
// connection that lived ≥ HealthyThreshold. Before the fix, production
// r1 logs showed backoff pinned at 60 s because every Binance recycle
// (every 6-12 min) doubled an already-large window and there was no
// reset path; the indexer was dropping ~60 s of CEX trades per cycle.
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

		// Healthy-lifetime reset (F-0029): a long-lived connection that
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

// runOnce handles one connect-and-read cycle. Returns on EOF, ctx
// cancel (the caller checks ctx), or read/parse error. Successful
// close returns nil; disconnect scenarios return an error so Run can
// decide whether to backoff.
func (l *Loop) runOnce(ctx context.Context, out chan<- canonical.Trade) error {
	conn, resp, err := websocket.Dial(ctx, l.URL, &websocket.DialOptions{
		HTTPClient: KeepAliveHTTPClient(),
	})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	defer func() {
		_ = conn.Close(websocket.StatusNormalClosure, "client shutdown")
	}()

	if l.Subscribe != nil {
		if err := l.Subscribe(ctx, conn); err != nil {
			return err
		}
	}

	for {
		if ctx.Err() != nil {
			return nil
		}
		_, data, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		trades, err := l.HandleFrame(data)
		if err != nil {
			if l.FatalFrameErr != nil && l.FatalFrameErr(err) {
				return err
			}
			// Single-frame parse errors are non-fatal (e.g. a new
			// symbol subscribed that isn't in PairMap yet). Count +
			// continue; dropping the whole stream would be a gross
			// overreaction to one bad line. F-1235 (codex
			// audit-2026-05-12): operators need this signal on
			// schema drift — the decode-error runbook depends on it.
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
