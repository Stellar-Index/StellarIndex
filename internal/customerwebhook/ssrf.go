// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package customerwebhook

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
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
		if isInternalIP(ip) {
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
		if isInternalIP(ip) {
			return nil, fmt.Errorf("customerwebhook: host %q resolved to internal address %s (SSRF defence)", host, ip.String())
		}
	}

	// All resolved addresses are public — dial the first one
	// explicitly rather than letting the kernel re-resolve.
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
}

// isInternalIP mirrors the same-named function in
// `internal/api/v1/dashboardwebhooks/handlers.go`. Duplicated here
// rather than imported so the delivery worker stays self-contained
// and the API package's import boundary isn't crossed by a
// non-api package.
func isInternalIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsPrivate() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 100 && (v4[1]&0xC0) == 64 {
			return true
		}
		if v4[0] == 0 {
			return true
		}
	}
	return false
}

// IsReservedTLD is a small helper kept exported so the worker's
// tests can reuse the same "this is documentation TLD" branch.
// Not used by ssrfGuardedDialContext itself — at delivery time we
// always re-resolve, and the resolver will fail for genuinely
// reserved TLDs.
func IsReservedTLD(host string) bool {
	h := strings.ToLower(host)
	for _, tld := range []string{".example", ".test", ".invalid", ".localhost"} {
		if h == tld[1:] || strings.HasSuffix(h, tld) {
			return true
		}
	}
	return false
}
