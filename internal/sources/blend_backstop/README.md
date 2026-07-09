# `internal/sources/blend_backstop`

Decoder for the **Blend Backstop** contract on Stellar (Soroban) — the
protocol's insurance / shared-liquidity module.

This is a **separate event surface** from the Blend pool /
pool-factory decoder in [`internal/sources/blend`](../blend/). The two
share neither contract addresses nor event vocabulary; do **not** fold
this package into that one.

## Scope

Two backstop contracts (gate on **both** — a backfill range replays
either):

| Contract | Address |
|---|---|
| Backstop V2 (current) | `CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7` |
| Backstop V1 | `blend.MainnetBackstopV1` (re-exported) |

Twelve event types (`topic[0]` = Symbol). V1 and V2 diverge on three of
them (`gulp_emissions` arity, and the `rw_zone`/`rw_zone_add` rename) —
see the Provenance section below for how each shape was verified:

| Event | Topics | Body | Promoted |
|---|---|---|---|
| `deposit` | `[sym, pool, user]` | `Vec[i128 tokens_in, i128 shares_minted]` | pool, user, amount=tokens_in, amount2=shares_minted |
| `claim` | `[sym, user]` | `i128 amount` | user, amount (no pool) |
| `donate` | `[sym, pool, from]` | `i128 amount` | pool, amount; `from`→attrs |
| `queue_withdrawal` | `[sym, pool, user]` | `Vec[i128 shares, u64 expiration]` | pool, user, amount=shares; `expiration`→attrs |
| `withdraw` | `[sym, pool, user]` | `Vec[i128 shares_burned, i128 tokens_out]` | pool, user, amount=tokens_out, amount2=shares_burned — note the body element ORDER is opposite deposit's (shares first, tokens second); Amount is normalized to token quantity so it means the same thing across both events |
| `distribute` | `[sym]` | `i128 amount` | amount only |
| `gulp_emissions` (V2) | `[sym, pool]` | `Vec[i128 new_backstop_emissions, i128 new_pool_emissions]` | pool, amount=data[0], amount2=data[1] |
| `gulp_emissions` (V1) | `[sym]` (no pool topic) | `i128` (bare, single amount) | amount only; Pool left NULL — never guessed |
| `dequeue_withdrawal` | `[sym, pool, user]` | `i128 amount` | pool, user, amount |
| `draw` | `[sym, pool]` | `Vec[Address to, i128 amount]` | pool, amount=data[1]; `to`→attrs |
| `rw_zone_add` (V2) | `[sym]` | `Vec[Address to_add, Option<Address> to_remove]` | pool=to_add; `to_remove`→attrs (omitted when void — the observed case in all 5 lake rows) |
| `rw_zone` (V1) | `[sym]` | `Vec[Address to_add, Address to_remove]` | pool=to_add; `to_remove`→attrs (no Option wrapper — both addresses always present) |
| `rw_zone_remove` (V2) | `[sym]` | `Address` (bare) | pool. **Unverified**: zero lake occurrences ever; decoded from the Blend team's published source, which has a doc-comment/code mismatch for this one function (see decode.go) |

## Symbol overlap — why the contract gate is load-bearing

The backstop's `claim` / `withdraw` / `queue_withdrawal` /
`gulp_emissions` symbols **collide** with Blend **pool** event symbols.
`Matches()` therefore gates on `IsBackstopContract(contract_id)` AND
`Classify() != ""` — never on the symbol alone. This is the
ADR-0035 factory-anchored gating model + the "Comet uses a shared
topic" trap in CLAUDE.md: a look-alike symbol from a non-backstop
contract must never mint a backstop row.

## Robustness

Per CLAUDE.md "EVERY-event" + "decode by field, degrade gracefully":

- A **promoted** field whose SCVal shape doesn't match (e.g. a
  `donate` `from` topic that isn't an Address) is left empty and the
  raw note is stashed in `attributes` (`*_error` key) rather than
  erroring the whole row.
- A **genuinely malformed** event (wrong arity, body that is neither
  i128 nor the expected Vec, an un-parseable required amount) returns
  an error — the dispatcher counts + skips it.
