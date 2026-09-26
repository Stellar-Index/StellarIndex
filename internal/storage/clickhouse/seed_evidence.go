package clickhouse

import (
	"context"
	"errors"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// SeedWalk configures a full-history seed walk over stellar.ledger_entry_changes
// ([StreamSACBalanceSeedsFullHistory], [StreamClaimableBalanceSeeds]).
type SeedWalk struct {
	// VerifyLake refuses the walk unless stellar.ledgers is contiguous and
	// hash-linked over every ledger it reduces ([SubstrateProblem]). A lake hole
	// hides the change that superseded an entry, so the latest-write-wins
	// reduction would elect a stale value as current state. Every served-tier
	// write path sets it; only fixtures over a synthetic lake leave it off.
	VerifyLake bool

	// Progress, when set, hears of each completed window [from, to]. The walk
	// emits nothing until its last window, so this is the only sign of life a
	// heartbeat gets for hours.
	Progress func(from, to uint32)
}

// progressScan wraps a window scan so Progress hears of each completed window.
func (w SeedWalk) progressScan(scan func(from, to uint32) error) func(from, to uint32) error {
	if w.Progress == nil {
		return scan
	}
	return func(from, to uint32) error {
		if err := scan(from, to); err != nil {
			return err
		}
		w.Progress(from, to)
		return nil
	}
}

// SeedEvidence is what a full-history seed walk established, as opposed to
// what its caller asked for. Provenance is built from this, never from flags.
type SeedEvidence struct {
	// FromLedger and ToLedger bound the ledgers the walk reduced; ToLedger is
	// also the ledger liveness was judged at.
	FromLedger, ToLedger uint32
	// LakeVerifiedThrough is ToLedger when [SeedWalk.VerifyLake] proved
	// [FromLedger, ToLedger] intact before anything was emitted, else 0.
	LakeVerifiedThrough uint32
}

// ErrSeedLakeIncomplete is returned, before any seed is emitted, when a
// VerifyLake walk finds the lake missing or mis-linking a ledger in its range.
var ErrSeedLakeIncomplete = errors.New("clickhouse: seed walk: lake is not intact over the range it would reduce")

// substrateCheck is [SubstrateProblem] bound to one address.
type substrateCheck func(from, to uint32) (problem uint32, hasProblem bool, detail string, err error)

// resolveSeedWalk returns the ledger range a full-history seed reduces and,
// under VerifyLake, proves it intact first.
func resolveSeedWalk(ctx context.Context, conn driver.Conn, addr string, walk SeedWalk) (SeedEvidence, error) {
	lo, hi, err := entryChangeLedgerBounds(ctx, conn)
	if err != nil {
		return SeedEvidence{}, err
	}
	if !walk.VerifyLake {
		return SeedEvidence{FromLedger: lo, ToLedger: hi}, nil
	}
	tip, err := lakeTipLedger(ctx, conn)
	if err != nil {
		return SeedEvidence{}, err
	}
	return verifySeedWalk(lo, hi, tip, func(from, to uint32) (uint32, bool, string, error) {
		return SubstrateProblem(ctx, addr, from, to)
	})
}

// verifySeedWalk clamps the walk to the stellar.ledgers tip and proves
// [lo, tip] intact. Sink.Flush writes stellar.ledgers LAST, so an entry change
// above its tip belongs to a ledger still being flushed and is left for the
// live observer rather than reduced half-written.
func verifySeedWalk(lo, hi, lakeTip uint32, check substrateCheck) (SeedEvidence, error) {
	if lakeTip < hi {
		hi = lakeTip
	}
	if hi < lo {
		return SeedEvidence{}, fmt.Errorf("%w: stellar.ledgers tip %d is below the entry-change floor %d", ErrSeedLakeIncomplete, lakeTip, lo)
	}
	problem, has, detail, err := check(lo, hi)
	if err != nil {
		return SeedEvidence{}, err
	}
	if has {
		return SeedEvidence{}, fmt.Errorf("%w [%d, %d]: %s (ledger %d) — heal it (ch-live-catchup near the tip, ch-backfill below it) and re-run",
			ErrSeedLakeIncomplete, lo, hi, detail, problem)
	}
	return SeedEvidence{FromLedger: lo, ToLedger: hi, LakeVerifiedThrough: hi}, nil
}
