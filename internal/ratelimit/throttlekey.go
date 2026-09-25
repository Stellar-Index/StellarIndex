// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package ratelimit

import "net/netip"

// ThrottleIPKey masks a client IP down to the network block the caller
// actually controls: the exact (unmapped) address for IPv4, the /64
// prefix for IPv6. Returns ip unchanged (including "") when it doesn't
// parse.
//
// SECURITY: keying a per-IP throttle on a full IPv6 /128 is NOT the
// analogue of keying an IPv4 throttle on its /32. Residential, mobile and
// hosting providers hand a single customer an entire /64 (often a /56 or
// /48), so an attacker with one routable IPv6 allocation mints a fresh
// /128 — and therefore a fresh, empty throttle bucket — per request.
// Aggregating to /64, the smallest block an ISP delegates per RFC 7421
// §2.1, makes the key match the unit an attacker can cheaply rotate
// within. IPv4 has no equivalent cheap-rotation problem at attacker
// scale, so it keeps its full address.
//
// This is the ONE definition of the throttle identity: every per-IP cap
// (the API rate limiter, the signup and login throttles, the streaming
// and tip-producer per-caller caps) keys through it, so a request cannot
// hold two budgets at two layers by spelling one address two ways.
// TestThrottleIPMaskHasOneDefinition fails on a second copy.
//
// NEVER use this for audit logging, admin display, or an IP allowlist
// check — those want the caller's precise address.
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
