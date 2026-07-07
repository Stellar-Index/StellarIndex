# SEP-41 supply & transfers — event verification

> **What this page is:** SEP-41 is a token *standard*, not a single
> protocol, so there is no team to confirm a contract set with. Instead
> this page documents the two SEP-41 event decoders, the three on-chain
> topic shapes they must handle, and — importantly — the **provenance
> caveats that bound any "total supply" claim** we serve.
>
> - **Enumeration method:** operator watched-set — `[supply]
>   watched_sep41_contracts` (supply) / the watched set for the transfers
>   audit trail. The decoders only attribute events from a watched
>   contract.
> - **Last verified:** 2026-07-06 (source: `internal/sources/sep41_supply`
>   + `internal/sources/sep41_transfers`; topic shapes lake-verified on r1
>   2026-06-15; genesis-baseline provenance per commit `418accb7`).
> - **Gate status:** ✅ Gated (watched-set): match fast-path is
>   `(contract_id ∈ watched_set)` AND `(topic[0] ∈ {…})`.

## Two decoders, one standard

| Decoder | Consumes | Purpose |
|---|---|---|
| `sep41_supply` (ADR-0023) | `mint` / `burn` / `clawback` | Algorithm-3 supply: `Σ mint − Σ(burn + clawback)` |
| `sep41_transfers` (F-0021) | `transfer` / `approve` / `set_admin` / `set_authorized` | per-account audit trail (net-position queryability — the Stellar-native moat CG/CMC structurally can't offer) |

`transfer` is deliberately **excluded** from the supply sum: it moves
ownership between holders without changing total supply.

## The three topic shapes (CAP-67 dual-shape)

The single most important correctness fact here: **topic count alone does
not disambiguate the counterparty position.** Supply-affecting events
arrive in three on-chain shapes (lake-verified on r1, 2026-06-15):

```
legacy SAC    mint     ["mint", admin, to]                (to  @ topic[2])
              clawback ["clawback", admin, from]          (from @ topic[2])
CAP-67/Whisk  mint     ["mint", to, sep0011_asset]        (to  @ topic[1])  ← dominant (~99.96%)
              clawback ["clawback", from, sep0011_asset]  (from @ topic[1]) ← dominant (100%)
              burn     ["burn", from, sep0011_asset]       (from @ topic[1])
bare SEP-41   mint     ["mint", to]                        (to  @ topic[1])
              burn     ["burn", from]                       (from @ topic[1])
```

CAP-67 (Whisk, mainnet 2025-09-03) replaced the legacy admin-prefixed SAC
form with the SEP-41-spec form **plus a trailing `sep0011_asset` STRING**
— so the *same topic count (3)* can carry the counterparty at a
**different index**. `sep0011_asset` is an `ScvString`, not an `Address`.
Counterparty extraction is therefore **shape-aware**: it is `topic[2]` iff
`topic[2]` is an `Address` (legacy form), else `topic[1]`; `burn` is
always `topic[1]`.

> This dual-shape is why SEP-41 mint attribution was silently losing
> ~99.96% of mints before the shape-aware fix (F-1316 / the CAP-67
> counterparty-position finding) — a decoder that keyed the counterparty
> on a fixed topic index dropped the dominant CAP-67 form.

### Body is also dual-shape

The amount (in stroops) arrives as EITHER a bare `i128` OR a CAP-67
`map { amount: i128, to_muxed_id: String }`. The map form appears when
the destination is a muxed account, or when the issuer stamps a memo into
`to_muxed_id` (mainnet-observed, e.g. "Auto recharge transaction").
`decodeAmount` type-tests and unwraps the map — an i128-only decode
rejects every map body and drops the row (2026-07-06 dropped-mints
finding).

For the transfers audit trail the same body dual-shape applies to
`transfer`; `approve` carries `[i128 amount, u32 live_until_ledger]`,
`set_admin` carries `Address(new_admin)`, `set_authorized` carries a
`bool`.

## Storage

| Decoder | Hypertable | Migration |
|---|---|---|
| `sep41_supply` | `sep41_supply_events` (+ `asset_supply_history`, `sep41_supply_rollup`) | 0015 / supply-pipeline |
| `sep41_transfers` | `sep41_transfers` | (transfers audit) |

## Provenance — what "total supply" actually means here

**Any lifetime-supply number we serve for a SEP-41 / SAC-wrapped asset
depends on two data slices with different provenance. Read this before
citing a supply figure as "verified".**

The Algorithm-3 refresher derives per-contract total from
`sep41_supply_events` in Postgres, which the observer fills **only over
the Soroban era `[50457424, tip]`** (the lake's earliest ledger). But a
classic asset's SAC wrapper (VELO, AQUA, yXLM, LIBRE, ACT, MBC, XAU, BTC,
GQX, …) was largely **issued before Soroban existed** — so over the
Soroban-era-only window the observer can read `Σburn > Σmint`, i.e. a
*negative* derived total. That is not corruption; it is a partial window.

The fix (commit `418accb7`, Option B — baseline seed) takes from the
ClickHouse `supply_flows` lake **only the pre-Soroban slice Postgres has
no data for** (ledger < 50457424, a disjoint partition) and seeds it as a
static per-kind opening balance
(`sep41_supply_rollup.genesis_{mint,burn,clawback}_total` +
`genesis_baseline_ledger`). Lifetime total is then served as
`genesis(ledger < boundary) ⊕ soroban(ledger ≥ boundary)`. A
Soroban-only token gets a zero baseline, so its served number is
unchanged.

Two provenance caveats follow, both material to a completeness claim:

1. **The pre-Soroban slice is replay-derived, not live-captured.**
   Pre-Soroban `supply_flows` are synthesised via post-P23 CAP-67 event
   replay — legitimate, but **core-version-dependent** (ADR-0033). The
   boundary ledger + seed time are recorded on the rollup row for
   auditability. This is not the same class of evidence as the
   contiguous, hash-chained ledger substrate that backs the "100%
   substrate coverage" claim (ADR-0034); a SAC-wrapper lifetime total is
   "faithful within a replay-derived opening balance", not
   "cryptographically provable to genesis".
2. **The served tier is watched-set-gated and bare-i128 in the Soroban
   era; the CH lake is network-wide and map/muxed-aware.** Their
   Soroban-era totals can therefore legitimately differ (migration 0085's
   header records why we do **not** re-point the per-tick read at
   ClickHouse). We seed only the disjoint pre-Soroban partition, never
   re-source the overlapping window, so there is no double-count.

Operationally, an unseeded SAC-wrapper reads negative and routes to the
benign `ErrNegativeTotalMissingBaseline` outcome (`missing_baseline`,
excluded from the error-dominant alert); a genuine post-seed
inconsistency keeps `ErrNegativeTotalSupply` (pages). The one-time seed is
`stellarindex-ops supply seed-sep41-genesis`.

## Backfill / catch-up

Projected sources (ADR-0031/0032): supply catch-up after a missing window
is `stellarindex-ops projector-replay -source sep41_supply -from <ledger>`
(and `-source sep41_transfers`), never a bespoke backfill subcommand. The
projector applies a `{mint, burn, clawback}` (supply) /
`{transfer, approve, set_admin, set_authorized}` (transfers) SQL
topic-prefilter so a far-behind catch-up window doesn't stream the entire
CAP-67 classic-token firehose (~99.8% of all events under the r1
archive's uniform V4 meta).

## References

- Source packages: `internal/sources/sep41_supply/doc.go`,
  `internal/sources/sep41_transfers/doc.go`
- ADR-0023 (SEP-41 supply observer); ADR-0033 (completeness verification);
  ADR-0034 (ClickHouse lake / Postgres served tier)
- `docs/architecture/supply-pipeline.md` (three-domain supply split)
- Classic supply observers (Algorithm 1 / 2): [supply-observers.md](supply-observers.md)
