// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package streaming

import (
	"net"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// Per-IP concurrent-SSE cap: the global cap ([maxConcurrentStreams]) alone lets one stalled client hold the whole budget. Disabled by
// default (0);
// wired via [SetMaxStreamsPerIP] and [SetStreamClientIPResolver].
var (
	maxStreamsPerIP int64 // 0 = disabled

	streamIPMu   sync.Mutex
	streamsPerIP = map[string]int{}

	// streamClientIP keys the per-IP cap. It defaults to the socket peer; the binary installs the
	// trusted-proxy-aware resolver so the cap sees the real client, not Caddy's single address.
	streamClientIPMu sync.RWMutex
	streamClientIP   = defaultStreamClientIP
)

// SetMaxStreamsPerIP sets the per-client-IP concurrent-SSE cap; <= 0 disables it (the global cap still applies). Call once at startup.
func SetMaxStreamsPerIP(n int) { atomic.StoreInt64(&maxStreamsPerIP, int64(n)) }

// SetStreamClientIPResolver overrides how the per-IP cap identifies a caller (the binary wires the trusted-proxy-aware resolver); nil restores the direct-peer default. Call once at startup.
func SetStreamClientIPResolver(f func(*http.Request) string) {
	streamClientIPMu.Lock()
	if f == nil {
		f = defaultStreamClientIP
	}
	streamClientIP = f
	streamClientIPMu.Unlock()
}

// defaultStreamClientIP returns the direct socket peer host; behind a reverse proxy the binary replaces it with a forwarded-header-aware
// resolver.
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
	return ratelimit.ThrottleIPKey(ip)
}

// acquireIPStreamSlot reserves one per-IP slot; ok=false means the client is at its cap and the caller must reject. release MUST be called
// when the stream ends (a no-op when the cap is disabled).
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

// ActiveStreamsForIP reports the open-stream count attributed to ip as the configured resolver produces it; a diagnostic/test hook.
func ActiveStreamsForIP(ip string) int {
	key := ratelimit.ThrottleIPKey(ip)
	streamIPMu.Lock()
	defer streamIPMu.Unlock()
	return streamsPerIP[key]
}
