#!/usr/bin/env bash
# Local sequential quality checks — run this before every push.
#
# CI runs these jobs in parallel; verify.sh is the strictly-sequential
# local equivalent that surfaces failures one at a time. Pattern
# borrowed from loop-app/scripts/verify.sh.

set -euo pipefail

cd "$(dirname "$0")/../.."

# ── Preconditions: five seconds, before anything expensive ───────────────
#
# Both failed local verify runs on 2026-09-07 died about ten minutes into a
# twelve-minute sequence, in sections unrelated to the change under test,
# on a missing install: `openapi-typescript: command not found` (web/explorer
# had no node_modules), then `tsc: command not found` (web/status had none).
# The conditions guarding those sections test for pnpm-lock.yaml, which is
# TRACKED and therefore always present — node_modules is not tracked and was
# never checked at all.
#
# This block changes nothing about WHAT any check below asserts. It resolves
# the preconditions those checks already depend on, in one pass, reports
# every gap at once, and stops before the first slow gate. Each condition
# below is the same condition as the section that needs it, so a gap named
# here is a gate that would fail later, and a gap absent here is not.
#
# Deferrable tools are only listed, never failed on — except under
# VERIFY_FAIL_ON_SKIP=1 (what `make prepush` sets), where defer_check exits 1
# and the identical verdict is simply reached ten minutes sooner.
preflight_gaps=()
preflight_defers=()
gap()   { preflight_gaps+=("$1"); }
willdefer() {
    if [ "${VERIFY_FAIL_ON_SKIP:-0}" = "1" ]; then gap "$1"; else preflight_defers+=("$1"); fi
}

echo "=== Preconditions ==="
# --show-current EXITS ZERO with empty output on a detached HEAD, so the
# branch has to be defaulted from the value, not from the exit code.
verify_branch="$(git branch --show-current 2>/dev/null || true)"
printf 'verify: HEAD=%s branch=%s tree=%s\n' \
    "$(git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)" \
    "${verify_branch:-<detached>}" \
    "$([ -z "$(git status --porcelain 2>/dev/null)" ] && echo clean || echo dirty)"

for tool in git make go python3 tar; do
    command -v "$tool" >/dev/null 2>&1 || gap "${tool} is not on PATH"
done

# make fmt and make lint call these out of $(GOBIN) by absolute path, so
# PATH is irrelevant and `command -v` would report a false miss.
# `|| true` because this file runs under set -e: with go absent, the bare
# substitution aborts the script at 127 with stderr suppressed, and the gap
# list below — which names the missing go — is never printed.
verify_go_bin="$(go env GOPATH 2>/dev/null || true)/bin"
for tool in gofumpt goimports golangci-lint; do
    [ -x "${verify_go_bin}/${tool}" ] || gap "${verify_go_bin}/${tool} is absent (make fmt / make lint call it by absolute path)"
done

# A Go-built tool embeds the toolchain it was compiled with, and golangci-lint
# REFUSES to run when that is older than the module's target:
#   can't load config: the Go language version (go1.25) used to build
#   golangci-lint is lower than the targeted Go version (1.26.8)
# Moving go.mod's toolchain therefore breaks every previously-installed local
# tool at once, and does it five minutes in, in the Lint section, with a
# message that reads like a config error rather than a stale binary. CI and
# the verify container never see it because both build these tools from source
# against the same go.mod; only a developer machine carries the old ones.
# Cheap here, so it is checked here — `make deps` reinstalls all three.
verify_go_target="$(awk '/^toolchain[ \t]+go/ { sub(/^go/, "", $2); print $2; exit }' go.mod 2>/dev/null || true)"
[ -n "$verify_go_target" ] || verify_go_target="$(awk '/^go[ \t]+[0-9]/ { print $2; exit }' go.mod 2>/dev/null || true)"
if [ -n "$verify_go_target" ] && command -v go >/dev/null 2>&1; then
    for tool in gofumpt goimports golangci-lint; do
        [ -x "${verify_go_bin}/${tool}" ] || continue
        verify_tool_go="$(go version "${verify_go_bin}/${tool}" 2>/dev/null | awk '{ print $2 }' || true)"
        verify_tool_go="${verify_tool_go#go}"
        [ -n "$verify_tool_go" ] || continue
        # awk NR==1 rather than `head -1`: head exits after the first line and
        # SIGPIPEs sort, which under `set -o pipefail` fails the whole run.
        verify_older="$(printf '%s\n%s\n' "$verify_go_target" "$verify_tool_go" | sort -V | awk 'NR==1')"
        if [ "$verify_older" = "$verify_tool_go" ] && [ "$verify_tool_go" != "$verify_go_target" ]; then
            gap "${tool} was built with go${verify_tool_go} but go.mod targets ${verify_go_target} — run 'make deps' (golangci-lint refuses to run on this skew)"
        fi
    done
fi

# node_modules, per app, under exactly the conditions that make each app's
# section run. The test is the executable each gate invokes, because a
# half-finished install leaves the directory present and the binary absent.
verify_have_pnpm_explorer=0
if command -v pnpm >/dev/null 2>&1 && [ -f web/explorer/pnpm-lock.yaml ]; then verify_have_pnpm_explorer=1; fi
if command -v node >/dev/null 2>&1 && command -v npx >/dev/null 2>&1; then
    [ -x web/explorer/node_modules/.bin/openapi-typescript ] || \
        gap "web/explorer/node_modules/.bin/openapi-typescript is absent — 'make web-generate-api' in the generated-artifact drift check needs it"
elif [ "$verify_have_pnpm_explorer" = 1 ]; then
    [ -x web/explorer/node_modules/.bin/openapi-typescript ] || \
        gap "web/explorer/node_modules is absent — the showcase typecheck/lint/test/build sections need it"
