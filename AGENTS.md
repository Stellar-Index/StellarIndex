# Stellar Index

A protocol explorer and API for the Stellar network: complete, verified,
per-protocol on-chain data captured from a certified raw ledger lake and
served through a public REST + SSE API. Go, Apache-2.0, pre-v1.

This file is rules. Each rule names the lint or test that enforces it; the few
that have none are kept because breaking them is unrecoverable. Reference
material is linked at the bottom.

## Commands

```sh
make help              # every target, with its description
make dev               # dependency stack (TimescaleDB + Redis + MinIO); app binaries run on the HOST
make test              # unit tests, ~2 min
make test-integration  # spins its own containers via testcontainers-go; needs Docker
make check             # fast read-only edit-loop feedback; not push clearance
make lint-changed      # the lints for the files you changed
make verify-changed    # vet + tests for the Go packages your change reaches, typecheck/vitest for touched web apps; one screen
```

- **CI on the pull request is the landing gate.** Every change lands as a PR that merges only when
  CI is green. Locally, run `make lint-changed` and `make check` before pushing; that is the whole
  local requirement.
- ALWAYS run `make lint-changed` before committing; `make hooks` installs it as the pre-commit hook.
- Verify with `make verify-changed`, not `go test ./...` or `make test`: it runs only what the change can
  reach and prints counts plus failing lines, with the full log's path. Don't `cat` the log; grep it.
- Wait for CI with `~/.claude/bin/wait-for pr <n>` run in the background (it ends with a `ci-status`
  summary), never with a sleep-and-check loop or `gh pr checks --watch` in the foreground.
- Run `make prepush` only when CI cannot answer the question (CI down or billing-capped, or a local
  repro of a CI failure). It needs its literal `ALL REQUIRED CHECKS PASSED` and runs in the
  BACKGROUND. Profiles: [docs/contributing/local-verification.md](docs/contributing/local-verification.md).
- A red check on `main` blocks every PR's CI. Fix `main` first rather than working around it.
- ALWAYS re-run all three generators together after editing `openapi/stellar-index.v1.yaml`:
  `make docs-api && make docs-postman && make web-generate-api`. Enforced by
  `scripts/ci/lint-docs.sh` (API reference) and the Postman and TS-types drift jobs in `ci.yml`.
- Check a live deployment: `bash scripts/dev/r1-smoke.sh` (exit code = failed assertions).

## Money — invariant 1 (ADR-0003), the rule we reject PRs over

Token amounts, reserves, prices and supplies are `canonical.Amount` (a `*big.Int` wrapper) in Go,
`NUMERIC` in Postgres, and decimal **strings** in JSON. JSON numbers are IEEE 754 doubles and lose
precision above 2^53.

```go
// NEVER — truncates silently above 2^63; our costliest recurring bug (scripts/ci/lint-i128.sh)
amount := int64(parts.Lo)

// ALWAYS — the canonical helpers carry the full 128 bits
amount := canonical.FromInt128Parts(int64(p.Hi), uint64(p.Lo))   // i128
amount := canonical.FromUInt128Parts(uint64(p.Hi), uint64(p.Lo)) // u128
```

- NEVER compare or accumulate money in `float64`. Use `*big.Int` or `*big.Rat`.
  `scripts/ci/lint-migrations.sh` rejects float money columns; for Go only
  `internal/canonical/i128_truncation_guard_test.go` (Float64 narrowing) guards it, and a wrong
  served number cannot be recalled.
- ALWAYS render a partial total as a lower bound, never as a total: set the response's
  `lower_bound` flag and name what was excluded, as `/v1/protocols`'s `tvl_total` does
  (`internal/api/v1/dex_tvl_identity_internal_test.go`).
- NEVER make a number faster by making it less true. An honest slow answer beats a fast wrong one.

## Architectural invariants (ADR-backed; long-form in `docs/adr/`)

The bracketed number is the invariant's stable id. Ten documents and one runtime
error string cite "AGENTS.md invariant N" — do not renumber these.

- **[2]** **NEVER integrate via Horizon** (ADR-0001). We do not run it, ingest from it, or proxy to
  it. If a protocol's only path to us is Horizon, we do not integrate it.
  `lint-imports.sh` rule C/no-horizon bans client imports only; HTTP use is caught in review.
- **[6]** **NEVER ingest via stellar-rpc.** Production ingest is
  `Galexie MinIO → internal/ledgerstream → internal/dispatcher → internal/sources/<venue>/decode`.
  A new source with an `rpc *stellarrpc.Client` field, a `BackfillRange` or a `StreamLive` method
  is wrong. stellar-rpc survives only for `rpc-probe` and fixture capture.
  `lint-imports.sh` rule A/no-rpc-in-ingest bans the import; the methods are caught in review.
- **[3]** **ALWAYS use S3-compatible storage, never Galexie's local filesystem backend**
  (ADR-0002). That backend silently drops per-object metadata; repairing a lake written
  through it means a full re-export. No lint.
