---
title: Runbook — served value diverges from independent ground truth
last_verified: 2026-09-18
status: ratified
severity: P3 (ticket)
---

# Runbook — `stellarindex_served_value_drift`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_served_value_drift` / `stellarindex_served_value_check_stale` / `stellarindex_served_value_persistently_skipped` / `stellarindex_served_value_unit_failed` / `stellarindex_sdf_reserve_list_drift` |
| Severity | ticket |
| Detected by | `stellarindex_served_value_ok == 0` for 26h (two daily runs) |
| Typical MTTR | investigation-bound (data derivation, not availability) |
| Impact | A customer-visible NUMBER is wrong while every availability signal is green — the CS-010 class (XLM market cap read +58% until hand-sampled). Credibility, not uptime. |

## Alert description

`stellarindex-ops verify-served-values` reconciles a curated set of
values we serve against INDEPENDENT sources — the SDF lumen API for
XLM supply, Stellar Expert for classic-asset supply — and emits
`stellarindex_served_value_{ok,rel_err,skipped,last_run_unix}`
textfile gauges, plus `stellarindex_sdf_reserve_list_drift{kind}`
for the reserve-LIST check described below. It runs from
`verify-served-values.timer` (daily, 06:20 UTC,
after the supply chain has refreshed for the day); the units are
ansible-managed and installed only where the ground truth is pubnet.
This alert means a served value sat outside its tolerance for two
consecutive runs. The companion `_check_stale` alert means the
harness itself is dark — timer dead, run crashing, every run
skipping every check (`last_run_unix` only advances on a run that
reached a verdict), or the timer never installed at all, which is
why it carries an
`absent_over_time` arm as well as a staleness one.

`_unit_failed` is the fourth, and it is the unit's exit status
rather than any gauge. The tool exits non-zero on three different
conditions — a drifted check, a run where every truth source was
dark, and a crash — so this ticket is deliberately NOT the
catch-all `stellarindex_systemd_unit_failed` (which would open it
15 minutes into a third-party outage). It rides out one bad day and
fires only when two consecutive runs left the unit failed. When it
fires ALONE, with `_drift` and `_persistently_skipped` both quiet,
the run is crashing before it writes anything, and `_check_stale`
will not confirm that for another day.

Deliberate scope notes: prices are NOT checked here (the divergence
worker cross-checks them continuously); lake↔served row counts are
NOT checked here (compute-completeness owns that). This harness is
specifically "is the VALUE right", not "is the pipe healthy".

## When this fires

