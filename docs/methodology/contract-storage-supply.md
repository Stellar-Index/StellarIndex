---
title: Contract-storage supply
last_verified: 2026-11-21
status: current
---

# Contract-storage supply

**Status:** implemented · `supply_basis = contract_storage_balances`
**Measured:** pubnet, 2026-09-15, against the certified ClickHouse lake on r1

A token's supply summed from the per-holder **balance ledger entries** in its
own Soroban contract storage, for tokens the SEP-41 / CAP-67 event log cannot
see.

---

## 1. The gap this closes

The supply pipeline derives supply from events — `Σmint − Σburn − Σclawback`
over `stellar.supply_flows`. That is complete for every token that *emits*
events, classic ones included: CAP-67 makes a classic issuer payment emit a
mint, which is why the lake holds 71.7M pre-Soroban flow rows back to ledger
7,863.

It is blind to a token that emits **none**.

Such a token is not undercounted in `supply_flows`. It is **absent** from it,
and a contract with no rows sums to `0`. Zero is a *claim* — "this token has
been fully burned" — and it is served with the same confidence as a measured
figure. Twenty-four private-credit deal tokens on pubnet were in exactly that
state: `GET /v1/assets/{contract}/supply` returned `total_supply: "0"` for every
one of them, while their contract storage held **548,113,042.88 tokens**.

The data was never missing. `stellar.ledger_entries_current` carries
`entry_type = 'contract_data'` with the raw `entry_xdr` — 616,854,424 rows. The
gap was that nothing read it.

---

## 2. It is a different basis, not a better reading

Every other supply basis in the vocabulary accumulates **issuance**. This one
measures **distribution**.

| | Event-derived | Storage-derived |
|---|---|---|
| Quantity | `Σmint − Σburn − Σclawback` | `Σ(held balances)` |
| Shape | an accumulation over history | a level, right now |
| Correct when | the log is complete from the token's first ledger | the lake captured every balance entry |
| Fails by | silently understating, to the point of `0` | missing entries the lake never captured |

They answer different questions and can legitimately differ. Where the event log
*is* complete they converge, and that was measured rather than assumed — see §5.

---

## 3. The handover rule: storage supersedes, never sums

A token whose event log starts partway through its life has both a storage level
and an event accumulation. **They are never added.** The same tokens are in
both; adding them double-counts every holder.

The two differ in what can make them wrong:

- A **level** measures what exists now. It carries no history, so it cannot be
  made wrong by missing history.
- An **accumulation** is only ever as complete as its log.

The serving path picks one reading per Soroban token, in one function
(`storageReadingSupersedes`, `internal/api/v1/asset_supply.go`) that
`/v1/assets/{id}/supply`, the asset detail and the RWA contract listing all
use:

- **No recorded flows:** the storage level is served; if storage offers no
  reading, nothing is.
