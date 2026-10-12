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

// creatorsRollupLockPath is chCreatorsRollup's exclusive advisory lock, in the same state directory as the other
// rollup locks (see holdersRollupLockPath).
const creatorsRollupLockPath = "/var/lib/stellarindex/ch-creators-rollup.lock"

// ch-creators-rollup recomputes the account-creator league table into staging and atomically exchanges it live
// (deploy/clickhouse/account_creators_rollup.sql); scan-shaped (movement_kind is not in account_movements' ORDER BY), so paid once per
// cycle. A creation is a classic create_account movement below the Protocol 23 boundary and a CAP-67 transfer paired with CreateAccount
// above it; -config supplies the boundary. acquireRollupLock guards the staging tables.
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

// creatorsBoundary resolves the creation arms' boundary ledger: config's stellar.movements_floor_ledger, else the
// pubnet P23 boundary; loaded as ch-cohort-rollup does and checked by [validateCreatorsBoundary].
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

// validateCreatorsBoundary rejects a boundary above the lake's tip: the classic arm would look for create_account movements never written,
// leaving the board silently empty. At or below the lake's first ledger is valid; an empty lake (lakeTip==0) has nothing to split.
func validateCreatorsBoundary(boundary, lakeTip uint32) error {
	if lakeTip > 0 && boundary > lakeTip {
		return fmt.Errorf("creators boundary ledger %d is above the lake's tip %d: "+
			"this chain has not reached it — set stellar.movements_floor_ledger for this network "+
			"(1 on a reset testnet/futurenet)", boundary, lakeTip)
	}
	return nil
}
