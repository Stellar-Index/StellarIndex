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

// ssrfGuardedDialContext is the delivery worker's [net/http.Transport] DialContext. It resolves
// the host at dial time and rejects any non-public address, closing the DNS-rebinding gap that
// registration-time validation cannot cover because the DNS answer can change after the URL was
// saved.
//
// It mirrors `internal/api/v1/dashboardwebhooks/handlers.go::isInternalIP`; the two block lists
// must agree, or a host that passes registration would be rejected at delivery.
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

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return nettools.DialFirstReachable(ctx, dialer, network, ips, port)
}
