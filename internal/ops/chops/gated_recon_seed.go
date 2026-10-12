package chops

import (
	"context"
	"fmt"
	"os"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// preseedFactoryChildren seeds a factory-anchored source's contractid.Registry from genesis to `to` (idempotent); a sub-range re-derive
// would otherwise drop earlier children's events and report false "missing rows". A creation event whose decoder panics
// (completeness.Guard) is a blind spot.
func preseedFactoryChildren(ctx context.Context, es completeness.EventStreamer, src reconSource, to uint32) (completeness.BlindSpots, error) {
	if len(src.factories) == 0 || src.dec == nil {
		return completeness.BlindSpots{}, nil
	}
	// A factory with genesis at or after `to` deployed no children before it, and the window is
	// inverted when genesis > to. Skip the empty walk; the re-derive self-seeds in range.
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

// preseedResultMessage reports a preseed outcome, including zero: over a non-empty window an empty registry makes gated Matches() reject
// every child, which the re-derive then reports as missing rows rather than a decoder-blind gap.
func preseedResultMessage(name string, seeded int) string {
	if seeded > 0 {
		return fmt.Sprintf("verify-reconciliation: pre-seeded %d %s factory children (gate registry)\n", seeded, name)
	}
	return fmt.Sprintf("verify-reconciliation: WARNING %s factory preseed walk found 0 children in a non-empty window — gate registry stays empty; any pre-existing pool's events will be undercounted as missing, not attributed, for the rest of this re-derive\n", name)
}