- i128 amounts round-trip through `canonical.Amount` / `*big.Int` per
  ADR-0003 and are emitted as decimal strings — never `int64`.

## Provenance — LIVE-CAPTURE ONLY

The per-event field layouts in `decode.go` / `events.go` were
originally **reverse-engineered from real mainnet lake samples on
2026-06-15** and validated against the golden frames in
`decode_test.go`.

**2026-07-09 update:** a read-only lake audit cross-checked the V2
shapes directly against the Blend team's published source
(`blend-contracts-v2` `backstop/src/events.rs`) and found six decode
bugs, all now fixed (see decode.go's package doc + CHANGELOG.md):

1. V1 `gulp_emissions` was 100% mis-decoded — wrong topic arity (1, not
   2) and wrong body shape (bare `i128`, not a 2-Vec).
2. V1's reward-zone-update topic is `rw_zone`, not `rw_zone_add` —
   Classify() never matched it (5 real events, silently dropped).
3. V2 `rw_zone_add`'s second body element is `Option<Address>`, not a
   `u32` index.
4. `rw_zone_remove` was unimplemented (zero lake occurrences ever —
   added per the EVERY-event principle; **unverified against real
   bytes**, see decode.go's source note on a doc-comment/code mismatch
   in Blend's own repo for this one function).
5. `gulp_emissions`' topic[1] is the pool address, not a "token".
6. `withdraw`'s body order is `(shares, tokens)` — opposite deposit's
   `(tokens, shares)` — but was promoted positionally; Amount now
   consistently means "token amount" across deposit + withdraw.

V1 has no published source available to us; its topic arities and body
shapes (other than the gulp_emissions/rw_zone divergences above) are
pinned against real lake bytes only, not an upstream source read.

Consequence: this source remains **live-capture only**. The fixes
above are schema-*correctness* fixes for the decoder as it runs going
forward — they do not retroactively repair rows already written under
the old (buggy) mappings. A historical replay
(`stellarindex-ops projector-replay -source blend_backstop -from
51499923`) is required to both backfill V1 genesis→now and correct
previously-stored `withdraw` / `gulp_emissions` / `rw_zone_add` rows;
see CHANGELOG.md. Do **not** flip any `BackfillSafe`-equivalent
posture beyond that replay without further confirmation — a drift in
the V1-only, lake-only-verified shapes would silently mis-attribute
backfilled rows.

## Wiring

- `dispatcher_adapter.go` — `Decoder`, a stateless topic Decoder gated
  on the two known backstop contracts.
- `consumer.go` — projects each decoded event into the
  `blend_backstop.Event` `consumer.Event`. One row per event.
- `internal/pipeline/sink.go` — `IsProjectedEvent` arm +
  `persistBlendBackstopEvent` writes via
  `Store.InsertBlendBackstopEvent`.
- `internal/projector/registry.go` — `buildSource` registers
  `blend_backstop.NewDecoder()` (projector is sole writer in Phase-4).
- Storage: `blend_backstop_events` hypertable, migration
  [`0063_create_blend_backstop_events`](../../../migrations/0063_create_blend_backstop_events.up.sql).
- `internal/storage/timescale/per_source_gaps.go` — gap target
  (`blend-backstop`, Genesis ≈ 56,627,571).
- `internal/storage/timescale/protocol_stats.go` — `blend_backstop`
  leg in the trailing-24h event census.

## Tests

- `decode_test.go` — golden frames built from real base64 lake samples
  (`deposit`, `claim`, `distribute`, `queue_withdrawal`, `withdraw`,
  V1+V2 `gulp_emissions`, V1 `rw_zone`, V2 `rw_zone_add`) pin the decode
  of the promoted fields; plus `Classify` coverage and
  short-topic / malformed-body guards. `rw_zone_remove` has no lake
  occurrences, so its test is synthetic-from-source and marked as such
  (`TestDecodeRwZoneRemove_SyntheticFromSource`).
- `dispatcher_adapter_test.go` — `Matches` gating (backstop symbol from
  backstop vs non-backstop contract, non-backstop symbol from backstop
  contract) + end-to-end `Decode`.