fi
if [ "$verify_have_pnpm_explorer" = 1 ]; then
    [ -x web/explorer/node_modules/.bin/tsc ] || \
        gap "web/explorer/node_modules/.bin/tsc is absent — 'make web-typecheck' needs it"
fi

# A PRESENT node_modules is not a CURRENT one. The checks above ask whether an
# install happened, never whether it matches the lockfile in the tree — so a
# pull that moves pnpm-lock.yaml leaves the gate running yesterday's packages
# and saying nothing. On 2026-09-08 `package.json` declared vitest `^5.0.0`
# (merged in #500) while `node_modules` still held **4.1.10**, so every local
# `make web-test` had been grading the tree under a different test runner than
# CI installs — the same shape as the Go toolchain skew checked above, and
# invisible for the same reason: the tool was present, just wrong.
#
# mtime comparison rather than a version probe: it is one stat per app, needs
# no package names, and catches ANY drift rather than the one dependency
# someone thought to check. `pnpm install` rewrites node_modules/.modules.yaml,
# so that file being older than the lockfile is exactly "the lockfile moved and
# nothing reinstalled".
for verify_app in explorer status dashboard; do
    verify_lock="web/${verify_app}/pnpm-lock.yaml"
    verify_stamp="web/${verify_app}/node_modules/.modules.yaml"
    [ -f "$verify_lock" ] || continue
    [ -f "$verify_stamp" ] || continue
    if [ "$verify_lock" -nt "$verify_stamp" ]; then
        gap "web/${verify_app}/node_modules is STALE — pnpm-lock.yaml is newer than the last install; run 'pnpm --dir web/${verify_app} install'"
    fi
done
if command -v pnpm >/dev/null 2>&1 && [ -f web/status/pnpm-lock.yaml ]; then
    [ -x web/status/node_modules/.bin/tsc ] || \
        gap "web/status/node_modules/.bin/tsc is absent — 'make status-typecheck' needs it"
fi

# The deferrable set, resolved with the same conditions their sections use.
command -v promtool >/dev/null 2>&1 || willdefer "Monitoring (promtool is not installed)"
command -v amtool >/dev/null 2>&1 || willdefer "Alertmanager config (amtool is not installed)"
command -v govulncheck >/dev/null 2>&1 || willdefer "Vuln (govulncheck is not installed)"
command -v gitleaks >/dev/null 2>&1 || willdefer "Secrets (gitleaks is not installed)"
if ! { command -v node >/dev/null 2>&1 && command -v npx >/dev/null 2>&1; }; then
    willdefer "Generated-artifact drift (node or npx is not installed)"
fi
if ! { command -v pnpm >/dev/null 2>&1 && [ -f web/explorer/pnpm-lock.yaml ]; }; then
    willdefer "Showcase (pnpm or web/explorer/pnpm-lock.yaml is unavailable)"
fi
if ! { command -v ansible-playbook >/dev/null 2>&1 && { [ -n "${DEPLOY_SYNC_CONNECTION:-}" ] || grep -q 'GNU tar' <<<"$(tar --version 2>/dev/null)"; }; }; then
    willdefer "Deploy migrations-sync self-test (needs ansible-playbook and GNU tar; use VERIFY_PROFILE=container on macOS)"
fi

if [ "${#preflight_defers[@]}" -gt 0 ]; then
    printf 'verify: %d check(s) will be deferred:\n' "${#preflight_defers[@]}"
    for d in "${preflight_defers[@]}"; do printf '  - %s\n' "$d"; done
fi
if [ "${#preflight_gaps[@]}" -gt 0 ]; then
    printf 'verify: FAIL — %d precondition(s) missing. Every one of them is listed here rather than one per run:\n' "${#preflight_gaps[@]}" >&2
    for g in "${preflight_gaps[@]}"; do printf '  - %s\n' "$g" >&2; done
    echo "" >&2
    echo "Fix them all in one command:  make bootstrap-worktree" >&2
    exit 1
fi
echo "verify: preconditions met"

deferred_checks=0
defer_check() {
    local label="$1"
    local reason="$2"
    if [ "${VERIFY_FAIL_ON_SKIP:-0}" = "1" ]; then
        echo "=== ${label} (FAILED: ${reason}) ===" >&2
        exit 1
    fi
    echo "=== ${label} (deferred: ${reason}) ==="
    deferred_checks=$((deferred_checks + 1))
}

# verify.sh ↔ CI parity (W5-ci-6). The #1 cause of "green locally, red in CI"
# is this gate drifting behind CI's import-checks job. This deterministic
# meta-check (no network) fails fast if CI runs a scripts/ci gate that the
# steps below do not mirror — so a lint added to CI obligates adding it here.
# The self-test runs first (the gate is only as trustworthy as its fixtures).
echo "=== verify↔CI parity self-test ===" && ./scripts/ci/check-verify-parity-test.sh
echo "=== verify.sh ↔ CI import-checks parity ===" && ./scripts/ci/check-verify-parity.sh
# The verifier image installs the Postman converter from a Dockerfile ARG whose
# default must equal the pin in scripts/dev/docs-postman.sh; the script refuses
# any other version and falls back to npx, so drift makes the image copy dead.
echo "=== Verifier image pins self-test ===" && ./scripts/ci/check-verify-image-pins-test.sh
echo "=== Verifier image pins ===" && ./scripts/ci/check-verify-image-pins.sh