- **[7]** **ONE writer per data domain** (ADR-0031/0032). A **projected** Soroban source is written
  by `internal/projector`; adding one means ONE `SourceSpec` with a `Projector` in
  `pipeline/source_spec.go`, which `buildSource`, `BuildDispatcher` and `IsProjectedEvent` all read
  (`internal/pipeline/lockstep_ast_test.go`). `band`, `soroswap_router`, `sdex` (specs with no
  `Projector`), CEX/FX connectors and supply observers write through the dispatcher. Until per-source promotion (ADR-0032 Phase 3,
  `persist_per_source=true` in every host's template) the dispatcher also writes them, except sep41.
- **[7]** **Catch-up depends on which side of that line you are on.** A projected domain uses
  `stellarindex-ops projector-replay`; a non-projected one uses `ch-rebuild` (`-sdex`,
  `-contract-calls`). NEVER add a bespoke `<source>-backfill` subcommand: it is a second writer.
  `backfill` refuses projected sources (`TestBackfill_RefusesProjectedSources`); `ch-rebuild
  -write` refuses a range the live projector is still inside. Decision table: [docs/architecture/ingest-pipeline.md](docs/architecture/ingest-pipeline.md#the-replay-decision-rule).
- **[8]** **ClickHouse is the raw lake; Postgres is the SERVED tier** (ADR-0034) — the recent
  working set, not the full archive. "100% coverage" means the ClickHouse substrate captured
  everything; "retention-scoped" means scoped to what has been PROJECTED, not a drop policy.
  `/v1/coverage`: `lake_complete` is the genesis-to-tip claim, `complete` adds the projection window.
- **[8]** **NEVER put a retention policy on `trades`.** Storage is not a constraint
  (`internal/storage/timescale/retention_policy_test.go`).
- **[4]** **`internal/` is private, `pkg/` is the public SemVer surface** (ADR-0005). One Go module.
  `lint-imports.sh` rule L/pkg-purity.
- **[5]** **NEVER put a validator key on disk unencrypted** (ADR-0004). No lint; leaks are permanent.

## Domain rules that will catch you out

Evidence for each: [docs/architecture/domain-traps.md](docs/architecture/domain-traps.md).

- **ALWAYS key an asset on `(code, issuer)`, a SAC address, or `native` — NEVER on code alone.**
  A scam token can claim `USDC` (example test, `internal/api/v1/oracle_identity_gate_test.go`; no repo-wide guard).
- **ALWAYS loop `canonical.AssetAliases` on every asset-id read path.** XLM has three disjoint
  identities (`native`, `crypto:XLM`, its SAC); handling one silently under-reports
  (example test, `internal/canonical/alias_registry_test.go`; no repo-wide guard).
- **ALWAYS correlate a Soroswap `SwapEvent` with the immediately-following `SyncEvent`** by
  `(ledger, tx_hash, op_index)` (`internal/sources/soroswap/adapter_test.go`).
- **ALWAYS group all 8 Phoenix events** to reconstruct one swap
  (`TestDecoder_Decode_completesAfterEighthField`).
- **ALWAYS gate a decoder on contract identity, never on topic alone** (ADR-0035). Comet's
  `("POOL", <event>)` topic is shared by every Balancer-v1 deployment. Each gated decoder
  package has a foreign-contract rejection test (e.g. `TestDecoder_GateRejectsForeignContract` in
  `comet/adapter_test.go`); no lint requires one.
- **ALWAYS type-test a SEP-41 `transfer` body before `MustI128()`** — it is a bare `i128` or a map
  carrying `amount` + `to_muxed_id` (`FuzzTransferAmount`, `FuzzTransferBodyShapes`).
- **NEVER assume off-chain amount scaling is uniform.** CEX and aggregators use 10^8, FX 10^6;
  read the per-source `Decimals` field (`internal/sources/external/amount_scale_test.go`).
- **NEVER drop an unmapped oracle symbol.** Record it verbatim as `raw:<symbol>`, and NEVER let a
  raw row reach VWAP, a pair leg or a supply key (`internal/canonical/asset_raw_test.go`,
  `internal/aggregate/mev/cascade_raw_test.go`).
- **NEVER normalise a stablecoin at ingest.** Store the real pair; the aggregator maps `USDT→USD`
  at compute time. No lint: eager normalisation hides a depeg, and no static check can tell it
  from a legitimate pair.
- **ALWAYS gate a Soroban backfill behind a per-WASM-hash decoder audit.** Contracts upgrade in
  place; backfill sees every prior version
  (`ch_rebuild_backfillsafe_test.go`).
- **ALWAYS collapse `stellar.operations` and `stellar.transactions` on their identity before
  aggregating** — they are `ReplacingMergeTree` and hold rows twice
  (`scripts/ci/lint-lake-dedup.sh`, .go/.sql only; ad-hoc queries: FINAL or uniqExact; [docs/contributing/lake-reads.md](docs/contributing/lake-reads.md)).

## Working style

- Dry and concise. No preambles, no flattery. Comments explain *why*, not *what*.
- Smallest PR that advances one thing. Every pushed branch gets a PR in the same session.
- Commit messages: [CONTRIBUTING.md](CONTRIBUTING.md#commit-messages); changelog (never per PR): [#changelog](CONTRIBUTING.md#changelog).

## Where the reference material is

| | |
|---|---|
| [docs/architecture/overview.md](docs/architecture/overview.md) | The system, its flows, and what lives in which directory |
| [docs/architecture/domain-traps.md](docs/architecture/domain-traps.md) | The evidence behind the domain rules above |
| [CAPABILITY-INVENTORY.md](CAPABILITY-INVENTORY.md) | Existing primitives — search it before writing a utility |
| [docs/contributing/task-recipes.md](docs/contributing/task-recipes.md) | "Add a source", "add an endpoint", "recover from disaster" |
| [docs/contributing/procedures/](docs/contributing/procedures/) | Nine step-by-step procedures with gate checklists |
| [docs/adr/](docs/adr/) | Decisions and their rationale (numbered, immutable) |
| [docs/architecture/](docs/architecture/) | Narrative design |
| [docs/engineering-standards.md](docs/engineering-standards.md) | The enforcement policy and Definition of Done |
| [docs/operations/maintainer-workflow.md](docs/operations/maintainer-workflow.md) | How the reference deployment is run — not needed to contribute |
| [CONTRIBUTING.md](CONTRIBUTING.md) | Workflow, commit standard, review |
| [SECURITY.md](SECURITY.md) | Disclosure — never open a public issue for a vulnerability |
