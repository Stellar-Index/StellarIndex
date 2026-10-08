// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

// Package wsclient is the shared WebSocket lifecycle for the CEX connectors:
// the reconnect [Loop], jitter, the upgrade-dial HTTP client and the
// disconnect classifier, kept as one copy instead of four.
package wsclient

import (
	"errors"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"time"
)

// Jitter returns d ±25% (uniform), so streamers do not reconnect in lockstep;
// d<=0 is returned unchanged.
func Jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	delta := float64(d) * 0.25
	offset := (rand.Float64()*2 - 1) * delta
	return d + time.Duration(offset)
}

// KeepAliveHTTPClient returns the shared client for upgrade dials (HTTP/2 off,
// bounded idle pool). TCP KeepAlive is 30 s idle before the first probe;
// interval and count stay at Go's 15 s and 9.
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
	// http-timeout-ok: this client only performs the WS upgrade dial; the
	// long-lived connection afterwards is a net.Conn read loop with its own
	// ping/stall detection (see ErrStreamStalled), not an http.Client call.
	return &http.Client{Transport: transport}
}

// ErrStreamStalled is returned by the read loop when the venue stopped
// answering WebSocket pings — a half-open socket that TCP has not yet
// noticed. See [Loop.PingInterval].
var ErrStreamStalled = errors.New("stream stalled: venue stopped answering pings")

// ClassifyDisconnect maps a disconnect error to a stable metric label: stall
// (unanswered pings), reset, broken_pipe, timeout, dial, or other (EOF and
// stray cancellations). Callers handle venue-specific reasons first.
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
