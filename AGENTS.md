# Stellar Index

A protocol explorer and API for the Stellar network: complete, verified,
per-protocol on-chain data captured from a certified raw ledger lake and
served through a public REST + SSE API. Go, Apache-2.0, pre-v1.

This file is rules. Reference material lives elsewhere and is linked at
the bottom — read that when you need to know *why*; read this before you
write anything.

## Commands

```sh
make help              # every target
make dev               # local dependency stack (TimescaleDB + Redis + MinIO) in docker-compose
                       # the app binaries run on the HOST; there is no API or ClickHouse service
make test              # unit tests, ~2 min
make test-integration  # spins its own containers via testcontainers-go; needs Docker
make verify            # THE pre-push gate: fmt, vet, lint, docs, vuln, test
```

- ALWAYS run `make verify` before pushing. NEVER substitute `make lint && make test` — that
  skips the doc, import, openapi and monitoring lints CI enforces.
- `make verify` exceeds a 10-minute foreground timeout and its exit code is unreliable. Run it
  backgrounded and grep the output for `ALL CHECKS PASSED`.
- `make verify` does NOT run integration tests. Run `make test-integration` when you change a
  storage query, a migration, or anything a fixture seeds.
- ALWAYS re-run all three generators together after editing `openapi/stellar-index.v1.yaml`:
  `make docs-api && make docs-postman && make web-generate-api`. Two of them have silently
  drifted onto main before.
- NEVER write a command that needs manual network access during development. If one does, that
  is a bug.
- ALWAYS check `CAPABILITY-INVENTORY.md` before writing a utility. Rebuilding an existing
  primitive is this repo's largest single source of maintenance debt.
- To check a live deployment: `bash scripts/dev/r1-smoke.sh` (or
  `API_BASE_URL=https://api.stellarindex.io bash scripts/dev/r1-smoke.sh`). Exit code is the
  number of failed assertions.

## Money — invariant 1 (ADR-0003), the rule we reject PRs over

Token amounts, reserves, prices and supplies are `canonical.Amount` (a `*big.Int` wrapper) in Go, `NUMERIC` in Postgres, and
decimal **strings** in JSON. JSON numbers are IEEE 754 doubles and lose precision above 2^53.

```go
// NEVER — truncates silently above 2^63 and is our most expensive recurring bug
amount := int64(parts.Lo)

// ALWAYS — the canonical helpers carry the full 128 bits
amount := canonical.FromInt128Parts(int64(p.Hi), uint64(p.Lo))   // i128
amount := canonical.FromUInt128Parts(uint64(p.Hi), uint64(p.Lo)) // u128
```

- NEVER compare or accumulate money in `float64`. Use `*big.Int` or `*big.Rat`.
- ALWAYS render a partial total as a lower bound, never as a total: set the response's
  `lower_bound` flag and name what was excluded, as `/v1/protocols`'s `tvl_total` does.
- NEVER make a number faster by making it less true. An honest slow answer beats a fast wrong one.

## Architectural invariants (ADR-backed; long-form in `docs/adr/`)

The bracketed number is the invariant's stable id. Ten documents and one runtime
error string cite "AGENTS.md invariant N" — do not renumber these.

- **[2]** **NEVER integrate via Horizon** (ADR-0001). We do not run it, ingest from it, or proxy to it.
  If a protocol's only path to us is Horizon, we do not integrate it.
- **[6]** **NEVER ingest via stellar-rpc.** Production ingest is
  `Galexie MinIO → internal/ledgerstream → internal/dispatcher → internal/sources/<venue>/decode`.
  A new source with an `rpc *stellarrpc.Client` field, a `BackfillRange` or a `StreamLive` method
  is wrong. stellar-rpc survives only for the `rpc-probe` diagnostic and fixture capture.
- **[3]** **ALWAYS use S3-compatible storage, never Galexie's local filesystem backend** (ADR-0002).
  That backend silently drops per-object metadata and warns about multi-process writes in its own
  docstring.
- **[7]** **ONE writer per data domain** (ADR-0031/0032). A **projected** Soroban source is written by
  `internal/projector` and only by it; adding one means a case in
  `projector/registry.go::buildSource` AND an arm in `pipeline/sink.go::IsProjectedEvent`.
  NOT every Soroban source is projected — `band` (ContractCall-derived), `soroswap_router`
  (log-only), `sdex`, the external CEX/FX connectors and the supply observers deliberately
  write through the dispatcher instead. `IsProjectedEvent`'s default branch is the list.
