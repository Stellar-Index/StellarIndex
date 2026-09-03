---
title: The pre-completion gate stack
last_verified: 2026-09-03
status: living doc
---

# docs/contributing/procedures/verify-done.md — the discipline layer

Every other skill's "verify" step is this skill. Do not shortcut it —
each rule below exists because its absence shipped a real breakage
(the incident is cited).

## 1. The gate stack

```sh
# Commit the candidate first: prepush refuses dirty trees and judges HEAD.
make prepush > /tmp/prepush.out 2>&1 & PID=$!
wait "$PID"; EXIT=$?
echo "exit=$EXIT"
grep "ALL REQUIRED CHECKS PASSED" /tmp/prepush.out
# Exit MUST be 0 AND the literal marker MUST be present.
```

Never pipe the gate through `tail`, `head`, `sed` or `tee`; that reads the
pipe's status instead of the gate. Profile and platform behavior is documented
in [local verification before CI](../local-verification.md).

If the diff touched any of these surfaces, ALSO run its generator and
commit the regenerated output (two of the three have silently drifted
onto main before):

| Touched | Run |
|---|---|
| `openapi/*.yaml` | `make docs-api && make docs-postman && make web-generate-api` (ALL THREE) |
| config struct tags | `make docs-config` |
| `internal/obs/metrics.go` | `make docs-metrics` |
| either Prometheus rule tree | `make monitoring-check` (promtool + dead-ref + tree-equivalence differ) |
| `pkg/client` or spec response shapes | `go test ./pkg/client/` (the SDK↔spec contract gate) |
| pipeline wiring (sink/registry/dispatcher) | `go test -run TestLockstep ./internal/pipeline/` |
| `web/explorer` | `cd web/explorer && pnpm typecheck && pnpm lint` |
| any `scripts/ci/*.baseline` | growth needs a `Baseline-Growth:` commit trailer (CS-098) |

## 2. Staged-content check (the 6161dd50 rule)

Before EVERY commit:

```sh
git diff --cached --stat
```

and read it: does the file count match what you changed? A failed
pathspec in a `git add` chain aborts the add but NOT a following
`commit` — commit 6161dd50 described 6 files and captured 2. After
committing, `git show --stat HEAD | tail -3` to confirm.

Stage by NAME, never `git add -A` (once swept an in-progress audit
workspace into an unrelated docs PR).

## 3. Behavioral verification (not just green gates)

Green gates prove you didn't break the build; they don't prove the
change WORKS. For anything with a runtime surface, exercise it:

- API change → curl the endpoint (local stack or
  `https://api.stellarindex.io` read-only) and READ the payload.
- ops subcommand → run it with safe flags against real data
  (the verify-served-values first run caught a 1e7 unit bug this way
  that unit tests had blessed).
- New guard/lint/alert → **probe it**: introduce the violation it
  guards against, watch it fail with a useful message, revert.
  A guard that has never failed is decorative.
- SQL / CAGG / migration claims → verify empirically on r1
  (read-only; SQL via file+scp, never inline `$$` over ssh — it
  expands to the shell PID and silently corrupts the query).
- **Ask what your check does NOT cover, then check that too.** A
  passing check over a narrow slice reads identically to a passing
  check over the whole surface. Two 2026-07-27/28 cases: the 13-GET
  smoke stayed green while **21 of 94 routes 503'd** (it touches none
  of them), and supply was "verified" for months against the trustline
  sum alone — exact, and therefore blind to the claimable component
  that was 13.2% short. Coverage tools now exist for both
  (`scripts/ops/route-sweep.sh`,
  `scripts/ops/reconcile-supply-vs-horizon.sh`); run them rather than
  re-deriving the same blind spot.
- **Aggregates hide what per-entity reads expose.** The all-asset
  supply reconciliation passed 5/8 on the SAME defect that made 19/50
  individual account balances wrong — summing thousands of entities
  lets errors cancel. When a total looks right, spot-check members.

## 4. Claim honesty

When reporting done: name what was verified AND what was not (e.g.
"unit-tested + probe-verified; not yet exercised against r1"). If a
test failed and you're deferring it, say so explicitly — never let a
green summary imply more than the gates proved.
