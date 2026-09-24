package chops

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// preseedFactoryChildren seeds a factory-anchored reconcile source's
// contractid.Registry (ADR-0035) by walking the factory's creation events
// from the source genesis up to `to` and running each through the
// decoder — dec.Decode registers the announced child as a side effect.
//
// No-op for non-gated sources (factory == ""). Idempotent.
//
// Why it's needed: the projection re-derive gates Matches() on the
// registry, so a child's business events are only counted once its
// creation event has been seen. A re-derive that starts at the source
// genesis self-seeds in-stream (the factory's creation events precede
// every child's events). But a re-derive over a CUSTOM sub-range
// (verify-reconciliation -from N, with N after some pool deploys) starts
// past those creation events, so without this pre-walk it would silently
// drop every pre-N child's events and report a false "missing rows"
// delta — the exact false-coverage signal the gate must not introduce.
//
// The walk is cheap: factory creation events are rare and the
// (contract_id, topic_0_sym) index on soroban_events serves the filter.
//
// FAIL CLOSED unless ledger_ingest_log fully covers [genesis, to]: a
// creation event inside a landing-zone hole is never seen, its child
// never seeded, and every one of its events is silently dropped from the
// re-derive — a false "missing" delta, or rows omitted by ch-rebuild -write.
func preseedFactoryChildren(ctx context.Context, store preseedStore, src reconSource, to uint32) error {
	if len(src.factories) == 0 || src.dec == nil {
		return nil
	}
	// A factory whose own genesis is at/after `to` deployed no children
	// BEFORE `to`, so the [genesis, to) preseed window holds nothing to
	// seed — and when genesis > to the window is inverted, which
	// StreamSorobanEvents rejects outright ("to < from"). This is exactly the
	// case a sub-range re-derive whose -from sits below a later-deploying
	// factory's genesis hits (e.g. a whole-lake ch-reproject -from below
	// defindex's genesis). Skip the empty walk; the re-derive over [to, hi]
	// self-seeds from the factory's in-range creation events.
	if src.genesis >= to {
		return nil
	}
	if err := preseedWindowCovered(ctx, store, src, to); err != nil {
		return err
	}
	seeded := 0
	err := store.StreamSorobanEvents(ctx, src.genesis, to,
		src.factories, []string{src.creationSym}, nil,
		func(row sorobanevents.Row) error {
			ev, rerr := sorobanevents.Reconstruct(row)
			if rerr != nil {
				return nil //nolint:nilerr // skip a broken row like the projector does
			}
			if src.dec.Matches(ev) {
				if _, derr := src.dec.Decode(ev); derr == nil {
					seeded++
				}
			}
			return nil
		})
	if err != nil {
		return fmt.Errorf("preseed %s factory children: %w", src.name, err)
	}
	fmt.Fprint(os.Stderr, preseedResultMessage(src.name, seeded))
	return nil
}

// preseedStore is the slice of *timescale.Store the preseed reads.
type preseedStore interface {
	FindLedgerIngestGaps(ctx context.Context, from, to uint32) ([]timescale.LedgerGap, error)
	StreamSorobanEvents(ctx context.Context, from, to uint32, contractIDs, topic0Syms, excludeTopic0Syms []string, fn func(sorobanevents.Row) error) error
}

// preseedWindowCovered applies the ledger_ingest_log full-coverage guard
// sdexCensusExpected and SorobanEventsTimeBound rely on to the preseed's
// [genesis, to] walk window (StreamSorobanEvents bounds are inclusive).
func preseedWindowCovered(ctx context.Context, store preseedStore, src reconSource, to uint32) error {
	gaps, err := store.FindLedgerIngestGaps(ctx, src.genesis, to)
	if err != nil {
		return fmt.Errorf("preseed %s factory children: coverage check [%d,%d]: %w", src.name, src.genesis, to, err)
	}
	if len(gaps) == 0 {
		return nil
	}
	const maxNamed = 5
	named := make([]string, 0, maxNamed+1)
	for i, g := range gaps {
		if i == maxNamed {
			named = append(named, fmt.Sprintf("… %d more", len(gaps)-maxNamed))
			break
		}
		named = append(named, fmt.Sprintf("%d-%d", g.Start, g.End))
	}
	return fmt.Errorf("preseed %s factory children: ledger_ingest_log has %d gap(s) in [%d,%d] (%s) — "+
		"factory creation events there cannot be proven seen, so their children's events would be dropped; "+
		"repair soroban_events for those ledgers and run `census-backfill` first",
		src.name, len(gaps), src.genesis, to, strings.Join(named, ", "))
}

// preseedResultMessage reports the outcome of a factory preseed walk,
// including the zero case. A silent zero is indistinguishable from "this
// source's window genuinely predates every deploy" (RLT-395): the walk
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
