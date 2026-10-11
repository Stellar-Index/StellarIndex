// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package postgresstore

import (
	"errors"
	"net"
	"testing"
)

// TestIPString_NilFailsClosed: a nil IP (clientIP returns it on any unparsable RemoteAddr, reaching CreateSession in production) must error, not become a fabricated "0.0.0.0" in a forensics column.
func TestIPString_NilFailsClosed(t *testing.T) {
	got, err := ipString(nil)
	if err == nil {
		t.Fatalf("ipString(nil) returned (%q, nil); want a non-nil error and no fabricated value", got)
	}
	if !errors.Is(err, errNilClientIP) {
		t.Errorf("ipString(nil) error = %v, want errNilClientIP", err)
	}
	if got != "" {
		t.Errorf("ipString(nil) value = %q, want empty string on error", got)
	}
}

// TestIPString_ValidPassthrough — a real IP round-trips through
// unchanged; the fail-closed change must not touch the happy path.
func TestIPString_ValidPassthrough(t *testing.T) {
	ip := net.ParseIP("203.0.113.9")
	got, err := ipString(ip)
	if err != nil {
		t.Fatalf("ipString(%v) returned unexpected error: %v", ip, err)
	}
	if got != "203.0.113.9" {
		t.Errorf("ipString(%v) = %q, want %q", ip, got, "203.0.113.9")
	}
}