# Best-effort awareness of main's CI health (W5-ci-6). ADVISORY ONLY: it needs
# network + an authenticated gh, so it is fully wrapped and can NEVER affect
# verify's exit code — it just warns when you're about to push onto a red main.
if command -v gh >/dev/null 2>&1; then
    echo "=== Main CI health (best-effort; never fails verify) ==="
    main_ci_conclusion="$(gh run list --workflow ci.yml --branch main --status completed --limit 1 \
        --json conclusion --jq '.[0].conclusion' 2>/dev/null || true)"
    case "${main_ci_conclusion:-}" in
        success)
            echo "main CI: latest completed run is green." ;;
        failure|timed_out|startup_failure)
            echo "⚠️  main CI: latest completed run concluded '${main_ci_conclusion}'." \
                 "You may be pushing on top of a RED main — check 'gh run list --workflow ci.yml --branch main'." ;;
        *)
            echo "main CI: no health signal (gh offline/unauthed, or no completed runs) — skipping." ;;
    esac
else
    echo "=== Main CI health (skipped — gh not installed; 'brew install gh' for the push-onto-red-main warning) ==="
fi

# ── Tier 0: every check measured under five seconds, before the Go build ────
#
# Cost order, not topic order. Each section below runs in under five seconds
# standalone (measured 2026-09-07 on a warm cache; the table is in
# docs/contributing/local-verification.md), and the sections after this block
# run from five seconds to several minutes. Nothing here changes WHAT any
# check asserts, WHICH checks run, or the ALL CHECKS PASSED / VERIFY
# INCOMPLETE verdict — only when each one is reached.
#
# The measurement that forced it: lint-shell-sigpipe.sh takes 2.5 s and used
# to sit at section ~95 of 103, so a one-line shell edit that tripped it was
# told so after about ten minutes of Go build, doc lints and web builds. It
# now answers in the first fifteen seconds of a run, and `make lint-changed`
# answers before the commit exists.
#
# Relative order inside this block is unchanged from before the move, so a
# section's neighbours — a gate and its self-test especially — still read
# together, and this reordering is a pure move rather than a rewrite.
#
# Two ordering facts worth stating, since both look like accidents:
#   * These lints now run BEFORE `make fmt` rewrites anything. They judge the
#     tree as committed, which is what CI judges; none of their assertions is
#     about formatting.
#   * The three `go run` lints in scripts/ci (lint-golangci-config,
#     lint-openapi-urls, lint-pk-discriminators) are here because each is a
#     standalone program (stdlib plus yaml/jsonschema, no repo imports), so
#     no amount of breakage in internal/ can make them fail. On a cold Go
#     build cache their first compile costs more than the sub-second figure.
# The container verifier runs native arm64 on Apple-silicon Docker, and the
# syscall table there differs from amd64: dup2 has no arm64 entry, so a
# call that vets clean on amd64 CI and on the production host does not
# compile in that lane. Cross-vet the one package that touches raw
# syscalls, so the break surfaces here rather than in a Docker run.
echo "=== Vet (linux/arm64 cross) ===" && GOOS=linux GOARCH=arm64 go vet ./internal/pipeline/
echo "=== golangci config schema ===" && go run ./scripts/ci/lint-golangci-config
echo "=== Agents file ===" && ./scripts/ci/lint-agents-file.sh
echo "=== Agents file self-test ===" && ./scripts/ci/lint-agents-file-test.sh
echo "=== Actions pinning ===" && ./scripts/ci/lint-actions-pinning.sh
echo "=== Actions pinning self-test ===" && ./scripts/ci/lint-actions-pinning-test.sh
# ci.yml's path-filter change-class computation, mirrored so a local run
# judges the same classification CI gates on.
# check-change-class.sh is a HELPER that CI invokes with a class and a file
# list, not a standalone gate — running it bare is a usage error. Its
# self-test is the meaningful local mirror, as for every other -test.sh here.
echo "=== Change-class computation self-test ===" && ./scripts/ci/check-change-class-test.sh
# Refuses a Dependabot PR that rides a go/toolchain or engines/packageManager
# bump in under a dependency-patch label — that decision is deliberate, never
# automatic (see #495).
# Another HELPER: CI passes it the PR author login. Its self-test is the mirror.
echo "=== Dependabot toolchain-bump guard self-test ===" && ./scripts/ci/check-dependabot-toolchain-bump-test.sh
# govulncheck must be built with the Go version go.mod declares, or a
# toolchain bump makes the vulnerability gate fail to parse the module graph
# instead of reporting on it.
echo "=== Go toolchain parity (govulncheck) ===" && ./scripts/ci/lint-go-toolchain-parity.sh
echo "=== Dead scheduled-control detector self-test ===" && ./scripts/ci/check-scheduled-controls-test.sh
echo "=== Archive tier-D self-test ===" && ./scripts/ci/verify-archive-tier-d-test.sh
echo "=== Coverage floor self-test ===" && ./scripts/ci/coverage-floor-test.sh
echo "=== Imports ==="       && ./scripts/ci/lint-imports.sh
echo "=== Imports self-test ===" && ./scripts/ci/lint-imports-test.sh
echo "=== Protocol registry sync ===" && ./scripts/ci/lint-protocol-registry-sync.sh
echo "=== Lexicon ==="       && ./scripts/ci/lint-lexicon.sh
echo "=== i128/NUMERIC ===" && ./scripts/ci/lint-i128.sh
echo "=== Migrations money ===" && ./scripts/ci/lint-migrations.sh
echo "=== Migration backward-compat self-test ===" && ./scripts/ci/lint-migration-compat-test.sh
echo "=== Migration immutability ===" && ./scripts/ci/lint-migration-immutability.sh
echo "=== Migration immutability self-test ===" && ./scripts/ci/lint-migration-immutability-test.sh
echo "=== Migration header commands ===" && ./scripts/ci/lint-migration-commands.sh
echo "=== Migration header commands self-test ===" && ./scripts/ci/lint-migration-commands-test.sh
echo "=== Completeness-staleness calibration ===" && ./scripts/ci/lint-completeness-staleness.sh
echo "=== Pre-push integration-routing self-test ===" && ./scripts/ci/prepush-integration-required-test.sh
echo "=== Integration-shard partition self-test ===" && ./scripts/ci/integration-shard-test.sh
echo "=== Shell SIGPIPE (pipe-into-head) ===" && ./scripts/ci/lint-shell-sigpipe.sh
echo "=== HTTP timeouts ===" && ./scripts/ci/lint-http-timeouts.sh
echo "=== HTTP timeouts self-test ===" && ./scripts/ci/lint-http-timeouts-test.sh
echo "=== Deploy-baseline self-test ===" && ./scripts/ci/deploy-baseline-test.sh
echo "=== Deploy-protection self-test ===" && ./scripts/ci/check-deploy-protection-test.sh
echo "=== Main-CI-health decision-core self-test ===" && ./scripts/ci/check-main-ci-health-test.sh
echo "=== SLA-evidence decision-core + k6-weekly wiring self-test ===" && ./scripts/ci/check-sla-evidence-test.sh
echo "=== deploy/systemd authority ===" && bash ./scripts/ci/lint-deploy-systemd-authority.sh
echo "=== Ansible task lint (pipefail/bash, secret-on-argv) ===" && ./scripts/ci/lint-ansible-tasks.sh
echo "=== Ansible task lint self-test ===" && ./scripts/ci/lint-ansible-tasks-test.sh
echo "=== Ansible-drift decision-core self-test ===" && ./scripts/ci/check-ansible-drift-test.sh
echo "=== run-heavy-job wrapper self-test ===" && ./scripts/ci/run-heavy-job-test.sh
echo "=== Deploy playbook jump/backup-gate lint ===" && ./scripts/ci/lint-deploy-playbook.sh
echo "=== EnvironmentFile verbatim-reader self-test ===" && ./scripts/ci/envfile-loader-test.sh
echo "=== Deploy workflow input-validation self-test ===" && ./scripts/ci/deploy-inputs-test.sh
echo "=== Jinja template parse gate ===" && ./scripts/ci/lint-jinja-templates.sh
echo "=== Jinja template parse gate self-test ===" && ./scripts/ci/lint-jinja-templates-test.sh
echo "=== ClickHouse Prometheus endpoint self-test ===" && ./scripts/ci/clickhouse-exporter-test.sh
echo "=== Alertmanager apply-path parity ===" && ./scripts/ci/check-alertmanager-parity.sh
echo "=== Alertmanager apply-path parity self-test ===" && ./scripts/ci/check-alertmanager-parity-test.sh
echo "=== pgBackRest backup wrapper self-test ===" && ./scripts/ci/pgbackrest-backup-test.sh
echo "=== API-smoke textfile self-test ===" && ./scripts/ci/smoke-textfile-test.sh
echo "=== Served-value harness scheduling + cadence ===" && ./scripts/ci/lint-served-value-cadence.sh
echo "=== Served-value harness scheduling self-test ===" && ./scripts/ci/lint-served-value-cadence-test.sh
echo "=== data-freshness watchdog self-test ===" && ./scripts/ci/data-freshness-test.sh
echo "=== TimescaleDB job/CAGG probe self-test ===" && ./scripts/ci/timescale-jobs-probe-test.sh
echo "=== galexie catchup probe self-test ===" && ./scripts/ci/galexie-catchup-probe-test.sh
echo "=== galexie archive contiguity self-test ===" && ./scripts/ci/galexie-archive-contiguity-test.sh
# BASE_SHA-gated. Both this and lint-replay-plan.sh below take their
# comparison base from the environment, and with none set they print a skip
# line and exit 0 — so invoking them bare made verify↔CI parity look honest
# while checking nothing. That gap is not theoretical: 38fb58019 shipped an
# undeclared .gitleaksignore entry through a fully green local run and turned
# main red, and the gate's own anti-heal walk then held it red.
#
# Locally the analogue of CI's push base is the merge-base with the upstream
# tip: it covers exactly the commits about to be pushed. With nothing
# unpushed the range is empty and both gates pass trivially, which is correct
# — there is no growth to declare.
if [ -z "${BASE_SHA:-}" ] && git rev-parse -q --verify origin/main >/dev/null 2>&1; then
    BASE_SHA="$(git merge-base origin/main HEAD 2>/dev/null || true)"
    [ -n "$BASE_SHA" ] && export BASE_SHA && echo "verify: BASE_SHA=${BASE_SHA} (merge-base with origin/main)"