- A supply-derivation basis is wrong or partially populated (the
  known standing cases at the time of writing:
  `xlm_circulating_supply` reads `xlm_total_only` until the operator
  sets `sdf_reserve_accounts` — CS-010's config half; and
  `usdc_total_supply` under-reads vs Stellar Expert — board #34).
- A backfill/observer gap left the supply hypertables incomplete.
- The GROUND TRUTH changed methodology (SE counts locked amounts,
  SDF changes basis) — verify before "fixing" our side
  (feedback: confirm windows/bases match before claiming undercount).

## How to investigate

```sh
# Reproduce with full detail (read-only, run anywhere):
stellarindex-ops verify-served-values -api https://api.stellarindex.io

# What do we serve, on what basis?
curl -s 'https://api.stellarindex.io/v1/assets/native' | jq '.data | {circulating_supply, total_supply, max_supply, supply_basis}'

# The independent sources:
curl -s https://dashboard.stellar.org/api/v3/lumens | jq .
curl -s 'https://api.stellar.expert/explorer/public/asset/USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN' | jq '{supply}'

# Supply pipeline state (r1):
sudo -u postgres psql -d stellarindex -c "SELECT asset, basis, max(time) FROM asset_supply_history GROUP BY 1,2 ORDER BY 3 DESC LIMIT 10"
```

Then read `docs/architecture/supply-pipeline.md` for which algorithm
(1 XLM / 2 classic / 3 SEP-41) derives the failing value.

## How to mitigate

There is no cache to flush — fix the derivation (or its config), or
document the differing basis honestly on the wire (`supply_basis`).
Never widen a tolerance to silence the alert without a written
methodology justification in the check's `note`.

## `stellarindex_sdf_reserve_list_drift` — the reserve LIST check (C4-069)

The value check tolerates 2% on `xlm_circulating_supply` (~700M
XLM) on purpose — it rides out methodology residuals such as the
fee pool — but that makes it blind to SDF adding or retiring ONE
reserve account: the program hot wallets each hold well under 2% of
circulating, so a stale `supply.sdf_reserve_accounts` would mis-state
circulating supply indefinitely behind a green value check. The same
harness therefore also diffs the configured account SET against the
list SDF publishes, on the same daily timer, reading the node's own
`/etc/stellarindex.toml` (`-config`).

**Where SDF publishes the list.** Not the dashboard API:
`/api/lumens`, `/api/v2/lumens`, `/api/v3/lumens` and
`/api/v3/lumens/all` (probed 2026-09-18) expose program-level sums
only, never an account id. The list is the `accounts` table plus the
`networkUpgradeReserveAccount` constant in the dashboard's own
source, <https://raw.githubusercontent.com/stellar/dashboard/master/common/lumens.js>
— exactly what its `noncirculatingSupply()` subtracts (the fee pool
aside) to produce the `circulatingSupply` the value check reads, and
the source r1's 16 accounts were transcribed from on 2026-07-02. The
burn address (`voidAccount`) is subtracted from TOTAL supply, not
circulating, and is not part of the set. SDF retires a row by
commenting it out, not deleting it, and the parser skips comments —
that is what surfaces a retirement as `extra`.

**Gauges.** `stellarindex_sdf_reserve_list_drift{kind="missing"}`:
SDF publishes it, we do not exclude it — we OVER-state circulating.
`{kind="extra"}`: we exclude it, SDF no longer publishes it — we
UNDER-state. Both are emitted only when both sides were read and
diffed. A dark or reshaped source emits
`stellarindex_served_value_skipped{check="sdf_reserve_list"}=1` and
NO drift gauge (absence is honest, as for `served_value_ok`), which
`_persistently_skipped` tickets after two runs. An unreadable config
file is OUR side and fails the run instead (`_unit_failed` after two
runs).

```sh
# The verdict, with account ids (the journal keeps the last runs):
journalctl -u verify-served-values -n 20 | grep sdf_reserve_list

# Re-run by hand on r1 against the node's own config:
sudo -u stellarindex stellarindex-ops verify-served-values -api http://127.0.0.1:3000 -config /etc/stellarindex.toml

# The two sides, side by side (published = table rows not commented
# out, plus the upgrade reserve; configured = the one-line TOML array):
SRC=https://raw.githubusercontent.com/stellar/dashboard/master/common/lumens.js
{ curl -s "$SRC" | sed -n '/const accounts = {/,/^};/p' | grep -v '^ *//' ;
  curl -s "$SRC" | grep -A1 'networkUpgradeReserveAccount =' ; } \
  | grep -o 'G[A-Z2-7]\{55\}' | sort -u > /tmp/published
grep '^sdf_reserve_accounts' /etc/stellarindex.toml | grep -o 'G[A-Z2-7]\{55\}' | sort -u > /tmp/configured
comm -3 /tmp/published /tmp/configured   # col 1 = missing, col 2 = extra
```

**Mitigate.** Fix is config, not code. First confirm the change
upstream (the stellar/dashboard commit that edited `accounts`) — a
genuine edit is a methodology change worth a CHANGELOG line. Then
update BOTH lists in
`configs/ansible/roles/archival-node/defaults/main.yml`
(`stellarindex_sdf_reserve_accounts` and the paired
`stellarindex_reserve_balances_stroops`: the supply writer refuses
to start with an account missing from the balance map), re-render
`/etc/stellarindex.toml` and restart the supply writer. The next
daily run clears the gauge. Never silence this by widening the
value check's tolerance — the value check is not what fired.

## How to escalate

Standing drift on a flagship value (XLM/USDC) that resists a day of
investigation → raise with the maintainer; it may need an upstream
(SDF/SE/Circle) methodology confirmation.

## Post-mortem notes from prior firings

- 2026-09-18 (C4-069, pre-alert): the list check landed. Its parser
  is pinned to the real `common/lumens.js` in
  `internal/ops/chops/testdata/`, where the published set equals
  r1's deployed 16 exactly (15 live table rows plus the upgrade
  reserve; the commented-out 2021 escrow and the burn address
  excluded). A parser that swept every G-strkey in the file would
  have reported r1 drifted by two on day one.
- 2026-07-02 (first run, pre-alert): caught the harness's own unit
  bug (served F2 supply is base-unit strings), the standing CS-010
  config gap (47% on XLM circulating), and the new USDC finding
  (board #34) — one run, three findings.
- Never fired before the timer existed. The three alerts, this
  runbook and the catalogue rows all described a daily harness that
  nothing scheduled: no systemd unit, no ansible task, no cron, so
  `served_values.prom` was never written and every rule selected a
  series that had never existed. `_check_stale` could not report it
  either — `time()` minus an absent vector is an empty vector, not a
  large number. Triage accordingly: if the alert is firing with the
  series ABSENT rather than old, check `systemctl status
  verify-served-values.timer` before looking at the data.
- The same gap is why `verify-served-values.service` is now named in
  `scripts/ci/unit-failed-dedicated.baseline` and excluded from the
  catch-all failed-unit rule. Its exit code is a VERDICT, not only a
  crash signal, and the verdicts it reports are ones the other rules
  deliberately wait 26h on; a 15-minute generic ticket would have
  undone that tuning the first time a truth source went dark.

## Related

- `docs/adr/0041-ingest-durability-semantics.md` — durability vs
  data-truth split.
- `docs/operations/runbooks/price-divergence.md` — the PRICE
  counterpart (continuous, divergence worker).
- `docs/architecture/supply-pipeline.md` — the three supply
  algorithms behind most values checked here.
