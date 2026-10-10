package chops

import (
	"context"
	"fmt"
	"os"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// preseedFactoryChildren seeds a factory-anchored reconcile source's
// contractid.Registry (ADR-0035) by walking the factory's creation events from
// source genesis up to `to` through the decoder (dec.Decode registers the
// announced child as a side effect). No-op for non-gated sources
// (factory == ""). Idempotent.
//
// Needed because the re-derive gates Matches() on the registry. A re-derive
// from genesis self-seeds in-stream, but one over a custom sub-range
// (verify-reconciliation -from N, past some pool deploys) would silently drop
// every pre-N child's events and report a false "missing rows" delta.
//
// It reads the certified ClickHouse lake (callers pass a
// clickhouse.ReconcileEventStreamer), never the Postgres landing zone; factory
// creation events are rare and contract-prefiltered, so it is cheap.
//
// The decoder runs under completeness.Guard: a creation event whose decoder
// panics leaves that child unseeded, so it is returned as a blind spot rather
// than crashing the caller, which decides what a blind preseed means for it.
func preseedFactoryChildren(ctx context.Context, es completeness.EventStreamer, src reconSource, to uint32) (completeness.BlindSpots, error) {
	if len(src.factories) == 0 || src.dec == nil {
		return completeness.BlindSpots{}, nil
	}
	// A factory whose own genesis is at/after `to` deployed no children
	// BEFORE `to`, so the [genesis, to) preseed window holds nothing to
	// seed — and when genesis > to the window is inverted. This is exactly
	// the case a sub-range re-derive whose -from sits below a
	// later-deploying factory's genesis hits (e.g. a whole-lake ch-reproject
	// -from below defindex's genesis). Skip the empty walk; the re-derive
	// over [to, hi] self-seeds from the factory's in-range creation events.
	if src.genesis >= to {
		return completeness.BlindSpots{}, nil
	}
	seeded := 0
	blind := completeness.NewBlindTracker()
	err := es.StreamContractEvents(ctx, src.genesis, to, src.factories, []string{src.creationSym},
		func(ev events.Event) error {
			if perr := completeness.Guard(func() {
				if src.dec.Matches(ev) {
					if _, derr := src.dec.Decode(ev); derr == nil {
						seeded++
					}
				}
			}); perr != nil {
				blind.Undecodable(ev.Ledger)
			}
			return nil
		})
	if err != nil {
		return completeness.BlindSpots{}, fmt.Errorf("preseed %s factory children: %w", src.name, err)
	}
	fmt.Fprint(os.Stderr, preseedResultMessage(src.name, seeded))
	if seeded == 0 {
		// An emptied or unreachable event store yields zero rows too, so a
		// warning alone lets the caller report good data as missing.
		return completeness.BlindSpots{}, fmt.Errorf("preseed %s factory children: 0 seeded from %q creation events in [%d,%d]",
			src.name, src.creationSym, src.genesis, to)
	}
	return blind.Result(), nil
}

// preseedResultMessage reports the outcome of a factory preseed walk,
// including the zero case. A silent zero is indistinguishable from "this
// source's window genuinely predates every deploy": the walk
// covers a non-empty, non-inverted window (preseedFactoryChildren already
// returned early otherwise), so finding no creation events there is
// suspicious enough to surface — an empty in-memory registry makes the
// gated decoder's Matches() reject every real child's events for the rest
// of the re-derive, which reports them as missing rather than as the
// decoder-blind gap they actually are.
func preseedResultMessage(name string, seeded int) string {
	if seeded > 0 {
		return fmt.Sprintf("verify-reconciliation: pre-seeded %d %s factory children (gate registry)\n", seeded, name)
	}
	return fmt.Sprintf("verify-reconciliation: WARNING %s factory preseed walk found 0 children in a non-empty window — gate registry stays empty; any pre-existing pool's events will be undercounted as missing, not attributed, for the rest of this re-derive\n", name)
}
