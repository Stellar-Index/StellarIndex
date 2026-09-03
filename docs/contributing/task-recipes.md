---
title: Common task recipes
last_verified: 2026-09-03
status: living doc
---

# Common task recipes

### "Bring up a new archival node" / "recover from disaster"

End-to-end recipe in [docs/operations/archival-node-bringup.md](../../docs/operations/archival-node-bringup.md).
Six steps from a fresh box to a running indexer (greenfield ≈ 10–13 h
wall, mostly bandwidth). Same doc has the disaster-recovery triage
tree (corrupt history-archive, partial galexie-archive partition,
wiped postgres, lost MinIO data dir).

Per-region storage shape varies by provider — see
[ADR-0016](../../docs/adr/0016-per-region-storage-strategy.md): R1
(Hetzner) is the full-mirror integrity leader; R2 (AWS) reads
galexie data direct from `aws-public-blockchain` S3, no local
mirror; R3 (Vultr) keeps galexie-archive on Vultr Object Storage
hybrid. R2 + R3 trust R1's primary verification but run their own
Tier A + Tier D periodically as defence-in-depth. The cross-region
"all regions serve the same rate" property is preserved by
[ADR-0015](../../docs/adr/0015-last-closed-bucket-rate-serving.md)'s
closed-bucket-only API contract.

### "Add a new CEX connector"

CEX/FX venues are NOT under the Galexie → dispatcher path and do NOT
use `internal/sources/<venue>/` or the on-chain five-file convention.
They live directly at `internal/sources/external/<venue>/` and
implement the `external.Connector` framework
(`internal/sources/external/framework.go`), not `consumer.Source`.
Copy the `binance` / `kraken` package as the template.

1. Review the venue's public API docs for its real endpoints + pair
   conventions before writing the parser.
2. Create `internal/sources/external/<venue>/` following the
   actual per-package layout: `events.go` (wire types), `parse.go`
   (vendor JSON → `canonical.Trade`), `streamer.go` (live WS/REST;
   implements `external.Streamer`) and/or a poller, `backfill.go`
   (historical OHLC; implements `external.Backfiller`), and
   `pairs.go` (symbol map). Tests sit alongside as `*_test.go`.
3. Implement the relevant `external.Connector` sub-interface(s) —
   `Streamer` (live push, e.g. binance/kraken), `Poller` (REST quote
   board, used by the FX pollers), and/or `Backfiller` (historical
   candles). NOT `consumer.Source` — that's the legacy on-chain seam.
4. Register the venue's `Metadata` (class / subclass / weight /
   `IncludeInVWAP` / `BackfillSafe`) in the `Registry` map in
   `internal/sources/external/registry.go`, then wire it into
   `buildExternal` in `cmd/stellarindex-indexer/main.go` (and the
   parallel block in `stellarindex-ops`) behind a `cfg.<Venue>.Enabled`
   gate.
5. Fixtures are inline golden frames in the package's `*_test.go`
   (e.g. `binance/streamer_test.go`) — there is no
   `test/fixtures/external/` directory.
6. Add an ADR if the venue has unusual constraints (e.g. requires
   paid tier, or has licensing restrictions on redistribution).

### "Add a new on-chain Soroban DEX"

**Full step-by-step checklist: [docs/contributing/add-onchain-source.md](../../docs/contributing/add-onchain-source.md).**
The package is SIX files (`README.md`, `events.go`, `decode.go`, `consumer.go`,
**`dispatcher_adapter.go`** — the production seam that implements `dispatcher.Decoder`;
this is the object the dispatcher actually calls — and `source_test.go`), PLUS **six
wiring edits in other packages** (config `KnownSources`, `pipeline/dispatcher.go`
BuildDispatcher, `pipeline/sink.go` HandleEvent + IsProjectedEvent,
`projector/registry.go` buildSource, `external/registry.go` Metadata). Miss a wiring
edit and the source compiles, registers nowhere, and silently emits nothing. Template:
`internal/sources/soroswap/`. Reuse the shared helpers (`internal/scval`,
`canonical.Amount`) — check CAPABILITY-INVENTORY.md before writing utilities.

### "Add a new supply observer"

Read [docs/architecture/supply-pipeline.md](../../docs/architecture/supply-pipeline.md)
first — it covers the three-domain split (Algorithm 1 XLM /
Algorithm 2 classic / Algorithm 3 SEP-41), which dispatcher hook
each observer plugs into, and where the per-class hypertables
live. New observers ship as a Go package with package-level docs
in `doc.go` (not `README.md` — supply observers follow Go
package-doc convention; `events.go`, `decode.go`, `consumer.go`,
and a `dispatcher_adapter*.go` pair complete the layout). Pick the
right dispatcher hook based on what the source emits:

- `LedgerEntryChangeDecoder` for `LedgerEntry` mutations (current
  use: AccountEntry / trustline / claimable / LP-reserve
  observers).
