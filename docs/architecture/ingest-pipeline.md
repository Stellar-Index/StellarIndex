---
title: Ingest pipeline — the one canonical data path, projector, replay and backfill
last_verified: 2026-10-05
status: binding
---

# Ingest pipeline

Binding. AGENTS.md invariants [2], [3], [6] and [7] point here. The
decisions are ADR-0001, 0002, 0029, 0031, 0032, 0034, 0035 and 0048;
this page holds only how the code implements them.

## The path

```
Stellar pubnet ──(captive-core)──► galexie   the single stellar-core on r1 (ADR-0002, CDP pattern)
    ──► MinIO galexie-live                   S3-compatible; .xdr.zst per ledger
    ──► internal/ledgerstream                Stream(ctx, from, to) yields xdr.LedgerCloseMeta;
    │                                        to=0 is the unbounded live tail;
    │                                        StreamArchiveThenLive (seamed.go) seams archive → live
    ──► internal/dispatcher                  the single consumer of ledgerstream; four decoder seams
    ──► internal/sources/<venue>             pure decoders → canonical.Trade | OracleUpdate | event
    ──► internal/pipeline/sink.go            fans each item out:
          ├─► ClickHouse lake (LiveSink)     every ledger, tx, op, contract_event, ledger_entry_change;
          │                                  contiguous + hash-chained to genesis — the CERTIFIED raw
          │                                  history (ADR-0034) that "100% coverage" is proven on (ADR-0033)
          ├─► soroban_events (Postgres)      ADR-0029 landing zone — legacy fallback only, decommission #803
          ├─► projector                      reads CH contract_events; sole writer of PROJECTED domains
          └─► dispatcher events goroutine    NON-projected domains write here directly
    ──► Postgres/TimescaleDB                 the SERVED tier: recent working set, verified faithful within
                                             what it holds (ADR-0033 projection reconcile)
    ──► /v1/* API
```

Off-chain CEX/FX connectors (`internal/sources/external`) feed the same
event channel and are non-projected.

## Decoder seams

`internal/dispatcher/dispatcher.go` exposes four interfaces, one per
shape of on-chain data. Decoders register with `AddDecoder` and
siblings; there is no `routes.go`.

| Seam | Input | Routed by | Lake source on re-derive |
|---|---|---|---|
| `Decoder` | Soroban contract event | `topic[0]` byte-equality against the source's `TopicPrefix*`/`TopicSymbol*` | `contract_events` |
| `OpDecoder` | classic op + result (SDEX, `change_trust`) | op type | `operations.body_xdr` + `operation_results.result_xdr` |
| `ContractCallDecoder` | InvokeContract with no event (Band `relay()`/`force_relay()`) | `(contract_id, function_name)` | `operations` + op args |
| `LedgerEntryChangeDecoder` | `LedgerEntry` mutation (account/trustline/claimable/LP-reserve supply observers) | entry type | `ledger_entry_changes` |

The dispatcher is the only place that byte-matches events, walks
classic ops, matches contract calls, routes entry changes, and feeds
per-source correlation state. Topic routing is a pre-filter: a decoder
still gates on contract identity (ADR-0035, `internal/contractid`).

**Enrichment keeps decoders pure.** Context a decoder cannot read from
the event body travels on `events.Event`, populated by the dispatcher
from the LCM on the live path and rebuilt byte-identically by the
ClickHouse readers on every re-derive path (projector CH feed,
`compute-completeness -ch`, `projected-rebuild`):

- `OpArgs` — the producing InvokeContract's args (base64 SCVals). Redstone
  needs it: `write_prices` carries prices, the feed ids are in the args.
  Lake column `contract_events.op_args_xdr`; the projector always reads it.
- `StateWriteKeys` — base64 LedgerKeys of the event's own contract's
  contract-data entries whose value the op changed (created, or `Val`
  differs from the pre-image). Redstone uses it for exact accepted-feed
  attribution, falling back to payload-median alignment when absent.
  Lake: batched `(ledger_seq, tx_hash, op_index)` lookups on
  `ledger_entry_changes` (`internal/storage/clickhouse/state_write_keys.go`);
  per-source opt-in `projector.Source.NeedsStateWriteKeys`. The rule lives
  in `internal/dispatcher/state_write_keys.go` and both paths MUST stay in
  lockstep.

A new tx-scoped input follows the same pattern: a field on
`events.Event`, populated in the dispatcher AND the lake reader.