fi
echo "=== Baseline-growth tripwire ===" && ./scripts/ci/lint-baseline-growth.sh
echo "=== Restore-drill contract + abort-path tests ===" && bash scripts/ops/restore-drill-test.sh && bash scripts/ops/restore-drill-run-test.sh
# BASE_SHA-gated like lint-baseline-growth.sh: self-skips locally, real in CI.
echo "=== Replay-plan tripwire ===" && ./scripts/ci/lint-replay-plan.sh
echo "=== External channels ===" && ./scripts/ci/lint-external-channels.sh
echo "=== External channels self-test ===" && ./scripts/ci/lint-external-channels-test.sh
echo "=== OpenAPI URLs (query discipline + served hosts) ===" && go run ./scripts/ci/lint-openapi-urls openapi/stellar-index.v1.yaml
echo "=== PK discriminators ===" && go run ./scripts/ci/lint-pk-discriminators
# Structural rule-file lint — pure-Python (no promtool), so it runs even
# on machines without a Prometheus install and catches the mis-indented-rule
# class that otherwise only CI's promtool job flags (2026-07-06 galexie-archive
# incident: alerts at group level → "field expr not found in type RuleGroup").
echo "=== Rule structure ===" && python3 ./scripts/ci/lint-rule-structure.py
# YAML-aware runbook guard (audit C4-1): runbook_url must be an annotation,
# not a label, on every alert — else the Alertmanager Discord templates
# render no runbook link. Fails if a runbook_url regresses back into labels.
echo "=== Runbook annotations ===" && python3 ./scripts/ci/lint-runbook-annotations.py
# Secret-rendering ansible template tasks must set `diff: false` (or
# no_log) so `--check --diff` never prints vault material into scrollback
# or the weekly drift job's CI log (audit-2026-08-28 backup-restore-7).
echo "=== Ansible secret-diff ===" && python3 ./scripts/ci/lint-ansible-secret-diff.py
# The metric-refs SELF-test (does the guard still detect a dead ref?)
# needs neither promtool nor the monitoring stack, so it runs
# unconditionally — outside the promtool branch above, which would
# otherwise skip it on any machine that HAS promtool. CI's import-checks
# job runs it, so verify.sh must too or check-verify-parity fails.
echo "=== Metric refs self-test ===" && ./scripts/ci/lint-metric-refs-test.sh
echo "=== Unit-failed baseline self-test ===" && ./scripts/ci/lint-unit-failed-baseline-test.sh
echo "=== ClickHouse ops-user contract self-test ===" && ./scripts/ops/ch-ops-user-test.sh
# scripts/ci runs the gate scripts this file mirrors; scripts/dev/lint-changed.sh
# is a dispatcher OVER those same gates and had no caller of its own — a
# 99-assertion self-test that no gate ever ran. check-verify-parity.sh only
# enforces the CI-\>verify direction for scripts/ci, so it could not have
# caught this. Run it here explicitly.
echo "=== Changed-file dispatcher self-test ===" && ./scripts/dev/lint-changed-test.sh
# govulncheck (F-0057). Graceful-skip when not installed locally —
# CI installs it via `make deps`. Mirrors the promtool pattern.
if command -v govulncheck >/dev/null 2>&1; then
    echo "=== Vuln ==="        && make vuln
