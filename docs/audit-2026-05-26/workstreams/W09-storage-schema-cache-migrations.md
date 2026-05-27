# W09 — Storage, schema, cache, migrations

## Scope

`migrations/0001..0045` (45 ups + 45 downs) + every file under
`internal/storage/timescale/` + `internal/storage/redisclient/`
+ `internal/cachekeys/`.

Plus the per-migration audit loop (`02-protocol.md` §7).

## Inputs

- `migrations/*.up.sql` + `*.down.sql`
- `internal/storage/timescale/*.go` (40 files)
- `internal/storage/redisclient/*.go`
- `internal/cachekeys/keys.go`
- ADR-0006 (Timescale), ADR-0007 (Redis schema), ADR-0024
  (Redis HA)

## Checks per migration

| Check | Result | Evidence |
| --- | --- | --- |
| 1. Up + down symmetry | | |
| 2. Concurrent-safe DDL | | |
| 3. Hypertable / CAGG semantics | | |
| 4. NUMERIC vs BIGINT (ADR-0003) | | |
| 5. PK includes partition key (TS103 — migration 0041 fixed this) | | |
| 6. Index coverage for hottest queries | | |
| 7. Trigger / view drift | | |
| 8. Reader correspondence | | |
| 9. ON CONFLICT shape match (writer ↔ PK) | | |

## NEW migrations since baseline (0029..0045)

- **0029** drop unused blend jsonb gin indexes
- **0030** asset_supply_history unique constraint
- **0031** remove trades retention
- **0032** seed soroswap router
- **0033** seed defindex vaults
- **0034** oracle_price_aggregates
- **0035** source_entry_counts
- **0036** pools_per_source cagg
- **0037** trades pair source ts index
- **0038** cctp_events
- **0039** rozo_events
- **0040** remove oracle_updates retention
- **0041** create soroban_events (ADR-0029; W27 owns details)
- **0042** comet_liquidity
- **0043** soroswap_skim_events
- **0044** phoenix_liquidity + phoenix_stake_events
- **0045** blend_money_market (positions + emissions + admin)

Each needs the full per-migration loop. Special attention:

- **0041 PK shape** (rc.79 fix): verify writer's ON CONFLICT
  matches PK exactly
- **0042-0045 four decoder tables**: ON CONFLICT semantics +
  per-source writer alignment
- **0031, 0040**: retention removal — verify no cascading
  effect on chunks (the chunks remain; retention policy is
  metadata)
- **0032, 0033**: seeded data — verify the seeds match the
  source's current contract list
- **0036**: pools_per_source CAGG — refresh policy, dependency
  on trades hypertable schema

## Cache checks

| # | Check | Method |
| --- | --- | --- |
| W09.C.1 | `internal/cachekeys/` is the ONLY key builder | grep |
| W09.C.2 | Every reader + writer uses the cachekeys API | grep |
| W09.C.3 | Prewarm symmetry (per-route audit W11.X.11 cross-ref) | code |
| W09.C.4 | Sentinel awareness (ADR-0024) | redisclient code |
| W09.C.5 | TTL / eviction policy | redisclient + Redis config |

## Storage adapter checks

| # | Check | Method |
| --- | --- | --- |
| W09.S.1 | Every hypertable has a reader/writer in `internal/storage/timescale/` | grep |
| W09.S.2 | Typed not-found errors (no nil + nil-error returns) | per-file |
| W09.S.3 | `usd_volume_quote_spec.go` spec correctness vs SQL queries | code |
| W09.S.4 | NEW: `Store.StreamSorobanEvents` filters push down to SQL | W27 cross-ref |
| W09.S.5 | NEW: `Store.{InsertBlendPositionEvent, InsertBlendEmissionEvent, InsertBlendAdminEvent}` use unique-on-PK ON CONFLICT | per-writer audit |

## Closure criteria

45 per-migration tables filled. Cache + storage checks
terminal. Findings on:

- any missing down.sql
- any non-concurrent DDL on production-sized tables
- any reader using direct connection.Query (bypassing the Store)
- any cache-key build path outside cachekeys