The dispatcher's `contractEventToEventsEvent` and the lake extractor's
`eventRow` (`internal/storage/clickhouse/extract.go`) both encode topics,
data and op args as `base64.StdEncoding(scval.MarshalBinary())`, so a CH
row converts to `events.Event` by field copy and feeds the decoders
unchanged. Keep the two encoders identical.

## Source packages are pure decoders

`internal/sources/<venue>/` exports a `SourceName`, pre-encoded
`TopicPrefix*`/`TopicSymbol*` bytes, `decode…` functions returning
`canonical.*`, and optionally an in-memory correlation buffer (Soroswap
swap+sync, Phoenix 8-field assembly) that lives for one dispatcher
goroutine. A source package MUST NOT hold a `*stellarrpc.Client`,
implement `BackfillRange`/`StreamLive`, poll, paginate, start goroutines
or keep cursors. If you are about to add any of those, the work belongs
in the dispatcher or `decode.go`.

**Adding a source:** the decoder package; a registration on the right
dispatcher seam; and, for a projected source, `Projector` set on the same
`SourceSpec`. One entry in `internal/pipeline/source_spec.go` drives
`BuildDispatcher`, the projector registry and `IsProjectedEvent`; the
`HandleEvent` persist arm and `tradeFromEvent` stay per-event-type switches.
`TestLockstep_SpecsListEveryEventType` and
`TestLockstep_ProjectedEventsHavePersistArms`
(`internal/pipeline/lockstep_ast_test.go`) check them together.

## The projector — one writer per projected domain

`internal/projector` is the only writer of Soroban-derived per-source
tables: trades for soroswap/aquarius/phoenix/comet/sushiswap_v3,
`blend_*` (incl. backstop and emitter), `phoenix_*`, `comet_*`,
`aquarius_*`, `upshift_vault_events`, `defindex_*`, `sorocredit_*`,
`soroswap_skim`, `cctp_events`, `rozo_events`, `sep41_*`, and
reflector/redstone `oracle_updates`. The authoritative list is
the specs with `Projector` set in `internal/pipeline/source_spec.go`. Everything else is non-projected: `sdex`,
`band`, `soroswap_router`, external CEX/FX, supply observers.

- It reads ClickHouse `contract_events` by default
  (`[storage] clickhouse_projector_source`, default true,
  `internal/config/config.go`); it tails Postgres `soroban_events` only
  when that is off.
- Cadence: `Interval` 5 s, `PerSourceTimeout` 60 s per cycle
  (`internal/projector/projector.go`), about 720k ledgers/hour.
- Config: `[projector] enabled` (default false) starts it.
  `persist_per_source` defaults true, parallel mode where the dispatcher
  also writes and the generation-guarded upsert absorbs the duplicate.
  At false the projector is sole writer, and the indexer refuses to
  start unless every continuous aggregate's refresh `start_offset`
  covers `pipeline.ProjectorStallBound` (15 min),
  checked by `pipeline.VerifySoleWriterCAGGCoverage`. Low lag is not
  enough: a lake hole stalls the projector and a shorter lookback never
  materialises the late rows. Sole-writer sources (`SoleWriter: true` in
  `internal/pipeline/source_spec.go`: `sep41`, `rozo`) are projector-only
  whatever the flag says (`pipeline.IsSoleWriterProjected`).
- Every projected write is an upsert guarded by
  `derive_generation <= EXCLUDED.derive_generation` (migrations 0110,
  0141). Live ingest writes generation 0; re-derives stamp higher.
- Alerts: `stellarindex_projector_lag_high`
  (`deploy/monitoring/rules/projector.yml`) and
  `stellarindex_ingest_gap_detected` (`ingestion.yml`).

## The structural lake ingest

Every LCM is decoded *structurally* into the `stellar.*` ClickHouse
tables — `ledgers`, `transactions`, `operations`, `operation_results`,
`contract_events`, `ledger_entry_changes` — with raw XDR retained, so
every decoder class can run from the lake without touching Galexie
again. The schema of record is `deploy/clickhouse/tier1_schema.sql`.
Header and per-ledger counts come from `dispatcher.CensusLedger`, which
is also the decoder-independent census oracle; fee-meta entry changes
carry `op_index = -1`.

