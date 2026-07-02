---
name: verify-done
description: The pre-completion discipline harness for Stellar Index — run before claiming ANY change is done, committing a batch, or pushing. Executes the full gate stack (verify.sh foreground, drift generators for touched surfaces, contract tests, staged-content check) and encodes the session-learned failure modes that have shipped broken changes before. Use when finishing a task, before git push, or when asked "is this done?".
---

# /verify-done — the discipline layer

Every other skill's "verify" step is this skill. Do not shortcut it —
each rule below exists because its absence shipped a real breakage
(the incident is cited).

## 1. The gate stack (run in order, ALL foreground)

```sh
# 1. The canonical gate. NEVER pipe through tail/head — the pipe's
#    exit code masks failures (shipped a lint break twice this way).
bash scripts/dev/verify.sh > /tmp/verify.out 2>&1; EXIT=$?
echo "exit=$EXIT"; grep "ALL CHECKS PASSED" /tmp/verify.out
# exit MUST be 0 AND the string MUST be present. A backgrounded run's
# "exit 0" task-notification can lie — always check the string.
```

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

## 4. Claim honesty

When reporting done: name what was verified AND what was not (e.g.
"unit-tested + probe-verified; not yet exercised against r1"). If a
test failed and you're deferring it, say so explicitly — never let a
green summary imply more than the gates proved.
