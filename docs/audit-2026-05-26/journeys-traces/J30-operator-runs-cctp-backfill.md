# J30 — Operator runs `ratesengine-ops cctp-backfill -from N -to M`

## Inputs

- Operator decision: backfill CCTP rows for ledger range [N, M] from
  the soroban_events landing zone (ADR-0029), per the rc.81 design.
- Concrete invocation:
  ```
  ssh root@136.243.90.96 ratesengine-ops cctp-backfill \
    -config /etc/ratesengine/ops.toml \
    -from 60000000 -to 62500000
  ```
- Precondition: `migration 0041_create_soroban_events.up.sql`
  applied; soroban_events table populated by live ingestion or
  the soroban-events-fill scan.

## Hops

| # | Stage | File:line | Notes |
| --- | --- | --- | --- |
| 1 | main.go dispatch | `cmd/ratesengine-ops/main.go:case "cctp-backfill"` | invokes `cctpBackfill(args[1:])` |
| 2 | flag parse | `cmd/ratesengine-ops/cctp_backfill.go` | `-from`, `-to`, `-config`, `-dry-run`, `-batch-size` |
| 3 | config load | `internal/config/config.LoadWithEnv` | reads `/etc/ratesengine/ops.toml` |
| 4 | store open | `internal/storage/timescale/store.go` | opens Postgres pool |
| 5 | StreamSorobanEvents query | `internal/storage/timescale/soroban_events.go` | SELECT … WHERE contract_id IN (3× CCTP contracts) AND ledger BETWEEN $from AND $to |
| 6 | per-row Reconstruct | `internal/sources/sorobanevents/reconstruct.go::Reconstruct(Row) (events.Event, error)` | rehydrates the raw event from the landing-zone row |
| 7 | per-row decode | `internal/sources/cctp/decode.go::Decoder.Decode(ev)` | dispatches by topic on the rehydrated event |
| 8 | per-row store | `internal/storage/timescale/cctp.go::InsertCCTPEvent` | INSERT INTO cctp_events ... ON CONFLICT DO NOTHING |
| 9 | progress logging | structured logs | `cctp-backfill: processed N rows (M decoded, K skipped) ledger=L` |
| 10 | shutdown | normal exit on -to reached; SIGTERM → context cancel + drain | |

## Sinks

- DB table: `cctp_events` (migration 0038 / cctp-backfill rc.81)
- Rows inserted on `ON CONFLICT DO NOTHING` — idempotent re-run
- Logs: structured to stdout; on r1 systemd unit captures to journal
- Metrics: `ratesengine_backfill_rows_inserted_total{source="cctp"}`,
  `ratesengine_backfill_rows_skipped_total{source="cctp",reason="no_match|duplicate|decode_err"}`
- No Redis touches in this path — backfill is Postgres-only, so it
  IS NOT cascade-affected (operator can run backfill during F-0039)

## Failure modes

| Hop | Bad input | Behaviour | Defence |
| --- | --- | --- | --- |
| 2 | `-from > -to` | early-exit `flag error` | parse-time check |
| 3 | bad config path | error to stderr, exit 1 | LoadWithEnv |
| 4 | Postgres unreachable | error, exit 1 | conn open errors propagate |
| 5 | SQL syntax error | logged + abort | recovers nothing; this is a code bug |
| 6 | malformed XDR in `body_xdr` | row-skip + counter increment | `decode_err` reason; doesn't abort backfill |
| 7 | unknown topic_0_sym | row-skip + counter increment | `no_match` reason |
| 8 | conflict on PK | NOOP (idempotent) | ON CONFLICT DO NOTHING |
| - | SIGTERM mid-run | context cancel, drains in-flight batch | proper context handling |

## Tests

- Unit tests: `internal/sources/cctp/decode_test.go`
- Integration tests: `test/integration/cctp_backfill_test.go`
  (build-tag `integration`; testcontainers Postgres)
- Adversarial cases: malformed XDR injection, unknown topics
- F-0070 caveat: `cctp-backfill` itself sets `TolerateTrailingMissing`
  in its `ledgerstream.Config` (cmd/ratesengine-ops/main.go:3253),
  but if backfill is replaced or extended with a related subcommand,
  the helper-ification fix (Wave 1) eliminates the recurring trap

## Live R1 trace (potential — not invoked during this audit)

The cctp-backfill subcommand was NOT executed live during this
audit. Per `[[project_open_backlog]]` the task list has it as
`pending`. The audit verified the code path statically via the
file references above.

## Audit verdict

- **Cascade-safe:** backfill does NOT touch Redis; can run during
  F-0039 without hitting MISCONF. This is the right design for
  backfill — it's an offline batch that should be independent of
  the hot serving path.
- **Idempotent:** ON CONFLICT DO NOTHING + Reconstruct stability
  → re-running with overlapping range is safe.
- **Bounded:** -from/-to required + Postgres-only → no risk of
  unbounded MinIO walk (compare to the pre-rc.81 fail mode
  where a misconfigured -to=0 walked to live tip).
- **Observable:** rows_inserted_total + rows_skipped_total per
  reason → operator can dashboard progress.

**Cross-references:**
- F-0079 POSITIVE (ADR-0029 design implemented)
- F-0070 (TolerateTrailingMissing consistency)
- XFI-0002 (cctp_backfill → Reconstruct → cctp.Decode → InsertCCTPEvent)
- XFI-0015 (migration 0041 → reader/writer → 6 backfill subcommands)
