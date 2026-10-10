// Binary stellarindex-ops is the admin CLI for operational tasks that don't
// belong in the long-running binaries.
//
// Subcommand implementations live in internal/ops/{ingest,archive,discovery,
// supply,diagnostics,chops,incident,usage,keys,accounts}, with helpers shared
// across them in internal/ops/opsutil. chops (the ADR-0033/ADR-0034
// ClickHouse-lake tools) is not named clickhouse to avoid shadowing
// internal/storage/clickhouse in every file there. main.go holds only the
// dispatch table and the few subcommands too small for a package (docs-config).
//
// The canonical subcommand list is the `subcommands` map below plus the
// `stellarindex-ops --help` output.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/accounts"
	"github.com/Stellar-Index/StellarIndex/internal/ops/archive"
	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	"github.com/Stellar-Index/StellarIndex/internal/ops/diagnostics"
	"github.com/Stellar-Index/StellarIndex/internal/ops/discovery"
	"github.com/Stellar-Index/StellarIndex/internal/ops/incident"
	"github.com/Stellar-Index/StellarIndex/internal/ops/ingest"
	"github.com/Stellar-Index/StellarIndex/internal/ops/keys"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/ops/supply"
	"github.com/Stellar-Index/StellarIndex/internal/ops/usage"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/version"
)

// main is a thin shim over realMain so deferred functions (notably
// the SilenceSDKChecksumWarnings flush) execute on every exit
// path. os.Exit skips defers — see the SilenceSDKChecksumWarnings
// docstring: without the flush, short-lived subcommands
// (`backfill -dry-run`, `backfill` with an error) would print only
// their first line and eat the rest, because the consumer goroutine
// behind fd 2's filter is killed mid-buffer.
func main() {
	os.Exit(realMain())
}

// subcommands maps each subcommand name to its handler: a package Run, or a
// leaf closure here wrapped in [leaf].
//
// A package Run receives the full argv starting at the subcommand name
// (args[0] is the map key); each internal/ops/* Run switches on args[0], which
// lets several keys share one Run.
//
// A leaf handler receives only its own flags: [leaf] strips the verb. This is
// load-bearing. Leaf handlers call fs.Parse(args) and Go's flag package stops
// at the first non-flag argument, so a verb-prefixed argv parses nothing and
// leaves every flag zero. Unit tests call handlers directly with flags-only
// argv, so only TestSubcommandDispatch_LeafHandlersSeeTheirFlags covers the
// dispatcher path.
//
// Handlers return an error to exit 1; realMain prints the "name: err" prefix.
// Return opsutil.ErrExitSilently to suppress it after printing a specific
// message. The canonical list is this table plus the usageBody help text.
var subcommands = map[string]func(args []string) error{
	"docs-config":           leaf(func([]string) error { return config.EmitMarkdown(os.Stdout) }),
	"mint-key":              leaf(keys.Mint),
	"upgrade-key":           leaf(keys.Upgrade),
	"emit-incident":         leaf(incident.Emit),
	"usage-rollup-backfill": leaf(usage.RollupBackfill),
	"freeze-unfreeze":       leaf(accounts.FreezeUnfreeze),
	"account-erase":         leaf(accounts.Erase),
	"change-summary-reset":  leaf(usage.ChangeSummaryReset),

	"rpc-probe":             diagnostics.Run,
	"verify-decoders":       diagnostics.Run,
	"verify-external":       diagnostics.Run,
	"hubble-check":          diagnostics.Run,
	"hubble-soroban-events": diagnostics.Run,

	"backfill":                ingest.Run,
	"backfill-external":       ingest.Run,
	"backfill-chainlink":      ingest.Run,
	"detect-gaps":             ingest.Run,
	"list-cursors":            ingest.Run,
	"reap-cursors":            ingest.Run,
	"resume-stalled":          ingest.Run,
	"find-data-gaps":          ingest.Run,
	"census-backfill":         ingest.Run,
	"tag-routed-via":          ingest.Run,
	"tag-signer":              ingest.Run,
	"tag-tx-index":            ingest.Run,
	"seed-soroswap-pairs":     ingest.Run,
	"seed-protocol-contracts": ingest.Run,
	"seed-entry-counts":       ingest.Run,
	"projector-replay":        ingest.Run,
	"scan-soroban-events":     ingest.Run,
	"state-snapshot":          ingest.Run,
	"issuer-enrich":           ingest.Run,
	"sep1-refresh":            ingest.Run,
	"issuer-flags":            ingest.Run,
	"directory-sync":          ingest.Run,
	"directory-override":      ingest.Run,
	"listing-sync":            ingest.Run,
	"curated-rwa-sync":        ingest.Run,
	"asset-registry-backfill": ingest.Run,

	"verify-archive":            archive.Run,
	"archive-completeness":      archive.Run,
	"cross-region-check":        archive.Run,
	"cross-region-monitor":      archive.Run,
	"trim-galexie-archive":      archive.Run,
	"rehydrate-galexie-archive": archive.Run,
	"galexie-mirror-verify":     archive.Run,
	"wasm-history":              archive.Run,
	"wasm-history-merge-jsonl":  archive.Run,
	"extract-wasm-from-galexie": archive.Run,
	"compare-entry-changes":     archive.Run,

	"discovery": discovery.Run,

	"supply": supply.Run,

	"ch-backfill":                  chops.Run,
	"ch-gate":                      chops.Run,
	"ch-reproject":                 chops.Run,
	"ch-rebuild":                   chops.Run,
	"trades-cagg-refresh":          chops.Run,
	"ch-supply":                    chops.Run,
	"ch-txindex-backfill":          chops.Run,
	"ch-contract-ledgers-backfill": chops.Run,
	"ch-instance-backfill":         chops.Run,
	"ch-census-rollup":             chops.Run,
	"ch-cap67-movements":           chops.Run,
	"ch-entry-history":             chops.Run,
	"ch-holders-rollup":            chops.Run,
	"ch-creators-rollup":           chops.Run,
	"ch-sponsors-rollup":           chops.Run,
	"ch-cohort-rollup":             chops.Run,
	"ch-participant-backfill":      chops.Run,
	"ch-recognition":               chops.Run,
	"verify-recognition":           chops.Run,
	"verify-reconciliation":        chops.Run,
	"compute-completeness":         chops.Run,
	"verify-served-values":         chops.Run,
	"verify-usd-volume":            chops.Run,
	"usd-volume-restamp":           chops.Run,

	"classic-movements-backfill": chops.Run,
	"projected-rebuild":          chops.Run,
	"reconcile-balances":         chops.Run,
	"verify-contiguity":          chops.Run,
	"verify-hashchain":           chops.Run,
	"verify-lake":                chops.Run,
	"verify-network-state":       chops.Run,
	"wasm-drift":                 chops.Run,
}

