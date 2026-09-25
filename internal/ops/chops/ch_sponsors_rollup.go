package chops

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// sponsorsRollupLockPath is where chSponsorsRollup takes its exclusive
// advisory lock. Same state directory as the other rollup locks (see
// holdersRollupLockPath).
const sponsorsRollupLockPath = "/var/lib/stellarindex/ch-sponsors-rollup.lock"

// ch-sponsors-rollup — #351: recompute the sponsor league table (who has
// entered into sponsorship arrangements, with how many distinct
// accounts, and how many revocations they issued) into staging and
// atomically exchange it live
// (deploy/clickhouse/account_sponsors_rollup.sql).
//
// One pass over stellar.operations lands a narrow, deduplicated
// projection of the three sponsorship operation types; every served
// figure derives from those rows. No body_xdr is read — the sponsored
// account is the End operation's source_account — which is what keeps
// the pass affordable.
//
// The cycle also records the span it aggregated. That floor is protocol
// 14's activation, where sponsorship began, so the surface can present
// it as the feature's genesis rather than as missing coverage.
//
// Takes an exclusive advisory lock before it runs anything, for the same
// reason ch-creators-rollup and ch-holders-rollup do: see
// acquireRollupLock (rollup_lock.go).
func chSponsorsRollup(args []string) error {
	fs := flag.NewFlagSet("ch-sponsors-rollup", flag.ContinueOnError)
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	lockPath := fs.String("lock-file", sponsorsRollupLockPath,
		"path to the exclusive advisory lock serializing this run against the timer or a second concurrent invocation")
	if err := fs.Parse(args); err != nil {
		return err
	}

	unlock, err := acquireRollupLock("ch-sponsors-rollup", *lockPath)
	if err != nil {
		return err
	}
	defer unlock()

	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	start := time.Now()
	if err := clickhouse.RunSponsorsRollup(ctx, *chAddr, func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "ch-sponsors-rollup: "+format+"\n", a...)
	}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "ch-sponsors-rollup: cycle complete in %s\n", time.Since(start).Round(time.Second))
	return nil
}
