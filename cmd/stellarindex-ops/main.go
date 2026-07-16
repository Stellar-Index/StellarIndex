// Binary stellarindex-ops is the admin CLI for operational tasks
// that don't belong in the long-running binaries. Subcommand
// implementations live in internal/ops/{ingest,archive,discovery,
// supply,diagnostics,chops} (one package per rough bucket; chops
// covers the ADR-0033/ADR-0034 ClickHouse-lake tools — named chops,
// not clickhouse, to avoid shadowing internal/storage/clickhouse in
// every file there) plus internal/ops/opsutil (helpers shared across
// more than one of those packages). main.go is only the dispatch
// table + the handful of subcommands too small or too miscellaneous
// to warrant their own package (docs-config, mint-key, upgrade-key,
// emit-incident).
//
//   - Ingest / backfill (internal/ops/ingest): `backfill`,
//     `backfill-external`, `backfill-chainlink`, `backfill-router`,
//     `detect-gaps`, `list-cursors`, `resume-stalled`,
//     `find-data-gaps`, `census-backfill`, `tag-routed-via`,
//     `seed-soroswap-pairs`, `seed-protocol-contracts`,
//     `seed-entry-counts`, `projector-replay`, `scan-soroban-events`,
//     `state-snapshot`, `issuer-enrich`, `sep1-refresh`.
//   - Archive integrity + WASM tracking (internal/ops/archive):
//     `verify-archive`, `archive-completeness`, `cross-region-check`,
//     `cross-region-monitor`, `trim-galexie-archive`,
//     `rehydrate-galexie-archive`, `wasm-history`,
//     `wasm-history-merge-jsonl`, `extract-wasm-from-galexie`.
//   - Soroban discovery (internal/ops/discovery): `discovery`.
//   - Supply (internal/ops/supply): `supply`.
//   - Diagnostics (internal/ops/diagnostics): `rpc-probe`,
//     `verify-decoders`, `verify-external`, `hubble-check`,
//     `hubble-soroban-events`.
//   - ClickHouse lake (internal/ops/chops): `ch-backfill`, `ch-gate`,
//     `ch-reproject`, `ch-rebuild`, `ch-supply`,
//     `ch-txindex-backfill`, `ch-participant-backfill`,
//     `ch-recognition`, `verify-recognition`, `verify-reconciliation`,
//     `compute-completeness`, `verify-served-values`,
//     `sdex-claim-audit`, `classic-movements-backfill`,
//     `projected-rebuild`, `reconcile-balances`, `verify-contiguity`,
//     `verify-hashchain`.
//   - Doc generation: `docs-config` (regenerates the config
//     reference from struct tags; called by `make docs-config`).
//
// This split (maintainability audit 2026-07-01, D1 finding M1-5) is
// mechanical, not a behavior change: every subcommand keeps its exact
// name, flags, defaults, and exit codes — only the Go package holding
// its implementation moved. The canonical subcommand list is the
// `subcommands` map below + the `stellarindex-ops --help` output.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/archive"
	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	"github.com/Stellar-Index/StellarIndex/internal/ops/diagnostics"
	"github.com/Stellar-Index/StellarIndex/internal/ops/discovery"
	"github.com/Stellar-Index/StellarIndex/internal/ops/ingest"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/ops/supply"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/version"
)

// main is a thin shim over realMain so deferred functions (notably
// the SilenceSDKChecksumWarnings flush) execute on every exit
// path. os.Exit skips defers — see SilenceSDKChecksumWarnings
// docstring for the rc.77 regression where short-lived subcommands
// (`backfill -dry-run`, `backfill` with an error) printed only
// their first line then ate the rest because the consumer goroutine
// behind fd 2's filter was killed mid-buffer.
func main() {
	os.Exit(realMain())
}

// subcommands maps each subcommand name to its handler package's Run
// function (or, for the handful of subcommands too small to warrant
// their own package, a leaf closure right here). Handlers receive the
// FULL argv starting at the subcommand name itself (args[0] == the
// map key that reached it, args[1:] its own flags) — each internal/ops/*
// package's Run does its own args[0] dispatch, which is what lets
// several map keys below point at the same package.Run reference.
// Handlers return an error to exit 1; realMain prints the "name: err"
// prefix uniformly. Handlers that have already printed a specific
// message return opsutil.ErrExitSilently to suppress the prefix. The
// canonical subcommand list is this table + the usageBody help text.
//
// Subcommands the usageBody flags as still-planned (cache-prime,
// verify-invariants) land via their feature PRs and add their own
// entry here.
var subcommands = map[string]func(args []string) error{
	"docs-config":   func([]string) error { return config.EmitMarkdown(os.Stdout) },
	"mint-key":      mintKey,
	"upgrade-key":   upgradeKey,
	"emit-incident": emitIncident,

	"rpc-probe":             diagnostics.Run,
	"verify-decoders":       diagnostics.Run,
	"verify-external":       diagnostics.Run,
	"hubble-check":          diagnostics.Run,
	"hubble-soroban-events": diagnostics.Run,

	"backfill":                ingest.Run,
	"backfill-external":       ingest.Run,
	"backfill-chainlink":      ingest.Run,
	"backfill-router":         ingest.Run,
	"detect-gaps":             ingest.Run,
	"list-cursors":            ingest.Run,
	"resume-stalled":          ingest.Run,
	"find-data-gaps":          ingest.Run,
	"census-backfill":         ingest.Run,
	"tag-routed-via":          ingest.Run,
	"seed-soroswap-pairs":     ingest.Run,
	"seed-protocol-contracts": ingest.Run,
	"seed-entry-counts":       ingest.Run,
	"projector-replay":        ingest.Run,
	"scan-soroban-events":     ingest.Run,
	"state-snapshot":          ingest.Run,
	"issuer-enrich":           ingest.Run,
	"sep1-refresh":            ingest.Run,

	"verify-archive":            archive.Run,
	"archive-completeness":      archive.Run,
	"cross-region-check":        archive.Run,
	"cross-region-monitor":      archive.Run,
	"trim-galexie-archive":      archive.Run,
	"rehydrate-galexie-archive": archive.Run,
	"wasm-history":              archive.Run,
	"wasm-history-merge-jsonl":  archive.Run,
	"extract-wasm-from-galexie": archive.Run,

	"discovery": discovery.Run,

	"supply": supply.Run,

	"ch-backfill":             chops.Run,
	"ch-gate":                 chops.Run,
	"ch-reproject":            chops.Run,
	"ch-rebuild":              chops.Run,
	"ch-supply":               chops.Run,
	"ch-txindex-backfill":     chops.Run,
	"ch-participant-backfill": chops.Run,
	"ch-recognition":          chops.Run,
	"verify-recognition":      chops.Run,
	"verify-reconciliation":   chops.Run,
	"compute-completeness":    chops.Run,
	"verify-served-values":    chops.Run,
	"sdex-claim-audit":        chops.Run,

	"classic-movements-backfill": chops.Run,
	"projected-rebuild":          chops.Run,
	"reconcile-balances":         chops.Run,
	"verify-contiguity":          chops.Run,
	"verify-hashchain":           chops.Run,
}

func realMain() int {
	// Wrap fd 2 with a line-filter BEFORE any aws-sdk-go-v2 code
	// captures os.Stderr. Drops the per-S3-GET "Response has no
	// supported checksum" WARN that floods journald during
	// verify-archive's 12-way parallel walk (~22k WARN/30s on
	// r1, ballooning logs to 1.65 GB). The rc.72 env-var
	// approach (QuietS3ChecksumWarnings) was a no-op because
	// go-stellar-sdk's datastore/s3.go:161 hardcodes
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
	if err := run(args); err != nil {
		var ec *opsutil.ExitCodeError
		if errors.As(err, &ec) {
			if ec.Err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", args[0], ec.Err)
			}
			return ec.Code
		}
		if !errors.Is(err, opsutil.ErrExitSilently) {
			fmt.Fprintf(os.Stderr, "%s: %v\n", args[0], err)
		}
		return 1
	}
	return 0
}

// usageBody is the static portion of `stellarindex-ops -h`. The header
// (with version) is prepended at print time so the binary's build
// version shows in the output. Kept as a package-level const so the
// printUsage func itself stays short — funlen lint counts the
// multi-line string literal against the function it's defined in.
const usageBody = `
Usage:
  stellarindex-ops <subcommand>

Subcommands:
  docs-config             Emit the generated config reference to stdout.
  rpc-probe [endpoint]    Diagnostic probe against a stellar-rpc endpoint.
                          Default: http://127.0.0.1:8000.
  list-cursors -config PATH
                          Print every source's last-indexed ledger + age.
  detect-gaps -config PATH [-threshold N]
                          Report sources lagging more than N ledgers (default 100)
                          behind the stellar-rpc network tip. Exit code 1 if any
                          source is lagging.
  verify-decoders -config PATH -from N -to N
                          Stream a bounded ledger range from Galexie through
                          every registered decoder and print a per-source tally
                          (events matched, outputs emitted, first sample). No
                          DB writes; dispatcher runs in a dry harness. Useful
                          as an end-to-end smoke test after a decoder change
                          and for proving each venue emits on the wire.
  scan-soroban-events -config PATH -from N -to N [-topic0 STR] [-contract CID] [-limit N] [-bucket NAME]
                          In-infra analogue of hubble-soroban-events (no
                          BigQuery): stream a galexie ledger range and dump
                          EVERY Soroban contract event as JSON
                          (contract_id, decoded topic[], body map keys +
                          value types), optionally filtered to topic[0]==STR
                          and/or a single contract. Decodes by reusing the
                          dispatcher's event extraction — ground-truth for
                          "what does protocol X actually emit on-chain"
                          (e.g. discovering real contract addresses + event
                          schemas before writing/auditing a decoder). No DB
                          writes. -bucket defaults to s3_bucket_archive then
                          s3_bucket_live; -limit caps matches (default 50).
  verify-external -config PATH [-timeout DUR]
                          Start every enabled off-chain connector
                          (cfg.External.<venue>.enabled = true), drain the
                          shared sink for up to -timeout (default 60s), and
                          print per-venue first-trade/update samples. Exits
                          early once every enabled venue has emitted at
                          least one output. No DB, no Timescale, no cursors.
  verify-archive -config PATH [-bucket NAME] [-from N] [-to N] [-tier MODE] [-archive-root PATH] [-peers URLs] [-peer-samples N] [-archivist-bin BIN] [-archivist-url URL] [-archivist-timeout DUR] [-fail-on-missed] [-max-runtime DUR] [-workers N] [-resume-from-hash HEX] [-metrics-listen ADDR] [-state-file PATH] [-from-last-verified] [-safety-overlap N]
                          Verify a galexie bucket at one or more tiers:
                            chain      (Tier A) — chain-link hash integrity:
                                       each ledger N's PreviousLedgerHash
                                       equals ledger N-1's Hash. Default.
                            checkpoint (Tier B) — cross-check our LCM's
                                       hash at every 64-ledger checkpoint
                                       against the canonical header-hash
                                       from the local history-archive
                                       (-archive-root, default
                                       /srv/history-archive).
                            peers      (Tier D) — sample checkpoints
                                       within the range and cross-compare
                                       history-XXXXXXXX.json across N
                                       tier-1 validator archives (-peers
                                       URL list or default set of 7).
                                       Consensus-level cryptographic
                                       agreement.
                            archivist  (Tier E) — shell out to
                                       stellar-archivist scan for a full
                                       bucket-by-bucket sha256 audit of
                                       the archive. -archivist-url
                                       defaults to file://<archive-root>;
                                       any peer's https:// archive URL
                                       also works. Requires
                                       stellar-archivist (or rs-stellar-
                                       archivist via -archivist-bin) on
                                       PATH; long-running, gated by
                                       -archivist-timeout (default 30m).
                            all        run all four.
                          -fail-on-missed: per ADR-0017 X1.7, treat
                                       checkpointsMissed > 0 as a hard
                                       failure. Default off for the
                                       pre-bootstrap workflow; flip on
                                       after archive-completeness has
                                       been run and the cross-anchor
                                       archive is provably complete.
                          Exit 0 = clean; 1 = first break with details.
  archive-completeness <mode> [flags]
                          Completeness check + repair across the dual-archive
                          stack per ADR-0017. Modes:
                            check      Read-only enumeration. Walks expected
                                       checkpoint positions in the cross-anchor
                                       archive and writes a JSON gap report.
                                       Flags: -archive-root PATH, -from N, -to N,
                                              -output-file PATH (default stdout).
                            fix        Run check, then fetch every missing
                                       checkpoint via the multi-source fallback
                                       chain (SDF core_live_001/002/003 +
                                       tier-1 validators) and place each file
                                       atomically. Re-checks after the fill so
                                       the emitted report reflects post-fix
                                       state. Flags: -archive-root, -from, -to,
                                              -workers N, -owner-user STR,
                                              -owner-group STR, -output-file.
                            verify     Daily-cron mode. Runs check → fix →
                                       re-check, then emits a Prometheus
                                       textfile for node_exporter to scrape.
                                       Flags: same as fix, plus -textfile-output
                                       PATH (target node_exporter's
                                       textfile_collector dir, e.g.
                                       /var/lib/node_exporter/textfile_collector/
                                       archive_completeness.prom).
                          Current implementation scope: cross-anchor archive
                          only. Primary MinIO-bucket structural/chain-link
                          enforcement is not shipped in this snapshot.
                          Exit 0 = clean; 1 = at least one missing file remains.
  cross-region-check -regions name=URL,name=URL,... [-pairs PAIR,...] [-metric vwap|twap|ohlc] [-window DUR] [-samples N] [-to TS]
                          Hit each region's /v1/{vwap|twap|ohlc} endpoint
                          for the same closed-bucket window and assert
                          equality across the stable user-visible payload
                          (data, sources, and flags, excluding
                          per-response as_of). Per
                          ADR-0015 the response should be byte-identical
                          across regions once trades have replicated;
                          divergence here flags one of: replication lag,
                          decoder version drift, upstream divergence,
                          or postgres replication broken. Designed for
                          periodic execution from a monitoring host.
                          Exit 0 = clean; 1 = divergence with diff.
                          Example:
                            stellarindex-ops cross-region-check \
                              -regions r1=https://r1.api.example.net,r2=https://r2.api.example.net \
                              -pairs native/fiat:USD,crypto:BTC/fiat:USD \
                              -metric vwap -samples 5
  cross-region-monitor -regions name=URL,name=URL,... [-pairs PAIR,...] [-metric vwap|twap|ohlc] [-window DUR] [-samples N] [-interval DUR] [-listen :PORT]
                          Long-running daemon variant of cross-region-check.
                          Runs the same per-bucket comparison on a fixed
                          interval and exposes the outcome as Prometheus
                          metrics on -listen (default :9479). Designed
                          to live as a sidecar systemd service on the
                          observability host. Metrics:
                            stellarindex_cross_region_checks_total{outcome=ok|divergence|error}
                            stellarindex_cross_region_divergences_total
                            stellarindex_cross_region_fetch_errors_total{region}
                            stellarindex_cross_region_last_run_timestamp_seconds
                          /healthz returns 503 until the first sweep
                          completes; 200 thereafter. Example:
                            stellarindex-ops cross-region-monitor \
                              -regions r1=...,r2=...,r3=... \
                              -interval 60s -listen :9479
  supply audit <asset> -config PATH [-cross-check <other-asset>] [-history-hours N]
                          Operator-side audit for ADR-0011 supply
                          derivation. Prints the latest snapshot
                          from asset_supply_history (total /
                          circulating / max / basis / observed_at /
                          ledger). When -cross-check is supplied,
                          fetches the counterpart's snapshot and
                          runs the SAC-wrapped cross-check from
                          PR #216 (asserts the two totals agree
                          within 1 stroop per ADR-0011). When
                          -history-hours is set, also prints the
                          recent N-hour snapshot trail so an
                          operator can spot whether divergence is
                          fresh or chronic. Asset accepts the
                          canonical wire form (native | CODE-G… |
                          C…). Cross-check pairing is operator-
                          supplied because SAC contract-id
                          derivation isn't wired in canonical yet.
  supply snapshot -config PATH [-asset <id>] [-ledger N] [-dry-run]
                          Compute a fresh supply snapshot and write
                          it to asset_supply_history. The CLI is
                          intentionally native-XLM only — classic
                          (Algorithm 2) + SEP-41 (Algorithm 3)
                          computers shipped (Tasks #55, #56) but
                          their refresh surface is the aggregator
                          goroutine path ([supply].aggregator_refresh_enabled).
                          Reserve balances come from the chained-
                          fallback reader: live LCM AccountEntry
                          observer (L2.12a) wins when populated;
                          operator-static [supply].reserve_balances_stroops
                          is the bring-up fallback. Default ledger
                          attribution is the max last_ledger across
                          all ingestion cursors; pass -ledger to
                          override. -dry-run prints without writing.
  supply seed-observations -config PATH [-ch-addr ADDR] [-dry-run]
                          Seed account_observations from the ClickHouse
                          lake for every [supply].sdf_reserve_accounts
                          entry (ADR-0021). Closes the dormant-account
                          bootstrap gap: an account that never changes
                          after the live observer starts would otherwise
                          keep the reserve reader on the static fallback
                          forever. Idempotent; the live observer
                          supersedes seeded rows on the next real change.
  supply seed-sep41-genesis -config PATH [-ch-addr ADDR] [-genesis-ledger N] [-dry-run]
                          Seed each [supply].watched_sep41_contracts
                          contract's pre-Soroban (ledger < 50457424)
                          per-kind opening balance into sep41_supply_rollup
                          from the ClickHouse supply_flows lake (migration
                          0088, incident 2026-07-06). Fixes SAC-wrappers
                          issued before Soroban reading a negative
                          Soroban-era-only total. Idempotent (baseline is
                          SET, not added); Soroban-only contracts seed a
                          zero baseline (served total unchanged).
  supply verify-rollup -config PATH [-contracts C1,C2,...] [-tolerance N] [-statement-timeout DUR] [-timeout DUR]
                          Derived-checkpoint reconcile (ADR-0033 fourth
                          integrity check): diff every watched contract's
                          sep41_supply_rollup fold (the served incremental
                          checkpoint) against the AUTHORITATIVE same-source
                          re-sum of the exact sep41_supply_events rows it
                          folds (ledger <= last_ledger). Reports any
                          (contract, kind) that diverge by more than
                          -tolerance (Delta > 0 = double-fold over-count,
                          the KALE 2× signature; Delta < 0 = a
                          below-checkpoint edit the worker never re-summed).
                          Exit 1 if any drift. SLOW / post-re-derive check,
                          NOT a per-tick job — each re-sum is the
                          full-history aggregate the served fast path
                          avoids (the incident's 30s probe timed out at 6
                          contracts), so it runs under -statement-timeout
                          (default 15m per contract), is scoped/resumable
                          via -contracts, and on r1 must run under the
                          heavy-job wrapper (run-heavy-job.sh).
  discovery list -config PATH [-since DUR] [-limit N]
                          List SEP-41 contracts auto-detected from the
                          event stream (the dispatcher's discovery
                          hook from #225 + the indexer wire-up from
                          #230 populate discovered_assets in
                          production). Output is one row per
                          contract: contract_id, first_seen_at,
                          first_seen_event, event_count. Ordered by
                          first_seen_at DESC so the most-recent
                          arrivals show up first.
                            -since 1h    only contracts first seen
                                         within the last 1h; default
                                         empty (no filter, full table)
                            -limit 100   cap result rows; default 100
  wasm-history -config PATH -contracts ID,ID,... [-from N] [-to N] [-bucket NAME]
                          Walk a galexie bucket and emit a per-contract
                          WASM-version timeline. For each watched contract,
                          tracks every change to its instance's executable
                          hash and reports the active ledger range per hash.
                          Read-only audit; no DB writes. Output is JSON to
                          stdout. Defaults to S3BucketArchive (the historical
                          bucket) — pass -bucket to override.
                          Example:
                            stellarindex-ops wasm-history \
                              -config /etc/stellarindex.toml \
                              -from 21000000 -to 25000000 \
                              -contracts CDLZ...,CARFAC... \
                              -checkpoint-dir /tmp/walk-checkpoint \
                              > soroswap-wasm-history.json
                          When -checkpoint-dir is set, each parallel
                          worker also writes its observed transitions
                          to <dir>/wasm-history-w<i>.jsonl. Recover the
                          canonical JSON from a crashed run with
                          wasm-history-merge-jsonl below.
  wasm-history-merge-jsonl -checkpoint-dir DIR -to N [-output PATH]
                          Reconstruct the canonical wasm-history JSON
                          from per-worker JSONL transition logs left
                          behind by a crashed wasm-history run with
                          -checkpoint-dir set. -to MUST match the
                          original walk's upper bound (closes the last
                          open range per contract). Output is the same
                          JSON shape wasm-history writes at end-of-run.
                          Empty-history contracts (the "ran but saw no
                          transitions" signal) are NOT emitted — the
                          JSONL only carries observed transitions.
                          Example:
                            stellarindex-ops wasm-history-merge-jsonl \
                              -checkpoint-dir /tmp/walk-checkpoint \
                              -to 62249727 \
                              -output recovered.json
  extract-wasm-from-galexie -config PATH -hashes HEX,HEX,... -output-dir DIR [-from N] [-to N] [-parallel N] [-bucket NAME]
                          Extract raw WASM bytes for one or more contract-
                          code hashes by walking the local galexie LCM
                          archive. Writes <hash>.wasm files into
                          -output-dir. Companion to wasm-history: walk the
                          history first to enumerate hashes, then run this
                          to pull bytes for the (likely-evicted from current
                          ledger state) older versions. r1's full archive is
                          the truer source than RPC getLedgerEntry —
                          works offline, doesn't depend on TTL retention.
                          Example:
                            stellarindex-ops extract-wasm-from-galexie \
                              -config /etc/stellarindex.toml \
                              -from 50457424 -to 62296694 -parallel 8 \
                              -hashes 4a64c8c8...,b400f7a8... \
                              -output-dir /var/wasm-audit
  backfill-external -config PATH -source SRC -pair SYM -from TS -to TS -granularity D
                          Pull historical candles from an external venue
                          (binance / kraken / bitstamp / coinbase) and
                          insert synthesised canonical.Trade rows into
                          the trades hypertable. -dry-run prints stats
                          only, no writes. Example:
                            stellarindex-ops backfill-external \
                              -config configs/prod.toml \
                              -source binance -pair XLMUSDT \
                              -from 2024-01-01T00:00:00Z \
                              -to   2024-12-31T00:00:00Z \
                              -granularity 1h
  backfill-chainlink -config PATH [-from-block N] [-to-block N] [-chunk-blocks N] [-sleep-ms N] [-dry-run]
                          Walk every configured Chainlink feed's
                          AnswerUpdated event log across the requested
                          block range and insert one OracleUpdate row
                          per historical round into oracle_updates.
                          Idempotent (deterministic synthesised
                          tx_hash + ON CONFLICT). ~33k eth_getLogs
                          calls / 7h wall time for all 516 mainnet
                          feeds at 5k blocks/chunk on Alchemy free
                          tier — run overnight. Example:
                            stellarindex-ops backfill-chainlink \
                              -config /etc/stellarindex.toml \
                              -from-block 15537393  # post-Merge marker
                              -sleep-ms 50          # ~20 req/s polite cap
  hubble-check -config PATH -from N -to N -bigquery-project PROJ [-max-mismatches N] [-dry-run-bytes]
                          Cross-check our SDEX trades against SDF's
                          published hubble-public.crypto_stellar.history_trades
                          BigQuery table for the same ledger range.
                          Reports every ledger where the counts disagree.
                          Catches decoder coverage gaps + over-eager
                          decoding on classic SDEX (ManageOffer +
                          classic LP) which Tier A/B/D/E (bytes-level)
                          and cross-region-check (intra-fleet) do not.
                          Soroban DEXes have no decoded Hubble counterpart;
                          covered by the per-WASM decoder audit instead.
                          Off-chain sources (CEX/FX) are out of scope.
                          Auth: Application Default Credentials (run
                          gcloud auth application-default login first).
                          Cost: ~$0.05 per 1M-ledger range at on-demand
                          pricing. Use -dry-run-bytes for a pre-flight
                          estimate. Example:
                            stellarindex-ops hubble-check \
                              -config /etc/stellarindex.toml \
                              -from 21000000 -to 22000000 \
                              -bigquery-project my-gcp-project
  hubble-soroban-events -from N -to N -bigquery-project PROJ -contracts CID,CID [-topic0 SYM] [-topic1 SYM] [-output json|total|csv] [-dry-run-bytes]
                          Per-ledger event-count primitive against
                          hubble-public.crypto_stellar.history_contract_events
                          for the supplied Soroban contract IDs, with
                          optional topic[0]/topic[1] filters. Operators
                          combine this with knowledge of per-source
                          (events ↔ trades) ratios to cross-check
                          decoder coverage on Soroswap / Aquarius /
                          Phoenix / Comet / Reflector / Redstone.
                          See docs/operations/hubble-event-counts.md
                          for the per-source recipe. Auth via
                          Application Default Credentials (same as
                          hubble-check). Cost: 20-40 GB scan per
                          1M-ledger range — use -dry-run-bytes for
                          a preview.
  backfill -config PATH -from N -to N [-source S,S,...] [-bucket NAME] [-dry-run] [-resume]
                          Replay a bounded ledger range through the
                          full ingest pipeline (galexie → dispatcher
                          → decoders → trades hypertable). Same code
                          path as the live indexer, no live tail; CAGGs
                          auto-roll on the inserted rows. Refuses to
                          run any source that isn't BackfillSafe in
                          internal/sources/external/registry.go — for
                          on-chain Soroban sources that means the
                          per-WASM-hash audit (stellarindex-ops
                          wasm-history) must land first per CLAUDE.md
                          "Soroban DeFi contracts upgrade in place".
                          Idempotent: the trades hypertable's unique
                          index on (source, ledger, tx_hash, op_index)
                          makes re-runs over the same range a no-op.
                          Example:
                            stellarindex-ops backfill \
                              -config /etc/stellarindex.toml \
                              -from 21000000 -to 25000000 \
                              -source soroswap,aquarius
  tag-routed-via -config PATH [-from N] [-to N] [-window N] [-resume]
                          Back-tag trades.routed_via='soroswap-router' for
                          every trade sharing (ledger, tx_hash) with a
                          persisted soroswap_router_swaps row (migration
                          0025 Phase B). SQL-only join — no Galexie walk;
                          run AFTER the router record itself is complete
                          (backfill-router / ch-rebuild -contract-calls).
                          Defaults to the full extent of
                          soroswap_router_swaps; windowed by ledger
                          (default 500k) so each UPDATE prunes trades
                          chunks; first-wins (never overwrites an existing
                          tag) so re-runs are no-ops. Checkpoints into
                          ingestion_cursors for resume. The live indexer
                          keeps the trailing 30 min tagged going forward.
  census-backfill -config PATH -from N -to N [-bucket NAME] [-resume]
                          Populate ledger_ingest_log (ADR-0033 substrate
                          record) for a historical range. Pure structural
                          walk — counts contract events + classic trade
                          effects per ledger and records the header
                          hash-chain anchors, no decoders run. The live
                          indexer writes this going forward; this fills
                          history so substrate continuity + hash-chain
                          checks cover [genesis, tip]. Idempotent
                          (ON CONFLICT DO UPDATE); checkpoints for resume.
  ch-backfill -config PATH -from N -to N [-bucket NAME] [-ch-addr H:P] [-flush-every N] [-parallel N]
                          ADR-0034 Phase 2: structurally decode [from,to]
                          from galexie into the ClickHouse stellar.* Tier-1
                          tables (ledgers/txs/ops/op_results/contract_events).
                          Decoder-independent; retains raw XDR. Idempotent
                          (ReplacingMergeTree), so re-running a range is safe.
                          -parallel N runs N concurrent range-walkers (each
                          its own Sink) — the throughput unlock for the full
                          historic backfill.
  ch-gate -config PATH -from N -to N [-bucket NAME] [-ch-addr H:P] [-project-to TIP]
                          ADR-0034 Phase 2 §6 gates over a backfilled range:
                          recompute the census + structural extract from
                          galexie, assert extract==census, then read the
                          range back from ClickHouse and assert STORED and
                          ACTUAL row counts both equal the census. Reports
                          compressed bytes/ledger + full-history projection
                          and walk throughput. Writes nothing; exits non-zero
                          if the completeness gate fails.
  ch-reproject -config PATH -from N -to N [-ch-addr H:P] [-max-list N]
                          ADR-0034 Phase 4 validation: re-derive the range
                          from BOTH the ClickHouse lake and Postgres
                          soroban_events using the SAME decoders, and assert
                          per-kind/per-ledger output counts match exactly —
                          proving decoders read ClickHouse identically. Writes
                          nothing; exits non-zero on any divergence.
  ch-txindex-backfill [-ch-addr H:P] [-from N] [-to N] [-window N]
                          Fill stellar.tx_hash_index (the hash-ordered
                          GET /v1/tx/{hash} lookup table, perf-todo §4)
                          from stellar.transactions history in windowed,
                          resumable INSERT…SELECT chunks (idempotent —
                          ReplacingMergeTree keyed on tx_hash). The
                          tx_hash_index_mv MV covers post-deploy ingest;
                          this covers the history behind it. -to 0 = lake
                          tip. Prints a resume point per window; serialize
                          it and run under the root-<2G watchdog on r1.
  ch-participant-backfill [-ch-addr H:P] [-from N] [-to N] [-window N] [-dry-run]
                          Fill stellar.operation_participants (the non-source
                          side of ADR-0038 Phase B account history) for
                          HISTORICAL ledgers by re-deriving participants from
                          stellar.operations.body_xdr in the ClickHouse lake —
                          NOT a Galexie re-walk (BACKLOG #59). Reuses the live
                          extractor's participant derivation, so the fill is
                          byte-identical to live capture. -to 0 = the
                          live-capture floor − 1 (exactly the gap). Windowed,
                          resumable (idempotent ReplacingMergeTree), prints a
                          resume point per window. -dry-run counts what WOULD
                          be written. Run under run-heavy-job.sh on r1.
  classic-movements-backfill -from N -to N [-window N] [-ch-addr H:P] [-resume] [-write] [-verify]
                          ADR-0047, all four phases, RETARGETED by ADR-0048
                          D2 to ClickHouse: reconstruct pre-P23 classic
                          movements from the lake into
                          stellar.account_movements (feed-shaped, two rows
                          per movement — see that table's DDL header in
                          deploy/clickhouse/tier1_schema.sql for the full
                          row-cardinality table). No Postgres connection —
                          the Postgres classic_movements hypertable
                          (migration 0105) stays applied but UNPOPULATED;
                          see migrations/README.md's 0105 row. Op-only
                          surface (stellar.operations join
                          operation_results): Payment, CreateAccount,
                          PathPaymentStrictReceive/Send,
                          CreateClaimableBalance, ClaimClaimableBalance,
                          ClawbackClaimableBalance, Clawback, AccountMerge.
                          Entry-changes-correlated surface (also needs
                          ledger_entry_changes — see ADR-0047 Phase 0):
                          LiquidityPoolDeposit/Withdraw + the CAP-0038
                          AllowTrust/SetTrustLineFlags trustline-revocation
                          auto-liquidation edge case; ops in this surface
                          report entry-changes-unavailable / are skipped
                          (counted + logged, never guessed) until Phase 0's
                          ch-backfill closes the ledger_entry_changes
                          fidelity gap for this range. ClaimClaimableBalance/
                          ClawbackClaimableBalance correlate against their
                          own CreateClaimableBalance row via an in-run index
                          + a ClickHouse fallback (previously Postgres);
                          unresolved ones are counted + logged, never
                          guessed. HISTORICAL-ONLY — -to is HARD-CLAMPED
                          below the P23 boundary (ledger 58762517,
                          2025-09-03) regardless of what is passed, since
                          every ledger from P23 onward already emits a
                          unified CAP-67 event via sep41_transfers.
                          Windowed (-window, default 500000 ledgers) and
                          resumable (-resume, default true; DATA-DERIVED —
                          queries the highest ledger already in
                          stellar.account_movements for [-from,-to] and
                          re-processes it, no ingestion_cursors row
                          involved, per ADR-0048's "no Postgres in the
                          loop"). -verify recounts each window from
                          ClickHouse and compares against this run's
                          decode-time counts (cheap reconciliation, not
                          full ADR-0033 machinery). Idempotent
                          (ReplacingMergeTree) — safe to re-run over an
                          already-written range (re-running after Phase 0
                          lands resolves anything reported
                          entry-changes-unavailable on a prior pass).
                          Defaults to DRY-RUN (count only); pass -write to
                          persist. Run under run-heavy-job.sh on r1. Example:
                            stellarindex-ops classic-movements-backfill \
                              -ch-addr 127.0.0.1:9300 \
                              -from 2 -to 58762516 \
                              -write -verify
  projected-rebuild -config PATH -source NAME -from N [-to N] [-workers K] [-window N] [-resume] [-write] [-ch-addr H:P] [-allow-live-overlap]
                          ADR-0048 D3: bulk catch-up for a projected
                          (Soroban-derived) source, replacing
                          projector-replay for any rewind beyond
                          roughly 1M ledgers. projector-replay rewinds
                          the LIVE projector's cursor and is bound by
                          its tick cadence + 60s per-cycle deadline
                          (~720k ledgers/hour ceiling); this tool runs
                          -workers (default 4, soft-capped 8) parallel
                          ledger-window goroutines with NO per-cycle
                          deadline, each streaming the ClickHouse lake
                          through the SAME decoder + sink
                          (pipeline.HandleEvent) the live projector
                          uses, at roughly 10-20x the projector-replay
                          rate. -to defaults to the live projector
                          cursor's current position (fill exactly the
                          history behind the live tail). One-writer
                          contract: REFUSES to run if the live
                          projector's cursor for -source is still
                          inside [-from,-to] (would mean two writers
                          racing the same range) unless
                          -allow-live-overlap is passed; never touches
                          the live projector's own cursor. Windowed
                          (-window, default 50000 ledgers — controls
                          checkpoint/scheduling granularity only; this
                          tool streams, never buffers a window) and
                          resumable (-resume, default true;
                          checkpoints into ingestion_cursors as
                          source="projected-rebuild",
                          sub_source="<name>:<wlo>-<whi>" per window).
                          Idempotent (every per-source table's ON
                          CONFLICT DO NOTHING) — safe to re-run or to
                          overlap with a retried window. Defaults to
                          DRY-RUN (count + report only); pass -write to
                          persist. Prints per-topic emitted counts on
                          completion for an operator eyeball-check
                          against the census tables — the ADR-0033
                          compute-completeness verdict remains
                          authoritative. Run under run-heavy-job.sh on
                          r1. Example:
                            stellarindex-ops projected-rebuild \
                              -config /etc/stellarindex.toml \
                              -source blend_backstop -from 51499546 \
                              -workers 6 -write
  reconcile-balances (-account G... | -sample N) [-ch-addr H:P] [-horizon URL] [-tolerance-stroops N] [-min-recent-ledger N] [-sleep-ms N] [-timeout DUR]
                          ADR-0033-style verification harness for the
                          "verified explorer" claim: proves
                          stellar.ledger_entry_changes reflects TRUE
                          on-chain state by comparing our latest
                          recorded NATIVE (XLM) balance for an account
                          against an independent external source
                          (public Horizon — a one-off, read-only
                          verifier is exactly the case ADR-0001's
                          Horizon ban doesn't cover; see the source
                          file's doc comment). Requires exactly one of
                          -account (reconcile one) or -sample N
                          (reconcile N accounts sampled from accounts
                          with a ledger_entry_changes row above
                          -min-recent-ledger, default 60000000, ordered
                          by a deterministic cityHash64 pseudo-shuffle
                          so their latest snapshot approximates current
                          chain state). Per account: reads our balance
                          via argMax(balance, ledger_seq) (zero rows =
                          NO_DATA, outside our coverage, not a
                          mismatch), fetches Horizon's current native
                          balance (404 = MERGED_OR_ABSENT — the account
                          existed in our data but is gone now, reported
                          not hard-failed), and classifies
                          MATCH/MISMATCH by
                          |our_stroops - ref_stroops| <= -tolerance-stroops
                          (default 0). Horizon's decimal XLM string is
                          parsed exactly via math/big (no float64
                          anywhere in the path, per ADR-0003). Polite
                          by default: serial requests with -sleep-ms
                          (default 250) between them and a bounded
                          429 backoff/retry. Prints a full MATCH/
                          MISMATCH/NO_DATA/MERGED_OR_ABSENT report plus
                          a one-line summary. Exit code = number of
                          MISMATCHes (capped at 255), mirroring
                          scripts/dev/r1-smoke.sh's convention so cron/
                          Healthchecks.io can consume it directly.
                          Example:
                            stellarindex-ops reconcile-balances \
                              -sample 50 -ch-addr 127.0.0.1:9300 \
                              -horizon https://horizon.stellar.org
  verify-contiguity [-config PATH] [-ch-addr H:P] [-from N] [-to N] [-ec-floor N] [-check ledgers|entrychanges|all]
                          Standing ADR-0034 data-verification tool for the
                          ClickHouse raw lake. Two checks: (1) ledger
                          substrate contiguity — every ledger_seq in
                          [-from,-to] (default 2..CH max) must be present in
                          stellar.ledgers exactly once; starts with a cheap
                          whole-range uniqExact() headline, and only pays for
                          a windowed (1M-ledger bucket) scan + a bounded
                          per-bucket gap-range localization pass (capped at
                          200 ranges total) when a deficit is found. (2)
                          stellar.ledger_entry_changes coverage vs.
                          tx-bearing ledgers, split at -ec-floor (default
                          63050000, the known live-ingest floor — coverage is
                          100% from there to tip and partial below it,
                          backfill in progress): missing coverage AT/ABOVE
                          the floor is a DEFICIENCY (counts toward the exit
                          code); missing BELOW the floor is reported as
                          BACKFILL-PENDING, informational only. -check
                          restricts to one check (ledgers|entrychanges),
                          default all. Read-only; touches ClickHouse only,
                          never Postgres. Exit code = ledger gaps +
                          entry-change deficiencies at/above -ec-floor
                          (capped at 255), mirroring reconcile-balances'
                          convention so cron/Healthchecks.io can consume it
                          directly. Example:
                            stellarindex-ops verify-contiguity \
                              -ch-addr 127.0.0.1:9300 -ec-floor 63050000
  verify-hashchain [-config PATH] [-ch-addr H:P] [-from N] [-to N]
                          Standing ADR-0034 data-verification tool proving the
                          ClickHouse raw lake's ledger substrate is
                          HASH-CHAINED, not just present — the second half of
                          the "ledgers contiguous AND hash-chained to genesis"
                          claim (verify-contiguity proves the first half). For
                          every ledger n in [-from,-to] (default 2..CH max),
                          asserts prev_hash == ledger n-1's ledger_hash. Two
                          checks, both windowed at the same 1M-ledger buckets
                          as verify-contiguity: (1) in-window links, via a
                          lagInFrame(ledger_hash) window function ordered by
                          ledger_seq, one cheap count-shaped query per window;
                          (2) boundary links, a 2-row point lookup at every
                          window seam (lagInFrame can't see across a window's
                          own WHERE bounds). A broken or absent predecessor at
                          a seam counts as a broken link either way, tagged
                          distinctly in the report. Per-ledger localization
                          (which exact ledger_seq is broken, capped at 200
                          entries) only runs for windows the headline already
                          flagged nonzero. NOTE: a missing ledger (a
                          contiguity gap) is ALSO reported as a broken link
                          here, because the chain necessarily fails to
                          connect across it — run verify-contiguity first to
                          tell "missing ledger" apart from "present but wrong
                          hash". Read-only; touches ClickHouse only, never
                          Postgres. Exit code = in-window broken links +
                          boundary broken links (capped at 255), mirroring
                          verify-contiguity's convention so cron/
                          Healthchecks.io can consume it directly. Example:
                            stellarindex-ops verify-hashchain \
                              -ch-addr 127.0.0.1:9300 -from 2 -to 60000000
  verify-recognition -config PATH -from N -to N
                          ADR-0033 Claim 2a: pull every distinct
                          (contract, topic[0]) shape from soroban_events
                          in the range and run each through the
                          production decoder chain's Matches(). Lists any
                          shape no decoder handles (a topic a WASM upgrade
                          added that we'd silently drop) and exits non-zero
                          if any exist. Cron/CI-gateable.
  verify-reconciliation -config PATH -from N -to N [-source S] [-max-list N]
                          ADR-0033 Claim 2b: re-derive how many trades
                          each soroban_events range WOULD produce (running
                          the real decoder) and diff per ledger against the
                          trades table. Lists ledgers where projected rows
                          went missing (or phantom rows appeared) and exits
                          non-zero. Covers every per-ledger source —
                          trades (soroswap/aquarius/phoenix/comet), oracles
                          (reflector/redstone), cctp/rozo/defindex, blend's
                          four tables (re-derive bucketed by EventKind), and
                          sdex (LCM census). Seeds soroswap pairs via RPC.
  compute-completeness -config PATH [-to N] [-source S]
                          ADR-0033 Phase 6: compute the per-source
                          completeness WATERMARK (substrate continuity +
                          hash chain ∧ projection reconciliation) and a
                          system recognition verdict, and write them to
                          completeness_snapshots for the API + status page.
                          -to defaults to the live ledgerstream tip. Run on
                          a cron; the headline replaces density/gap_free.
  seed-soroswap-pairs -config PATH [-rpc URL] [-timeout DUR]
                          Bootstrap the soroswap_pairs registry table
                          via stellar-rpc simulateTransaction. Walks the
                          factory's all_pairs() / token_0() / token_1()
                          view functions and upserts each (pair, token0,
                          token1) tuple. Run once on first deployment;
                          live new_pair events keep the table fresh
                          afterwards (see migrations/0016_create_soroswap_pairs.up.sql).
  seed-protocol-contracts -config PATH -source NAME|all [-to LEDGER] [-timeout DUR]
                          Bootstrap the protocol_contracts registry for a
                          factory-anchored gated decoder (ADR-0035): walks
                          the source's factory creation events (e.g. Blend
                          pool-factory deploy) from the lake and upserts
                          every child contract. Deploy precondition before
                          relying on the gate; live creation events keep the
                          table fresh afterwards.
                          ~3N+1 RPC calls at 300ms throttle, so wall-time
                          scales linearly with pair count (~3 min for 200
                          pairs). Idempotent — re-running is safe.
  seed-entry-counts -config PATH [-timeout DUR]
                          Authoritatively recompute source_entry_counts
                          (the "entries" column on /v1/diagnostics/
                          ingestion) from a full GROUP BY over trades +
                          oracle_updates. The writers keep it live going
                          forward; this one-shot folds in pre-counter
                          history + any crash drift. Run ONCE post-
                          backfill (scans every trades chunk). Idempotent
                          — SETs not ADDs, so re-running converges.
  trim-galexie-archive -config PATH -older-than-ledger N [-dry-run|-commit] [-no-verify-upstream] [-max-files N]
                          Per ADR-0027 §Step 2: DESTRUCTIVE — deletes
                          LCM files from the local hot tier
                          (galexie-archive on MinIO) whose ledger range
                          is entirely below -older-than-ledger, after
                          verifying their presence in the cold tier
                          (aws-public-blockchain). Reclaims pool
                          capacity by tiering off historical mirror.
                          Safety stack:
                            * --dry-run is the DEFAULT when neither
                              flag is set; --commit MUST be explicit.
                            * --verify-upstream is the DEFAULT; every
                              candidate is HEAD'd against cold before
                              deletion. --no-verify-upstream skips
                              this and is NOT RECOMMENDED.
                            * --max-files caps deletions per run
                              (default 100000) — a typo cannot delete
                              the full archive in one shot.
                            * --older-than-ledger is REQUIRED — no
                              implicit "trim everything below tip - N".
                            * Cold tier MUST be configured. Refuses
                              to run otherwise.
                          Rollback: stellarindex-ops
                          rehydrate-galexie-archive -from N -to N.
  rehydrate-galexie-archive -config PATH -from N -to N [-dry-run]
                          Per ADR-0027 §Step 2: copy LCM files for the
                          ledger range [-from, -to] from the configured
                          cold tier (storage.s3_cold_*; production is the
                          aws-public-blockchain bucket) back into the
                          local hot tier (storage.s3_bucket_archive on
                          MinIO). Idempotent — uses PutFileIfNotExists,
                          so files already present in hot are skipped.
                          -dry-run reports the file list + skipped vs
                          would-copy counts without writing.
                          Use cases:
                            * recover from accidental trim;
                            * pre-warm hot before a planned backfill;
                            * cold-tier integrity spot check (surfaces
                              files missing in cold via the
                              missing_in_cold counter).
                          Refuses to run if cold tier is not configured
                          (cfg.Storage.ColdTieringEnabled() == false).
  mint-key -config PATH -identifier ID -label LABEL [-tier T] [-rate-limit-per-min N] [-expires-in DUR]
                          Issue a fresh API key directly via the
                          Redis API-key store. Operator-only path
                          to bootstrap a customer's first key —
                          /v1/account/keys self-service can't be
                          hit until a pre-existing authenticated
                          subject already exists (chicken + egg).
                          Plaintext goes to stdout; audit metadata
                          (KeyID, Tier, CreatedAt) goes to stderr,
                          so a >key.txt redirect captures only
                          the secret. Plaintext is shown ONCE —
                          unrecoverable. Pipe stdout to an
                          encrypted transport (Bitwarden, vault,
                          encrypted email) immediately. Tiers:
                          apikey | sep10 | operator. Stripe webhook
                          integration (future) calls the same
                          internal/auth.RedisAPIKeyStore.Create
                          path from a small HTTP handler instead
                          of from the CLI.
                          Example:
                            stellarindex-ops mint-key \
                              -config /etc/stellarindex.toml \
                              -identifier customer-acme-corp \
                              -label 'ACME Corp - production' \
                              -tier apikey \
                              -rate-limit-per-min 1000
  upgrade-key -config PATH -key-id KID -rate-limit-per-min N
                          Lift (or lower) an existing API key's
                          per-minute rate-limit budget. Operator-
                          side path for manual paid-tier upgrades
                          before the Stripe webhook ships; same
                          internal/auth.RedisAPIKeyStore.UpdateRateLimit
                          path the future webhook will call.
                          The customer's existing plaintext key
                          keeps working — they don't need to
                          rotate to pick up the new budget;
                          effective on the next request.
                          Tier reference (matches the /signup page):
                              1000 = Starter,  10000 = Pro,
                             50000 = Business, custom = Enterprise.
                          Example:
                            stellarindex-ops upgrade-key \
                              -config /etc/stellarindex.toml \
                              -key-id kid_515c8d94191f4e93 \
                              -rate-limit-per-min 10000
  emit-incident -config PATH -slug SLUG -event {sev1|resolved}
                          Fan out one incident.sev1 or
                          incident.resolved customer webhook for
                          the named incident slug. The slug must
                          already exist in internal/incidents/data/
                          (embedded into this binary at build
                          time). F-1249 (codex audit-2026-05-12):
                          part of the SEV runbook — draft the .md,
                          merge + deploy, then emit. See
                          docs/operations/sev-playbook.md.
                          Example:
                            stellarindex-ops emit-incident \
                              -config /etc/stellarindex.toml \
                              -slug 2026-05-12-redis-blip \
                              -event sev1
  version                 Print version + build date.
  help                    This help.
`

func printUsage() {
	fmt.Fprintf(os.Stderr, "stellarindex-ops %s\n%s", version.String(), usageBody)
}
