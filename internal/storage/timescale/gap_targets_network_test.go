// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sourcenet"
)

// TestGapDetectorTargetsResolveToKnownSources: sourcenet.Applicable is false for unclassified names, so an
// unknown SourceNetKey is silently dropped from non-pubnet scans. Every target must resolve to a
// sourcenet name directly or via an explicit CanonicalSource.
func TestGapDetectorTargetsResolveToKnownSources(t *testing.T) {
	t.Parallel()

	for _, target := range DefaultGapDetectorTargets {
		key := target.SourceNetKey()
		if !sourcenet.Known(key) {
			t.Errorf("gap-detector target %q resolves to sourcenet key %q, which sourcenet does not classify.\n"+
				"Set CanonicalSource on the target to the source it belongs to (or classify %q in "+
				"internal/sourcenet). Unclassified names are excluded from EVERY non-pubnet network.",
				target.Source, key, key)
		}
	}
}

// TestApplicableGapDetectorTargets_ScopesToTheNetwork pins the filter: each target's Genesis is a PUBNET
// deploy ledger, so a pubnet-only target on a lower-tip test net yields an inverted window and
// FindPerSourceLedgerGaps fails with `invalid range` every cadence.
func TestApplicableGapDetectorTargets_ScopesToTheNetwork(t *testing.T) {
	t.Parallel()

	// Pubnet is the identity: r1 must scan exactly what it scanned
	// before, in the same order.
	for _, network := range []string{"", sourcenet.Pubnet} {
		got := ApplicableGapDetectorTargets(DefaultGapDetectorTargets, network, nil)
		if len(got) != len(DefaultGapDetectorTargets) {
			t.Fatalf("network %q scans %d of %d targets, want all of them",
				network, len(got), len(DefaultGapDetectorTargets))
		}
		for i := range got {
			if got[i].Source != DefaultGapDetectorTargets[i].Source {
				t.Fatalf("network %q reordered the scan: position %d is %q, want %q",
					network, i, got[i].Source, DefaultGapDetectorTargets[i].Source)
			}
		}
	}

	for _, network := range []string{sourcenet.Testnet, sourcenet.Futurenet} {
		kept := map[string]bool{}
		for _, target := range ApplicableGapDetectorTargets(DefaultGapDetectorTargets, network, nil) {
			kept[target.Source] = true
		}

		// Ledger-anchored substrate exists on every network and MUST keep its coverage
		// signal. sep41-* and soroban-events are the ones a naive name match would drop:
		// their target keys are hyphenated while sourcenet spells them with underscores.
		for _, src := range []string{"sdex", "sep41-transfers", "sep41-supply", "soroban-events"} {
			if !kept[src] {
				t.Errorf("%s: target %q was dropped, but its substrate is the ledger itself — "+
					"the test net loses a real coverage signal", network, src)
			}
		}

		// Pubnet contract identities (ADR-0035): the decoders match
		// nothing here, so scanning them can only produce noise. One
		// per naming shape — bare, sub-table, and underscore-canonical.
		for _, src := range []string{
			"soroswap", "soroswap-skim", "aquarius-liquidity", "blend-positions",
			"blend-backstop", "blend-emitter", "comet-liquidity", "sorocredit-events",
			"phoenix-stake", "defindex-fees", "reflector-fx", "band",
		} {
			if kept[src] {
				t.Errorf("%s: target %q is still scanned, but its contract set is pubnet-only — "+
					"its genesis sits above this network's tip, so every cycle errors", network, src)
			}
		}
	}
}
