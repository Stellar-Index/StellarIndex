---
title: Checklist — add a migration
last_verified: 2026-07-01
status: current
---

# Checklist — add a migration

Reference + full rules: `migrations/README.md`.

- [ ] Create `migrations/NNNN_<desc>.up.sql` + `.down.sql` — **dense sequential** numbering
      (next after the current head), matching pair. `down.sql` reverses `up.sql` (or a banner
      comment explains the asymmetry — e.g. the deliberate NO-OP retention-removal downs).
- [ ] Amounts → **`NUMERIC`** (never `bigint` / `double precision`, ADR-0003). Asset IDs →
      canonical text (`<code>-<issuer>` / `C…` / `native`). Timestamps → `timestamptz` UTC.
- [ ] Hypertable/CAGG changes go through the Timescale API. **A CAGG without a refresh policy is
      a silent bug** — add the refresh policy in the same file. **Do not add `drop_after` to
      `trades`, `oracle_updates`, `prices_15m` or any coarser price rung** — ADR-0034 keeps the raw
      lake and the long-window served rungs forever, and migrations 0031/0040 removed the policies
      0001/0002/0003 had placed on them. `trades` staying permanent is load-bearing, not a
      preference: it is the only thing that makes a dropped aggregate recoverable.
- [ ] **`prices_1m` is the one reviewed exception** — migration 0156 attaches a 90-day
      `drop_after` to that view alone, and ships it `scheduled => false` so arming it is a
      deliberate operator act. It qualified because it is the largest and most redundant rung and
      is recomputable from `trades`. **Adding a second exception is a design change, not a
      checklist tick:** measure the rung, state which surfaces stop being answerable and write that
      on the wire, prove the drop is reversible from a source that has no retention of its own, and
      declare it in `internal/storage/timescale/retention_policy_test.go` — which fails until you
      do, in both directions.
- [ ] Event-hypertable PK **leads with `ledger_close_time`** (TS103 lesson) and carries a
      per-event discriminator (`event_index` or equivalent) — the `lint-pk-discriminators` CI gate
      requires it for protocol-row tables.
- [ ] Header comment explains the *why*. `CREATE … IF NOT EXISTS` where idempotent.
- [ ] Add a row to the **"Current migrations"** table in `migrations/README.md`.
- [ ] Refresh the checksum baseline — `./scripts/ci/lint-migration-immutability.sh --write`. New
      migrations are append-only and the gate FAILS an unbaselined file; `scripts/dev/verify.sh`
      runs under `set -euo pipefail`, so a missing entry aborts the run before any later check.
- [ ] Apply as the **`stellarindex` app role** via `make db-migrate-up` — never as the `postgres`
      superuser (ownership trap).

**Done when:** `make db-migrate-up` then `make db-migrate-down` both succeed locally; README updated.
