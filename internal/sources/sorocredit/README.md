# `internal/sources/sorocredit`

Decoder for an **unbranded consumer-USDC credit / CDP protocol** on
Stellar (Soroban). The protocol ships no on-chain brand, so we key
everything off its single main contract and give it the neutral name
`sorocredit` ("Soroban credit").

Cross-link: [`docs/protocols/sorocredit.md`](../../../docs/protocols/sorocredit.md).

| | |
|---|---|
| Main contract | `CCG5EWFY2KCWWYYEIUMIRG6WSAQFLDR5QE5FMCWY25N36XA5GYTCPQWR` |
| Creator | `GADI6FHS…` |
| WASM | `84a88013…` |
| Genesis ledger | `61_620_822` (2026-03-12) |
| Class | `lending` — no published price, **never** VWAP |

The protocol runs its **own USDC credit book** (verified independent —
not a wrapper). A user opens a position, which deploys a per-user
`Collateral-<uuid>` child contract; the protocol then publishes periodic
per-position statements and settles them on a schedule.

## Event surface (8 topic[0] symbols, all emitted BY the main contract)

Schemas verified against **real r1-lake fixtures, 2026-07-07** (the golden
frames in `source_test.go`). Bodies are Soroban `Vec` tuples — decoded
positionally (there are no map field names to key on).

| Symbol | Topics | Body | → table |
|---|---|---|---|
| `NewCollateralContract` | `[sym, Address(child)]` | `Vec[String("Collateral-<uuid>"), Address(owner)]` | `credit_positions` |
| `StatementPublished` | `[sym, String(stmt_uuid), String(pos_uuid)]` | `Vec[i128(amount), Address(collateral), u64(timestamp)]` | `credit_statements` |
| `Liquidation` | `[sym, Address(collateral), String(pos_uuid), String(stmt_uuid)]` | `Vec[Address(settler), Vec[Address](debt_assets), Vec[i128](amounts), …]` | `credit_settlements` |
| `Withdrawal` | `[sym, Address(collateral)]` | `Vec[Address(token), Address(recipient), i128(amount)]` | `credit_events` |
| `BeaconUpdated` | `[sym]` | `Vec[Void, Address(new_beacon)]` | `credit_events` |
| `SupportedAssetAdded` | `[sym, Address(asset)]` | `Vec[…config…]` | `credit_events` |
| `CollateralHashUpdated` | `[sym]` | `Vec[Bytes(old), Bytes(new)]` | `credit_events` |
| `TreasuryUpdated` | `[sym]` | `Vec[Address(old), Address(new)]` | `credit_events` |

Every event is decoded — the EVERY-event invariant (no partial decoder).
The four low-volume config events (1–4 occurrences each) land in
`credit_events` with the full body captured in `attributes`, alongside
the meaningful-volume `Withdrawal`.

## CRITICAL SEMANTIC — `Liquidation` is a SCHEDULED SETTLEMENT

The on-wire topic is the symbol **`Liquidation`**, but these events are
**NOT distressed liquidations**. The discovery audit + the lake prove it:

- a **single keeper account** (`GA3PWX3H…`) executes **all** of them;
- ~**1:1** with `StatementPublished` (lake 2026-07-07: 187,926 statements
  vs 187,718 "Liquidation"s over the contract's life);
- ~**14 per user per month, uniformly**.

i.e. they are **recurring scheduled settlements** of published statements,
not risk events. We therefore surface them as **`settlement`**
(`TypeSettlement` → `credit_settlements`) — **never** as "liquidations".
The table, column, EventType, and reconciliation kind are all named
`settlement` on purpose. **Do NOT let any downstream surface report a
"221k liquidations" risk signal from this source.** The
`TestGolden_Settlement` test pins the naming.

## Gating (ADR-0035)

Single trust root: the main contract. `Matches()` gates on **contract
identity**, not topic symbol — the eight symbols are distinctive but
**two other mainnet contracts emit them** (~159 events total, lake
2026-07-07), and those must be rejected.

- `NewCollateralContract` matches **only** from the trust root
  (`IsFactory`) — only the protocol root announces a position. On decode
  it Seeds the announced child `Collateral-<uuid>` C-address (topic[1])
  into the [`contractid.Registry`](../../contractid/) child set.
- every other event matches from the trust root **or** a registered child
  (`Has`).

**Coverage note:** in practice ALL eight event types are emitted by the
trust root and the child contracts emit **nothing** (verified). So the
child branch never fires today — the trust-root check is what actually
gates. The childgate is **forward-compat defense-in-depth** for a future
contract version that might route events through the per-position
children. That is also why this source is **NOT** registered in
`pipeline.gatedSources` (no `protocol_contracts` DB-warm/persist):
DB-warming ~139k+ never-emitting children would be pure overhead with zero
coverage benefit — unlike blend, whose seeded pools DO emit. Live
in-memory seeding suffices.

## Discipline

- **ADR-0003 / i128:** every amount round-trips through
  `scval.AsAmountFromI128` → decimal string → NUMERIC; **never** `int64`.
- **ADR-0013:** SCVal is read exclusively through `internal/scval`; this
  package never imports `go-stellar-sdk/xdr` (enforced by
  `scripts/ci/lint-imports.sh`; the xdr import in `source_test.go` is
  confined to synthetic-fixture construction).
- **Graceful degradation:** a promoted field whose nested shape drifts
  (the settlement debt-asset / amount legs) degrades into an
  `attributes` note rather than failing the whole row; a genuinely
  malformed tracked event returns `ErrMalformedPayload` (counted +
  skipped by the dispatcher/projector).

## Provenance — LIVE-CAPTURE, audited

Schemas were reverse-engineered from real lake fixtures (2026-07-07),
**not** from a published contract source. The WASM-history audit landed
2026-07-07 at
[`docs/operations/wasm-audits/sorocredit.md`](../../../docs/operations/wasm-audits/sorocredit.md):
the main contract has run a **single** instance WASM (`84a88013…810ea`,
set at deploy) with no executable change in the dense-coverage window
`[62.0M→tip]`, and **all 8 event types have one invariant on-wire
schema across the contract's whole life** (`NewCollateralContract`
structurally identical from its first occurrence 61,624,053 through
63,363,505, spanning the sparse early window). `BackfillPerWASM` is
therefore the policy in `external.Registry`, safe **from genesis
(61,620,822)**. Historical re-derive:
`stellarindex-ops projector-replay -source sorocredit -from 61620822`
(under the heavy-job wrapper).

