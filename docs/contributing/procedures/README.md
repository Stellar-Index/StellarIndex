---
title: Project procedures
last_verified: 2026-09-03
status: living doc
---

# Project procedures

Nine checklists encoding this repo's release, deploy, review and diagnosis
discipline, plus the judgement from its incident corpus. If you are doing one
of these tasks, read the procedure rather than working from memory — each one
exists because a step was missed at least once.

| Procedure | Use for |
|---|---|
| [add-onchain-source](add-onchain-source.md) | A new Soroban protocol: six files, six wiring edits in other packages, contract-identity gating, and the lockstep checks. Miss a wiring edit and the source compiles, registers nowhere, and silently emits nothing. |
| [add-cex-connector](add-cex-connector.md) | A new CEX or FX venue: the `Connector` framework, and the per-source amount-scaling traps. |
| [add-endpoint](add-endpoint.md) | Any API route or wire-shape change: the spec, all three generators, SDK triage, and the cache policy. |
| [add-metric](add-metric.md) | A metric plus BOTH rule trees, its runbook, and the five-lint guard chain. |
| [cut-release](cut-release.md) | CHANGELOG promotion and guard-rail tagging. |
| [deploy-r1](deploy-r1.md) | The deploy workflow, the post-deploy verification battery, and rollback. |
| [review-stellarindex](review-stellarindex.md) | Per-subsystem adversarial review checklists drawn from the finding corpus. |
| [diagnose-stellarindex](diagnose-stellarindex.md) | Incident decision trees: frozen cursor, stale prices, red verdict. |
| [verify-done](verify-done.md) | The pre-completion gate stack every other procedure ends with. |

For narrative "how do I do X" guidance rather than a gated checklist, see
[../task-recipes.md](../task-recipes.md).
