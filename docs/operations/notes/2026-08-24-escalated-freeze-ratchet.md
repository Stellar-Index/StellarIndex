# 2026-08-24 — escalated-freeze ratchet + unwired alert delivery

**Status: incident resolved by operator unfreeze; two structural defects
identified, one needing a design decision, one needing credentials.**

## What happened

Three pairs — `crypto:XLM/fiat:EUR` (frozen 08-22 04:03), `crypto:XLM/
fiat:GBP` (08-22 05:09), `crypto:BTC/fiat:EUR` (08-23 04:37) — ran the
full ADR-0019 extension ladder, escalated, and served pinned
last-known-good prices for up to two days with `flags.frozen=true`.
Their current inputs were verified sane before release (direct venue
trades ≈ cached VWAP ≈ USD-cross within 0.3 % on all three);
`freeze-unfreeze -write` cleared all three on 2026-08-24 ~09:50 UTC and
fresh values republished immediately. The pre-#124 caveat: these
Phase-2 conditions fired for weeks before v0.40.0, but with no marker
written they were consequence-free; #124 made freezes real, so the
latent defect below started actually pinning prices.

## Defect 1 — the frozen-z ratchet (design; needs an ADR-0019 amendment)

`computeConfidence` derives `returnPct = (curr − prev)/prev` where
`prev` is the pair/window's cached VWAP — which a freeze deliberately
stops updating (`keepFrozenVWAPAlive` refreshes only the TTL). So while
frozen, z measures **total drift since freeze-time**, divided by a
per-minute MAD (~0.001), and `MaxZScore` takes the tightest of the
30 m/1 d/7 d MADs. Consequences, all observed live:

- Auto-unfreeze (z < 3 twice) is reachable only if the market RETURNS
  to within ~3 MAD (≈0.3 %) of the freeze-time price. Any genuine move
  while frozen ratchets the ladder to escalation instead
  (XLM/EUR read z=87 ≈ its real ~2–8 % drift since Friday ÷ a
  ten-thousandths MAD; BTC/EUR z=11.97 ≈ 0.25 % drift).
- XLM/GBP auto-recovered its condition (z=0.17, streak 2) but stayed
  pinned because escalation removes auto-release — correct per the ADR,
  absurd given the z that escalated it was drift, not anomaly.
- Stale per-window caches are kept alive by the freeze: the pair's
  86400-window key held 0.1601 (a weeks-old value, 12 % off its own
  trades) — every 1d-window tick re-fired against it.
- The lifecycle is NOT generally broken: ETH/EUR froze 08-23 and
  auto-released after one extension (its market returned quickly).

**Proposed fix (decision needed, D1-adjacent):** during a freeze, feed
the z evaluation a *shadow* prev that tracks the refused-but-computed
fresh VWAPs (per-tick return, not drift-since-freeze), so auto-unfreeze
measures "is the market calm NOW" as the ADR intends. Alternatively (or
additionally) cap the drift-z's contribution once frozen. Either way the
change amends ADR-0019's auto-unfreeze semantics — proposing, not
landing, until ratified. Also worth fixing regardless of the decision:
`keepFrozenVWAPAlive` should not preserve a cache value the fresh
computation contradicts by >X % for N ticks (the 0.1601 case).

## Defect 2 — no alert delivery is wired at ALL (needs credentials)

The escalation P1 (`stellarindex_anomaly_freeze_escalated`, severity
page) **did fire** — counter at 5, alert active in Prometheus. It went
nowhere: the deployed `alertmanager.yml`'s `chat-page`, `chat-default`,
and `deadmansswitch` receivers contain **no webhook/Slack/PagerDuty
config** ("degrades silently if neither is set"). Every severity,
including pages and the dead-man's-switch heartbeat, only accumulates
in the Alertmanager UI. The only notifications that have ever reached
an inbox are GitHub Actions ci-health emails. Unblocking is operator
credentials: a Slack webhook URL and/or PagerDuty integration key
provisioned into the ansible apply — after which the D1 freeze-paging
decision actually has a delivery path to decide about.

## Also fixed in the same sweep (2026-08-24)

`sep1-refresh` ran daily in dry-run (unit drift, missing `-write` — the
identical class as supply-snapshot on 08-22; live unit synced to the
ansible template, backup in `/root/unit-backups-20260824`, issuers
freshness now green). That is the SECOND dry-run-by-drift unit found in
three days: a full `diff <live unit> <rendered template>` sweep across
every stellarindex systemd unit on r1 is cheap and would close the
class — recommended as a standing check or a small drift-audit script.
