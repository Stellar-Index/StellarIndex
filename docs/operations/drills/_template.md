---
title: "<<YYYY-MM>> <<short scenario name>>"
date: <<YYYY-MM-DD>>
type: tabletop | chaos | annual-dr
scenario: scenarios/<<file>>.md
participants: [<<name>>, <<name>>, <<name>>]
last_verified: <<YYYY-MM-DD>>
---

# <<YYYY-MM>> drill writeup — <<short scenario name>>

Clone to `docs/operations/drills/<<YYYY-MM>>-<<short-name>>.md` at drill start.
Shape follows [sev-playbook.md §6](../sev-playbook.md), shorter. File action items
with the `drill-action` label, then add a row to the log in [README.md](README.md).

## Trigger

<<Scenario's "Trigger" verbatim. For chaos drills add the injection command + UTC time.>>

## Response narrative

5-10 bullets, T+ relative to injection: what the team did, runbook section, dashboard, command.

- T+<<MM:SS>> — <<…>>

## Gaps observed

Runbook, tooling or playbook gaps; each becomes an action item.

- <<gap>>

## Action items

- [ ] <<action>> — owner @<<handle>>, due <<YYYY-MM-DD>>, <<#issue>>

## Score

Each of the scenario's pass criteria as `pass` / `partial` / `fail`; a `partial` or `fail` has an action item.

| # | Criterion | Score | Notes |
| --- | --- | --- | --- |
| 1 | <<…>> | <<…>> | <<…>> |

**Overall:** <<pass / partial / fail>>.

## Sign-off

Drill leader, scribe, date action items were posted, playbook PR (if any).