else
    defer_check "Vuln" "govulncheck is not installed"
fi

# ── Generated-artifact drift: MUST run before any parallel lane starts ─────
#
# CI enforces three of these — docs/reference/api and examples/postman in
# the `openapi` job, web/explorer/src/api/types.ts in the `web/explorer`
# job — each by regenerating and diffing. verify.sh ran none of them, so an
# OpenAPI change that regenerated two of the three passed local gate and
# reddened CI on the third (2026-07-25).
#
# Regenerating here is deliberate: unlike CI, a local run should FIX the
# drift rather than just report it, so the operator commits the result. The
# diff is still checked so the run is loud about having changed files.
#
# This step is pulled out of the parallel block below and run BEFORE any
# lane starts, on purpose: it regenerates web/explorer/src/api/types.ts,
# which lane c's typecheck/build reads, and a concurrent regenerate-while-
# read is exactly the race the ordering rule under "Parallel phase" exists
# to forbid. Running it here also means every lane sees the freshly
# regenerated tree, which serial order already guaranteed for `Test` and
# `Showcase` and now guarantees for lane a and lane d's readers too.
if command -v node >/dev/null 2>&1 && command -v npx >/dev/null 2>&1; then
    echo "=== Generated API reference + Postman + web client drift ==="
    ./scripts/dev/docs-api.sh >/dev/null
    ./scripts/dev/docs-postman.sh >/dev/null
    make web-generate-api >/dev/null
    if ! git diff --exit-code --stat -- docs/reference/api examples/postman web/explorer/src/api/types.ts; then
        echo "⚠️  Generated artifacts were STALE and have been regenerated above."
        echo "    Commit these files — CI fails on exactly this diff."
        exit 1
    fi
else
    defer_check "Generated-artifact drift" "node or npx is not installed"
fi

# ── Parallel phase (TIER 1b) ─────────────────────────────────────────────────
#
# Same sections, same assertions as before; only WHEN and IN WHAT PROCESS
# each runs has changed. Four lanes, grouped so each shares no file with any
# other — the property that makes concurrent execution safe rather than
# merely fast:
#
#   a  doc lints            reads markdown + this file's own targets; writes nothing
#   b  Go build/vet/tests   the ONLY lane that writes a tracked file (`make fmt`)
#   c  web typecheck/build  pnpm/node only; never touches *.go
#   d  everything else      ops/ansible/migration/monitoring self-tests; none of it
#                           invokes go, invokes pnpm, or writes a tracked file
#
# Measured 2026-09-07 on this machine, one real run, after the doc-links
# self-test fix above cut its cost from 227 s to 66 s: serial total 918 s,
# with Lint 154 s, Monitoring 146 s, Test 128 s and Doc links self-test 66 s
# as the four largest remaining costs — no longer one section dominating,
# four roughly comparable ones (Monitoring's figure includes contention from
# another process on the same machine; a quiet run measured this section at
# 90 s). Grouped into the lanes above, the ceiling is lane b (Format + Vet +
# Lint + Test + Integration build) at ~304 s against the 918 s serial total:
# a ~2.3x reduction on a contended run, more on a quiet one.
#
# Lane d keeps promtool's Monitoring, amtool's Alertmanager config and
# gitleaks' Secrets scans — none of those tools, nor anything else in lane
# d, touches *.go or web/**, so lane d is safe to run alongside b and c
# rather than needing a lane of its own.
#
# A lane runs in a background subshell, so it cannot write back to this
# script's own `deferred_checks` variable or exit the parent on failure —
# each lane keeps its own deferred-count file and its own exit code, and
# both are collected after every lane finishes.
LANEDIR="$(mktemp -d "${TMPDIR:-/tmp}/verify-lanes.XXXXXX")"
trap 'rm -rf "$LANEDIR"' EXIT

