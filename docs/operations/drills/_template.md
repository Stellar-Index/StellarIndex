---
title: "<<YYYY-MM>> <<short scenario name>>"
date: <<YYYY-MM-DD>>
type: tabletop | chaos | annual-dr
scenario: scenarios/<<file>>.md
participants: [<<name>>, <<name>>, <<name>>]
last_verified: <<YYYY-MM-DD>>
---

# <<YYYY-MM>> drill writeup — <<short scenario name>>

This is the canonical writeup template. Clone to
`docs/operations/drills/<<YYYY-MM>>-<<short-name>>.md` at the start
of a drill and fill in the `<<…>>` placeholders as the drill runs.

The shape mirrors the `Postmortem-but-shorter` discipline pinned in
[README.md §"Writeup template"](README.md#writeup-template) and
[sev-playbook.md §6](../sev-playbook.md). Action items go to the
issue tracker with the `drill-action` label and feed back into the
playbook + relevant runbooks.

## Trigger

<<Copy the scenario script's "Trigger event" section verbatim. For
chaos drills, also note the actual injection command run + UTC
timestamp at injection.>>

## Response narrative

5–10 bullets, in order, with timestamps (T+ relative to injection).
What did the team actually do? Cite the runbook section consulted,
the dashboard checked, the command run.

- T+<<MM:SS>> — <<oncall acknowledged page; opened #incident-<<…>> channel>>
- T+<<MM:SS>> — <<scribe started writeup; commander read trigger aloud>>
- T+<<MM:SS>> — <<…>>

## Gaps observed

What did the runbook miss? What tooling didn't exist? What
playbook step was unclear? Be specific — every gap here should
become an action item below.

- <<gap>>: <<what was missing / what didn't work>>
- <<gap>>: …

## Action items

Each item: owner (GitHub @-handle), due date (YYYY-MM-DD), issue
link. File the issue with the `drill-action` label.

- [ ] <<action>> — owner @<<handle>>, due <<YYYY-MM-DD>>, <<#issue>>
- [ ] <<action>> — owner @<<handle>>, due <<YYYY-MM-DD>>, <<#issue>>

## Score

Read each criterion from the scenario script's *Validation
criteria* section and score `pass` / `partial` / `fail`. A
`partial` or `fail` always corresponds to an action item above.

| # | Criterion (verbatim from scenario) | Score | Notes |
| --- | --- | --- | --- |
| 1 | <<…>> | <<pass / partial / fail>> | <<…>> |
| 2 | <<…>> | <<pass / partial / fail>> | <<…>> |

**Overall:** <<pass / partial / fail>>.

## References

- Scenario script: [`scenarios/<<file>>.md`](scenarios/<<file>>.md)
- Runbook(s) exercised: <<list with paths>>
- SEV playbook: [`../sev-playbook.md`](../sev-playbook.md)

## Sign-off

- **Drill leader:** <<name>>
- **Scribe:** <<name>>
- **Posted action items by:** <<YYYY-MM-DD>>
- **Promoted to playbook update:** <<yes / no — link to PR if yes>>
