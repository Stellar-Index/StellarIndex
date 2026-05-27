# W20 — CG/CMC parity execution

## Scope

Execute the matrix in
[08-cgcmc-parity-matrix.md](../08-cgcmc-parity-matrix.md). Every row
must resolve to `covered` / `partial` / `gap` / `non-goal` / `n/a`
with evidence.

## Method

Per row:
1. Read the cell's "feature" description.
2. Locate our equivalent (file:line or live URL).
3. Compare scope: what does CG / CMC ship that we don't, or
   vice versa.
4. Mark cell.
5. If `partial` or `gap`, file a finding (severity per rubric;
   on launch-headline features, minimum `high`).

## Cross-reference

This workstream's findings feed [05-findings-register.md](../05-findings-register.md).

## Closure criteria

Matrix fully populated. No blank `?` cells. Findings opened on
every `gap` row.
