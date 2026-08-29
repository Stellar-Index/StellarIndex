---
title: FX history empty / fx_quotes table missing — apply migration 0028 + backfill
last_verified: 2026-08-29
status: living procedure
---

# FX history empty / `fx_quotes` table missing

## At a glance

| Field | Value |
| ----- | ----- |
| Trigger | Customer report: "FX history is empty" / operator-noticed `history_1y: 0` on `/v1/assets/<fiat>`. No specific Prometheus alert fires today — surfaces as a recurring WARN in the API log. |
| Severity | P3 (data-quality, not data-loss) |
| Detected by | API log: `forex: fx_quotes persist failed ... pq: relation "fx_quotes" does not exist` |
| Typical MTTR | 5–15 min (one-shot operator action: apply migration + restart) |
| Impact | FX history endpoints serve `history_1y: 0` and `history_all: 0` for every ticker. `history_7d` populates normally because it reads from a different surface. The aggregator's stablecoin-fiat proxy is unaffected (uses `[trades].usd_pegged_classic_assets`, not `fx_quotes`). |

Companion to [`db-disk-full.md`](db-disk-full.md) and
[`redis-write-blocked-disk-full.md`](redis-write-blocked-disk-full.md).
Different shape: a database migration that ships in the repo
(0028) but hasn't been applied to the deployment, so a feature
that depends on the new table fails silently at runtime.

This runbook captures the 2026-05-10 finding on r1 + the recovery
sequence so future operators don't re-investigate from
"FX history is empty for EUR" backwards. (Original 2026-05-10
investigation was against `/v1/currencies/EUR`, retired in
rc.48 — same data now flows through `/v1/assets/eur`; the
underlying `fx_quotes` table is the same and the runbook below
applies unchanged.)

## Signal

- `/v1/assets/eur` (or any other fiat ticker) returns
  `history_1y: 0` and `history_all: 0` on the wire while
  `history_7d` populates normally.
- API log shows recurring WARN every forex refresh tick:
  ```
  {"level":"WARN","msg":"forex: fx_quotes persist failed",
   "rows":810,
   "err":"timescale: InsertFXQuoteBatch ticker=\"AED\":
          pq: relation \"fx_quotes\" does not exist
          at position 2:15 (42P01)"}
  ```
- `psql -tA -c "SELECT to_regclass('public.fx_quotes')"` returns
  empty.
- `psql -tA -c "SELECT version FROM schema_migrations
  ORDER BY version DESC LIMIT 1"` returns a version below 28 —
  the general signal. ("Returns 27" was the specific 2026-05-10
  state on r1; HEAD's migrations run far past it — 0150 at the
  2026-08-29 re-verification — so any `version < 28` means 0028
  was never applied.)

## Why this happens

The `fx_quotes` hypertable was added in PR #1041 (task #104,
"Persistent fx_quotes hypertable + 10y backfill") via migration
0028. The migration ships in the repo at
`migrations/0028_create_fx_quotes.up.sql`. Two operator-side
steps make it live on a deployment:

1. **Copy the migration file** to the deployment's canonical
   migrations directory (`/usr/local/share/stellarindex/migrations/`
   on r1 — the dir the deploy playbook syncs + applies from; NOT
   `/var/lib/stellarindex/migrations/`, which is a stale unmanaged
   leftover).
2. **Apply it** via `stellarindex-migrate up`.

> Note: as of the `migrations_skip | bool` fix, `deploy.yml` syncs +
> applies pending migrations automatically before swapping binaries,
> so a normal `gh workflow run deploy.yml` deploy already runs this.
> The manual steps below are the fallback for an out-of-band fix.

Once the table exists, the forex worker (running inside
`stellarindex-api`) starts persisting on its next refresh tick,
so live data backfills forward as it arrives. The live worker
polls Massive (paid feed, `MASSIVE_API_KEY`) with a keyless ECB
daily-reference-rates fallback; Frankfurter is used by the
backfill script only. Historical depth needs the one-shot
`fx-history-backfill` script — see step 3.

## Triage (1 min)

```sh
# 1. Confirm the table is missing
sudo -u postgres psql -d stellarindex -tA -c "SELECT to_regclass('public.fx_quotes')"
# → empty line means missing

# 2. Confirm migration version
sudo -u postgres psql -d stellarindex -tA -c "SELECT version, dirty FROM schema_migrations ORDER BY version DESC LIMIT 1"
# → 27|f means migration 0028 hasn't been applied yet

# 3. Confirm the API log shows the symptom. The forex worker's
#    refresh cadence is HOURLY (`time.Hour`, wired in
#    cmd/stellarindex-api/main.go), so expect ONE WARN per hourly
#    tick — grep a window wide enough to catch at least one:
journalctl -u stellarindex-api --since "2 hours ago" -o cat | grep "fx_quotes persist failed" | tail -1
```

## Recovery (5 min)

### 1. Copy the migration file