- Tables are `ReplacingMergeTree(ingested_at)` on the row's identity, so
  re-ingesting any range is idempotent. Unmerged parts can hold a row
  twice: count with `FINAL` or `uniqExact` on the sort key.
- History: `stellarindex-ops ch-backfill -config PATH -from L -to L
  -bucket galexie-archive [-parallel N] -write`. The default bucket is the
  trimmed live one, so historical ranges need `-bucket`.
  `scripts/ops/ch-full-backfill.sh` drives resumable 1M-ledger windows.
  Measured 2026-06-05: about 4,400 ledgers/s on sparse early history at
  `-parallel 8`, about 50 ledgers/s per worker on dense recent ranges.
- `stellarindex-ops ch-gate` checks a range against the census: every
  ledger present and per-ledger tx/op/event/classic-trade counts equal.
- Live: the indexer dual-sinks each ledger to the lake and the served
  tier, so pricing latency never waits on the lake.

## Re-deriving from the lake

A decoder fix or a new decoder re-runs against ClickHouse and
repopulates Postgres; it never re-walks MinIO. Backfill and live tail
share the decoders, not a code path: there are no per-source
`BackfillRange`/`StreamLive` methods and no `<source>-backfill`
subcommands (deleted in ADR-0032 Phase 5; NEVER add one).

**`projector-replay` vs `projected-rebuild` (ADR-0048 D3).** Both refill
a projected source from the lake through the same decoders and the
same generation-guarded upserts.

- `stellarindex-ops projector-replay -config PATH -source <name> -from <ledger>`
  rewinds the LIVE projector's cursor and lets its normal cadence walk
  forward (other flags: `-refresh-caggs -wait -wait-timeout
  -refresh-only -refresh-to`; there is no `-to`). It writes generation 0,
  so it cannot correct a row a re-derive already stamped higher. Use it
  for short rewinds over gen-0 rows: an outage, a missing range.
