---
title: Checklist — add an on-chain source (Soroban)
last_verified: 2026-09-09
status: current
---

# Checklist — add an on-chain source (Soroban DEX / event decoder)

Reference implementation: `internal/sources/soroswap/`. Binding rules: AGENTS.md
invariants #6 (ingest path), #7 (one writer per domain), ADR-0035 (gating).
**Before writing any helper, check `/CAPABILITY-INVENTORY.md` — reuse `internal/scval`,
`canonical.Amount/Trade/Asset`.**

## 1 — Create the package `internal/sources/<name>/`
- [ ] `README.md` — cross-link `docs/protocols/<name>.md`; document the event topic
      shapes + quirks.
- [ ] `events.go` — event-identifier constants + `classify()` topic predicates + the
      `consumer.Event` wrappers (`TradeEvent{Trade canonical.Trade}` with
      `EventKind()`/`Source()`, plus `var _ consumer.Event = TradeEvent{}` compile-check).
      Amounts are `canonical.Amount` (i128 → never int64, ADR-0003).
- [ ] `decode.go` — pure `events.Event → canonical.Trade`; decode SCVals via
      `internal/scval`; **decode by Map field NAME, not position** (schema-evolution rule).
- [ ] **`dispatcher_adapter.go` — the production seam.** Implement `dispatcher.Decoder`:
      `Name() string`, `Matches(events.Event) bool`, `Decode(events.Event) ([]consumer.Event, error)`,
      + `NewDecoder(...)`. **`Matches()` MUST gate on contract identity** (a registered
      pool/factory set), not topic bytes (ADR-0035 — topic symbols collide across every AMM).
- [ ] `*_test.go` — table-driven unit tests + a real-mainnet-fixture test walking
      `test/fixtures/<name>/<wasm-version>/` (copy `soroswap/real_fixture_test.go`).
      Regenerate fixtures with `scripts/dev/capture-<name>-fixtures.sh`.

## 2 — Wire it (6 edits — miss one and the source silently emits nothing)
- [ ] `internal/config/validate.go` → add the name to **`KnownSources`** (map ~L31).
- [ ] `internal/pipeline/dispatcher.go` → `BuildDispatcher`: `case <name>.SourceName:`
      appending `<name>.NewDecoder(...)` to `decoders` (or `opDecoders`/`callDecoders`).
- [ ] `internal/pipeline/sink.go` → `HandleEvent`: `case <name>.TradeEvent:` → your
      `persistTrade(...)` / `Store.Insert<X>` writer.
- [ ] `internal/pipeline/sink.go` → **`IsProjectedEvent`**: add your event types to the
      projected switch **if** this is a projected Soroban source (writes via `soroban_events`).
- [ ] `internal/projector/registry.go` → `buildSource`: `case <name>.SourceName:` returning
      a `Source{Decoder:…}` — required for any projected source.
- [ ] `internal/sources/external/registry.go` → `Registry` map: a `Metadata{Class, Subclass,
      IncludeInVWAP, BackfillSafe:false, …}` entry (`BackfillSafe` stays false until a WASM audit).

For a contract-gated (factory-anchored) source, also add a `GatedMeta` entry in
`internal/pipeline/gated_registry.go` and forward `gated[source]` in the two wiring points (see `blend`).

## 3 — Storage
- [ ] Add a migration (→ [add-migration.md](add-migration.md)) + the `Store.Insert<X>`
      writer/reader in `internal/storage/timescale`.

## 4 — Actually run it (the step that shipped broken twice)
Everything above makes the decoder *possible*. Only this makes it *happen*: the
deployed indexer runs the sources named in `[ingestion] enabled_sources`, and that
list comes from **one** place.
- [ ] `configs/ansible/roles/archival-node/defaults/main.yml` →
      **`stellarindex_enabled_sources`**. If the source is not ready to serve, put it
      in the sibling **`stellarindex_sources_not_yet_enabled`** map with a reason, and
      keep it out of the compute-completeness catalogue
      (`internal/ops/chops/reconciliation_catalogue.go`) until it is — a source we do
      not run must not publish a verdict about itself.

Skipping this is silent by construction: the archive stays complete, the lake holds
the events, `/v1/coverage` reports the source INCOMPLETE forever with
`watermark_ledger` one below genesis, and the served tier is empty. `sushiswap_v3`
sat that way until 2026-09-09; `upshift` (#503) was found hours later.

### 4b — Seed the contract gate (contract-gated sources only)

A gated decoder (ADR-0035/0040) only decodes events from contracts in its
`contractid.Registry`, and that registry is warmed by
`pipeline.GatedRegistryOptions` from the `protocol_contracts` table. Which half
of this you owe depends on the gate mechanism:

- [ ] **Factory-anchored** (`Factories` non-empty — blend, aquarius, defindex,
      phoenix, sushiswap_v3): the children are discovered from the factory's
      creation events, so there is nothing in code to seed them with.
      `stellarindex-ops seed-protocol-contracts -source <name>` is a **deploy
      precondition** (ADR-0040 §2.4) — run it once the lake covers the factory
      genesis. Until it runs the warm logs a WARN naming the source and the
      remedy; a fresh host legitimately sits there for a while, which is why the
      indexer warns rather than refusing to boot.
- [ ] **Curated-set** (`Factories` empty — comet, blend_emitter, upshift; ADR-0040
      §1 mechanism 3): the trust root is the in-code `<pkg>.MainnetGatedSet()`.
      Put it on the `GatedMeta` entry's **`CuratedSet`** field. That field is the
      whole mechanism: the warm seeds it into every registry it builds and, on the
      indexer path, reconciles it into `protocol_contracts` so the roster reads
      (`GET /v1/protocols/{name}`, the explorer's contract attribution) agree with
      the gate. No operator command is required. Omitting it declares a gate with
      no trust root — every event dropped, and `seed-protocol-contracts` reporting
      "upserted 0 child contract(s)" with exit 0.

`upshift` shipped with `CuratedSet` set but nothing outside the CLI reading it:
on r1 (2026-09-09) `protocol_contracts` held aquarius 352, blend 29, defindex 16,
sushiswap_v3 58 and upshift **0**, `/v1/protocols/upshift` served an empty
contract roster, and the only line about it read
`gated registry warmed source=upshift factories=null children=0` — a line that
reads identically for a protocol with no pools yet.

## Guards that will catch mistakes
`TestIsProjectedEvent_TableDriven` fails if the sink/projector arms drift; `config.Validate`
rejects an `enabled_sources` name missing from `KnownSources`; `lint-imports.sh` enforces
boundaries; `scripts/ci/lint-source-enablement.sh` fails when a `KnownSources` name is
neither enabled nor declared not-yet-enabled, and when a declared-unrun source is still
tracked by the completeness catalogue;
`TestGatedSources_curatedOnlyDeclaresTrustRoot` fails when a curated-only `GatedMeta`
entry declares no `CuratedSet`, and
`TestGatedRegistryOptions_curatedSetSeededWithEmptyTable` fails if the registry warm
stops installing that set for a source whose `protocol_contracts` rows are missing.

## Done when
Unit + fixture tests pass; `bash scripts/dev/verify.sh` is green; enabling the source and
running against a known ledger range produces rows in its table. Catch-up is
`stellarindex-ops projector-replay -source <name> -from <ledger> -write` (fail-closed:
no `-write` = dry run) — **never** a bespoke
`<name>-backfill` subcommand.
