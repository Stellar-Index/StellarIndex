// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package scval_test

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// FuzzDisplayB64 fuzzes the scval accessor reached with an attacker-chosen ScVal and no schema expectation; [scval.DisplayB64]
// must be total: no panic, degrade rather than error, stay bounded. Past defects: zero-value ScVal (Type==Bool, nil B) nil-
// deref'd in MustB; truncateDisplay cut a multi-byte rune. Fuzz run: `go test -run=^$ -fuzz=FuzzDisplayB64 ./internal/scval/`.
func FuzzDisplayB64(f *testing.F) {
	// Seeds: real on-wire blobs from this repo's fixtures, plus the
	// shapes the known defects lived in.
	seeds := []string{
		"", // the early-return
		// Real mainnet SEP-41 bodies (internal/sources/sep41_supply
		// dispatcher_adapter_test.go): a bare i128 burn amount and the
		// CAP-67 map { amount, to_muxed_id } that an i128-only decode
		// would drop.
		"AAAACgAAAAAAAAAAAAAAABOrZoA=",
		"AAAAEQAAAAEAAAACAAAADwAAAAZhbW91bnQAAAAAAAoAAAAAAAAAAAAAAABZ+LFaAAAADwAAAAt0b19tdXhlZF9pZAAAAAAOAAAAGUF1dG8gcmVjaGFyZ2UgdHJhbnNhY3Rpb24AAAA=",
		"AAAADwAAAARtaW50", // Symbol "mint"
		"AAAAEgAAAAAAAAAA8yVUX1QM+WJ+g+VPETScU03dLHaWSQx2iPuod3JtIzY=",                                 // Address
		"AAAADgAAADxFVEg6R0JGWE9IVkFTNDNPSVdOSU83WExSSkFIVDNCSUNGRUlLT0pMWlZYTlQ1NzJNSVNNNENNR1NPQ0M=", // String
		// Real REDSTONE feed_ids arg (internal/sources/redstone
		// subset_test.go): Vec<String>.
		"AAAAEAAAAAEAAAACAAAADgAAAANFVEgAAAAADgAAAANCVEMA",
		// The zero-value ScVal: Type==ScvBool(0) with a nil B pointer —
		// the exact body that panicked display().
		"AAAAAA==",
		// Void, and a Vec of one Void (container arm with a degenerate
		// element).
		"AAAAAQ==",
		"AAAAEAAAAAEAAAABAAAAAQ==",
		// Not base64 / not XDR: the degrade-to-empty path.
		"not base64 at all",
		"AAAA",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, b64 string) {
		// Property 1 — totality. DisplayB64 must never panic, whatever
		// the bytes decode to. (A panic here fails the fuzz run.)
		got := scval.DisplayB64(b64)

		// Property 2 — degrade, never error, and never invent. A body
		// that does not parse renders as the empty string; anything else
		// would put decoder-internal text on an explorer surface.
		if b64 != "" {
			if _, err := scval.Parse(b64); err != nil {
				if got != "" {
					t.Fatalf("unparseable body rendered %q, want the empty string", got)
				}
				return
			}
		}

		// Property 3, determinism: the renderer walks maps in stored order (ScMap is a slice), so the same body renders identically every
		// time; ranging a Go map would make explorer rows flicker.
		if again := scval.DisplayB64(b64); again != got {
			t.Fatalf("DisplayB64 is not deterministic:\n first: %q\nsecond: %q", got, again)
		}

		// Property 4, bounded output (what displayMaxDepth and truncateDisplay exist for): every terminal is bounded and containers
		// truncate their joined children, so no body may render past this ceiling; unbounded output would amplify response size.
		const maxRendered = 256
		if len(got) > maxRendered {
			t.Fatalf("rendered %d bytes (> %d) — the display bound is not holding:\n%q",
				len(got), maxRendered, got)
		}

		// Property 5: the truncation marker is the only ellipsis the renderer emits and it always terminates the truncated fragment; a
		// mid-string "…" would make a truncated render indistinguishable from a complete one.
		if i := strings.Index(got, "…"); i >= 0 && i != len(got)-len("…") {
			rest := got[i+len("…"):]
			if strings.Trim(rest, "]}") != "" {
				t.Fatalf("truncation marker is not at the end of its fragment: %q", got)
			}
		}
	})
}