defer_check_lane() { # defer_check_lane <deferred-file> <label> <reason> —
                      # same contract as defer_check above, scoped to one
                      # lane's counter file instead of the shared variable.
    local file="$1" label="$2" reason="$3"
    if [ "${VERIFY_FAIL_ON_SKIP:-0}" = "1" ]; then
        echo "=== ${label} (FAILED: ${reason}) ===" >&2
        exit 1
    fi
    echo "=== ${label} (deferred: ${reason}) ==="
    echo "$(( $(cat "$file" 2>/dev/null || echo 0) + 1 ))" > "$file"
}

lane_a() { # doc lints
    echo "=== Docs ==="          && ./scripts/ci/lint-docs.sh
    echo "=== Doc links ===" && ./scripts/ci/lint-doc-links.sh
    echo "=== Doc links self-test ===" && ./scripts/ci/lint-doc-links-test.sh
}

lane_b() { # Go build/vet/unit tests
    echo "=== Format ==="        && make fmt
    echo "=== Vet ==="           && make vet
    echo "=== Lint ==="          && make lint
    echo "=== Test ==="          && make test
    # Compile-only: catches interface-extension breakage in
    # build-tagged integration adapters without spinning testcontainers.
    # Real `make test-integration` lives outside verify because Docker
    # isn't always available locally.
    echo "=== Integration build ===" && make test-integration-build
}

lane_c() { # web typecheck/lint/test/build. Graceful-skip when pnpm isn't
           # installed locally — CI runs the same gate via the `web/explorer`
           # job, so a local skip just defers the check. The build catches
           # Next.js output: 'export' constraints (e.g. dynamic =
           # 'force-static' on sitemap/robots) that typecheck alone misses.
    if command -v pnpm >/dev/null 2>&1 && [ -f web/explorer/pnpm-lock.yaml ]; then
        echo "=== Showcase typecheck ===" && make web-typecheck
        echo "=== Showcase lint ==="      && make web-lint
        # The vitest suite ran in NEITHER CI nor this gate (cold audit
        # 2026-08-04): 209 tests across 49 files, all green and all dead —
        # including safe-domain.test.ts, the isSafeHomeDomain /
        # isSafePublicImageUrl phishing + client-SSRF regression gate, and
        # the AGT-06 stale-flag regression. A refactor loosening any of
        # them would have landed green.
        # BUILD BEFORE TEST, deliberately. nav-shell.built.test.ts asserts on
        # the static export in web/explorer/out, so with the build after it the
        # suite graded whatever `out/` a previous run happened to leave behind.
        # On 2026-09-08 that was a five-day-old export from before /rwa existed,
        # and the test duly reported `/rwa: no rwa/index.html in the export` plus
        # three pages with an empty <main> — a stale artifact, not a regression in
        # the source. A test whose verdict depends on leftover build output is
        # worse than no test: it reads green while the tree it claims to grade is
        # unbuilt, and red for a reason that has nothing to do with the diff.
        echo "=== Showcase build ==="     && \
            NEXT_PUBLIC_API_BASE_URL=http://api.local-stub.invalid make web-build >/dev/null
        echo "=== Showcase tests ==="     && make web-test
    else
        defer_check_lane "$LANEDIR/c.deferred" "Showcase" "pnpm or web/explorer/pnpm-lock.yaml is unavailable"
    fi
    # Dashboard SPA — same pnpm gate. Skipped silently when the
    # lockfile is missing (e.g. fresh checkouts that haven't installed).
    if command -v pnpm >/dev/null 2>&1 && [ -f web/dashboard/pnpm-lock.yaml ]; then
        echo "=== Dashboard typecheck ===" && make dashboard-typecheck
        echo "=== Dashboard lint ==="      && make dashboard-lint
        echo "=== Dashboard build ==="     && \
            NEXT_PUBLIC_API_BASE_URL=http://api.local-stub.invalid make dashboard-build >/dev/null
    fi
    # Status page — same pnpm gate.
    if command -v pnpm >/dev/null 2>&1 && [ -f web/status/pnpm-lock.yaml ]; then
        echo "=== Status typecheck ===" && make status-typecheck
        echo "=== Status lint ==="      && make status-lint
        echo "=== Status build ==="     && \
            NEXT_PUBLIC_API_BASE_URL=http://api.local-stub.invalid make status-build >/dev/null
    fi
}