## Storage + wiring

- Storage: migration
  [`0090_create_sorocredit_credit_tables`](../../../migrations/0090_create_sorocredit_credit_tables.up.sql)
  creates `credit_positions`, `credit_statements`, `credit_settlements`,
  `credit_events`. Row structs + writers live in
  `internal/storage/timescale/sorocredit.go` (defined in the storage
  package — the sink converts `sorocredit.Event` → the row struct, so
  storage keeps its no-upward-import boundary).
- `internal/pipeline/dispatcher.go` — `BuildDispatcher` registers
  `sorocredit.NewDecoder()` (event Decoder).
- `internal/pipeline/sink.go` — `IsProjectedEvent` arm +
  `persistSoroCreditEvent` routes by `EventType` to the four tables.
- `internal/projector/registry.go` — `buildSource` registers the source
  with a `Topic0Syms` prefilter (the eight distinctive symbols); the
  projector is the **sole writer** (ADR-0031/0032).
- `internal/config/validate.go` — `KnownSources` includes `sorocredit`.
- `internal/sources/external/registry.go` — `Metadata{Class: lending,
  IncludeInVWAP: false, Backfill: BackfillPerWASM}`.
- `internal/storage/timescale/per_source_gaps.go` — four gap targets
  (`sorocredit-{positions,statements,settlements,events}`).
- `internal/ops/chops/reconciliation_catalogue.go` — a `reconSource`
  so the ADR-0033 projection reconcile covers all four tables (the
  dynamic `EventKind()` — `sorocredit.<event_type>` — gives per-table
  attribution).

## Catch-up

The projector fills from deploy onward. The historical back-window is
re-derived from the lake:

```sh
stellarindex-ops projector-replay -source sorocredit -from 61620822
```

Run it under `/usr/local/sbin/run-heavy-job.sh` (AGENTS.md heavy-job
doctrine). **Never** a bespoke `sorocredit-backfill` subcommand.

## Served surface

- `GET /v1/protocols/sorocredit` — the `bespoke` block, built by
  `bespokeCredit` in `internal/storage/timescale/bespoke_lending.go`:
  positions opened (window and all-time), a window-scoped open-position
  proxy, unique users, statements published, scheduled settlements
  (count, volume, series), withdrawals, latest activity and a recent
  scheduled-settlements table. Settlements are labelled as scheduled
  settlements, never liquidations. Amounts are base units, not USD.
- `GET /v1/accounts/{g_strkey}/positions` — one `credit` position per
  owned position (`basis: stateful`, `amount_semantics:
  stateful_current_unconfirmed_unit`: the latest published statement amount, raw and unscaled — its unit is not contract-confirmed), read by
  `CreditPositionsByOwner` in `internal/storage/timescale/positions.go`.
- `GET /v1/accounts/{g_strkey}/activity` — `defi_actions` carries a
  `sorocredit` `position_opened` count (`credit_positions.owner`;
  `credit_events` has no owner column).

Not served:

- An aggregate total-credit-outstanding figure.
- A per-position statement/settlement history view.

None of it feeds pricing.
