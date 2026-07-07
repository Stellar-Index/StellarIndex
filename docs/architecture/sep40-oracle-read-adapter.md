---
title: SEP-40 on-chain oracle read adapter — scope investigation + design
last_verified: 2026-07-06
status: proposed
---

# SEP-40 on-chain oracle read adapter

**BACKLOG #60 (RFP §6 accommodation).** This note is the investigation
+ design that gates whether a "SEP-40 oracle read adapter" is a thin
add-on, already covered, or a genuine new source. Decision record:
[ADR-0045](../adr/0045-sep40-oracle-read-adapter.md) (Proposed).

**Verdict up front:** SEP-40 as a *served wire shape* is already
shipped, and every oracle we ingest today is already a SEP-40 contract
ingested by its native mechanism. A *generic* adapter that ingests an
arbitrary SEP-40 oracle **by reading the SEP-40 interface itself**
(contract state, not events) is a real new capability — bigger than one
commit — because SEP-40 is a **storage-read** interface and many
SEP-40 oracles emit no events. It should ride the ADR-0039 contract
state reader, not the event-decode dispatcher path. Recommendation:
**defer** behind this design + the ADR, build only if an integration
target lands that we cannot ingest any other way.

## What SEP-40 actually is

SEP-40 ("Price Feed Oracle") standardises the **read interface** a
Soroban oracle contract exposes to consumers. The methods are pull
reads, not a push feed:

| Method | Returns |
| --- | --- |
| `lastprice(asset)` | `Option<PriceData>` — latest `(price, timestamp)` |
| `price(asset, timestamp)` | `Option<PriceData>` at/near a timestamp |
| `prices(asset, records)` | `Option<Vec<PriceData>>` — last N records |
| `x_last_price(base, quote)` | cross-pair `Option<PriceData>` |
| `assets()` | `Vec<Asset>` the oracle covers |
| `base()` / `decimals()` / `resolution()` | quote asset, scale, cadence |

Key facts that drive the design:

1. **SEP-40 is a read interface, not an event schema.** A compliant
   oracle stores `PriceData` in contract storage and answers reads.
   The standard says **nothing** about emitting events. Reflector and
   Redstone happen to *also* emit events; a conformant oracle need
   not. So "ingest a SEP-40 oracle" generically means **reading
   contract state**, which is architecturally different from the
   event-decode path all our on-chain sources use today.