- `stellarindex-ops projected-rebuild -config PATH -source <name> -from <l> [-to <l>] [-workers K] [-window N] -write`
  runs K ledger-window workers with no per-cycle deadline through the
  same registry decoder and `pipeline.HandleEvent`, roughly 10-20× the
  replay rate. It never moves the live cursor: it fills history strictly
  behind it and refuses a range the live cursor is still inside unless
  `-allow-live-overlap` is passed. `-resume` continues a run. A
  post-decoder-fix re-walk must go through here, because only a higher
  generation overwrites stored rows. Procedure:
  [projector-replay runbook](../operations/runbooks/projector.md#stellarindex_projector_replay_stalled)
  and the doc comment in `internal/ops/chops/projected_rebuild.go`.

**A wrong key needs a clean slate.** An upsert cannot fix a wrong PK: an
additive `ch-rebuild -write` over rows written under the old
`event_index = 0` collision doubled aquarius trades (1,947 → 5,090) on
62.70–62.71M. `scripts/ops/ch-rebuild-projected.sh` DELETEs a window and
re-derives it, and its header is the source of truth for the five rules
that bound the DELETE:
1. run `ch-rebuild -write -preflight` first and delete nothing without
   its `preflight ok […] rederive=…` verdict;
2. delete only the sources that verdict lists, in one transaction
   (`sushiswap_v3` has no DELETE map on purpose);
3. record `lo hi sources` in `$DIRTY` before deleting, and rebuild dirty
   windows first on every run;
4. file an emptied window as a projection dirty window
   (`ch-rebuild -from LO -to HI -sources <deleted> -record-dirty-window`)
   so the ADR-0033 verdict cannot certify it;
5. follow any window whose `trades` changed with
   `stellarindex-ops trades-cagg-refresh -from LO -to HI` (tracked in
   `$STALE` until it succeeds).

`stellarindex-ops ch-reproject` re-derives a range from the lake and
diffs it against the served tables per source, as a read-only check.
It runs soroswap unseeded, so a soroswap mismatch there is the tool,
not the data.
`ch-reproject` buckets re-derived output per source (applying each
source's `contractIDs` prefilter); otherwise the three reflector variants
merge.

## The replay decision rule

"A decoder changed — what replays history?" has one answer, and the
`Replay-Plan:` commit trailer that `scripts/ci/lint-replay-plan.sh`
requires on a decoder or allow-list change names a command from this
table. First ask **who writes the domain** (invariant [7]): a source is
PROJECTED if its `SourceSpec` in `pipeline/source_spec.go` has `Projector`
set.

| The domain | The replay command |
|---|---|
| **Projected**, rewind ≲ 1M ledgers over gen-0 rows | `stellarindex-ops projector-replay -config PATH -source <name> -from <ledger>` |
| **Projected**, bulk (≳ 1M ledgers) or correcting stored rows | `stellarindex-ops projected-rebuild -config PATH -source <name> -from <l> [-to <l>] -write` |
| **Non-projected** lake re-derive (`sdex`, `band` / `soroswap_router` contract calls) | `stellarindex-ops ch-rebuild -config PATH -from <l> -to <l> -write` with the pass flag `-sdex` / `-contract-calls` |
| `sep41` lake re-derive (`ch-rebuild -sep41 -write`) | `sep41_*` is PROJECTED, so this pass is a second writer, which is why `-write` refuses a range the live projector is still inside. Prefer `projector-replay` unless you need the lake re-derive specifically. |
| Anything else | ask first; do not invent a third path |

- **`stellarindex-ops backfill` is never the answer.** It is a MinIO walk
  through the dispatcher, not a lake re-derive, and it refuses every
  projected source (`checkBackfillNotProjected` in
  `internal/ops/ingest/backfill.go`): it would be a second writer, and
  for blend it would write nothing and exit 0.
- **Re-deriving a timestamp (`oracle_updates`).** `ts` is part of the
  table's primary key (TimescaleDB requires the partition column in every
  unique index), so the on-chain identity `(source, ledger, tx_hash,
  op_index)` is NOT enforced: a decoder change that shifts the `ts` of an
  already-stored event makes the re-derive INSERT a second row beside the
  stale one, and nothing deletes the stale row. Per-decoder golden tests
  (`decode_ts_golden_test.go` in `reflector`, `redstone`, `band`) fail on
  any such change. When one must change, re-derive ONLY via
  `projected-rebuild` (reflector, redstone) or `ch-rebuild -contract-calls`
  (band), never `projector-replay` (generation 0, async cursor rewind).
  Both commands end a completed `-write` run with a sweep
  (`Store.SweepOracleRederive`): in the run's ledger range it deletes an
  on-chain (`ledger > 0`) row from an older derive generation when a row the
  run itself wrote (its own generation) shares its identity with a
  different `ts`, and decrements `source_entry_counts` by what it deleted.
  Band's identity adds `(asset, quote)`, because nested relays in one op
  share an `op_index`. A row with no twin from the run is never deleted;
  op_index-shifted rows are counted for hand inspection, not deleted. A dry
  run prints the would-delete count against any newer generation. The
  sweep prints the swept `ts` span; refresh the `oracle_prices_*` caggs over
  it. Off-chain rows (`ledger = 0`) embed `ts` in their identity and are
  never deduplicated this way.
- **`ch-rebuild`'s event pass is guarded, not forbidden, on projected
  domains.** Its one sanctioned projected use is the clean-slate repair
  above. `-write` reads every projected source's live cursor and refuses
  if any is inside `[-from,-to]`, as `projected-rebuild` does
  (`-allow-live-overlap` overrides). A dry run writes nothing and is not
  guarded.

## Contract schema evolution

Soroban contracts upgrade in place (`update_contract`), so live ingest
sees only the current WASM and a backfill sees every version that ever
ran. Upgrades rename, add or reorder body fields, widen i64 → i128, or
change topic shape (CAP-67 in P23 added a 4th topic to classic asset
movement events). A new factory can run beside the old one for months.

- **Decode by map field name; dispatch on `topic[0]`.** Never by field
  position, contract address or cached WASM hash. Where a body really is
  a positional tuple, guard its arity.
- **Gate every replay on a per-WASM audit.** `wasmaudit.GateReplay`
  (`internal/wasmaudit/gate.go`) refuses a range unless every WASM active
  in it on every admitted contract is attested in
  `internal/wasmaudit/audited_wasm.json`. It runs in `backfill` (and so
  `resume-stalled`), `backfill-router`, `projector-replay`,
  `projected-rebuild` and `ch-rebuild -write`. Known gap:
  `ch-cap67-movements` uses the standard SEP-41 decoders, which the
  gate's policy check exempts.
- Per-source policy is the `Backfill` field in
  `internal/sources/external/registry.go`: `BackfillPerWASM`,
  `BackfillNoWASM` or `BackfillUnsafe` (`upshift` today; an unknown
  source falls back to unsafe). Admitting a source takes the WASM audit
  under `docs/operations/wasm-audits/`, the manifest entry and the
  registry value in one PR.
- The WASM behind any row is derived from the lake's code history
  (`ContractCodeHistory` in `internal/storage/clickhouse/wasm_lake_reader.go`);
  rows carry no `contract_wasm_hash` column by decision. Live drift is
  caught by `stellarindex-ops wasm-drift`; history is enumerated from
  Galexie by `wasm-history`, `wasm-history-merge-jsonl` and
  `extract-wasm-from-galexie`.
- Aquarius has an `UPGRADE_DELAY = 259200s` (3 days) governance window
  with an emergency-mode bypass, so an upgrade can land with no notice.
- Per-connector WASM inventories are the audit logs and
  [decoder-wasm-matrix.md](../operations/wasm-audits/decoder-wasm-matrix.md);
  decoder upgrade notes live in `internal/sources/<venue>/README.md`.
  SDEX is classic and has no WASM.

Wire shapes that caught us, confirmed against mainnet captures:

| Source | Shape |
|---|---|
| Soroswap | `topic[0]` is `ScvString` `"SoroswapPair"`/`"SoroswapFactory"`, not a Symbol; the event name is a Symbol in `topic[1]`; body is an `ScvMap` keyed by Symbol field names |
| Phoenix (legacy pools) | both topics `ScvString`; body is a bare scalar (`ScvAddress` or `ScvI128`), 8 events per swap sharing `(ledger, tx_hash, op_index)` |
| Phoenix (map pools) | `swap` / `provide_liquidity` / `withdraw_liquidity` emit one event, a single `ScvSymbol` topic and an `ScvMap` body; `classifyAny` picks the shape |
| Aquarius | topics `[Symbol("trade"), token_in, token_out, user]`; body is a positional `ScvVec` of 3 i128 (in, out, fee), arity-guarded with `scval.AsTupleN(body, 3)`; the "user" is usually the router contract |
| Reflector | `#[contractevent]` wraps even one field in a Map: `{"update_data": Vec<(Val, i128)>}`; the timestamp is `topic[2]`, u64 **milliseconds**; `Asset::Other(Symbol)` is fiat or crypto, tried in that order (ADR-0010, ADR-0014) |

Fixtures are per version, `test/fixtures/<venue>/<wasm_hash or vN-date>/`.
A decoder PR without the fixture it targets cannot be reviewed.

## No stellar-rpc in production ingest

`internal/stellarrpc` serves only the `rpc-probe` diagnostic, a few
read-only ops checks and fixture capture. `stellarindex-indexer` MUST NOT
import it. `scripts/ci/lint-imports.sh` enforces the allowlist
(`internal/stellarrpc/`, named files under `internal/ops/`,
`scripts/dev/`, `/decode.go`, `/factory_seed.go`, `_test.go`) together
with the xdr-in-`internal/scval` rule (ADR-0013) and the no-Horizon rule
(ADR-0001). Legacy violations sit in `scripts/ci/lint-imports.baseline`,
which may only shrink. It runs as `make lint-imports` and in the
`repo-gates` CI job.

Fixtures are captured over RPC by `scripts/dev/capture-{aquarius,phoenix,reflector,soroswap}-fixtures.sh`
against `mainnet.sorobanrpc.com`. That is fine because RPC and the LCM
carry byte-identical `xdr.ContractEvent` payloads. Integration tests that
need Galexie use a MinIO testcontainer seeded with a recorded `.xdr.zst`,
never a live RPC call.

## The sep41 zero-writer hole (2026-07-13 to 2026-07-27)

After the sole-writer deploy at ledger 63,419,139 (2026-07-13) the dispatcher skipped the sep41 domain while the projector never registered the sep41 sources: `BuildRegistry` builds only from `enabled_sources` and the sep41 names were not in `KnownSources`, so no config could carry them. Both sep41 tables froze at ledger 63,419,138 (249k mismatched ledgers). Fixed in `ae7a082d` (registry always attempts sep41 from the watched set, plus a regression test pinning the production shape). Catch-up used `projected-rebuild -source sep41_supply -from 63419138 -write -allow-live-overlap`. Lesson: when a domain moves to one writer, a test must build the registry the way production does. Source: `git show 52aacb972:docs/operations/v1-launch-plan.md`, lines 3822-3832.
