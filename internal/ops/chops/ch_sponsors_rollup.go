package chops

import (
	"fmt"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// sponsorsRollupLockPath is chSponsorsRollup's exclusive advisory lock, in the same state directory as the other rollup
// locks (see holdersRollupLockPath).
const sponsorsRollupLockPath = "/var/lib/stellarindex/ch-sponsors-rollup.lock"

// ch-sponsors-rollup recomputes the sponsor league table into staging and atomically exchanges it live
// (deploy/clickhouse/account_sponsors_rollup.sql) from one deduplicated pass over stellar.operations; no body_xdr is read. The recorded
// span's floor is protocol 14, shown as genesis, not missing coverage. Takes the exclusive lock first (acquireRollupLock, rollup_lock.go).
func chSponsorsRollup(args []string) error {
	fs, gate := opsutil.NewMutatingFlagSet("ch-sponsors-rollup")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	lockPath := fs.String("lock-file", sponsorsRollupLockPath,
		"path to the exclusive advisory lock serializing this run against the timer or a second concurrent invocation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := gate.RequireStatedMode(); err != nil {
		return fmt.Errorf("ch-sponsors-rollup: %w", err)
	}
	if !gate.Banner() {
		fmt.Fprintln(os.Stderr, "ch-sponsors-rollup: DRY RUN — would recompute the sponsor league table and EXCHANGE it live; pass -write to apply")
		return nil
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
