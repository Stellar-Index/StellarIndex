---
title: Runbook — network-state-verify
last_verified: 2026-10-03
status: draft
severity: P3
---

# Runbook — `stellarindex_network_state_verify_stale` / `stellarindex_network_state_verify_failed`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_network_state_verify_stale`, `stellarindex_network_state_verify_failed` |
| Severity | P3 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/storage.yml`; multi-host twin in `deploy/monitoring/rules/storage.yml`. |
| Typical MTTR | 15 min to triage; a correction is a code change |
| Impact | Current state served from `ledger_entries_current` (account pages, holder lists, supply reads) may disagree with the network for the reported keys. |

`verify-network-state.timer` runs `stellarindex-ops verify-network-state`
daily at 13:47 UTC (+ ≤ 10 min jitter) through `run-heavy-job.sh`. It is the
only check that compares the lake's derived state with state the network
publishes itself; every other verifier compares our rows with our own
ledger meta. The verdict lands in
`/var/lib/node_exporter/textfile_collector/network_state_verify.prom`, written
only after every check completed.

| Check (`check=` label) | Compares | Fails when |
| ----- | ----- | ----- |
| `hot_archive` | Every archived entry in the history archive's hot-archive buckets at the newest checkpoint both sides have reached, against its `ledger_entries_current` row | The entry body differs, or the lake row is a removal. Also fails once when entries exist but none could be compared. |
| `lumens` | Native XLM in accounts, claimable balances, liquidity pools and native-SAC balances plus `fee_pool`, against `total_coins`, as of the newest `stellar.ledgers` row | The residual (held + fee_pool − total_coins) is not exactly 0. |

A protocol upgrade can rewrite hot-archive entries without emitting ledger
meta (CAP-0076 at ledger 59,501,299 changed 394 of them), so the lake never
sees the change. `hot_archive` is the check that surfaces it.

Keys the lake does not hold (`class="absent"`) and keys the lake changed after
the checkpoint (`class="newer"`, e.g. restored since) are reported in
`stellarindex_network_state_verify_hot_archive_entries` but are not failures.

## Symptoms

- `_stale`: no completed run for 48 h. The timer is not installed or enabled,
  the history archive is unreachable, or every run crashed before writing its
  verdict.
- `_failed{check="hot_archive"}`: `{{ $value }}` archived entries disagree with
  the lake. `journalctl -u verify-network-state -n 300` lists up to 20
  `MISMATCH` lines (entry type, base64 key, reason).
- `_failed{check="lumens"}`: the journal prints each domain's sum and the
  residual in stroops. Negative = the lake is missing holdings; positive = the
  lake holds lumens the network no longer counts.

## Quick diagnosis (≤ 5 min)

```sh
systemctl list-timers verify-network-state.timer
systemctl status verify-network-state.service
journalctl -u verify-network-state --since -50h | tail -n 80
cat /var/lib/node_exporter/textfile_collector/network_state_verify.prom
```

- Timer absent: the archival-node role has not been applied since the unit
  landed. Apply with `--tags ops-jobs`.
- Journal shows an archive error: check `stellar.history_archive_url` in the
  config and that the archive publishes the checkpoint named in the journal.
- Journal shows a ClickHouse error: fix ClickHouse first; the previous verdict
  stays in place and `_stale` fires once it ages out.

## Mitigation

- [ ] `hot_archive`, date the change: re-run at the checkpoints either side of
      a suspect upgrade ledger, e.g.
      `stellarindex-ops verify-network-state -config /etc/stellarindex.toml -checks hotarchive -checkpoint 59501247`
      and `-checkpoint 59501311`. Clean before and failing after means the
      network rewrote the entries without ledger meta. There is no repair
      verb: `ledger_entries_current` has one writer, the
      `ledger_entry_changes` view. Record the keys and open an issue; the
      correction is a code change.
- [ ] `hot_archive`, lake behind: if `verify-lake` also reports
      `entry_changes` failures, re-read the deficient range with
      `stellarindex-ops ch-backfill -config PATH -from LO -to HI -bucket galexie-archive -write`
      and re-run.
- [ ] `lumens`: a negative residual on a lake whose entry changes do not
      reach back to genesis means dormant holdings were never captured. Use
      `reconcile-balances` on a sample of accounts to find which domain is
      short.
- [ ] Verification: `systemctl start verify-network-state.service`; every
      `stellarindex_network_state_verify_failures` series reads 0 and both
      alerts clear within one scrape.

## Seed entry state from a history-archive checkpoint

Use this when the lake has no earlier state for entries that were dormant before its capture window (ADR-0021; the supply seeder reports such accounts). It reads one checkpoint's ledger-entry state and fills `ledger_entry_changes`. Size the write set first with `-dry-run`; `-write` needs `-limit 0`.

```sh
stellarindex-ops state-snapshot -config /etc/stellarindex.toml -scope contracts -limit 0 -dry-run
stellarindex-ops state-snapshot -config /etc/stellarindex.toml -scope contracts -limit 0 -write
```

## Related

- [lake-verify](lake-verify.md) — soundness of the raw lake against its own meta.
- [alerts-catalog](../alerts-catalog.md)
