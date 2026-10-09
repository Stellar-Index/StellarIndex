package chops

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// creatorsRollupLockPath is where chCreatorsRollup takes its exclusive
// advisory lock. Same state directory as the other rollup locks (see
// holdersRollupLockPath).
const creatorsRollupLockPath = "/var/lib/stellarindex/ch-creators-rollup.lock"

// ch-creators-rollup recomputes the account-creator league table (funder →
// accounts created, with the created set's surviving accounts and current
// XLM) into staging and atomically exchanges it live
// (deploy/clickhouse/account_creators_rollup.sql). The aggregation is
// scan-shaped (movement_kind is not in account_movements' ORDER BY), so it is
// paid once per cycle; the endpoint reads a keyed board and seven metric rows.
//
// A creation changes representation at Protocol 23 rather than stopping:
// classic create_account movements below the boundary, the CAP-67 transfer
// paired with a CreateAccount operation above it. -config supplies the
// network's boundary (stellar.movements_floor_ledger; a reset testnet starts
// post-P23, so the classic arm owns nothing); without it the pubnet boundary
// applies. The cycle also writes the span it aggregated, so the API never
// assumes the board covers the whole chain.
//
// It holds an exclusive advisory lock (acquireRollupLock): this cycle,
// ch-holders-rollup and a manual run all TRUNCATE -> fill -> EXCHANGE
// global staging tables and would otherwise race.
func chCreatorsRollup(args []string) error {
	fs, gate := opsutil.NewMutatingFlagSet("ch-creators-rollup")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	cfgPath := fs.String("config", "", "Path to TOML config file — supplies stellar.movements_floor_ledger, the network's P23 boundary the two creation arms split at (default: the pubnet boundary)")
	lockPath := fs.String("lock-file", creatorsRollupLockPath,
		"path to the exclusive advisory lock serializing this run against the timer or a second concurrent invocation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := gate.RequireStatedMode(); err != nil {
		return fmt.Errorf("ch-creators-rollup: %w", err)
	}
	if !gate.Banner() {
		fmt.Fprintln(os.Stderr, "ch-creators-rollup: DRY RUN — would recompute the account-creator league table and EXCHANGE it live; pass -write to apply")
		return nil
	}

	unlock, err := acquireRollupLock("ch-creators-rollup", *lockPath)
	if err != nil {
		return err
	}
	defer unlock()

	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	boundary, err := creatorsBoundary(ctx, *chAddr, *cfgPath)
	if err != nil {
		return err
	}

	start := time.Now()
	fmt.Fprintf(os.Stderr, "ch-creators-rollup: P23 boundary ledger %d\n", boundary)
	if err := clickhouse.RunCreatorsRollup(ctx, *chAddr, boundary, func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "ch-creators-rollup: "+format+"\n", a...)
	}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "ch-creators-rollup: cycle complete in %s\n", time.Since(start).Round(time.Second))
	return nil
}

// creatorsBoundary resolves the creation arms' boundary ledger: the
// config's stellar.movements_floor_ledger when a config is given and sets
// one, else the pubnet P23 boundary. Mirrors ch-cohort-rollup's config
// load (config.LoadWithEnv) so the unit's CONFIG_PATH serves both. The
// result is checked against the lake's tip (see [validateCreatorsBoundary]).
func creatorsBoundary(ctx context.Context, chAddr, cfgPath string) (uint32, error) {
	boundary := clickhouse.P23BoundaryLedger
	if cfgPath != "" {
		cfg, err := config.LoadWithEnv(cfgPath)
		if err != nil {
			return 0, err
		}
		if cfg.Stellar.MovementsFloorLedger != 0 {
			boundary = cfg.Stellar.MovementsFloorLedger
		}
	}
	lakeTip, err := clickhouse.MaxLedger(ctx, chAddr)
	if err != nil {
		return 0, fmt.Errorf("creators boundary: read lake tip: %w", err)
	}
	if err := validateCreatorsBoundary(boundary, lakeTip); err != nil {
		return 0, err
	}
	return boundary, nil
}

// validateCreatorsBoundary rejects a boundary above the lake's tip: a chain
// that never reached its boundary (pubnet's P23 ledger on a reset test net)
// hands every ledger to the classic arm, which looks for create_account
// movements that chain never wrote, so the board comes back silently empty.
// A boundary at or below the
// lake's first ledger is valid — a test net's movements_floor_ledger is 1,
// every ledger post-P23. lakeTip==0 (an empty lake) has nothing to split.
func validateCreatorsBoundary(boundary, lakeTip uint32) error {
	if lakeTip > 0 && boundary > lakeTip {
		return fmt.Errorf("creators boundary ledger %d is above the lake's tip %d: "+
			"this chain has not reached it — set stellar.movements_floor_ledger for this network "+
			"(1 on a reset testnet/futurenet)", boundary, lakeTip)
	}
	return nil
}
