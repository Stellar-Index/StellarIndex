// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package streaming

import (
	"net"
	"net/http"
	"sync"
	"sync/atomic"
)

// Per-IP concurrent-SSE-connection cap (C3-8 / CS-013).
//
// The global cap ([maxConcurrentStreams]) bounds TOTAL concurrent
// streams, but on its own it lets a single client hold open the entire
// budget — a stalled/non-reading client leaks goroutine+conn+FD per
// stream (until the rolling write deadline fires) and can starve the
// streams for everyone else. This per-IP cap gives each client its own
// small ceiling so no one address can monopolise the fan-out.
//
// Disabled by default (cap 0); the binary wires the operator-configured
// value via [SetMaxStreamsPerIP] at startup and the forge-resistant IP
// resolver via [SetStreamClientIPResolver].
var (
	maxStreamsPerIP int64 // 0 = disabled

	streamIPMu   sync.Mutex
	streamsPerIP = map[string]int{}

	// streamClientIP resolves the caller identity used to key the per-IP
	// cap. Defaults to the direct socket peer host; the binary overrides
	// it with the trusted-proxy-aware resolver (middleware.RemoteIP) so
	// the cap keys off the real client behind Caddy rather than the
	// proxy's single address.
	streamClientIPMu sync.RWMutex
	streamClientIP   = defaultStreamClientIP
)

// SetMaxStreamsPerIP sets the per-client-IP concurrent-SSE cap. Pass
// <= 0 to disable (the global cap still applies). Call once at startup.
func SetMaxStreamsPerIP(n int) { atomic.StoreInt64(&maxStreamsPerIP, int64(n)) }

// SetStreamClientIPResolver overrides how the per-IP cap identifies a
// caller. The binary wires the trusted-proxy-aware resolver so the cap
// keys off the true client IP. Passing nil restores the default
// direct-peer resolver. Call once at startup.
func SetStreamClientIPResolver(f func(*http.Request) string) {
	streamClientIPMu.Lock()
	if f == nil {
		f = defaultStreamClientIP
	}
	streamClientIP = f
	streamClientIPMu.Unlock()
}

// defaultStreamClientIP returns the direct socket peer host. Correct
// for direct connections; when a reverse proxy fronts the API the
// binary replaces this with a forwarded-header-aware resolver.
func defaultStreamClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func resolveStreamClientIP(r *http.Request) string {
	streamClientIPMu.RLock()
	f := streamClientIP
	streamClientIPMu.RUnlock()
	ip := f(r)
	if ip == "" {
		// No resolvable IP → collapse into one shared bucket rather than
		// exempting the request (fail-closed for the cap).
		return "unknown"
	}
	return ip
}

// acquireIPStreamSlot reserves one per-IP stream slot for r's client.
// Returns ok=false when the client is already at its cap (caller must
// reject the connection). The returned release MUST be called exactly
// once when the stream ends. When the per-IP cap is disabled it always
// succeeds with a no-op release.
func acquireIPStreamSlot(r *http.Request) (release func(), ok bool) {
	limit := atomic.LoadInt64(&maxStreamsPerIP)
	if limit <= 0 {
		return func() {}, true
	}
	ip := resolveStreamClientIP(r)

	streamIPMu.Lock()
	if int64(streamsPerIP[ip]) >= limit {
		streamIPMu.Unlock()
		return nil, false
	}
	streamsPerIP[ip]++
	streamIPMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			streamIPMu.Lock()
			if streamsPerIP[ip] <= 1 {
				delete(streamsPerIP, ip)
			} else {
				streamsPerIP[ip]--
			}
			streamIPMu.Unlock()
		})
	}, true
}

// ActiveStreamsForIP reports the current open-stream count attributed to
// ip — a diagnostic/test hook. The ip must match what the configured
// resolver produces.
func ActiveStreamsForIP(ip string) int {
	streamIPMu.Lock()
	defer streamIPMu.Unlock()
	return streamsPerIP[ip]
}
