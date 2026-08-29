---
title: Runbook — supply-snapshot-circulating-zero
last_verified: 2026-08-29
status: ratified
severity: P2
---

# Runbook — `stellarindex_supply_snapshot_circulating_zero`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_supply_snapshot_circulating_zero` |
| Severity | P2 (`severity: page`) |
| Detected by | `configs/prometheus/rules.r1/supply-snapshot.yml` (group `stellarindex.supply_snapshot`, `severity: page`, `for: 5m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/supply-snapshot.yml`. |
| Typical MTTR | 15–60 min |
| Impact | `/v1/assets/native` reports `circulating_supply: 0`, which is visibly wrong to anyone glancing at the response. Customer-visible data-quality incident. |

## Coverage caveat — timer-path-only alert (LIVE on r1)

`stellarindex_supply_snapshot_circulating_xlm` is emitted by
`internal/supply/textfile.go`, which only runs from the systemd-
timer path (`supply-snapshot.timer` →
`stellarindex-ops supply snapshot`). The aggregator-resident
goroutine path (gated by `[supply] aggregator_refresh_enabled =
true`) writes directly to `asset_supply_history` without going
through the textfile, so this alert **cannot fire** on a
goroutine-only deployment — the metric series simply doesn't
exist. See [supply-pipeline.md](../../architecture/supply-pipeline.md)
for the two-path overview.

**On r1 the timer path IS deployed and enabled** — the ansible
archival-node role installs `/etc/default/supply-snapshot` with
`TEXTFILE_OUTPUT` set (`10-observability.yml`), so the gauge is
emitted and this alert is live. The remaining blind spot ("the
gauge series is absent entirely, so `<= 0` evaluates to no data")
is covered by the
`stellarindex_supply_snapshot_never_initialized` sibling —
[supply-snapshot-never-initialized.md](supply-snapshot-never-initialized.md).

## Symptoms

- `stellarindex_supply_snapshot_circulating_xlm{asset_key="XLM"} <= 0`
  for ≥ 5 min.
- Per ADR-0011 native XLM circulating = total − Σ(SDF reserves).
  A non-positive value means either:
  - The reserve-balance sum (live observer OR operator-static
    fallback — see root cause 0) equals or exceeds the frozen
    total, or
  - The XLMComputer math is producing nonsense (regression bug).

## Quick diagnosis (≤ 5 min)

Note: since the chained-fallback reserve reader landed (L2.12a,
PRs #411–#413), the writer reads reserve balances from the live
LCM AccountEntry observer **first** — check the database before
the TOML.

```sh
ssh root@136.243.90.96

# 1. What's the latest snapshot? (flags BEFORE the positional —
#    Go's flag package stops parsing at the first positional arg,
#    so `supply audit native -config ...` fails with "-config is
#    required")
stellarindex-ops supply audit -config /etc/stellarindex.toml native

# 2. Which reserve-balance source did the writer actually use?
#    The chained reader consults the account_observations hypertable
#    FIRST; the TOML map is only consulted when at least one watched
#    account has NO observation at-or-before the snapshot ledger
#    (the whole call then drops to the static map — no mixing).
#    Check observer coverage for each account in sdf_reserve_accounts:
runuser -u postgres -- psql -d stellarindex -c \
  "SELECT account_id, max(ledger) AS latest_obs,
          (SELECT balance_stroops FROM account_observations b
            WHERE b.account_id = a.account_id
            ORDER BY ledger DESC LIMIT 1) AS latest_balance
     FROM account_observations a
    WHERE account_id IN (SELECT unnest(ARRAY['G...','G...']))  -- paste sdf_reserve_accounts
    GROUP BY account_id;"

# 3. Only if step 2 shows an UNCOVERED account (fallback path in
#    play): sum the TOML reserve balances and compare to the frozen
#    total. This check applies to the fallback path ONLY — a wrong
#    TOML value is harmless while the observer covers every account.
grep -A 100 "^\[supply" /etc/stellarindex.toml
python3 -c "
import re, sys
content = open('/etc/stellarindex.toml').read()
balances = re.findall(r'^\\s*\"?([A-Z0-9]+)\"?\\s*=\\s*\"?(\\d+)\"?', content, re.M)
total = sum(int(b) for _, b in balances if len(_) == 56)
print(f'sum of reserve balances: {total} stroops = {total/1e7:.2f} XLM')
print(f'frozen total:           500018068120000000 stroops = 50,001,806,812.00 XLM')
print(f'difference:             {500018068120000000 - total} stroops')
"

# 4. Dry-run with current config to confirm reproduction. NB: the
#    writer also dials ClickHouse for the ledger's real close time
#    (-ch-addr, default 127.0.0.1:9300) — a down ClickHouse fails
#    the run (that's unit_failed territory, not this alert).
stellarindex-ops supply snapshot -config /etc/stellarindex.toml -dry-run
```

## Typical root causes

1. **Corrupt / inflated observer balances.** The live
   `account_observations` rows are what the writer sums first
   (`internal/supply/config_reader.go` doc block + the chained
   reader in `internal/ops/supply/supply.go`). A decode or backfill
   bug that inflated a reserve account's `balance_stroops` drives
   circulating to ≤ 0 with a perfectly correct TOML.
   - Signal: step 2's `latest_balance` for one account is
     implausibly large vs. stellar.expert.
   - Mitigation: file a P2 against the AccountEntry observer;
     as a stopgap the affected rows can be corrected from chain
     state and the writer re-run.

2. **Reserve balance overstated in the TOML fallback** (only in
   play when step 2 shows an uncovered account). Operator copied
   an SDF announcement value with the wrong scale (e.g. wrote a
   USD-equivalent or an XLM value where stroops were expected,
   inflating by 10^7).
   - Signal: the diagnostic Python sums show
     reserve_total ≈ 10^7 × frozen_total.
   - Mitigation: divide the offending balance entry by 10^7;
     re-run the writer.

3. **All-reserve config — every account labelled "reserve".** A
   mistaken `sdf_reserve_accounts` list that includes the issuer
   account or a payment account. This poisons BOTH paths — the
   observer sums whatever accounts the list names.
   - Signal: extra G-strkeys in the list compared to SDF's
     announcement.
   - Mitigation: remove the misclassified accounts.

4. **XLMComputer bug.** Should not happen — the algorithm is
   trivial — but if a recent code change broke it, this would
   fire.
   - Signal: `stellarindex-ops supply snapshot -dry-run` produces
     the same wrong value with verified-correct inputs on both
     reserve paths.
   - Mitigation: roll back the writer binary; file a P2 bug.

## Mitigation

- [ ] Step 1 — Identify root cause via Quick diagnosis (observer
      coverage FIRST, TOML only if the fallback is in play).
- [ ] Step 2 — If observer-data error: correct / re-derive the
      affected `account_observations` rows; file the observer bug.
- [ ] Step 3 — If config error: fix the TOML, force a run.
- [ ] Step 4 — If algorithm bug: roll back; file a P2 bug.
- [ ] Step 5 — In every case, verify `circulating_supply > 0` on
      the next snapshot.
- [ ] Verification: alert clears within 5 min after a corrected
      snapshot lands.

## Known false-positive patterns

- **Hypothetical post-XLM-burn future.** If Stellar somehow burned
  every XLM in circulation (e.g. a coordinated network shutdown),
  this alert would be correct, not a false positive. ADR-0011's
  zero-is-a-valid-answer note doesn't apply to native XLM
  specifically — XLM is hard-capped and indestructible by design.

## Related

- `supply-snapshot-unit-failed.md` — covers the writer-failure
  path; this alert presumes the writer ran successfully but
  produced a wrong value.
- `supply-snapshot-never-initialized.md` — the absent-series blind
  spot's owner (gauge never emitted at all).
- `supply-cross-check-divergence.md` — divergence between classic
  + SAC counterparts.
- ADR-0011 §"Algorithm 1 — native XLM".
- ADR-0021 — the chained-fallback reserve-reader decision.
- `docs/architecture/supply-pipeline.md` §"The chained-fallback
  reader pattern".

## Changelog

- 2026-08-29 — re-verified against HEAD. Primary diagnosis command
  fixed: `supply audit native -config ...` doesn't parse (Go's flag
  package stops at the first positional) — flags now precede the
  positional. Root-cause model updated for the chained-fallback
  reserve reader (L2.12a): the live account_observations hypertable
  is consulted FIRST and the TOML map is fallback-only, so an
  observer-data root cause was added and the Python reserve-sum
  check scoped to the fallback path. Noted the writer's ClickHouse
  close-time dependency (-ch-addr, default 127.0.0.1:9300).
  Coverage caveat: r1's timer path is deployed + enabled (alert
  live); absent-gauge blind spot rerouted to
  `_never_initialized`. Rule citation →
  `rules.r1/supply-snapshot.yml`; commands use r1 shapes.
- 2026-04-30 — initial draft alongside #295 (textfile + alerts).
- 2026-04-30 — coverage caveat added: this alert is timer-path-
  only and silently doesn't fire on aggregator-resident-only
  deployments. Cross-references supply-pipeline.md for the
  two-path overview and notes the equivalent API-layer probe.