lane_d() { # everything else
    echo "=== Ansible galexie-restart self-test ===" && ./scripts/ci/ansible-galexie-restart-test.sh
    # CI's import-checks job runs these gate scripts too; verify.sh must mirror
    # them or it issues a green CI won't honour (W5-ci-6, enforced by the parity
    # check above). All are deterministic + network-free.
    echo "=== Migration backward-compat ===" && ./scripts/ci/lint-migration-compat.sh
    echo "=== Lake dedup (aggregating reads of duplicate-bearing archives) ===" && ./scripts/ci/lint-lake-dedup.sh
    echo "=== Lake dedup self-test ===" && ./scripts/ci/lint-lake-dedup-test.sh
    echo "=== Shell SIGPIPE self-test ===" && ./scripts/ci/lint-shell-sigpipe-test.sh
    echo "=== Public-dataset drift-verdict self-test ===" && ./scripts/ci/check-public-dataset-test.sh
    echo "=== Fleet release-drift verdict self-test ===" && ./scripts/ci/check-fleet-release-drift-test.sh
    echo "=== zfs-snapshot job self-test ===" && ./scripts/ci/zfs-snapshot-test.sh
    # Migrations-sync self-test: structural half needs only python; the
    # behavioural half runs the task file with ansible and needs GNU tar on the
    # target (unarchive --diff). macOS ships bsdtar — point it at a container
    # via DEPLOY_SYNC_CONNECTION/DEPLOY_SYNC_HOST (see the script header) or
    # let CI's ansible-check job (ubuntu) run it. Graceful-skip only when the
    # tools are missing, same convention as promtool below.
    if command -v ansible-playbook >/dev/null 2>&1 && { [ -n "${DEPLOY_SYNC_CONNECTION:-}" ] || grep -q 'GNU tar' <<<"$(tar --version 2>/dev/null)"; }; then
        echo "=== Deploy migrations-sync self-test ===" && ./scripts/ci/deploy-sync-test.sh
    else
        defer_check_lane "$LANEDIR/d.deferred" "Deploy migrations-sync self-test" "needs ansible-playbook and GNU tar; use VERIFY_PROFILE=container on macOS"
    fi
    echo "=== Baseline-growth tripwire self-test ===" && ./scripts/ci/lint-baseline-growth-test.sh
    echo "=== Config-apply gate self-test ===" && ./scripts/ci/config-apply-gate-test.sh
    # Two import-checks gates that landed (#287, #305) without their verify.sh
    # twin — check-verify-parity was red on main for everyone until added here.
    echo "=== Public-dataset drift decision-core self-test ===" && ./scripts/ci/check-public-dataset-test.sh
    echo "=== Replay-plan tripwire self-test ===" && ./scripts/ci/lint-replay-plan-test.sh
    echo "=== Verdict helpers self-test (oneshot waits, sentinel gates) ===" && bash scripts/ops/ops-verdict-test.sh
    # Prometheus rule files. Graceful-skip when promtool isn't
    # installed locally — CI installs it explicitly. The Makefile
    # target hard-fails on missing promtool; verify.sh wraps it with
    # an existence check so local-dev `bash scripts/dev/verify.sh`
    # keeps working without a full Prometheus install.
    if command -v promtool >/dev/null 2>&1; then
        echo "=== Monitoring ===" && make monitoring-check
    else
        defer_check_lane "$LANEDIR/d.deferred" "Monitoring" "promtool is not installed"
        # The dead-metric-ref guard needs no promtool, so run it even when
        # the promtool-dependent monitoring-check is skipped (F-1329).
        echo "=== Metric refs ===" && ./scripts/ci/lint-metric-refs.sh
    fi
    # Alertmanager config — validate BOTH render branches of apply.sh
    # (all URLs empty → stub path through the block-stripper; all URLs
    # set → substitution path). Graceful-skip when amtool isn't
    # installed, same convention as promtool above; CI's
    # monitoring-rules job installs it explicitly and never skips.
    if command -v amtool >/dev/null 2>&1; then
        echo "=== Alertmanager config ==="
        ALERTMANAGER_SECRETS=/dev/null bash configs/alertmanager/apply.sh --check-only
        AM_DUMMY_ENV=$(mktemp)
        printf 'HEALTHCHECKS_DEADMANSSWITCH_URL=https://hc-ping.com/x\nDISCORD_WEBHOOK_URL_PAGES=https://discord.com/api/webhooks/1/a\nDISCORD_WEBHOOK_URL_ALERTS=https://discord.com/api/webhooks/2/b\n' > "$AM_DUMMY_ENV"
        ALERTMANAGER_SECRETS="$AM_DUMMY_ENV" bash configs/alertmanager/apply.sh --check-only
        rm -f "$AM_DUMMY_ENV"
        # And the fail-closed guard's SELF-test: an empty URL must be
        # refused at apply time. Renders green either way without this —
        # the 31-day outage installed a receiver-less config through a
        # fully-passing gate.
        bash configs/alertmanager/apply-test.sh
    else
        defer_check_lane "$LANEDIR/d.deferred" "Alertmanager config" "amtool is not installed"
    fi
    # gitleaks (secret scan). CI runs this as its own job; verify.sh didn't,
    # so a new base64/XDR test fixture that trips the generic-api-key entropy
    # heuristic passed local gate but reddened CI (2026-07-06). Graceful-skip
    # when absent (mirrors promtool/govulncheck).
    #
    # TWO scans, because neither subsumes the other and running only the first
    # is what let a leak through on 2026-07-25:
    #
    #   --no-git  scans the WORKING TREE, including uncommitted edits. This is
    #             the one that catches a fixture before you commit it, when the
    #             fix is still cheap. It cannot see history.
    #   (default) scans COMMITTED HISTORY, which is exactly what CI runs (with
    #             fetch-depth: 0). It cannot see uncommitted edits.
    #
    # The gap that bit: a test fixture was committed, then removed from the
    # working tree in a follow-up commit. --no-git went green — the string was
    # genuinely gone from the tree — while CI stayed red, because the commit
    # that INTRODUCED it is still in the log and always will be. A local gate
    # that calls itself "run before every push" has to run what CI runs, or it
    # is issuing a pass it has no basis for. History findings that are provably
    # not credentials are exempted by fingerprint in .gitleaksignore.
    if command -v gitleaks >/dev/null 2>&1; then
        echo "=== Secrets (gitleaks, working tree) ===" && \
            gitleaks detect --no-git --no-banner --redact --config .gitleaks.worktree.toml
        echo "=== Secrets (gitleaks, history — CI parity) ===" && \
            gitleaks detect --no-banner --redact --config .gitleaks.toml
    else
        defer_check_lane "$LANEDIR/d.deferred" "Secrets" "gitleaks is not installed"
    fi
}

