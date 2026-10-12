// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

// Package nettools holds the single canonical SSRF blocklist used by every outbound fetch of an
// issuer/customer-controlled URL (SEP-1 metadata resolution, customer-webhook delivery and
// registration). It is a stdlib-only leaf package so any layer can import it.
//
// Per-call-site copies drift: a guard missing 192.0.0.0/24 (Oracle metadata 192.0.0.192) would let a
// customer webhook make the host dial its own metadata endpoint. IsBlockedIP is the UNION of every
// range any call site checks; add a range here once and every guard gets it.
package nettools

import (
	"context"
	"errors"
	"net"
	"strings"
)

// extraBlockedNets covers ranges the net.IP stdlib predicates miss, parsed once (a bad CIDR is a programmer bug): CGNAT and cloud
// metadata (100.64.0.0/10, 192.0.0.0/24), non-routable ranges, and v6 forms that EMBED a v4 address (NAT64, 6to4, IPv4-compatible
// and -translated, Teredo; e.g. `64:ff9b::a9fe:a9fe` is 169.254.169.254), which would otherwise bypass every v4 range.
var extraBlockedNets = func() []*net.IPNet {
	out := make([]*net.IPNet, 0, 14)
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
		"::ffff:0:0:0/96",
		"2001::/32",
		"240.0.0.0/4",
		"255.255.255.255/32",
		"192.88.99.0/24",
	} {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			panic("nettools: bad extraBlockedNets CIDR: " + cidr)
		}
		out = append(out, n)
	}
	return out
}()

// IsBlockedIP reports whether ip is in a non-public / SSRF-dangerous range; nil is blocked (fail-closed). It is the canonical guard, never re-implemented per package.
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

// DialFirstReachable dials each of ips on port in order and returns the first success, or every attempt's error joined. Callers must have passed every ip through IsBlockedIP; dialing the literal (never the hostname) keeps the kernel from re-resolving to an unchecked address, and falling through keeps one dead A/AAAA record from failing the dial.
func DialFirstReachable(ctx context.Context, dialer *net.Dialer, network string, ips []net.IP, port string) (net.Conn, error) {
	if len(ips) == 0 {
		return nil, errors.New("nettools: no addresses to dial")
	}
	errs := make([]error, 0, len(ips))
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	return nil, errors.Join(errs...)
}

// IsReservedTLD reports whether host is (or is under) a documentation/reserved TLD per RFC 2606 / RFC 6761, never a real internet destination.
func IsReservedTLD(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, tld := range []string{"example", "test", "invalid", "localhost"} {
		if h == tld || strings.HasSuffix(h, "."+tld) {
			return true
		}
	}
	return false
}
