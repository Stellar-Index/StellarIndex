// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package customerwebhook

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestSSRFGuardedDialContext_LiteralBlockedIPsRefused pins that the delivery-time guard
// (net.ParseIP + nettools.IsBlockedIP) refuses every non-public literal before dialing: RFC1918,
// loopback and the 169.254.169.254 cloud-metadata endpoint, the ranges an attacker steers a
// webhook URL at. Hermetic: a blocked literal returns before any dialer exists.
func TestSSRFGuardedDialContext_LiteralBlockedIPsRefused(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for _, addr := range []string{
		"10.0.0.1:80",        // RFC1918
		"172.16.5.4:443",     // RFC1918
		"192.168.1.1:80",     // RFC1918
		"127.0.0.1:80",       // loopback
		"169.254.169.254:80", // cloud metadata (link-local)
		"[::1]:80",           // IPv6 loopback
		"[fd00::1]:80",       // IPv6 ULA (private)
		"0.0.0.0:80",         // unspecified
	} {
		conn, err := ssrfGuardedDialContext(ctx, "tcp", addr)
		if conn != nil {
			_ = conn.Close()
			t.Errorf("ssrfGuardedDialContext(%q) returned a live conn; must refuse", addr)
		}
		if err == nil {
			t.Errorf("ssrfGuardedDialContext(%q) = nil error; want SSRF refusal", addr)
			continue
		}
		if !strings.Contains(err.Error(), "SSRF defence") {
			t.Errorf("ssrfGuardedDialContext(%q) err = %q, want an SSRF-refusal error", addr, err)
		}
	}
}

// TestSSRFGuardedDialContext_InvalidAddrRejected — a dial target without a
// port cannot be split and must error before any resolution or dial.
func TestSSRFGuardedDialContext_InvalidAddrRejected(t *testing.T) {
	t.Parallel()

	_, err := ssrfGuardedDialContext(context.Background(), "tcp", "no-port-here")
	if err == nil {
		t.Fatal("expected an error for an addr with no port")
	}
	if !strings.Contains(err.Error(), "invalid dial addr") {
		t.Errorf("err = %q, want it to name the invalid dial addr", err)
	}
}

// TestSSRFGuardedDialContext_NamedHostResolvingToLoopbackRefused exercises the resolve-at-dial
// branch (the anti-DNS-rebind half): a NAMED host is resolved and every result is block-checked,
// so `localhost` (loopback) must be rejected rather than dialed. Registration-time validation
// cannot cover this, since the DNS answer can flip between save and callback.
//
// Tolerates a sandbox that cannot resolve `localhost`: the guard then returns a "resolve" error
// (still a refusal, never a dial) and the literal-IP test above remains the hard guard.
func TestSSRFGuardedDialContext_NamedHostResolvingToLoopbackRefused(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, err := ssrfGuardedDialContext(ctx, "tcp", "localhost:80")
	if conn != nil {
		_ = conn.Close()
		t.Fatal("dialed localhost — the resolve-and-reject branch failed to refuse a loopback result")
	}
	if err == nil {
		t.Fatal("expected localhost to be refused (resolves to loopback), got nil error")
	}
	switch {
	case strings.Contains(err.Error(), "SSRF defence"):
		// Strong outcome: resolved to loopback and was rejected by the
		// block list — the anti-rebind reject path we care about.
	case strings.Contains(err.Error(), "resolve"):
		t.Logf("environment could not resolve localhost (%v); "+
			"literal-IP cases still cover the block-list composition", err)
	default:
		t.Errorf("unexpected error shape for localhost: %q", err)
	}
}

// NOTE (coverage gap): the anti-rebind DIAL-TARGET property, that a public-resolving host is
// dialed at the RESOLVED IP rather than re-resolved by the kernel (ssrf.go dials
// net.JoinHostPort(ips[0], port)), is not asserted here. ssrfGuardedDialContext hard-codes its
// *net.Resolver and *net.Dialer, so neither the answer nor the dialed address is observable
// without a real public dial, which unit tests disallow. An injectable resolver/dialer would
// let a stub prove it.
