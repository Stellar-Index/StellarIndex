// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import "net/netip"

// throttleIPKey masks a client IP down to the network block the caller
// actually controls: the exact address for IPv4, the /64 prefix for
// IPv6. Returns ip unchanged (including "") when it doesn't parse.
//
// SECURITY (audit-2026-07-23, same class as SEC-15 on the API middleware):
// keying a per-IP throttle on a full IPv6 /128 is NOT the analogue of
// keying an IPv4 throttle on its /32. Residential, mobile and hosting
// providers hand a single customer an entire /64 (often a /56 or /48), so
// an attacker with one routable IPv6 allocation mints a fresh /128 — and
// therefore a fresh, empty throttle bucket — per request, for free. The
// signup and magic-link caps are exactly the controls that vector defeats:
// bulk account minting and inbox bombing. Aggregating to /64 — the
// smallest block an ISP delegates per RFC 7421 §2.1 — makes the bucket
// key match the unit an attacker can cheaply rotate within. IPv4 has no
// equivalent cheap-rotation problem at attacker scale, so it keeps its
// full address.
//
// The throttles mask HERE, at the point the Redis key is derived, rather
// than trusting callers to hand over a pre-masked value: the key
// derivation is the throttle's own invariant, and a caller that legitimately
// needs the exact address for logging/audit (both handlers do) would
// otherwise have to remember to mask separately. Mirrors
// `internal/api/v1/middleware/remoteip.go::maskIPForThrottleKey` and
// `internal/api/streaming/periplimit.go`; masking is idempotent, so the
// two layers compose safely.
//
// NEVER use this for audit logging, admin display, or an IP allowlist
// check — those want the caller's precise address.
func throttleIPKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	if addr.Is4() || addr.Is4In6() {
		// Unmap so "1.2.3.4" and "::ffff:1.2.3.4" — two spellings of one
		// address — can never resolve to two separate budgets.
		return addr.Unmap().String()
	}
	// Zone (scoped link-local, e.g. "fe80::1%eth0") would otherwise ride
	// along into the key and defeat the aggregation.
	return netip.PrefixFrom(addr.WithZone(""), 64).Masked().Addr().String()
}
