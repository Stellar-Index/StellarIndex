// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package customerwebhook

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestSSRFGuardedDialContext_LiteralBlockedIPsRefused pins W6-tst-4: the
// F-1245 delivery-time guard composes net.ParseIP + nettools.IsBlockedIP
// and must refuse every non-public literal before it ever dials. These
// are the ranges an attacker steers a webhook URL at — RFC1918, loopback,
// and the 169.254.169.254 cloud-metadata endpoint. All hermetic: a blocked
// literal returns before any dialer is constructed, so no packet leaves.
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

// TestSSRFGuardedDialContext_NamedHostResolvingToLoopbackRefused exercises
// the resolve-at-dial branch (the anti-DNS-rebind half): a NAMED host is
// resolved and every result is block-checked. `localhost` resolves to a
// loopback address, which IsBlockedIP refuses — so the guard must reject it
// rather than dialing. This is the path registration-time validation cannot
// cover (the DNS answer can flip between save and callback).
//
// Resilient to a sandbox that cannot resolve `localhost` at all: in that
// case the guard returns a "resolve" error (still a refusal, never a dial),
// and the literal-IP test above remains the hard block-list guard.
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

// NOTE (coverage gap, W6-tst-4): the anti-rebind DIAL-TARGET property —
// that a public-resolving host is dialed at the RESOLVED IP rather than
// re-resolved by the kernel (ssrf.go dials net.JoinHostPort(ips[0], port))
// — is not asserted here because ssrfGuardedDialContext hard-codes its
// *net.Resolver and *net.Dialer, so neither the resolved answer nor the
// dialed address is observable from a test without a real public dial
// (disallowed: no network egress in unit tests). Making the resolver/dialer
// injectable would let a stub prove it; deferred as a follow-up rather than
// widen this LOW cleanup's blast radius into production wiring.
