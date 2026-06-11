---
title: Ingest pipeline — the one canonical data path
last_verified: 2026-06-12
status: binding
---

# Ingest pipeline

**Every byte of on-chain data flows through one path:**

```
Stellar pubnet
    │   (SCP via Galexie's captive-core)
    ▼
galexie                    ← the SINGLE stellar-core on r1 (ADR-0002 + CDP pattern)
    │   (writes .xdr.zst to)
    ▼
MinIO galexie-live         ← S3-compatible object store
    │
    ▼
internal/ledgerstream/     ← SDK BufferedStorageBackend wrapper
    │   Stream(ctx, from, to) yields xdr.LedgerCloseMeta per ledger
    │   (StreamArchiveThenLive seams archive replay → live tail)
    ▼
internal/dispatcher/       ← single consumer of ledgerstream
    │   per tx, fed to four registered decoder seams (see Rule 3):
    │     • Decoder                 — Soroban contract events (topic[0] byte-match)
    │     • OpDecoder               — classic operations (e.g. SDEX, change_trust)
    │     • ContractCallDecoder     — InvokeContract ops with no events (Band relay())
    │     • LedgerEntryChangeDecoder — LedgerEntry mutations (supply observers)
    ▼
internal/sources/{soroswap,aquarius,phoenix,reflector,sdex,band,…}/
    │   each is a pure decoder + (optional) per-source correlation
    │   state (Soroswap swap+sync, Phoenix 8-field assembly).
    │   NO goroutines, NO RPC clients, NO pagination loops.
    │   decode(...) → canonical.Trade | canonical.OracleUpdate | event
    ▼
internal/pipeline/sink.go  ← fans each decoded item to its destination:
    │
    ├─► ClickHouse structural lake (ADR-0034) ──── the CERTIFIED raw history.
    │     LiveSink writes every ledger + contract_event to ClickHouse;
    │     ledgers are contiguous + hash-chained to genesis. This is the
    │     substrate that proves "100% coverage" (ADR-0033).
    │
    ├─► soroban_events landing zone (ADR-0029, Postgres) ── raw Soroban events
    │     │   tailed by ↓
    │     ▼
    │   internal/projector/  ← the ONE writer for Soroban-derived per-source
    │     │   tables (trades, blend_*, phoenix_*, comet_*, soroswap_skim,
    │     │   cctp_events, rozo_events, sep41_*, reflector/redstone oracle_updates).
    │     │   ADR-0031/0032. Catch-up = `projector-replay -source <n> -from <l>`.
    │     ▼
    └─► dispatcher events-goroutine sink ── NON-projected events write here
          directly (sdex, external CEX/FX, band, supply observers). These do
          NOT flow through soroban_events; pipeline/sink.go::IsProjectedEvent
          decides which path an event takes.
              │
              ▼
    Postgres / TimescaleDB  ← the SERVED tier (ADR-0034): the recent working
    │   set the API queries, NOT the full archive. Verified faithful within
    │   what it holds (ADR-0033 projection reconcile, retention-scoped).
    ▼
/v1/* API
```

**Backfill and live-tail share the streaming code, but backfill
re-derives from the lake, not a fresh MinIO walk.** Live tail is
`internal/ledgerstream.Stream(ctx, from, 0)` (unbounded). Decoder
backfills re-derive from the certified ClickHouse lake (SQL /
`ch-rebuild`); projected-source catch-up is `projector-replay`. There
are no separate `BackfillRange` / `StreamLive` methods on sources, and
no per-source `<source>-backfill` subcommands (the whole family was
deleted in rc.97 / ADR-0032 Phase 5).

---

## Binding rules

### 1. No stellar-rpc in production ingest

`internal/stellarrpc/` exists only for:
- `ratesengine-ops rpc-probe` — operator diagnostic against a
  public endpoint.
- Development-time fixture capture via scripts in
  `scripts/dev/capture-*-fixtures.sh`.

`ratesengine-indexer` MUST NOT import `internal/stellarrpc`. Any
source's ingest path that calls `rpc.GetEvents` or
`rpc.LatestLedgerSequence` is wrong and blocks merge. This was
established 2026-04-23 when stellar-rpc was removed from r1 (see
`docs/operations/r1-deployment-state.md`). The fact that stellar-rpc
returns the same base64 SCVal strings as ledger-meta decoding is a
coincidence, not an architectural option — only one of those paths
exists in production.

### 2. Source packages are pure decoders

Each `internal/sources/<venue>/` package exports:
- `SourceName` constant.
- `TopicPrefix*` / `TopicSymbol*` pre-encoded SCVal bytes (for the
  dispatcher's byte-equality routing).
- `decode...(event | rawPair | rawSwap) → canonical.*` functions.
- Optional per-source correlation buffer (Soroswap swap+sync,
  Phoenix 8-field). The buffer's state lives in memory for the
  lifetime of one dispatcher goroutine — no per-source goroutine,
  no RPC, no cursors.

A source package MUST NOT:
- Hold a `*stellarrpc.Client`.
- Implement `consumer.Source.BackfillRange` / `StreamLive`.
- Poll.
- Paginate.

### 3. Dispatcher owns routing — four decoder seams

`internal/dispatcher/` is the single consumer of
`internal/ledgerstream`. Decoders register on the dispatcher
(`dispatcher.go`, e.g. `AddDecoder`) — there is no `routes.go`. The
dispatcher exposes **four** decoder interfaces (`dispatcher.go`),
matching the four shapes of on-chain data:

- **`Decoder`** — Soroban contract events, routed by `topic[0]`
  byte-equality against each source's `TopicPrefix*`/`TopicSymbol*`
  constants (Soroswap, Phoenix, Comet, Aquarius, Reflector, …).
- **`OpDecoder`** — classic operations (SDEX trades, `change_trust`
  supply observers).
- **`ContractCallDecoder`** — InvokeContract ops that update storage
  without emitting events (Band's `relay()`/`force_relay()`; match by
  `(contract_id, function_name)`, decode from op args).
- **`LedgerEntryChangeDecoder`** — `LedgerEntry` mutations
  (account/trustline/claimable/LP-reserve supply observers).

It is the ONLY place where contract-event byte-matching, classic-op
walking, contract-call matching, and ledger-entry-change routing
happen, and where per-source correlation state is fed.

Adding a new source means:
- Adding its decoder package under `internal/sources/<venue>/`.
- Registering it on the dispatcher via the appropriate seam.
- For a new Soroban-derived source: also adding a case in
  `internal/projector/registry.go::buildSource` AND an arm in
  `internal/pipeline/sink.go::IsProjectedEvent` (one-writer rule,
  ADR-0031/0032).
- That's it — no wire-layer code, no new goroutines.

### 4. Fixture captures stay RPC-based

`test/fixtures/<venue>/<wasm_hash>/*.json` fixtures are recorded from
`scripts/dev/capture-*-fixtures.sh` against the public
`mainnet.sorobanrpc.com` endpoint. This is fine because the event
bytes are identical whether pulled from RPC or extracted from
`LedgerCloseMeta` — both embed the same `xdr.ContractEvent` / SCVal
payloads. Using RPC for fixture capture is a convenience for
developers without r1 MinIO access.

Integration tests that need a live Galexie source use a MinIO
testcontainer seeded with a recorded `.xdr.zst` — never a live RPC
call.

---

## Why this doc exists

Agent + @ash mistake, 2026-04-23: built Task #164 (decoders) correctly
but wired the per-source `consumer.go` to `rpc *stellarrpc.Client` →
`rpc.GetEvents`. This worked for tests (public RPC endpoint) but would
never run on r1 because stellar-rpc was removed from r1 the same day.
The mistake was *consistent extension of pre-existing RPC-based code*
without auditing whether that code was still the production path.

Preventive controls put in place:

- **CLAUDE.md** "Invariants — never violate these" now has a
  dedicated rule #6 pointing at this doc.
- **CLAUDE.md** "Things that will surprise you" highlights the 2026-
  04-23 RPC removal.
- **This doc** is binding (status: binding, not "living"); it gets
  linked from every PR description that touches the ingest path.
- **CI check (live):** `scripts/ci/lint-imports.sh` blocks
  `internal/stellarrpc` imports outside the allowlist
  (`cmd/ratesengine-ops/`, `scripts/dev/`, `internal/stellarrpc/`
  itself, `*_test.go`). Soroban contract-event types now live in the
  transport-neutral `internal/events/` package (no longer in each
  source's `decode.go`). Also enforces rule B
  (xdr scoped to internal/scval, ADR-0013) and rule C (no
  Horizon, ADR-0001). Current legacy violations grandfathered in
  `scripts/ci/lint-imports.baseline`; the lint fails on NEW
  violations or on stale baseline entries so the baseline has to
  shrink monotonically. Runs via `make lint-imports`,
  `make verify`, and the `import-checks` CI job.

If you find yourself adding a `rpc *stellarrpc.Client` field to a
source struct or writing a new `BackfillRange`/`StreamLive` method:
**stop.** Your work belongs in the dispatcher or in decode.go, not
in a per-source poll loop.

---

## References

- [ADR-0001](../adr/0001-horizon-deprecated.md) — no Horizon.
- [ADR-0002](../adr/0002-minio-s3-compat-storage.md) — S3-compatible
  storage is MinIO; not local filesystem.
- [ADR-0013](../adr/0013-go-stellar-sdk-xdr-for-scval.md) — SDK
  dependency, which gives us `ingest.ApplyLedgerMetadata`.
- [ADR-0029](../adr/0029-soroban-events-landing-zone.md) — the
  `soroban_events` raw landing zone the projector tails.
- [ADR-0031](../adr/0031-data-derived-coverage-signal.md) /
  [ADR-0032](../adr/0032-per-source-tables-as-projections.md) —
  data-derived coverage + per-source tables as projections; the
  projector is the sole writer for Soroban-derived per-source tables.
- [ADR-0034](../adr/0034-tiered-clickhouse-architecture.md) —
  ClickHouse is the certified raw lake; Postgres/TimescaleDB is the
  served tier.
- [r1-deployment-state.md](../operations/r1-deployment-state.md) —
  what's actually running on r1.
- [architecture_cdp.md memory](../../../../.claude/projects/-Users-ash-code-ratesengine/memory/architecture_cdp.md) —
  the CDP pattern.
- [contract-schema-evolution.md](contract-schema-evolution.md) —
  per-contract WASM versioning (unrelated to transport; still
  applies).