# Lane CONCURRENCY is bounded by memory, not by core count. Lane b runs
# golangci-lint (multi-GB on this tree) and lane c runs a Next.js build
# (another GB-scale Node process); on a host with tens of GB all four lanes
# coexist happily, but `make prepush` runs this inside Docker, and this
# machine's Docker VM has 7.65 GiB for all of it. Running all four there
# OOM-killed golangci-lint — `make[1]: *** [Makefile:158: lint] Killed`, a
# bare SIGKILL with no diagnostic, which reads like a lint failure and is
# not one. The host run that preceded it passed, so the parallel win is real
# and worth keeping where the memory exists; it must simply not be assumed.
#
# Below the threshold the two heavy lanes are separated rather than the whole
# tier being serialised: a and d are both light (doc lints; shell/python/
# ansible self-tests plus gitleaks, which is CPU-hungry but not memory-hungry)
# and still run together, then b alone, then c alone. VERIFY_LANES=1 forces
# that schedule and VERIFY_LANES=4 forces the parallel one, so a machine that
# disagrees with the heuristic is not stuck with it.
verify_total_mem_mb() {
    # cgroup v2 limit first: inside a container it is the number that binds,
    # and it is smaller than the host's MemTotal that /proc would report.
    if [ -r /sys/fs/cgroup/memory.max ]; then
        local v; v="$(cat /sys/fs/cgroup/memory.max 2>/dev/null || echo max)"
        [ "$v" != "max" ] && [ -n "$v" ] && { echo $((v / 1024 / 1024)); return; }
    fi
    if [ -r /proc/meminfo ]; then
        awk '/^MemTotal:/ { print int($2 / 1024); exit }' /proc/meminfo && return
    fi
    if command -v sysctl >/dev/null 2>&1; then
        local b; b="$(sysctl -n hw.memsize 2>/dev/null || echo 0)"
        [ "$b" -gt 0 ] 2>/dev/null && { echo $((b / 1024 / 1024)); return; }
    fi
    echo 0   # unknown -> treated as constrained below, which is the safe side
}

VERIFY_LANE_MEM_FLOOR_MB="${VERIFY_LANE_MEM_FLOOR_MB:-12288}"
if [ -n "${VERIFY_LANES:-}" ]; then
    lane_mode="$VERIFY_LANES"
else
    _mem_mb="$(verify_total_mem_mb)"
    if [ "$_mem_mb" -ge "$VERIFY_LANE_MEM_FLOOR_MB" ] 2>/dev/null; then
        lane_mode=4
    else
        lane_mode=1
        echo "verify: ${_mem_mb} MiB available (floor ${VERIFY_LANE_MEM_FLOOR_MB} MiB) — separating the two memory-heavy lanes; set VERIFY_LANES=4 to override"
    fi
fi

lane_rc_a=0; lane_rc_b=0; lane_rc_c=0; lane_rc_d=0
if [ "$lane_mode" = "4" ]; then
    lane_a > "$LANEDIR/a.log" 2>&1 & pid_a=$!
    lane_b > "$LANEDIR/b.log" 2>&1 & pid_b=$!
    lane_c > "$LANEDIR/c.log" 2>&1 & pid_c=$!
    lane_d > "$LANEDIR/d.log" 2>&1 & pid_d=$!
    wait "$pid_a" || lane_rc_a=$?
    wait "$pid_b" || lane_rc_b=$?
    wait "$pid_c" || lane_rc_c=$?
    wait "$pid_d" || lane_rc_d=$?
else
    lane_a > "$LANEDIR/a.log" 2>&1 & pid_a=$!
    lane_d > "$LANEDIR/d.log" 2>&1 & pid_d=$!
    wait "$pid_a" || lane_rc_a=$?
    wait "$pid_d" || lane_rc_d=$?
    lane_b > "$LANEDIR/b.log" 2>&1 || lane_rc_b=$?
    lane_c > "$LANEDIR/c.log" 2>&1 || lane_rc_c=$?
fi

# Every lane's log, in full, lane order — a lane runs concurrently with the
# others but is itself strictly sequential, so within one lane's block the
# output reads exactly as the old serial run did.
echo ""
echo "── lane a: doc lints ──────────────────────────────────────────────"
cat "$LANEDIR/a.log"
echo ""
echo "── lane b: Go build/vet/unit tests ─────────────────────────────────"
cat "$LANEDIR/b.log"
echo ""
echo "── lane c: web typecheck/lint/test/build ───────────────────────────"
cat "$LANEDIR/c.log"
echo ""
echo "── lane d: everything else ─────────────────────────────────────────"
cat "$LANEDIR/d.log"

# A lane failure fails the whole run, and names every lane that failed —
# not just the first one bash happened to notice, since all four already
# ran to their own completion or their own first failure independently.
failed_lanes=()
[ "$lane_rc_a" -eq 0 ] || failed_lanes+=("a:doc-lints(exit ${lane_rc_a})")
[ "$lane_rc_b" -eq 0 ] || failed_lanes+=("b:go-build-vet-test(exit ${lane_rc_b})")
[ "$lane_rc_c" -eq 0 ] || failed_lanes+=("c:web(exit ${lane_rc_c})")
[ "$lane_rc_d" -eq 0 ] || failed_lanes+=("d:everything-else(exit ${lane_rc_d})")
if [ "${#failed_lanes[@]}" -gt 0 ]; then
    echo "" >&2
    echo "VERIFY FAILED — lane(s) failed: ${failed_lanes[*]}" >&2
    exit 1
fi

for f in "$LANEDIR"/*.deferred; do
    [ -e "$f" ] || continue
    deferred_checks=$((deferred_checks + $(cat "$f")))
done

echo ""
if (( deferred_checks > 0 )); then
    echo "VERIFY INCOMPLETE: $deferred_checks check(s) deferred; use make prepush for push clearance"
    exit 1
fi
echo "ALL CHECKS PASSED"
