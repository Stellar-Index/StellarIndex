package main

import (
	"context"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

const (
	genesisAutoSeedRetry   = 10 * time.Minute
	genesisAutoSeedTimeout = 15 * time.Minute
	// sep41CoverageSettle keeps the compared range below ledgers either tier may still be writing (about ten minutes).
	sep41CoverageSettle = 120
)

// newSEP41CoverageCheck compares a contract's Soroban-era per-kind sums in the served tier with the lake's, which is not watch-list filtered.
func newSEP41CoverageCheck(cfg config.Config, store *timescale.Store) supply.HistoryCoverageCheck {
	boundary := cfg.Stellar.SorobanGenesisLedger
	if boundary == 0 {
		boundary = clickhouse.SorobanGenesisLedger
	}
	addr := cfg.Storage.ClickHouseAddr
	return func(ctx context.Context, contractID string, ledger uint32) error {
		ctx, cancel := context.WithTimeout(ctx, genesisAutoSeedTimeout)
		defer cancel()
		reader, err := dialSupplyLake(ctx, addr)
		if err != nil {
			return err
		}
		defer func() { _ = reader.Close() }()
		return checkSEP41Coverage(ctx, reader, store, uint32(boundary), contractID, ledger)
	}
}

// sep41CoverageLake is the lake side of the coverage check.
type sep41CoverageLake interface {
	LakeTip(ctx context.Context) (uint32, error)
	TokenSupplyInLedgerRange(ctx context.Context, contractID string, fromLedger, toLedger uint32) (clickhouse.TokenSupply, error)
	FirstContractEvent(ctx context.Context, contractID string) (uint32, time.Time, error)
}

// sep41CoverageStore is the served side of the coverage check.
type sep41CoverageStore interface {
	SEP41SupplyDurableLedger(ctx context.Context) (uint32, error)
	SEP41EventTotalsInRange(ctx context.Context, contractID string, fromLedger, toLedger uint32) (timescale.SEP41KindTotals, error)
	SEP41FirstServedLedger(ctx context.Context, contractID string, notBefore time.Time) (uint32, error)
}

func checkSEP41Coverage(ctx context.Context, lake sep41CoverageLake, store sep41CoverageStore, boundary uint32, contractID string, ledger uint32) error {
	tip, err := lake.LakeTip(ctx)
	if err != nil {
		return err
	}
	// The projector's durable cursor bounds the served side: past it the served tier is behind, not holed.
	projected, err := store.SEP41SupplyDurableLedger(ctx)
	if err != nil {
		return err
	}
	upTo, ok := sep41CoverageUpTo(boundary, tip, ledger, projected)
	if !ok {
		return fmt.Errorf("%w: no settled Soroban-era ledger to compare (lake tip %d, snapshot %d, projector %d)",
			supply.ErrSEP41HistoryNotCovered, tip, ledger, projected)
	}
	lakeSum, err := lake.TokenSupplyInLedgerRange(ctx, contractID, boundary, upTo)
	if err != nil {
		return err
	}
	served, err := store.SEP41EventTotalsInRange(ctx, contractID, boundary, upTo)
	if err != nil {
		return err
	}
	if err := sep41TotalsMatch(lakeSum, served, boundary, upTo); err != nil {
		return explainSEP41Gap(ctx, lake, store, contractID, upTo, err)
	}
	obs.AggregatorSEP41HistoryGapFromLedger.WithLabelValues(contractID).Set(0)
	return nil
}

// explainSEP41Gap names the leading window the served tier lacks (a contract watched later than its first event) and the
// commands that fill it; mismatch stays the wrapped cause, so the outcome is still `missing_baseline`.
func explainSEP41Gap(ctx context.Context, lake sep41CoverageLake, store sep41CoverageStore, contractID string, upTo uint32, mismatch error) error {
	lakeFirst, lakeFirstAt, err := lake.FirstContractEvent(ctx, contractID)
	if err != nil {
		return fmt.Errorf("%w (served-history gap not located: %v)", mismatch, err) //nolint:errorlint // the lookup failure is context, not the cause.
	}
	gauge := obs.AggregatorSEP41HistoryGapFromLedger.WithLabelValues(contractID)
	if lakeFirst == 0 {
		gauge.Set(0)
		return mismatch
	}
	servedFirst, err := store.SEP41FirstServedLedger(ctx, contractID, lakeFirstAt)
	if err != nil {
		return fmt.Errorf("%w (served-history gap not located: %v)", mismatch, err) //nolint:errorlint // the lookup failure is context, not the cause.
	}
	if servedFirst == 0 {
		servedFirst = upTo + 1
	}
	from, to, ok := sep41GapWindow(lakeFirst, servedFirst)
	if !ok {
		gauge.Set(0)
		return mismatch
	}
	gauge.Set(float64(lakeFirst))
	return fmt.Errorf("%w; contract %s: lake events start at ledger %d, served rows at %d, so ledgers [%d, %d] are missing. "+
		"Decompress both tables' chunks over the window (%s), then run "+
		"`stellarindex-ops projected-rebuild -config /etc/stellarindex.toml -source sep41_transfers -from %d -to %d -resume=false -write` and "+
		"`stellarindex-ops projected-rebuild -config /etc/stellarindex.toml -source sep41_supply -from %d -to %d -resume=false -write`",
		mismatch, contractID, lakeFirst, servedFirst, from, to, sep41GapRunbook, from, to, from, to)
}

const sep41GapRunbook = "docs/operations/runbooks/supply.md#stellarindex_aggregator_sep41_history_gap"

// sep41GapWindow is the leading range the served tier lacks: from the lake's first event to the ledger before the first served
// row. servedFirst is one past the compared range when nothing is served.
func sep41GapWindow(lakeFirst, servedFirst uint32) (from, to uint32, ok bool) {
	if lakeFirst == 0 || servedFirst <= lakeFirst {
		return 0, 0, false
	}
	return lakeFirst, servedFirst - 1, true
}

// sep41CoverageUpTo is the last ledger all three positions have settled, or false when none is past the boundary.
func sep41CoverageUpTo(boundary, lakeTip, snapshot, projected uint32) (uint32, bool) {
	upTo := min(lakeTip, snapshot)
	if upTo < boundary+sep41CoverageSettle {
		return 0, false
	}
	upTo = min(upTo-sep41CoverageSettle, projected)
	return upTo, upTo >= boundary
}

func sep41TotalsMatch(lake clickhouse.TokenSupply, served timescale.SEP41KindTotals, from, to uint32) error {
	if lake.Mint.Cmp(served.Mint) == 0 && lake.Burn.Cmp(served.Burn) == 0 && lake.Clawback.Cmp(served.Clawback) == 0 {
		return nil
	}
	return fmt.Errorf("%w: ledgers [%d, %d]: lake mint=%s burn=%s clawback=%s, served mint=%s burn=%s clawback=%s",
		supply.ErrSEP41HistoryNotCovered, from, to, lake.Mint, lake.Burn, lake.Clawback, served.Mint, served.Burn, served.Clawback)
}

// newGenesisAutoSeeder is the in-process equivalent of `stellarindex-ops supply seed-sep41-genesis -write` for one contract;
// it dials the lake per attempt (seeding is rare) so construction never touches the network.
func newGenesisAutoSeeder(cfg config.Config, store *timescale.Store) supply.GenesisSeeder {
	boundary := cfg.Stellar.SorobanGenesisLedger
	if boundary == 0 {
		boundary = clickhouse.SorobanGenesisLedger
	}
	addr := cfg.Storage.ClickHouseAddr
	return func(ctx context.Context, contractID string) error {
		// A boundary above the Soroban genesis would double-count Soroban-era flows.
		if boundary > clickhouse.SorobanGenesisLedger {
			return fmt.Errorf("genesis boundary %d exceeds the Soroban genesis ledger %d", boundary, clickhouse.SorobanGenesisLedger)
		}
		ctx, cancel := context.WithTimeout(ctx, genesisAutoSeedTimeout)
		defer cancel()
		reader, err := dialSupplyLake(ctx, addr)
		if err != nil {
			return err
		}
		defer func() { _ = reader.Close() }()
		below, err := reader.TokenSupplyBelowLedger(ctx, contractID, uint32(boundary))
		if err != nil {
			return fmt.Errorf("pre-genesis sum for %s: %w", contractID, err)
		}
		return store.UpsertSEP41GenesisBaseline(ctx, contractID,
			timescale.SEP41KindTotals{Mint: below.Mint, Burn: below.Burn, Clawback: below.Clawback}, uint32(boundary))
	}
}

// dialSupplyLake dials per attempt: both callers run rarely and retry on their own timer, so a cold lake costs one attempt.
func dialSupplyLake(ctx context.Context, addr string) (*clickhouse.SupplyReader, error) {
	return clickhouse.NewSupplyReader(ctx, addr)
}
