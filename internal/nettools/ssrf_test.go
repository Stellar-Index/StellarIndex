// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package nettools

import (
	"net"
	"testing"
)

func TestIsBlockedIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		// Public — must be allowed.
		{"1.1.1.1", false},
		{"8.8.8.8", false},
		{"93.184.216.34", false}, // example.com's historical A record
		{"2606:4700:4700::1111", false},

		// Loopback / unspecified / link-local.
		{"127.0.0.1", true},
		{"0.0.0.0", true},
		{"::1", true},
		{"169.254.169.254", true}, // AWS/GCP/Azure metadata (link-local)

		// RFC 1918 + ULA.
		{"10.0.0.1", true},
		{"172.16.5.5", true},
		{"192.168.1.1", true},
		{"fc00::1", true},

		// Extra ranges — the ones the webhook guards USED to miss (CS-008).
		{"100.100.100.200", true}, // Alibaba Cloud metadata (100.64/10 CGNAT)
		{"192.0.0.192", true},     // Oracle Cloud metadata (192.0.0.0/24)
		{"198.18.0.1", true},      // RFC 2544 benchmarking
		{"0.1.2.3", true},         // 0.0.0.0/8

		// NAT64-translated v4 (C3-110). The prefix EMBEDS the v4 address
		// in the low 32 bits, and Go's IsLinkLocalUnicast/IsPrivate do not
		// unwrap it — so before the fix every v4 range above was
		// bypassable by translating it through a NAT64 gateway.
		{"64:ff9b::a9fe:a9fe", true},   // 169.254.169.254 — cloud metadata
		{"64:ff9b::a00:1", true},       // 10.0.0.1 — RFC 1918
		{"64:ff9b::7f00:1", true},      // 127.0.0.1 — loopback
		{"64:ff9b:1::a9fe:a9fe", true}, // RFC 8215 local-use NAT64 prefix
		// Adjacent, NOT NAT64 — must stay allowed so the /96 isn't
		// silently widened into a public-address blackhole.
		{"64:ff9a::1", false},
		{"64:ff9b:2::1", false},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("bad test IP %q", c.ip)
		}
		if got := IsBlockedIP(ip); got != c.want {
			t.Errorf("IsBlockedIP(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
	if !IsBlockedIP(nil) {
		t.Error("IsBlockedIP(nil) = false, want true (fail-closed)")
	}
}

func TestIsReservedTLD(t *testing.T) {
	for _, h := range []string{"example", "foo.test", "bar.invalid", "localhost", "x.localhost", "site.example."} {
		if !IsReservedTLD(h) {
			t.Errorf("IsReservedTLD(%q) = false, want true", h)
		}
	}
	for _, h := range []string{"example.com", "stellarindex.io", "a.testnet.io"} {
		if IsReservedTLD(h) {
			t.Errorf("IsReservedTLD(%q) = true, want false", h)
		}
	}
}
