# Execution Protocol

## 1. Evidence Discipline

Nothing is accepted from memory.

Every material claim must cite at least one of:

- a local file reference with line anchor
- a generated inventory artifact in this directory
- a command output transcribed into `evidence/log.md`
- a test file and the behavior it actually asserts

## 2. Evidence Log Format

Use [evidence/log.md](evidence/log.md).

One row per evidence item:

- `EV-XXXX`
- date
- claim or observed fact
- source refs
- notes

If multiple findings rely on one evidence item, reuse the same
evidence ID instead of duplicating prose.

## 3. Cross-File Interaction Log

Use [evidence/cross-file-interactions.md](evidence/cross-file-interactions.md).

Record any interaction that crosses a package or subsystem boundary,
including:

- binary -> package wiring
- package -> package interfaces
- handler -> storage adapter
- dispatcher -> decoder
- aggregator -> Redis/API
- docs -> code contracts
- workflow -> script -> generated artifact

Each interaction should say what the dependency is, what can break, and
where the evidence sits.

## 4. Per-File Audit Loop

For every tracked file in `inventory/file-coverage.tsv`:

1. Read the file.
2. Record its role.
3. Record inbound dependencies.
4. Record outbound dependencies.
5. Record any invariant or trust boundary it touches.
6. Record tests that exercise it, if any.
7. Record docs that describe it, if any.
8. Mark the row:
   - `done`
   - `excluded`
   - `blocked`
9. Add evidence refs.
10. If it affects any cross-file seam, add an entry to the
    cross-file interaction log.

No file may remain `todo` at the end.

## 5. Status Rules

Use these statuses in `inventory/file-coverage.tsv`:

- `todo`: untouched
- `in_progress`: currently under review
- `done`: reviewed for this audit snapshot
- `blocked`: review attempted but dependency unresolved
- `excluded`: explicitly not audited in this run, with reason

## 6. Finding Rules

Use [05-findings-register.md](05-findings-register.md).

A finding needs:

- stable ID
- severity
- concise title
- affected files or subsystems
- evidence refs
- current disposition

Severity scale:

- `critical`: integrity/security/correctness issue with systemic or
  launch-blocking implications
- `high`: serious correctness, security, or operational risk
- `medium`: real defect or drift with bounded blast radius
- `low`: minor drift, weak control, or low-risk bug
- `note`: non-finding observation worth preserving

## 7. Exclusion Rules

Use [06-exclusions-register.md](06-exclusions-register.md).

Anything skipped must be recorded with:

- exact thing excluded
- why it is excluded
- whether exclusion is temporary or permanent for this audit
- what evidence would be needed to bring it back into scope

## 8. Documentation-Truth Rules

When docs disagree with code:

- do not silently trust either
- log the discrepancy
- record evidence from both sides
- if one side is clearly stale, register a finding or note

## 9. Test Interpretation Rules

Tests are evidence of asserted behavior, not proof of correctness.

For each important test suite, record:

- what behavior is actually asserted
- what is not asserted
- whether the test is unit, integration, or fixture-based
- whether CI runs it

## 10. Final Condition

The audit is only execution-ready when:

- the inventory is complete
- the control docs are in place
- the evidence and finding logs are usable without relying on memory