- `OpDecoder` for classic operations (e.g. `change_trust_op`).
- `Decoder` (event-based) for Soroban contract events (current
  use: SEP-41 mint/burn/clawback observer).

The reader/storage seam is the same across all three: each
observer writes to a per-class hypertable
(`migrations/0011-0014_*.sql` etc.), and `StorageClassicSupplyReader`
/ `StorageSEP41SupplyReader` aggregate the rows at refresh time.
Wire the new observer into `cmd/stellarindex-indexer/main.go`
alongside the existing supply observers and add an integration
test under `test/integration/` if it touches NUMERIC arithmetic
(see `test/integration/classic_supply_storage_test.go` /
`sep41_supply_storage_test.go` for the testcontainers-go pattern).

### "Audit a Soroban source's WASM history (flip BackfillSafe)"

Procedure: [docs/operations/wasm-audits/README.md](../../docs/operations/wasm-audits/README.md).
One audit log per source under that directory; each is the
evidence trail for flipping `internal/sources/external/registry.go`'s
`BackfillSafe` flag from `false` → `true`. The flag gates
`stellarindex-ops backfill` from running an unaudited Soroban
source against historical ranges (AGENTS.md "Soroban DeFi
contracts upgrade in place").

### "Investigate a price divergence"

Start at [docs/operations/runbooks/price-divergence.md](../../docs/operations/runbooks/price-divergence.md).
Aggregator-layer alerts (silent / outlier-storm / class-drop-spike)
have their own runbooks under the same directory; see
[docs/architecture/aggregation-plan.md](../../docs/architecture/aggregation-plan.md)
for the policy chain that drives them.

### "Find why a metric is alerting"

Every alert references a runbook at `docs/operations/runbooks/<alert-name>.md`.
If it doesn't, that's a CI failure.

### "Add a new Prometheus metric"

1. Declare the metric in `internal/obs/metrics.go` (one of the
   typed `*Vec` variables near the bottom) and register it in
   **`registerAppMetrics()` / `registerAppMetricsTail()`** (NOT
   `init()` directly — `init()` delegates to those, which are split
   to stay under the `funlen` ceiling).
2. Wire it at the point of observation. For goroutine workers
   doing IO, the established pattern is paired:
   - `*Total{outcome}` counter for outcomes (per-attempt count).
   - `*DurationSeconds{outcome}` histogram for latency.
   Operators chart `outcome="ok"` p95/p99 separately from
   failure outcomes to detect "endpoint slow" vs "endpoint
   failing" independently. The wave-88/89/90/91 series
   (`customer_webhook_delivery`, `divergence_refresh`,
   `aggregator_supply_refresh`, `anomaly_freeze_recovery_sweep`)
   are the canonical examples.
3. Document in `docs/reference/metrics/README.md` with a
   when-to-look-at-this prose block.
4. If the metric warrants alerting, add a rule to BOTH
   `deploy/monitoring/rules/<area>.yml` (multi-host) and
   `configs/prometheus/rules.r1/<area>.yml` (R1 overlay). CI's
   `monitoring-rules` job validates both directories with
   promtool (wave 96); the wave-82 lint catches sibling-file
   presence and the wave-78 template-presence lint catches
   missing runbook structure.
5. Write a regression test in the worker's test file using
   `obstest.HistogramSampleCount` (`internal/obstest/`,
   wave 100) — the helper exists because
   `HistogramVec.WithLabelValues(...)` returns a
   `prometheus.Observer` not a Collector, so the official
   `testutil.CollectAndCount` can't act on per-label children
   directly.

### "Change the OpenAPI spec"

1. Edit `openapi/stellar-index.v1.yaml`.
2. Regenerate **every** spec-derived artifact and commit the diffs —
   three separate generators, and it's easy to forget the last two
   (both have silently drifted onto main before):
   - `make docs-api` — rendered reference + colocated YAML (the only
     one `make docs-all` and the CI drift-lint cover).
   - `make docs-postman` — `examples/postman/…json` (deterministic
     since the generator's RNG is seeded; drift-guarded in CI by an
     exact `git diff --exit-code -- examples/postman` in the
     postman-check job).
   - `make web-generate-api` — `web/explorer/src/api/types.ts`, the
     explorer's compile-time contract (drift-guarded in CI by an
     exact `git diff --exit-code -- src/api/types.ts` in the
     web-explorer job).
3. Handlers in `internal/api/v1/` get updated; contract tests
   verify they match.
4. Bump the API minor version if the change is additive, major
   if breaking.
5. CHANGELOG entry under `[Unreleased]`.

### "Cut a release" / "Deploy to the reference deployment"

Both are maintainer operations against our own boxes rather than
things a contributor does, so they live in
[docs/operations/maintainer-workflow.md](../../docs/operations/maintainer-workflow.md).
Full runbooks: [release-process.md](../../docs/operations/release-process.md)
and [deploy-workflow.md](../../docs/operations/deploy-workflow.md).

---

For the nine step-by-step procedures with their gate checklists, see
[procedures/](procedures/).