2. **`x_*` and TWAP are not universal.** Reflector v3 has no on-chain
   `twap`/`x_*` methods (CLAUDE.md "Reflector v3 has no on-chain
   `twap`"); we compute those locally. A generic adapter cannot assume
   the richer methods exist — only `lastprice` / `prices` / `assets` /
   `decimals` are safe to depend on.

## What already exists (so we don't rebuild it)

### 1. The SEP-40 *serve* surface — DONE

`internal/api/v1/oracle_sep40.go` already exposes **our aggregated
prices** through the SEP-40 method shape:

- `GET /v1/oracle/lastprice?asset=` → SEP-40 `lastprice`
- `GET /v1/oracle/prices?asset=&records=` → SEP-40 `prices` (≤200)
- `GET /v1/oracle/x_last_price?base=&quote=` → SEP-40 `x_last_price`
- `GET /v1/oracle/latest` → richer non-SEP-40 view

This is a **passthrough over our own VWAP/TWAP**, quoting `fiat:USD`,
matching the "what an on-chain SEP-40 oracle returns" contract. It is
the *outbound* half of SEP-40 and is not what #60 is about.

### 2. The oracle *ingest* sources — already SEP-40 contracts

The three oracle sources we ingest are all SEP-40-compliant contracts,
but we ingest each by its **native** mechanism, not via the SEP-40
read interface:

| Source | Ingest mechanism | Why not SEP-40 reads |
| --- | --- | --- |
| `reflector-{dex,cex,fx}` | `("REFLECTOR","update")` **events** (`internal/sources/reflector`) | emits events → cheaper to subscribe than poll-read |
| `redstone` | `("REDSTONE")` batch **events** (`internal/sources/redstone`) | ditto; one event per batch push |
| `band` | `relay()` / `force_relay()` **ContractCall** (`internal/sources/band`) | **emits zero events** — already a "read the write" adapter |

Band is the instructive case: it is a SEP-40-ish oracle that publishes
**no events**, so we already observe its `relay()` InvokeContract call
via the dispatcher's `ContractCallDecoder`. A generic SEP-40 *state*
read is the same problem class (no event to subscribe to) generalised
from "watch the write call" to "read the stored PriceData".

### 3. The contract state reader — the right substrate (ADR-0039)

[ADR-0039](../adr/0039-soroban-contract-state-reader.md) already gives
us **read-time decode of current contract state from the lake** (used
for Blend pool state, Soroswap pair reserves). SEP-40 `PriceData` lives
in contract storage; a SEP-40 read adapter is a natural consumer of the
same reader rather than a new ingest goroutine or an RPC client
(ADR-0001/CLAUDE.md invariant: no stellar-rpc in the ingest path).

## Why a generic SEP-40 adapter is a *new* thing (and non-trivial)

A generic "ingest any SEP-40 oracle" adapter has to solve problems the
event-decode sources don't:

1. **No trigger.** Events give us a natural per-ledger trigger. A
   pure-storage SEP-40 oracle changes state with no event; we'd need
   either (a) a `LedgerEntryChangeDecoder` watching the oracle's
   storage entries, or (b) a periodic state-snapshot poll of
   `lastprice`/`prices` over the lake's latest state. Option (a) is
   preferred (event-driven off the actual storage delta, lake-native),
   but requires knowing the oracle's storage key layout per contract.
2. **A registry of *which* oracles + *which* assets.** SEP-40 says
   nothing about identity. We'd need a curated, contract-identity-gated
   set (ADR-0035/0040 pattern — the `internal/contractid` registry) so
   a look-alike contract can't inject fabricated oracle updates. This
   is the same fail-closed discipline defindex/phoenix use.
3. **Scale / decimals per contract.** `decimals()` and `base()` vary
   per oracle; the adapter must read them (not assume), and the i128
   price must be preserved end-to-end (ADR-0003), same as every other
   oracle source.
4. **Projection + completeness.** New rows in `oracle_updates` need a
   projector arm (ADR-0031/0032) and an ADR-0033 completeness treatment
   — but state-read coverage ("did we read every state change?") is a
   *different* completeness claim than event coverage, and needs
   thinking through before shipping.

None of these are exotic, but together they are clearly more than one
reviewable commit, and steps 1 + 4 need a real integration target to
design against (guessing a storage layout would repeat the
tag-1.0.0-vs-mainnet mistake that the defindex audit caught).

## Proposed shape (if/when built)

Follow the on-chain-source conventions with a state-read twist:

1. `internal/sources/sep40/` — a **generic** decoder keyed on a
   curated `contractid.Registry` of SEP-40 oracle contracts, gated on
   contract identity (never topic bytes).
2. Ingest via `LedgerEntryChangeDecoder` on each registered oracle's
   `PriceData` storage entries (event-less, lake-native), decoding
   `(asset, price, timestamp)` by name, reading `decimals()`/`base()`
   from the same state read. Fall back to a periodic
   `stellarindex-ops` state-snapshot only if the storage-delta path is
   impractical for a given contract.
3. Emit `canonical.OracleUpdate` (same sink arm as reflector/band) —
   which means it reuses `persistOracle` and needs **no** new
   hypertable, only a projector/registry wiring edit and a
   `notSunkEvents`/`IsProjectedEvent` decision.
4. Config gate `[sources.sep40]` with a curated contract list; new
   oracles fail-closed into an ADR-0033 recognition gap until seeded
   (defindex precedent).

## Recommendation

**Defer.** The serve side is done; the three oracles we care about are
already ingested; a generic reader is real work whose hardest parts
(storage-key layout, state-read completeness) cannot be designed
honestly without a concrete SEP-40 target we must ingest and cannot
reach via events/calls. When such a target appears, this note + the
ADR are the starting point; until then, building it would be
speculative infrastructure.

## References

- [ADR-0045](../adr/0045-sep40-oracle-read-adapter.md) — decision (Proposed)
- [ADR-0039](../adr/0039-soroban-contract-state-reader.md) — Soroban contract state reader (the substrate)
- [ADR-0035](../adr/0035-factory-anchored-contract-gating.md) / [ADR-0040](../adr/0040-completing-contract-gating.md) — contract-identity gating
- [ADR-0031](../adr/0031-data-derived-coverage-signal.md) / [ADR-0032](../adr/0032-per-source-tables-as-projections.md) — one writer per domain
- Code: `internal/api/v1/oracle_sep40.go` (serve), `internal/sources/{reflector,redstone,band}` (existing oracle ingest)
- SEP-40: <https://github.com/stellar/stellar-protocol/blob/master/ecosystem/sep-0040.md>
