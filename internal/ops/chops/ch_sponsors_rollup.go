package chops

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

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
func chSponsorsRollup(args []string) error {
	fs := flag.NewFlagSet("ch-sponsors-rollup", flag.ContinueOnError)
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	if err := fs.Parse(args); err != nil {
		return err
	}
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