- **[7]** **Catch-up depends on which side of that line you are on.** A projected domain uses
  `stellarindex-ops projector-replay -config PATH -source <name> -from <ledger>`; a
  non-projected one uses `ch-rebuild` (`-sdex`, `-contract-calls`). NEVER add a bespoke
  `<source>-backfill` subcommand — those were deleted in ADR-0032 Phase 5. `-sep41` is
  projected, so `ch-rebuild -sep41` would be a second writer; `ch-rebuild -write` refuses a
  range the live projector is still inside. Decision table:
  [docs/architecture/ingest-pipeline.md](docs/architecture/ingest-pipeline.md#the-replay-decision-rule).
- **[8]** **ClickHouse is the raw lake; Postgres is the SERVED tier** (ADR-0034) — the recent working set,
  not the full archive. "100% coverage" means the ClickHouse substrate captured everything; the
  served tier is verified faithful only within what it holds. `/v1/coverage` publishes both axes:
  `lake_complete` is the archive's genesis-to-tip claim, `complete` is additionally gated by the
  projection window. "Retention-scoped" means scoped to what has been PROJECTED — NOT a database
  drop policy.
- **[8]** **NEVER put a retention policy on `trades`.** Migration 0031 removed the old 90-day one and
  storage is not a constraint. A `drop_after` on `trades` is drift — remove it.
- **[4]** **`internal/` is private, `pkg/` is the public SemVer surface** (ADR-0005). One Go module.
- **[5]** **NEVER put a validator key on disk unencrypted** (ADR-0004).

## Domain rules that will catch you out

Full evidence for each: [docs/architecture/domain-traps.md](docs/architecture/domain-traps.md).

- **ALWAYS key an asset on `(code, issuer)`, a SAC address, or `native` — NEVER on code alone.**
  Code alone is an impersonation vector; a scam token can claim `USDC`.
- **ALWAYS loop `canonical.AssetAliases` on every asset-id read path.** XLM has three disjoint
  identities (`native`, `crypto:XLM`, its SAC) and they are different venue populations. A read
  path that handles one silently under-reports.
- **ALWAYS correlate a Soroswap `SwapEvent` with the immediately-following `SyncEvent`** by
  `(ledger, tx_hash, op_index)`. `SwapEvent` carries no post-state reserves.
- **ALWAYS group all 8 Phoenix events** to reconstruct one swap. It emits one event per field.
- **ALWAYS gate a decoder on contract identity, never on topic alone** (ADR-0035). Comet uses a
  shared `("POOL", <event>)` topic across every pool contract; any Balancer-v1 deployment looks
  identical on the wire.
- **ALWAYS type-test a SEP-41 `transfer` body before `MustI128()`** — it is either a bare `i128`
  or a map carrying `amount` + `to_muxed_id`.
- **NEVER assume off-chain amount scaling is uniform.** On-chain uses per-asset decimals, CEX and
  aggregators use 10^8, FX uses 10^6. Read the per-source `Decimals` field.
- **NEVER drop an unmapped oracle symbol.** Record it verbatim as `raw:<symbol>`. Raw rows are
  record-layer only: NEVER let one reach VWAP, a pair leg or a supply key — filter on
  `Asset.IsMapped()` or `asset NOT LIKE 'raw:%'`.
- **NEVER normalise a stablecoin at ingest.** Store the real pair; the aggregator maps
  `USDT→USD` at compute time. Eager normalisation hides a depeg.
- **NEVER auto-populate `internal/currency` from an external aggregator.** It is a hand-vetted
  trust surface; adding a currency is a code change.
- **ALWAYS gate a Soroban backfill behind a per-WASM-hash decoder audit.** Contracts upgrade in
  place: live ingest sees only current WASM, backfill sees every prior version.
- **ALWAYS decode by map field name and dispatch on `topic[0]`** — never by field position or
  contract address.

## Working style

- Dry and concise. No preambles, no flattery. Comments explain *why*, not *what*.
- Smallest PR that advances one thing. NEVER "ship and clean up later".
- ALWAYS state a measurement with its units and the command that produced it. A performance claim
  without a number is not a claim.
- NEVER report a gate as passing on its exit code alone when the code is unreliable. `verify.sh`
  and `r1-smoke.sh` both exit non-zero for reasons unrelated to the assertions; grep the output
  for `ALL CHECKS PASSED` / the failure count. NEVER pipe a gate through `tee`, `head` or `sed` —
  you then read the pipe's status, not the gate's.
- ALWAYS check for prior art before starting on a symptom: `gh pr list --state all --search`,
  `git branch -r | grep`, the runbook, and the backlog. Record the result in the PR body.
- Every pushed branch gets a PR in the same session. A branch with no PR is not work, it is loss.
- Commit messages: see [CONTRIBUTING.md](CONTRIBUTING.md#commit-messages).

## Where the reference material is

| | |
|---|---|
| [docs/architecture/repo-map.md](docs/architecture/repo-map.md) | What lives in which directory |
| [docs/architecture/domain-traps.md](docs/architecture/domain-traps.md) | The evidence behind the domain rules above |
| [docs/contributing/task-recipes.md](docs/contributing/task-recipes.md) | "Add a source", "add an endpoint", "recover from disaster" |
| [docs/contributing/procedures/](docs/contributing/procedures/) | Nine step-by-step procedures with gate checklists |
| [docs/adr/](docs/adr/) | Decisions and their rationale (numbered, immutable) |
| [docs/architecture/](docs/architecture/) | Narrative design |
| [docs/engineering-standards.md](docs/engineering-standards.md) | The enforcement policy and Definition of Done |
| [docs/operations/maintainer-workflow.md](docs/operations/maintainer-workflow.md) | How the reference deployment is run — not needed to contribute |
| [CONTRIBUTING.md](CONTRIBUTING.md) | Workflow, commit standard, review |
| [SECURITY.md](SECURITY.md) | Disclosure — never open a public issue for a vulnerability |
