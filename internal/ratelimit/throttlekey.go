// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package ratelimit

import "net/netip"

// ThrottleIPKey masks a client IP to its throttle identity: exact address for IPv4, /64 prefix for IPv6 (a /128 would let an attacker mint a fresh bucket per request, RFC 7421); the ONE definition (TestThrottleIPMaskHasOneDefinition), so every per-IP cap keys through it; NEVER use for audit logs, display or allowlists.
func ThrottleIPKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	if addr.Is4() || addr.Is4In6() {
		// Unmap so "1.2.3.4" and "::ffff:1.2.3.4" — two spellings of one
		// address — can never resolve to two separate budgets.
		return addr.Unmap().String()
	}
	// A zone (scoped link-local, e.g. "fe80::1%eth0") would otherwise ride
	// along into the key and defeat the aggregation.
	return netip.PrefixFrom(addr.WithZone(""), 64).Masked().Addr().String()
}
