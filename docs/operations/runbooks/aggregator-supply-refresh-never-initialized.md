---
title: Runbook — aggregator-supply-refresh-never-initialized
last_verified: 2026-05-12
status: draft
severity: P3
---

# Runbook — `ratesengine_aggregator_supply_refresh_never_initialized`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `ratesengine_aggregator_supply_refresh_never_initialized` |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/aggregator.yml` |
| Typical MTTR | 15–60 min |
| Impact | The aggregator's supply-refresh goroutine has never produced a successful tick since process start. F2 fields (`circulating_supply`, `total_supply`, `max_supply`, `market_cap_usd`, `fdv_usd`) on `/v1/assets/{id}` are NULL for every asset. |

## Symptoms

- `ratesengine_aggregator_supply_refresh_total{outcome="ok"} == 0` since aggregator boot.
- `/v1/assets/USDC-G…` returns the `AssetDetail` envelope with all `*_supply` and `*_cap_usd` fields null.
- Aggregator log shows no `supply refresh complete` info lines.

## Quick diagnosis (≤ 5 min)

```sh
# Confirm the goroutine wired
journalctl -u ratesengine-aggregator -n 200 --no-pager | grep -iE 'supply.*refresh|watched_'

# Check the operator config for the watched-set knobs
grep -E '\\[supply\\]|watched_classic|watched_sep41|sdf_reserve_accounts' /etc/ratesengine/config.toml

# Sample one watched asset's supply storage
sudo -u postgres psql -d ratesengine -c "SELECT * FROM asset_supply_history ORDER BY time DESC LIMIT 5;"
```

Key signals:
- **Empty `[supply].watched_*`** → the aggregator's supply-refresh path is gated on having a non-empty asset set; un-set = goroutine is intentionally silent. **This is the most common cause** (F-1266 audit-2026-05-12).
- **Non-empty config but `asset_supply_history` empty** → goroutine is wired but every asset is failing. Check the LCM reader / classic-supply observers.
- **Aggregator process recently restarted** → wait 5 min; the first refresh is delayed by the bootstrap window.

## Mitigation (≤ 15 min)

- [ ] Step 1 — populate the watched asset list. Edit `/etc/ratesengine/config.toml`:
  ```toml
  [supply]
  watched_classic = [
      "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
      "EURC-GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2",
      # ... etc; full list per docs/operations/supply-snapshot.md
  ]
  watched_sep41 = []
  sdf_reserve_accounts = ["GA…"]
  ```
- [ ] Step 2 — restart the aggregator: `systemctl restart ratesengine-aggregator`.
- [ ] Step 3 — within 5 min, `ratesengine_aggregator_supply_refresh_total{outcome="ok"}` should increment. Sample a watched asset's `/v1/assets/{id}` and confirm the F2 fields populate.
- [ ] Verification: `circulating_supply` non-null on at least one watched asset; `market_cap_usd` non-null when the asset has a USD price.

## Root cause analysis

This alert is almost always operator-config drift. Most common chain:
1. New asset launches; operator doesn't add it to `[supply].watched_classic`.
2. F2 fields on its `/v1/assets/{id}` show null.
3. Customer support ticket lands.

Long-term fix: auto-populate `watched_classic` from the verified-currency catalogue (`internal/currency/data/seed.yaml`). Tracked separately.

## Known false-positive patterns

- **First 5 min after aggregator boot**: the supply-refresh goroutine waits on the first baseline window before producing a tick. The alert fires only after `for: 30m` per the rule definition, but operators eyeballing the metric within the first 5 min may misread "no data yet" as "broken".

## Related

- `supply-refresh-stalled.md` — when the goroutine HAS produced ticks but not recently.
- `supply-refresh-error-dominant.md` — when most ticks are failing.
- `supply-snapshot-never-initialized.md` — sibling for the operator-CLI snapshot path.
- ADR-0011 — three-domain supply algorithm.
- F-1266 (audit-2026-05-12) — the on-r1 manifestation of this alert.

## Changelog

- 2026-05-12 — initial draft (audit-2026-05-12 F-1237 closure).