- **Storage level above the event net:** the storage level is a floor, so a
  level above the net proves the log missed issuance (it starts partway
  through the token's life). The storage level is served as a floor, unless it
  disagrees with the contract's own declared total or holder count
  (`supply_consistent: false`); then it proves nothing and the event net
  stands. How each route marks the floor:
  - `/v1/assets/{id}/supply`: `source: contract_storage_balances` with
    `circulating_supply_lower_bound: true`.
  - RWA rows: `supply_basis: contract_storage_balances` with
    `circulating_supply_lower_bound: true`; a market cap built on it carries
    `market_cap_lower_bound: true`.
  - `/v1/assets/{id}`: `supply_basis: contract_storage_balances`, and no
    `market_cap_usd` or `fdv_usd` is built on it, since the detail has no
    lower-bound flag for either.
- **Storage level at or below the event net, or storage declines by design:**
  the event net is served (`source: mint_burn_flows`), unflagged. The reader
  declines a Stellar Asset Contract, a contract with no captured instance entry
  (the SAC check cannot run), a contract with no balances, and a holder set
  above its 25,000-entry cap; for those the event net is not checked against
  storage at all.
- **Storage read failed** (any other error, or the request budget ran out) on a
  token with flows: the event net is withheld, not served as a total.
  `/v1/assets/{id}/supply` returns a `supply-error` problem (502, or the 503
  timeout problem), and the asset detail and RWA listing mark the supply
  unread so the gap is not cached. A flowless token whose storage read fails
  keeps its no-reading outcome (404 on `/supply`, no figure elsewhere), except
  that a spent budget is unread on every route.

The figure is always one reading or the other. A log that is partial but still
nets above the storage level is not detected, because `events_start_from` is
not consulted. A classic asset's Stellar Asset Contract keeps its balances in
trustlines, so its storage is asked only when its log is empty.

Events remain valuable against this basis as a **cross-check**, never as an
addend.

---

## 4. It is a lower bound, and a different kind from the trustline sum

The lake never records an eviction: a balance entry whose TTL lapsed keeps its
last-known row in the current-state projection exactly as if it were live. So
every summed balance is judged against its TTL at the lake tip:

- a lapsed **temporary** entry has been deleted by the network and is dropped;
- a lapsed **persistent** entry has been archived, not destroyed — still owned,
  restorable, and counted by the contract's own `TotalSupply` — so it stays in
  the sum and is disclosed as `archived_balance_entries` /
  `archived_balance_total`;
- an entry with no TTL row is kept: only a proven lapse justifies a drop.

What the reading cannot see is a balance entry the lake's current-state
projection never captured — one dormant since before that projection's
coverage began. The figure is therefore a floor, and carries
`circulating_supply_lower_bound` on the wire.

This is a different blindness from the classic trustline sum's:

- the **trustline sum** misses whole holding *domains* — claimable balances,
  liquidity-pool reserves, SAC-held contract balances;
- **this** misses *entries outside the projection's coverage*.

Where a contract publishes its own `HolderCount`, the reading reports whether
that count matched the number of entries we could see. A disagreement is exactly
what a missing entry looks like from here, and it is surfaced
(`supply_consistent: false`) rather than hidden — the figure is still served,
because a mostly-complete measurement is worth more than no measurement, but it
is never presented as exact.

---

## 5. Validation

### 5.1 The instrument check

Three event-emitting Wasm tokens were summed **both ways**. Storage sum against
`Σmint − Σburn − Σclawback`, same day, same lake:

| Token | Contract | Storage sum | Event-derived net | Δ |
|---|---|---|---|---|
| EUTBL | `CBGV2QFQ…` | 28,327,867,109,034 | 28,327,867,109,034 | **0** |
| USTBL | `CARUUX2F…` | 3,621,637,634,835 | 3,621,637,634,835 | **0** |
| deJTRSY | `CBI7UCH5…` | 8,763,619,974,700,234,898,508,352 | 8,763,619,974,700,234,898,508,352 | **0** |

Exact to the unit, across three different scales (5, 5 and 18 decimals) and two
value encodings. That is what licenses the basis: when both instruments can
measure, they agree.

### 5.2 The negative control

KALE (`CB23WRDQ…`) — a **Stellar Asset Contract** — disagrees by 6.8×:

| | |
|---|---|
| Storage sum | 471,938,508,419,832 |
| Event-derived net | 3,224,226,487,856,012 |

This is **not** a decoder fault. A SAC's storage holds only the slice of a
classic asset wrapped into Soroban; the rest sits in trustlines, claimable
balances and liquidity-pool reserves. The storage sum is a correct answer to a
question nobody asked, and nothing about it looks wrong on its own — which is
why the reader **refuses SACs structurally**, keyed on the instance executable
type, rather than with a plausibility check on the number.

### 5.3 The twenty-four

All 24 deal tokens, read through the production reader against r1:

- **24/24** — our balance sum equals the contract's own declared `TotalSupply`,
  exactly.
- **24/24** — our balance-entry count equals the contract's own declared
  `HolderCount`, exactly. No entry is missing from view.
- **24/24** — decimals **read from the chain**, not borrowed (§6).
- Total: **5,481,130,428,800,000 raw = 548,113,042.88 tokens.**

---

## 6. Decimals are read, never assumed

The scale is on-chain and is read from the contract's own instance storage.
This was the single largest risk in the work and it resolved in our favour.

The prior reading of these contracts concluded they carry **no `METADATA` key at
all**, so their exponent was not derivable from the chain and had to be borrowed
from a third-party seed file. That is wrong. The exponent **is** on-chain — it
sits under a `Config` map rather than `METADATA`, and under a key encoded as a
single-element vector (`Vec[Symbol("Config")]`, Rust's derived encoding for a
fieldless enum variant) rather than a bare symbol. Reading only the token-sdk
spelling found nothing and reported "no metadata".

So the scale is read on two axes. The **map** is `METADATA` or `Config`, each
under either key encoding. The **field** inside it is `decimal` — the
soroban-token-sdk spelling — or `decimals`, which hand-written tokens use.
Every combination is read, and blended never: a contract declaring two different
scales for itself — two fields in one map, or two maps — has not told us its
scale, and the reader refuses rather than picking one. The field axis is
measured in
[the RWA definition](rwa-definition.md#valuing-a-contract-asset).

The distinction matters because it is the difference between a published money
figure resting on our own measurement and one resting on someone else's
spreadsheet. A wrong exponent is a supply figure wrong by a power of ten.

Where the chain declares no scale, `decimals` is **omitted** from the response
and the raw figure is still served. A consumer must not substitute a default.

---

## 7. Why the reader is general, not a list of addresses

The twenty-four addresses are third-party data with a stated provenance, and the
obvious alternative was to bring them in as curated bindings. The reader is
**general over any contract** instead, and the reason is that the safety comes
from on-chain evidence rather than from the list:

- **SACs are refused** on their instance executable type (§5.2).
- **Decimals must be declared on-chain**, or no decimalised figure is published
  (§6).
- **The contract's own `TotalSupply` and `HolderCount`** cross-check our sum
  where published, and the result is surfaced (§4).
- **An oversized holder set is refused**, not truncated — a truncated sum is a
  wrong figure that looks exactly like a right one.
- **A negative balance refuses the whole reading** — impossible under correct
  token accounting, so it means the wrong field was decoded.

None of those checks needs to know *which* token it is looking at. A curated list
would have to be maintained, would go stale, and would leave every other
event-blind token still reporting zero — while adding no safety the on-chain
checks do not already provide.

Naming the deal tokens as *data* — in this document, in tests, in the directory
that decides what is worth displaying — remains normal and necessary. The
distinction is that the **reader** does not branch on identity.

---

## 8. Cost

The lookup rides `ledger_entries_current`'s `(entry_type, key_xdr)` primary key.
A contract-data key opens with 40 fixed bytes — entry type, address
discriminant, 32-byte contract id — so the first 52 base64 characters are a
primary-key prefix range for exactly one contract, not a scan.

Within that range, balance, holder-count and instance keys are picked out by
fixed base64 windows at a fixed offset (a whole number of base64 groups falls at
byte 39, so every following byte lands at a constant character position). These
are plain string comparisons on the stored column — no `base64Decode`, no
per-row XDR parse.

The markers are a **pre-filter only**. Every surviving row is decoded and
re-checked in Go, because the marker proves the key's shape and not the value's
meaning.

The API caches each contract's reading, or the reader's decline, with one
fill in flight per contract, as it does the event net. A reading is fresh
for five minutes. After that the cached reading is still served at once
while one refresh runs. A failed fill or refresh stores nothing, but a
refresh that fails leaves the previous reading in place, so it keeps being
served.

Known limitation: a served reading carries no age. If refreshes keep
failing, the last good reading is served with no upper bound on its age,
and the response's `as_of_ledger` and `flags.stale` come from the lake
watermark, not from when that reading was taken. A frozen reading can
therefore be stamped as current.

---

## 9. What this does not do

- **No price.** These tokens are not listed on any price feed we consume. The
  figure is **supply-only**. The contracts do publish a self-declared `Nav`
  (all 24 at `1.0000000`, with its own on-chain `decimals`), which is why the
  supply figure and the third-party dollar figure coincide — but an
  issuer-declared NAV is not a market price and is not wired into valuation
  here.
- **No circulating-vs-total distinction.** The sum is every held balance. No
  locked-set or admin exclusion is applied, because the entries carry no
  admin identity.
- **No history.** A level has no past. Storage-derived supply cannot be
  back-dated, and the supply history tables are untouched by this basis.
