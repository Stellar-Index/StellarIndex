// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

// Package wsclient holds the shared WebSocket-streamer lifecycle used by the
// external CEX connectors (binance / kraken / coinbase / bitstamp): the
// dial → subscribe → read → reconnect [Loop] (capped exponential backoff,
// healthy-lifetime reset, per-source disconnect/decode metrics), backoff
// jitter, the keep-alive HTTP client for upgrade dials, and the disconnect
// error → metric-label classifier. Extracting them here keeps a single
// canonical copy instead of four drifting duplicates; venues supply only
// their subscribe frame(s) and frame parser.
package wsclient

import (
	"errors"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"time"
)

// Jitter returns d ±25% (uniform). d<=0 is returned unchanged.
//
// The variation avoids thundering-herd reconnects if many streamers happen
// to time their retries on the same boundary.
func Jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	delta := float64(d) * 0.25
	offset := (rand.Float64()*2 - 1) * delta
	return d + time.Duration(offset)
}

// KeepAliveHTTPClient returns the shared keep-alive HTTP client used by the
// WS streamers' upgrade dials (HTTP/2 disabled, bounded idle pool).
//
// Its Transport dials TCP with a 30 s OS-level keepalive. Go's net.Dialer
// defaults to no keepalive on the underlying socket; venues that issue TCP
// RST after their own timeout window then surface as "connection reset by
// peer" reads instead of being detected earlier by the dialer. F-0029.
func KeepAliveHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{Transport: transport}
}

// ErrStreamStalled is returned by the read loop when the venue stopped
// answering WebSocket pings — a half-open socket that TCP has not yet
// noticed. See [Loop.PingInterval] (C2-017/C2-031, audit-2026-07-23).
var ErrStreamStalled = errors.New("stream stalled: venue stopped answering pings")

// ClassifyDisconnect maps a disconnect error to a stable metric label
// (stall / reset / broken_pipe / timeout / dial / other). Venue-specific
// reasons are handled by the caller before delegating here.
//
// Reasons: "stall" (venue stopped answering pings — see [ErrStreamStalled]),
// "reset" (TCP RST from venue), "broken_pipe" (write failed, peer hung up
// mid-frame), "timeout" (read timed out), "dial" (handshake failed),
// "other" (everything else, including EOF / context cancellations that
// slipped past the ctx.Err() check).
func ClassifyDisconnect(err error) string {
	if err == nil {
		return "other"
	}
	// Checked before the string match: a stall's wrapped cause is often a
	// context deadline, which would otherwise classify as "timeout" and be
	// indistinguishable from a venue-side read timeout.
	if errors.Is(err, ErrStreamStalled) {
		return "stall"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection reset by peer"):
		return "reset"
	case strings.Contains(msg, "broken pipe"):
		return "broken_pipe"
	case strings.Contains(msg, "i/o timeout"), strings.Contains(msg, "timeout"):
		return "timeout"
	case strings.HasPrefix(msg, "dial:"):
		return "dial"
	default:
		return "other"
	}
}
