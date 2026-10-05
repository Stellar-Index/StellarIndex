# Blend V1 pool auction events: real lake captures

These are rows of r1's `stellar.contract_events` for the four V1-factory
pools on WASM `baf978f1…`, captured read-only on 2026-10-05. The file
is the byte-for-byte `FORMAT JSONEachRow` output of `capture.sql`
(ledger-bounded, `max_threads = 2`). Nothing here is synthesised.

`lake_events.jsonl` has 19 rows:

- 10 `fill_auction` rows: UserLiquidation from all four pools, plus
  BadDebt and Interest.
- 6 `new_auction` rows: BadDebt and Interest.
- 3 `bad_debt` rows.

Two bad-debt lifecycles are complete: `CDVQVKOY…` at 52,428,304 to
52,430,677, and `CDE65QK2…` at 55,570,394 to 55,570,549.

`internal/sources/blend/v1_pool_auction_test.go` pins these rows. The
shape evidence is in `docs/operations/wasm-audits/blend.md`, in the
"V1 pool WASM `baf978f10efdbcd8`" section.
