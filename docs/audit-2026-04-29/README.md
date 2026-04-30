# Audit Workspace — 2026-04-29

This directory is the execution workspace for the cold adversarial audit
of the Rates Engine repository.

It is not a narrative report. It is the control plane for the audit:

- what must be audited
- in what order
- how evidence is recorded
- how per-file coverage is tracked
- how findings, exclusions, and cross-file interactions are logged

## Start Here

Read in this order:

1. [00-plan.md](00-plan.md)
2. [02-protocol.md](02-protocol.md)
3. [03-journeys.md](03-journeys.md)
4. [04-reconciliation.md](04-reconciliation.md)
5. [01-tracker.md](01-tracker.md)
6. [inventory/repo-snapshot.md](inventory/repo-snapshot.md)
7. [inventory/file-coverage.tsv](inventory/file-coverage.tsv)
8. [07-remediation-plan.md](07-remediation-plan.md)

## Working Rules

- Every claim needs an evidence reference.
- Every exclusion must be written down explicitly.
- Every tracked file must move from `todo` to a terminal status in
  `inventory/file-coverage.tsv`.
- Every material cross-file interaction must be recorded in
  [evidence/cross-file-interactions.md](evidence/cross-file-interactions.md).
- Findings belong in [05-findings-register.md](05-findings-register.md),
  not in ad hoc notes.

## Directory Layout

- [00-plan.md](00-plan.md): repo-specific audit plan.
- [01-tracker.md](01-tracker.md): master section and pass tracker.
- [02-protocol.md](02-protocol.md): execution protocol and evidence rules.
- [03-journeys.md](03-journeys.md): mandatory end-to-end and hostile-path journeys.
- [04-reconciliation.md](04-reconciliation.md): required comparison passes.
- [05-findings-register.md](05-findings-register.md): live findings register.
- [06-exclusions-register.md](06-exclusions-register.md): exclusions, assumptions, blocked items.
- [07-remediation-plan.md](07-remediation-plan.md): prioritized post-audit remediation sequence tied to finding IDs.
- [evidence/](evidence/): evidence log and cross-file interaction log.
- [inventory/](inventory/): generated repo snapshot and per-file coverage inventory.

## Inventory Refresh

The inventory files are generated from the current checkout by:

```sh
./docs/audit-2026-04-29/inventory/generate.sh
```

Run that again if the working tree changes materially during the audit.

## Scope Reminder

This workspace is for the audit of the repository as checked out at the
time the inventory was generated. Hosted GitHub controls, remote infra,
and live third-party services may need separate verification artifacts if
they are pulled into scope later.