From your local checkout:

```sh
scp migrations/0028_create_fx_quotes.{up,down}.sql \
    root@<host>:/usr/local/share/stellarindex/migrations/
```

(R1 host: `136.243.90.96`. `/usr/local/share/stellarindex/migrations`
is the canonical path the deploy playbook syncs to with `delete:true`
and that `stellarindex-migrate` should read. `/var/lib/stellarindex/migrations`
is a stale unmanaged dir — don't use it.)

### 2. Apply the migration

```sh
ssh root@<host> '
  set -e
  set -a; . /etc/default/stellarindex; set +a
  /usr/local/bin/stellarindex-migrate \
    -migrations /usr/local/share/stellarindex/migrations \
    -dsn "$STELLARINDEX_POSTGRES_DSN" \
    up
'
```

Expected output: `1/u create_fx_quotes` then exit 0.

The migration is forward-only, additive, and idempotent on
re-runs (the `create_hypertable` call uses `if_not_exists =>
TRUE`; the table itself doesn't but won't be re-attempted because
`schema_migrations.version` advances). Safe to apply on a live
deployment — no service restart needed; the forex worker picks
up the new table on its next refresh tick (hourly cadence — up
to 1 h away).

### 3. Confirm the worker started persisting

```sh
# The refresh cadence is hourly, so post-fix confirmation can take
# up to 1 h — a zero count here only proves absence-of-failure once
# a tick has actually fired since the migration:
journalctl -u stellarindex-api --since "2 hours ago" -o cat \
  | grep -c "fx_quotes persist failed"
# → 0 once the next refresh tick fires (hourly cadence)

sudo -u postgres psql -d stellarindex -tA -c "SELECT count(*) FROM fx_quotes"
# → > 0 within ~1 h
```

### 4. Backfill historical depth (slow path — separate step)

The forward-flow worker only writes the LATEST snapshot per
refresh tick — it doesn't go back in time. The 1y / all-time
fiat charts need historical data that the one-shot
`fx-history-backfill` binary fetches from the ECB-backed
Frankfurter API (frankfurter.dev) — free, no API key, ~32
currencies, daily granularity back to 1999-01-04.

```sh
# On the operator's workstation:
export DATABASE_URL=postgres://...:5432/stellarindex
go run ./scripts/ops/fx-history-backfill --years=25
```

No cost — Frankfurter is free (ECB reference rates,
maintained as a public utility). The script walks the window in
5-year chunks (one HTTP request per chunk) so a 25-year backfill
is ~6 requests total. Safe to interrupt and resume — the writer
upserts on `(ticker, bucket)` so re-running on the same range
is a no-op.

The script logs one line per chunk to stderr; on completion it
writes a final summary (total chunks, total rows, elapsed).

## Prevention

The 2026-05-10 finding exposed a process gap: a release that
adds a migration ships the binary changes via the deploy
workflow, but the migration files + `stellarindex-migrate up`
were operator-side actions not automated by the same workflow.

**Path 1 is DONE (F-1220):** the deploy workflow now syncs the
migrations directory and runs `stellarindex-migrate up` before
any binary swap, unless the operator passes the
`migrations_skip` input (`.github/workflows/deploy.yml` +
`configs/ansible/playbooks/deploy-binary.yml`). A normal
`gh workflow run deploy.yml` deploy cannot reproduce this
incident class anymore; the manual steps above remain only as
the out-of-band fallback.

**Still open — the startup-gate idea:** `stellarindex-api`'s
ready check could compare the binary's expected schema version
(computed at build time from the embedded migrations) against
`schema_migrations.version`; readyz returns 503 with a
diagnostic if they diverge. Doesn't auto-apply but would catch
any remaining out-of-band drift (e.g. a hand-copied binary)
instead of letting it silently fail at runtime.
TODO(ash): decide whether the startup gate is still worth it
post-F-1220, or close it as superseded.

## Related runbooks

- [`db-disk-full.md`](db-disk-full.md) — different shape; the
  postgres-side disk-pressure surface.
- [`redis-write-blocked-disk-full.md`](redis-write-blocked-disk-full.md) —
  another silent-runtime-failure shape (Redis writes blocked).

## Changelog

- 2026-08-29 — re-verified against HEAD (Wave I). The forex
  worker's refresh cadence corrected from "~5 min" to HOURLY
  (`time.Hour`) in the triage grep, the persist-pickup note, and
  the post-fix confirmation windows; migration-version signal
  generalised to `version < 28` (27 was the 2026-05-10 snapshot;
  HEAD runs to 0150); Prevention path 1 marked DONE via F-1220
  (deploy workflow syncs migrations + runs `stellarindex-migrate
  up` pre-swap unless `migrations_skip`), leaving only the
  startup-gate idea open; noted the live worker polls Massive
  (`MASSIVE_API_KEY`) with keyless ECB fallback — Frankfurter is
  backfill-only.
