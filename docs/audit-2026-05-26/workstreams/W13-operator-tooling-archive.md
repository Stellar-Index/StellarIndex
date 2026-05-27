# W13 — Operator tooling, archive completeness, DR

## Scope

`cmd/ratesengine-ops/` subcommands. The SLA probe. Migration
runner. Archive completeness verifier. Per-runbook coverage.

## Inventory of ratesengine-ops subcommands

| Subcommand | File | Notes |
| --- | --- | --- |
| docs-config | main.go | |
| rpc-probe | main.go | dev/diagnostic |
| list-cursors | main.go | |
| detect-gaps | main.go | |
| backfill | backfill.go | including soroban-events pseudo |
| backfill-external | main.go | |
| backfill-chainlink | main.go | NEW (chainlink reference) |
| verify-decoders | main.go | |
| scan-soroban-events | main.go | NEW; W27 cross-ref |
| verify-external | main.go | |
| verify-archive | main.go | W34 |
| archive-completeness | main.go | |
| discovery | discovery.go | |
| supply | supply.go | |
| sep1-refresh | sep1_refresh.go | |
| wasm-history | wasm_history.go | W24 |
| wasm-history-merge-jsonl | main.go | NEW |
| extract-wasm-from-galexie | main.go | NEW |
| cross-region-check | cross_region_check.go | |
| cross-region-monitor | cross_region_monitor.go | |
| seed-soroswap-pairs | seed_soroswap_pairs.go | |
| mint_key | mint_key.go | W19 cross-ref |
| upgrade_key | upgrade_key.go | W19 cross-ref |
| hubble-check | hubble_check.go | |
| hubble-soroban-events | hubble_soroban_events.go | |
| cctp-backfill | cctp_backfill.go | NEW; W29 |
| rozo-backfill | rozo_backfill.go | NEW; W29 |
| soroswap-skim-backfill | soroswap_skim_backfill.go | NEW; W29 |
| comet-liquidity-backfill | comet_liquidity_backfill.go | NEW; W29 |
| phoenix-backfill | phoenix_backfill.go | NEW; W29 |
| blend-backfill | blend_backfill.go | NEW; W29 |

## Other binaries

- `cmd/ratesengine-sla-probe/`: probe config, output schema,
  textfile collector contract
- `cmd/ratesengine-migrate/`: applies all migrations in order
- `cmd/ratesengine-aggregator/`, `cmd/ratesengine-api/`,
  `cmd/ratesengine-indexer/` (covered in other workstreams)

## Checks

| # | Check | Method |
| --- | --- | --- |
| W13.1 | Each subcommand documented in `docs/operations/` | grep |
| W13.2 | Each subcommand has at least one operator runbook entry | grep |
| W13.3 | `ratesengine-migrate up` applies every checked-in migration | shell |
| W13.4 | `ratesengine-migrate version` shows current schema_migrations.version | shell |
| W13.5 | SLA probe writes textfile in node_exporter format | `--help` + sample run |
| W13.6 | SLA probe latency thresholds derive from ADR-0009 / docs/operations/sla-probe.md | doc + code |
| W13.7 | archive-completeness: tier A/B/C/D semantics + reports | `archivecompleteness/` |
| W13.8 | cross-region check + monitor (W23 cross-ref) | code |
| W13.9 | `configs/audit/wasm-walk-contracts.yaml` content vs `wasm-history` flags | inspection |
| W13.10 | `docs/operations/runbooks/`: every alert has a runbook | per-runbook |
| W13.11 | NEW: each new backfill subcommand has a documented operational recipe | doc audit |
| W13.12 | `scripts/dev/r1-smoke.sh` reachable from r1's local filesystem | r1 probe |
| W13.13 | `configs/healthchecks/smoke.sh` content vs r1-smoke.sh content | diff |

## Closure criteria

Every subcommand + binary has terminal status. Findings on:
- any subcommand without help text
- any subcommand that silently elevates privileges (mint_key
  without `--tier`, etc.)
- any runbook that references a subcommand that's been renamed
