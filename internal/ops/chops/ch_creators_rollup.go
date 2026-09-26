package chops

import (
	"context"
	"flag"
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

// ch-creators-rollup — #351: recompute the account-creator league table
// (funder → accounts created, with the created set's surviving accounts
// and current XLM) into staging and atomically exchange it live
// (deploy/clickhouse/account_creators_rollup.sql).
//
// The aggregation is scan-shaped over stellar.account_movements because
// movement_kind is not in that table's ORDER BY, so it is a cycle cost
// paid once, not a per-request cost: the endpoint reads a keyed board
// and seven metric rows.
//
// It reads that archive on both sides of the Protocol 23 boundary, where
// a creation changes representation rather than stopping: the classic
// create_account movements below it, and above it the CAP-67 transfer
// paired with the CreateAccount operation in stellar.operations (#493).
// The boundary is the network's, not a constant: -config supplies
// stellar.movements_floor_ledger (the pubnet P23 boundary on r1, the
// chain's start on a reset testnet/futurenet, where every ledger is
// post-P23 and the classic arm owns nothing). Without -config the pubnet
// boundary applies.
//
// The same cycle writes the coverage span it aggregated, so the API
// never has to assume the board covers the whole chain.
//
// Takes an exclusive advisory lock before it runs anything: this cycle
// and ch-holders-rollup's timer both TRUNCATE -> fill -> EXCHANGE their
// own global staging tables, and a manual `stellarindex-ops
// ch-creators-rollup` invocation can otherwise race a concurrent one on
// those same tables. See acquireRollupLock (rollup_lock.go).
func chCreatorsRollup(args []string) error {
	fs := flag.NewFlagSet("ch-creators-rollup", flag.ContinueOnError)
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	cfgPath := fs.String("config", "", "Path to TOML config file — supplies stellar.movements_floor_ledger, the network's P23 boundary the two creation arms split at (default: the pubnet boundary)")
	lockPath := fs.String("lock-file", creatorsRollupLockPath,
		"path to the exclusive advisory lock serializing this run against the timer or a second concurrent invocation")
	if err := fs.Parse(args); err != nil {
		return err
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
