# W16 — Documentation truth, RFP/proposal/ADR alignment

## Scope

Every doc claim re-tested against code or runtime. Reconciliation
runbook is at [04-reconciliation.md](../04-reconciliation.md).

## Inputs

- `docs/stellar-rfp.md`, `docs/freighter-rfp.md`,
  `docs/ctx-proposal.md`
- `docs/discovery/*.md`
- `docs/architecture/*.md`
- `docs/operations/*.md`
- `docs/reference/{api,config,metrics}/`
- `docs/blog/`
- `CHANGELOG.md`
- `CLAUDE.md`
- prior audit directories (`docs/audit-2026-04-29`,
  `docs/audit-2026-05-02`, `docs/audit-2026-05-12`)
- agent-memory files under
  `~/.claude/projects/-Users-ash-code-ratesengine/memory/`

## Checks

| # | Check | Method |
| --- | --- | --- |
| W16.1 | R01..R08 in 04-reconciliation.md fully populated | per-row |
| W16.2 | Every ADR has terminal status | R01 |
| W16.3 | Every architecture doc has terminal status | R02 |
| W16.4 | Every operations doc has terminal status | R03 |
| W16.5 | Every RFP / proposal row | R04 |
| W16.6 | Every reference doc | R05 |
| W16.7 | Every CHANGELOG entry rc.71..rc.81 | R06 |
| W16.8 | CLAUDE.md drift | R07 |
| W16.9 | Memory entries verified (memory-truth pass) | R08 |
| W16.10 | NEW: prior audit residue audit (`docs/audit-2026-04-29/`, `docs/audit-2026-05-02/`, `docs/audit-2026-05-12/`) is read-only reference; closed findings re-tested cold per protocol §1 | grep |
| W16.11 | NEW: `docs/audit-2026-05-12/CLOSURE-SUMMARY.md` claims of closure — re-test cold against current code | per-claim |
| W16.12 | NEW: postmortems in `docs/operations/postmortems/` are accurate accounts (cross-ref with git history) | per-postmortem |

## Closure criteria

R01..R08 fully populated. Findings on every drift.
