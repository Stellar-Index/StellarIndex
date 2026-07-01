---
title: D7 — Type-modeling quality
---

# D7 — Type-modeling quality

**Headline:** the bones are strong (`canonical.Amount`, `Asset` discriminated union,
`Pair` are exemplary — invariants made unrepresentable); the misses are the type
system **not being asked to enforce invariants it easily could**, and the API/wire
boundary **throwing away types the domain layer established**.

## Well-modeled (the template to emulate)
`canonical.Amount` (*big.Int wrapper, no exported field, i128 two's-complement,
JSON-string never number, Valuer/Scanner — you literally can't get an int64 out);
`canonical.Asset`+`ParseAsset` (discriminated union on AssetType, prefix-dispatch,
per-shape Validate rejecting cross-field contamination); `Pair` (base≠quote, orientation);
`FanoutOpIndex` (panics on 16-bit overflow — loud); `external.Class/Subclass` (fail-closed
Lookup); SDK-backed strkey CRC validation.

## M1 — real traps
- **M1-1 — no exhaustiveness lint for the ~8 bare-string enums, and ADR-0010 explicitly
  MANDATED one.** `.golangci.yml` doesn't enable `exhaustive`; ADR-0010 §"Four variants"
  says a CI exhaustiveness check "would be tidy… Go has `analyzers/exhaustive`" + asks for
  grep-TODO comments on each AssetType switch — **never done**. Adding a 7th AssetType
  compiles clean; every `switch a.Type` w/o default misbehaves silently. **Single
  highest-leverage fix: enable `exhaustive` (`default-signifies-exhaustive:true`),
  at least scoped to `canonical`.**
- **M1-2 — `consumer.Event` is an OPEN interface → 3 hand-synced switches** (sink.HandleEvent,
  IsProjectedEvent, projector.buildSource). The sink.go comment claims an "ADR-0030 lint
  guard catches drift" — **no such script exists**; only a runtime default + a hand-kept
  test table. **Fix:** give Event a `Persist(ctx,*Store) error` / `Projected() bool` method
  (polymorphic — "add a source" becomes one file, compiler-enforced), or generate the switch.
- **M1-3 — `HistoryGranularity` is a typed enum in storage but a bare `string` at the API
  reader boundary** (`HistoryReader.HistoryPoints(..., granularity string, ...)`), while OHLC
  does it right (`ohlcInterval` typed at the edge). Same codebase, two conventions + two
  divergent granularity vocabularies floating as loose strings.
- **M1-4 — dual-shape `/v1/assets/{slug}` has no wire discriminator (LC-040)** — clients
  shape-sniff on presence of `ticker` vs `asset_id`. **Fix (cheapest):** add a
  `kind:"currency"|"asset"` field to both shapes.

## M2 — nits
Primitive obsession on identity strings (Maker/Taker both `string` → swap invisible;
`type AccountID/ContractID/TxHash string` + ctor); `canonical.Amount` downgraded to raw
`string`+`map[string]any` in the projected event structs (blend_backstop/cctp) — needless
after Amount was made first-class; bare-string `EventType` discriminators; `auth.Tier` vs
`platform.Tier` name collision (modeling is correct — two orthogonal axes — just consider
`CredentialClass`/`Plan`); `Orient(a,b string)` re-parses structure `Asset` already holds
(same package — use `a.Type`/`a.Code`); duplicate internal `HistoryPoint` (timescale + v1).
