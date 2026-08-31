---
title: Runbook — stellarindex_systemd_unit_failed
last_verified: 2026-08-31
status: ratified
severity: P3
---

# Runbook — `stellarindex_systemd_unit_failed`

## At a glance

| | |
| --- | --- |
| **Severity** | ticket (P3) |
| **Fires when** | any systemd unit sits in `failed` for 15m+, except units with their own alert |
| **Producer** | node_exporter's systemd collector (`node_systemd_unit_state`) |
| **Exclusions** | `scripts/ci/unit-failed-dedicated.baseline` — units whose own alert carries better triage |

**Why a catch-all.** This host runs ~20 oneshot timers and, before
2026-08-31, only five were named in any alert rule. `directory-sync` is
what made that urgent: it is the **sole writer of `account_directory`**,
the table the scam-pricing gate consults on every aggregated price serve.
It is `Type=oneshot` with no `OnFailure` and no metric — so if it stops,
the table freezes at its last good snapshot, a newly-flagged scam issuer
is never learned, and the gate keeps serving happily on stale input
(wave-D LID-6).

Naming units one at a time is how that gap opened, so this alert inverts
it: **everything is covered unless it has a better alert of its own.**

## The thing to understand first

Most of these units write data that something else *reads and then
trusts*. A failed sync therefore does not usually surface as an error —
it surfaces as a **consumer serving stale data confidently**, somewhere
else entirely, possibly days later.

So the question is never only "why did the unit fail", but **"what has
been reading its output since it stopped?"**

## Quick diagnosis (≤ 5 min)

```sh
systemctl status <unit>
journalctl -u <unit> --since -24h --no-pager | tail -50
systemctl list-timers <unit%.service>.timer   # is the timer even enabled?
```

Then, per unit class:

| Unit | Writes | Who trusts it | Staleness symptom |
| --- | --- | --- | --- |
| `directory-sync` | `account_directory` | the scam-pricing gate, on every serve | a flagged issuer keeps getting a published price |
| `holders-rollup` / `census-rollup` | rollup tables | holder counts / census endpoints | figures frozen at last good run |
| `issuer-flags` | issuer flag set | asset payloads | stale verification badges |
| `sep1-refresh` | SEP-1 metadata | asset detail | stale home domains |
| `cap67-movements` | classic movement rows | supply Algorithm 2 | supply divergence |

## Mitigation

1. Fix the cause, then `systemctl start <unit>` — do not just
   `reset-failed`, which clears the state without doing the work and
   silences this alert while the data stays stale.
2. Confirm the unit actually wrote something: check the table's
   `max(synced_at)` / row count moved, not just that the unit exited 0.
3. If the failure is upstream (a non-200 from a third-party directory,
   say) and will not resolve, say so on the ticket — the consumer is
   still serving stale data and that is the reportable fact.

## When NOT to act

- A unit failed once and its own timer already restarted it
  successfully: `for: 15m` should have ridden that out, so check whether
  it is genuinely still failed before digging.

## Adding an exclusion

Only when the unit gets a **dedicated alert with better triage** — never
to quiet a noisy one. Add it to
`scripts/ci/unit-failed-dedicated.baseline` with the owning alert named;
`scripts/ci/lint-unit-failed-baseline-test.sh` fails if the entry is not
actually referenced by a rule, so an exclusion cannot become a silent
suppression.

## Related

- [`verify-archive-unit-failed.md`](verify-archive-unit-failed.md) — dedicated alert, excluded here.
- [`pgbackrest-backup-unit-failed.md`](pgbackrest-backup-unit-failed.md) — dedicated alert, excluded here.
- [`price-divergence.md`](price-divergence.md) — where a stale `account_directory` eventually shows up.
