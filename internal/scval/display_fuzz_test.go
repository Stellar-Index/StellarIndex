// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package scval_test

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// FuzzDisplayB64 fuzzes the one accessor in this package that is reached
// with an ATTACKER-CHOSEN ScVal and no schema expectation at all.
//
// Everything else in internal/scval is called by a decoder that already
// knows the shape it wants and returns an error on a mismatch.
// [scval.DisplayB64] is different: the explorer's event views hand it
// whatever base64 the contract emitted and render the result, so its
// contract is a set of TOTALITY properties rather than a value — it must
// not panic, must degrade rather than error, and must stay bounded.
//
// Both defects this function has actually had were of that class and both
// were found by reading rather than by a test:
//
//   - the zero-value ScVal reports Type==Bool with a nil B pointer, so
//     MustB dereferenced nil and panicked the goroutine (display.go:74;
//     cold audit 2026-08-04) — reachable because MapField returns
//     xdr.ScVal{} on a miss and the miss path logged the value;
//   - truncateDisplay sliced BYTES, so a multi-byte rune straddling byte
//     120 produced invalid UTF-8 (display.go:159).
//
// A fuzz target is the durable guard for that class: it exercises the
// type switch's every arm, including the arms a hand-written fixture
// never reaches, against bodies no contract we watch would emit.
//
// Runs as a plain seed-corpus test under `go test`; the generative run is
// `go test -run=^$ -fuzz=FuzzDisplayB64 -fuzztime=20s ./internal/scval/`.
func FuzzDisplayB64(f *testing.F) {
	// Seeds: real on-wire blobs from this repo's fixtures, plus the
	// shapes the known defects lived in.
	seeds := []string{
		"", // the early-return
		// Real mainnet SEP-41 bodies (internal/sources/sep41_supply
		// golden_dropped_mint_test.go): a bare i128 burn amount and the
		// CAP-67 map { amount, to_muxed_id } that the i128-only decode
		// used to drop.
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

		// Property 3 — determinism. The renderer walks maps in stored
		// order (ScMap is a slice, not a Go map), so the same body must
		// render identically every time; a range over a Go map anywhere
		// in the walk would make explorer rows flicker between requests.
		if again := scval.DisplayB64(b64); again != got {
			t.Fatalf("DisplayB64 is not deterministic:\n first: %q\nsecond: %q", got, again)
		}

		// Property 4 — bounded output. This is what displayMaxDepth and
		// truncateDisplay exist for: "a pathological nested value can't
		// blow the stack or the response size" (display.go:17). Every
		// terminal is itself bounded (Symbol ≤ 32 by the XDR decoder,
		// String truncated at 120, i256/u256 ≤ 78 digits, strkey ≤ 69),
		// and a container truncates its joined children, so no body may
		// render past this ceiling. An unbounded render is a response-size
		// amplification: one event field, arbitrarily many bytes.
		const maxRendered = 256
		if len(got) > maxRendered {
			t.Fatalf("rendered %d bytes (> %d) — the display bound is not holding:\n%q",
				len(got), maxRendered, got)
		}

		// Property 5 — the truncation marker is the only ellipsis the
		// renderer emits, and it always terminates the fragment it
		// truncated. A "…" in the MIDDLE of a container's children means
		// a child was dropped rather than the join being cut, which
		// would make a truncated render indistinguishable from a
		// complete one.
		if i := strings.Index(got, "…"); i >= 0 && i != len(got)-len("…") {
			rest := got[i+len("…"):]
			if strings.Trim(rest, "]}") != "" {
				t.Fatalf("truncation marker is not at the end of its fragment: %q", got)
			}
		}
	})
}
