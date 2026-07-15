// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package customerwebhook

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/nettools"
)

// ssrfGuardedDialContext is the [net/http.Transport]'s DialContext
// override that the customer-webhook delivery worker uses. It
// resolves the destination host at dial time and rejects any
// address in a non-public range — closing the DNS-rebinding gap
// that registration-time validation cannot cover, since the DNS
// answer can change between when the URL was saved and when the
// callback fires. F-1245 (codex audit-2026-05-12).
//
// The check mirrors the registration-time logic in
// `internal/api/v1/dashboardwebhooks/handlers.go::isInternalIP`.
// Both must agree on the block list, or a host that passes
// registration could be rejected at delivery (a worse-than-the-
// alternative UX).
func ssrfGuardedDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("customerwebhook: invalid dial addr %q: %w", addr, err)
	}

	// Literal IP: check directly.
	if ip := net.ParseIP(host); ip != nil {
		if nettools.IsBlockedIP(ip) {
			return nil, fmt.Errorf("customerwebhook: refusing to dial internal address %s (SSRF defence)", ip.String())
		}
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		return dialer.DialContext(ctx, network, addr)
	}

	// Named host: resolve, reject any result that's internal.
	resolver := &net.Resolver{}
	ips, err := resolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("customerwebhook: resolve %q: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("customerwebhook: host %q resolved to zero addresses", host)
	}
	for _, ip := range ips {
		if nettools.IsBlockedIP(ip) {
			return nil, fmt.Errorf("customerwebhook: host %q resolved to internal address %s (SSRF defence)", host, ip.String())
		}
	}

	// All resolved addresses are public — dial the first one
	// explicitly rather than letting the kernel re-resolve.
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
}

// IsReservedTLD reports a documentation/reserved TLD. Thin delegate to the
// canonical implementation; kept exported for the worker's existing tests.
func IsReservedTLD(host string) bool { return nettools.IsReservedTLD(host) }
