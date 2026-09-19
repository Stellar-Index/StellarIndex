# Phoenix factory events — real lake captures

Rows of r1's `stellar.contract_events` for the Phoenix factory
`CB4SVAWJA6TSRNOJZ7W2AWFW46D5VR4ZMFZKDIKXEINZCZEGZCJZCKMI`, captured
read-only on 2026-09-19 for audit finding F048. Byte-for-byte
`FORMAT JSONEachRow` output; nothing here is synthesised. The queries
that produced them are alongside (`capture_2024.sql`,
`capture_2026.sql`): ledger-bounded, one contract id, `max_threads = 2`.

| file | rows |
| --- | --- |
| `create_2024-05-07_ledgers_51572026-51572101.jsonl` | three `("create","liquidity_pool")` events |
| `factory_2026-07-02_ledgers_63293663-63293708.jsonl` | one `("Factory","Updated Config")` event (body ScvVoid: `data_xdr` `AAAAAQ==` is SCV type 1), then one `("create","liquidity_pool")` event emitted after the Map-schema pool WASM |

## What they settle

Pinned by `test/controlwiring/phoenix_factory_create_fixture_test.go`
(default suite):

1. **The creation events are in the lake**, from ledger 51,572,026 —
   ten ledgers after the factory's genesis (51,572,016). The claim that
   they "predate the lake" was false.
2. **`topic[0]` and `topic[1]` are `ScvString`, not `ScvSymbol`.** The
   lake fills `topic_0_sym` only for Symbol topics
   (`internal/storage/clickhouse/extract.go`, `eventRow`), so the column
   is EMPTY on these rows and a ClickHouse filter
   `topic_0_sym IN ('create')` matches none of them. The Postgres landing
   zone fills it for Symbol or String
   (`internal/sources/sorobanevents/events.go`), so the two stores
   disagree for this event. A lake-side walk has to match
   `topics_xdr[1]`.
3. **The body is one contract `Address`: the pool.** The factory emits
   exactly one event per create transaction, so the per-pool STAKE
   contract is never announced. Self-registration from this event could
   admit pools and never stakes.
4. **The shape is identical in 2024 and 2026.**
5. **Each announced address is a pool the curated seed already lists**,
   at the ledger its seed comment cites
   (`internal/sources/phoenix/events.go`).

## What they do NOT settle

Whether the announced address can be trusted. Four honest historical
rows say nothing about what a hostile caller could make the factory
publish. Before any decoder admits a pool from this event, someone must
establish, from the source or WASM of the factory build installed
today, that `create_liquidity_pool` is allow-listed and that the
published address is the one the factory deployed rather than a
caller-supplied argument. `defindex` lost its self-registration on
2026-08-25 to exactly that vector. Status and the evidence gathered so
far: `docs/operations/wasm-audits/phoenix.md`, "Factory create event".

The red test waiting on that is
`TestK023_PhoenixFactoryCreateEventIsAdmissible`
(`go test -tags k023evidence ./test/controlwiring/ -run TestK023_Phoenix -v`).
