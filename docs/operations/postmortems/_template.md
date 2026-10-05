---
title: "[SEV-N] one-line summary — YYYY-MM-DD"
date: YYYY-MM-DD
severity: SEV-?
status: draft | published
duration: HH:MM
authors: [maintainer]
---

<!-- Copy to YYYY-MM-DD-short-slug.md. Write for the person who joins in six months and inherits the runbook this
incident motivated; no blame. Keep status: draft until facts are straight; publish publicly only after stakeholder review. -->

# [SEV-N] one-line summary — YYYY-MM-DD

## TL;DR

<!-- 2-3 sentences: what happened and what we did about it. -->

## Impact

<!-- Surfaces and pairs/assets affected, customer-visible duration (may be shorter than the incident), request count or "unmeasured but bounded by N". -->

## Timeline (UTC)

<!-- Terse; from logs/git/journalctl, not memory: first external signal, first alert, triage decision, mitigation start/complete, all-clear. -->

- HH:MM — …

## Root cause

<!-- The mechanism: latent condition + trigger. Cite code paths, configs, runbook gaps. -->

## What went well / poorly / lucky

<!-- Be specific: "alert X fires after 30 min but the customer signal arrived at 5 min", not "detection was slow". Name near-misses. -->

## Action items

<!-- Each a tracked PR or ticket with owner and due date: - [ ] Action — owner — link -->

- [ ] …

## Lessons

<!-- 1-3 bullets worth a docs/architecture/domain-traps.md entry or a SEV-playbook checklist line. -->

## Related

- Alerts fired / runbooks triggered / fix PRs / code paths:
