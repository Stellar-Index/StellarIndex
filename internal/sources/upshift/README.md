# internal/sources/upshift

Decoder for the **Upshift tokenized vaults** on Stellar — institutional
yield vaults curated by Gami Labs and Stake Capital Group, with Upshift
supplying the vault contract.

The public verification page — how the two vault contracts were
established from the lake, what is proven and what is inferred — is
[docs/protocols/upshift.md](../../../docs/protocols/upshift.md). This
README is the implementation note.

## Shape

An Upshift vault is ERC-4626-shaped: one underlying in, proportional
**shares** out, and the vault contract IS the share token. Two vaults on
pubnet, both curated in `events.go`:

| Vault | Contract | Underlying |
|---|---|---|
| earnUSDC | `CCL3WITW…` | native USDC |
| earnXLM  | `CC6TRAPQ…` | native XLM |

Four of the twelve emitted symbols produce rows — `deposit`, `withdraw`,
`transfer` (the vault's own share token), `deployed_assets_changed`. The
other eight (custody mirror, governance, allowance) are recognised,
gated, and decode to **zero rows with no error** so the ADR-0033
re-derive counts their ledgers as expected-zero.

## Gate (ADR-0035 / ADR-0040)

Curated-set mechanism, the `internal/sources/comet` shape: no factory
exists to fan out from, so `MainnetGatedSet` plus the
`protocol_contracts` warm is the trust root. `Matches` requires BOTH a
recognised symbol AND a registered emitter — `deposit` alone is emitted
by four distinct contracts in a 20k-ledger census.

## Files

| File | What |
|---|---|
| `events.go` | Package doc (the lake evidence), source name, curated vault set, genesis, topic symbols, errors |
| `decode.go` | `classify` + the four body decoders. Fields read BY NAME per `docs/architecture/contract-schema-evolution.md` |
| `consumer.go` | The single `Event` the sink writes to `upshift_vault_events` |
| `dispatcher_adapter.go` | `Decoder` — the identity gate, `Matches` / `Decode` / `GatedContractSet` |
| `golden_realbytes_test.go` | Seven fixtures of real lake bytes, cited by ledger/tx/op/event index |
| `adapter_test.go` | Gate conjunction, zero-row contract, malformed-body errors, curated-set invariants |
| `registration_test.go` | One test per registration site — config, dispatcher, projector, sink, gated registry, sourcenet, source metadata, gap target |

## Amounts

`assets` and `shares` are on DIFFERENT scales (an ERC-4626 decimals
offset of 6). Nothing here divides one by the other or applies a decimals
assumption; both are stored raw as `canonical.Amount` → NUMERIC
(ADR-0003).

TVL is deliberately not derived — the vault's idle balance is not
observable in any event, so `deployed_assets_changed.new_amount` is one
leg and never the whole. Share supply IS exactly derivable; see
`Store.UpshiftVaultShareSupplies`.

## Replaying history

Every event this source serves is already in the ClickHouse lake from the
protocol's genesis (62,623,313) — it was landed by the raw
`soroban_events` path and simply never decoded. `upshift` is a PROJECTED
source (one writer), so the replay is:

```
stellarindex-ops projector-replay -source upshift -from 62623313
```

not `backfill` / `ch-rebuild`. See the replay decision rule in
`docs/architecture/ingest-pipeline.md`.
