# Aquarius — contract & event verification

> **For the Aquarius team:** this is how Rates Engine attributes Aquarius
> pools and the router. Please confirm the router address and help us with
> the **open question below** about enumerating the complete pool set.
>
> - **Enumeration method:** lake event observation (pools emit `trade` /
>   `update_reserves`; the router emits `swap` / `add_pool`). Pool *creation*
>   isn't cleanly in the lake, so the complete set needs the router's pool
>   registry.
> - **Last verified:** 2026-06-12 (r1 lake).
> - **Gate status:** ⏳ pending (pool enumeration not yet pinned).

## Router

| Role | Contract | Lake events |
|---|---|---|
| Liquidity-pool router | `CBQDHNBFBZYE4MKPWBSJOPIYLW4SFSXAXUTSXJN76GNKYVYPCKWC6QUK` | `swap` ×55,925, `config_rewards` ×4,351, `add_pool` ×6, `commit_upgrade`, `apply_upgrade`, `set_protocol_fee`, … |

The router is the user entry point; it proxies to the correct pool, which
emits the trade event.

## Pools (lake counts)

- **~177 contracts** emit `trade` (153,297 events, 56.67M → 62.99M).
- **~180 contracts** emit `update_reserves` (156,119 events) — the
  overlap with the `trade` emitters is our confident Aquarius pool set.
- 16–17 also emit `deposit` / `withdraw`.

The full pool address list is derivable from the lake; we'll attach it once
the open question is settled.

## ⚠️ Open question (please advise)

We can *observe* ~177 pools emitting `trade`, but we can't reliably
*enumerate* the complete, authoritative pool set from on-chain events:
the router's `add_pool` event appears only 6 times in our window, far
fewer than the 177 active pools (most pools predate our lake or were
registered another way).

1. What is the **authoritative way to enumerate all Aquarius pool
   addresses** — a router view function (a pool registry / `get_pools`),
   or a creation event we should be subscribing to?
2. Is the `trade` topic emitted **only** by Aquarius pools, or do other
   contracts share it (so we can't gate on the topic alone)?

## Events decoded

| Source (topic[0]) | Where it lands |
|---|---|
| pool `trade` | `trades` (source=aquarius) |
| pool `deposit` / `withdraw` / `update_reserves` / `reserves_sync` | liquidity / reserves tracking |
| router `add_pool` | (candidate) pool registration |