// leaf adapts a flags-only handler to the dispatch table's full-argv
// calling convention by stripping the verb. See the [subcommands] doc for
// why this exists: without it a leaf's fs.Parse stops on the verb and no
// flag is ever read.
func leaf(fn func(args []string) error) func(args []string) error {
	return func(args []string) error {
		if len(args) == 0 {
			return fn(nil)
		}
		return fn(args[1:])
	}
}

func realMain() int {
	// Wrap fd 2 with a line-filter BEFORE any aws-sdk-go-v2 code
	// captures os.Stderr. Drops the per-S3-GET "Response has no
	// supported checksum" WARN that floods journald during
	// verify-archive's 12-way parallel walk (~22k WARN/30s on
	// r1, ballooning logs to 1.65 GB). An env-var approach is a no-op
	// because go-stellar-sdk's datastore/s3.go:161 hardcodes
	// ChecksumMode: Enabled per request. Fail-soft.
	//
	// flush MUST be deferred so realMain's return paths drain
	// the pipe before main() calls os.Exit with the int.
	flush := pipeline.SilenceSDKChecksumWarnings()
	defer flush()

	args := os.Args[1:]
	if len(args) == 0 {
		printUsage()
		return 2
	}

	switch args[0] {
	case "version", "--version", "-v", "-version":
		fmt.Println(version.String())
		return 0
	case "help", "--help", "-h", "-help":
		printUsage()
		return 0
	}

	run, ok := subcommands[args[0]]
	if !ok {
		fmt.Fprintf(os.Stderr, "stellarindex-ops: unknown subcommand %q\n", args[0])
		printUsage()
		return 2
	}
	if code := installNetworkFromArgs(args[1:], os.Stderr); code != 0 {
		return code
	}
	return dispatchExitCode(args[0], run(args), os.Stderr)
}

// dispatchExitCode turns a subcommand handler's returned error into the
// process exit code, writing the "<subcommand>: <err>" prefix line to
// stderr where one is warranted. Split out of realMain so the error
// classification (ExitCodeError / flag.ErrHelp / ErrExitSilently / plain
// error) is testable without going through os.Args and the fd-2 filter
// realMain installs.
func dispatchExitCode(name string, err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	var ec *opsutil.ExitCodeError
	if errors.As(err, &ec) {
		if ec.Err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", name, ec.Err)
		}
		return ec.Code
	}
	// Every subcommand's flag.FlagSet uses flag.ContinueOnError (see
	// opsutil.NewMutatingFlagSet), so `-h`/`-help` on a subcommand already
	// printed that subcommand's usage via fs.Parse and returns
	// flag.ErrHelp as the error — it is not a failure. Without this case
	// it fell through to the generic branch below and printed
	// "<subcommand>: flag: help requested" then exited 1, telling a
	// scripted caller that asked for help that it failed.
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if !errors.Is(err, opsutil.ErrExitSilently) {
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
	}
	return 1
}

// installNetworkFromArgs installs the configured network (passphrase and
// alias registry) once, before any subcommand runs, from the -config flag
// every config-reading subcommand shares. A subcommand without -config, or
// one whose config does not load, is left on the pubnet default; the
// subcommand reports its own config error. A config that loads but whose
// network cannot be installed fails closed (non-zero), like the indexer.
func installNetworkFromArgs(args []string, stderr io.Writer) int {
	path := configPathFromArgs(args)
	if path == "" {
		return 0
	}
	cfg, err := config.LoadWithEnv(path)
	if err != nil {
		return 0
	}
	if err := canonical.InstallNetwork(cfg.Stellar.Passphrase(), cfg.Supply.SACWrappers); err != nil {
		fmt.Fprintf(stderr, "stellarindex-ops: install network: %v\n", err)
		return 1
	}
	return 0
}

// configPathFromArgs returns the -config/--config value with flag-package
// semantics: last occurrence wins, scanning stops at the first positional.
// The token after a bare flag is taken as its value (the flag set is not known
// here), so `-source x -config y` still finds the config.
func configPathFromArgs(args []string) string {
	path := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || !strings.HasPrefix(a, "-") || a == "-" {
			break
		}
		name := strings.TrimLeft(a, "-")
		if name != "config" && !strings.Contains(name, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
			continue
		}
		if v, ok := strings.CutPrefix(name, "config="); ok {
			path = v
		} else if name == "config" && i+1 < len(args) {
			i++
			path = args[i]
		}
	}
	return path
}
