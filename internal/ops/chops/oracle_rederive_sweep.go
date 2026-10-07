package chops

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/band"
	"github.com/Stellar-Index/StellarIndex/internal/sources/redstone"
	"github.com/Stellar-Index/StellarIndex/internal/sources/reflector"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// oracleRederiveSweeper is the one store method the post-run sweep needs.
type oracleRederiveSweeper interface {
	SweepOracleRederive(ctx context.Context, sw timescale.OracleRederiveSweep) (timescale.OracleRederiveSweepResult, error)
}

// oracleRederiveSweepScope reports whether a re-derived source writes
// on-chain oracle_updates rows, and whether its identity needs (asset,
// quote): band's op_index is shared by nested relays in one op.
func oracleRederiveSweepScope(source string) (sweep, assetScoped bool) {
	switch source {
	case reflector.SourceDEX, reflector.SourceCEX, reflector.SourceFX, redstone.SourceName:
		return true, false
	case band.SourceName:
		return true, true
	}
	return false, false
}

// sweepOracleRederive runs the end-of-run stale-row sweep for one source a
// completed re-derive wrote at generation gen over [from, to]. Only the
// re-derive commands that stamp their own generation call it: a dry run
// counts against any newer generation and deletes nothing.
func sweepOracleRederive(ctx context.Context, w io.Writer, store oracleRederiveSweeper, cmd, source string, from, to uint32, gen int64, write bool) error {
	sweep, assetScoped := oracleRederiveSweepScope(source)
	if !sweep {
		return nil
	}
	sw := timescale.OracleRederiveSweep{Source: source, From: from, To: to, AssetScoped: assetScoped, DryRun: !write}
	verb := "would delete"
	if write {
		sw.Generation, verb = gen, "deleted"
	}
	res, err := store.SweepOracleRederive(ctx, sw)
	if err != nil {
		return fmt.Errorf("%s: oracle_updates stale-row sweep %s [%d,%d] (deleted %d before the error): %w", cmd, source, from, to, res.Deleted, err)
	}
	_, _ = fmt.Fprintf(w, "%s: oracle_updates sweep %s [%d,%d] gen=%d: %s %d stale row(s) (%d future-dated >=1h past the re-derived ts); kept %d op_index-shifted row(s) for inspection\n",
		cmd, source, from, to, sw.Generation, verb, res.Deleted, res.FutureTS, res.OpIndexShifted)
	if res.Deleted > 0 {
		_, _ = fmt.Fprintf(w, "%s: oracle_prices_* cover the swept ts span [%s, %s]; refresh them over it\n",
			cmd, res.TSFrom.Format(time.RFC3339), res.TSTo.Format(time.RFC3339))
	}
	return nil
}

// sweepCHRebuildOracle sweeps each oracle source ch-rebuild's contract-call
// pass re-derived. A source with a failed write is skipped: the run did not
// complete it.
func sweepCHRebuildOracle(ctx context.Context, w io.Writer, store oracleRederiveSweeper, cat []reconSource, enabled func(string) bool, contractCalls bool, failed map[string]int, lo, hi uint32, gen int64, write bool) error {
	if !contractCalls {
		return nil
	}
	for _, src := range cat {
		if src.callDec == nil || !enabled(src.name) {
			continue
		}
		if failed[src.name] > 0 {
			_, _ = fmt.Fprintf(w, "ch-rebuild: oracle_updates sweep %s skipped: %d failed write(s)\n", src.name, failed[src.name])
			continue
		}
		if err := sweepOracleRederive(ctx, w, store, "ch-rebuild", src.name, lo, hi, gen, write); err != nil {
			return err
		}
	}
	return nil
}

// sweepProjectedRebuildOracle sweeps after a projected-rebuild run that
// landed every row; an interrupted or lossy run sweeps nothing.
func sweepProjectedRebuildOracle(ctx context.Context, w io.Writer, store oracleRederiveSweeper, source string, from, to uint32, gen int64, write bool, r ProjectedRebuildResult, runErr error, interrupted bool) error {
	if runErr != nil || interrupted || projectedRebuildOutcome(r, nil, false) != nil {
		return nil //nolint:nilerr // the caller already carries runErr; a lossy run just skips the sweep
	}
	return sweepOracleRederive(ctx, w, store, "projected-rebuild", source, from, to, gen, write)
}
