// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

// Package nettools holds the single canonical SSRF blocklist used by every
// outbound fetch of an issuer/customer-controlled URL (SEP-1 metadata
// resolution, customer-webhook delivery + registration). It is a stdlib-only
// leaf package so any layer can import it.
//
// It exists because the block-list logic was previously copy-pasted into three
// packages with DIVERGENT coverage (CS-008): metadata/sep1.go blocked
// 192.0.0.0/24 (Oracle Cloud metadata 192.0.0.192) + 198.18.0.0/15, but the
// two webhook guards did not — so an Oracle-hosted deployment could be made to
// dial its own metadata endpoint via a customer webhook. IsBlockedIP is the
// UNION of every range any call site ever checked; add a range here once and
// every guard gets it.
package nettools

import (
	"net"
	"strings"
)

// extraBlockedNets covers ranges the net.IP stdlib predicates don't flag.
// Parsed once — a bad CIDR here is a programmer bug.
//
//   - 100.64.0.0/10  — RFC 6598 CGNAT / shared address space. Includes
//     Alibaba Cloud's metadata IP 100.100.100.200.
//   - 192.0.0.0/24   — IETF Protocol Assignments. Includes Oracle Cloud's
//     metadata IP 192.0.0.192.
//   - 198.18.0.0/15  — RFC 2544 benchmarking; not internet-routable.
//   - 0.0.0.0/8      — RFC 1122 "this host on this network".
//   - 64:ff9b::/96   — RFC 6052 well-known NAT64 prefix, and
//   - 64:ff9b:1::/48 — RFC 8215 local-use NAT64 prefix. These EMBED an
//     IPv4 address in the low bits, so `64:ff9b::a9fe:a9fe` is the cloud
//     metadata IP 169.254.169.254 wearing an IPv6 costume. Go's
//     IsLinkLocalUnicast / IsPrivate do NOT unwrap the NAT64 prefix (only
//     the ::ffff:0:0/96 IPv4-mapped form), so without these entries every
//     v4 range above is bypassable by translating it — whenever a NAT64
//     gateway is on the egress path (C3-110, audit-2026-07-23).
//   - 2002::/16      — RFC 3056 6to4. Bits 16..48 EMBED a v4 address, so
//     `2002:a9fe:a9fe::` is 169.254.169.254, `2002:7f00:1::` is 127.0.0.1.
//     First byte is 0x20, so Go's IsPrivate / IsLinkLocal* don't flag it —
//     the exact 6to4 mirror of the NAT64 bypass above.
//   - ::/96          — RFC 4291 deprecated IPv4-compatible IPv6 (`::a.b.c.d`).
//     The low 32 bits EMBED a v4 address (`::a9fe:a9fe` = 169.254.169.254);
//     To4() unwraps only the ::ffff:0:0/96 mapped form, NOT this one, so it
//     reads as generic global-unicast v6 and slips every v4 range above.
//     `::1` (loopback) and `::` (unspecified) are numerically inside ::/96
//     but are already blocked by the IsLoopback / IsUnspecified checks that
//     run BEFORE this list, so they stay blocked for the right reason; real
//     v4 (e.g. 8.8.8.8) can never match ::/96 because To4() shortens it to
//     4 bytes and net.IPNet.Contains rejects the length mismatch.
//   - fec0::/10      — RFC 3879 deprecated site-local unicast. Matches
//     neither IsLinkLocalUnicast (fe80::/10) nor IsPrivate (fc00::/7).
var extraBlockedNets = func() []*net.IPNet {
	out := make([]*net.IPNet, 0, 9)
	for _, cidr := range []string{
		"100.64.0.0/10",
		"192.0.0.0/24",
		"198.18.0.0/15",
		"0.0.0.0/8",
		"64:ff9b::/96",
		"64:ff9b:1::/48",
		"2002::/16",
		"::/96",
		"fec0::/10",
	} {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			panic("nettools: bad extraBlockedNets CIDR: " + cidr)
		}
		out = append(out, n)
	}
	return out
}()

// IsBlockedIP reports whether ip is in a non-public / SSRF-dangerous range.
// A nil IP is treated as blocked (fail-closed). This is the canonical guard;
// do NOT re-implement it per package.
func IsBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// Loopback / link-local (covers 169.254.169.254 — the AWS/GCP/Azure
	// metadata IP) / multicast / unspecified.
	if ip.IsLoopback() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	// RFC 1918 (10/8, 172.16/12, 192.168/16) + RFC 4193 (fc00::/7 ULA).
	if ip.IsPrivate() {
		return true
	}
	for _, n := range extraBlockedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// IsReservedTLD reports whether host is (or is under) a documentation/reserved
// TLD per RFC 2606 / RFC 6761 — never a real internet destination.
func IsReservedTLD(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, tld := range []string{"example", "test", "invalid", "localhost"} {
		if h == tld || strings.HasSuffix(h, "."+tld) {
			return true
		}
	}
	return false
}
