package chops

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

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
func chCreatorsRollup(args []string) error {
	fs := flag.NewFlagSet("ch-creators-rollup", flag.ContinueOnError)
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	cfgPath := fs.String("config", "", "Path to TOML config file — supplies stellar.movements_floor_ledger, the network's P23 boundary the two creation arms split at (default: the pubnet boundary)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	boundary, err := creatorsBoundary(*cfgPath)
	if err != nil {
		return err
	}
	ctx, cancel := opsutil.SignalContext()
	defer cancel()

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
// load (config.LoadWithEnv) so the unit's CONFIG_PATH serves both.
func creatorsBoundary(cfgPath string) (uint32, error) {
	if cfgPath == "" {
		return clickhouse.P23BoundaryLedger, nil
	}
	cfg, err := config.LoadWithEnv(cfgPath)
	if err != nil {
		return 0, err
	}
	if cfg.Stellar.MovementsFloorLedger == 0 {
		return clickhouse.P23BoundaryLedger, nil
	}
	return cfg.Stellar.MovementsFloorLedger, nil
}
