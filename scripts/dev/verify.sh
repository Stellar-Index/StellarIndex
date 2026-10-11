#!/usr/bin/env bash
# Local sequential quality checks: run before every push. CI runs the same
# jobs in parallel; this surfaces failures one at a time.

set -euo pipefail

cd "$(dirname "$0")/../.."

# ── Preconditions: five seconds, before anything expensive ───────────────
#
# Missing installs (`openapi-typescript: command not found`, `tsc: command not
# found`) would kill a run ten minutes into its twelve, because the section
# guards test pnpm-lock.yaml, which is tracked and always present, never
# node_modules. This block resolves those same conditions up front, reports
# every gap at once and stops before the first slow gate. It asserts nothing
# new: a gap named here is a gate that would fail later.
#
# Deferrable tools are only listed, never failed on, except under
# VERIFY_FAIL_ON_SKIP=1 (set by `make prepush`), where defer_check exits 1.
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

# make fmt and make lint call these by absolute path under $(GOBIN), so PATH is
# irrelevant and `command -v` would report a false miss.
# `|| true`: under set -e a bare substitution with go absent aborts at 127 with
# stderr suppressed, and the gap list naming the missing go never prints.
verify_go_bin="$(go env GOPATH 2>/dev/null || true)/bin"
for tool in gofumpt goimports golangci-lint; do
    [ -x "${verify_go_bin}/${tool}" ] || gap "${verify_go_bin}/${tool} is absent (make fmt / make lint call it by absolute path)"
done

# A Go-built tool embeds its build toolchain, and golangci-lint REFUSES to run
# when that is older than the module's target:
#   can't load config: the Go language version (go1.25) used to build
#   golangci-lint is lower than the targeted Go version (1.26.8)
# A go.mod toolchain bump therefore breaks every old local tool at once, five
# minutes in, with a message that reads like a config error. CI and the verify
# container build the tools from source against go.mod and never see it.
# `make deps` reinstalls all three.
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

# node_modules per app, under the conditions that make each app's section run.
# The test is the executable each gate invokes: a half-finished install leaves
# the directory and not the binary.
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

# A PRESENT node_modules is not a CURRENT one: a pull that moves pnpm-lock.yaml
# otherwise leaves the gate running yesterday's packages (vitest 4.1.10 installed
# while package.json declared ^5.0.0, so local `make web-test` graded the tree
# under a different runner than CI).
#
# mtime, not a version probe: one stat per app, catches any drift. `pnpm install`
# rewrites node_modules/.modules.yaml, so that file older than the lockfile means
# the lockfile moved and nothing reinstalled.
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

# verify.sh <-> CI parity: green locally and red in CI usually means this gate
# drifted behind CI's repo-gates job. This network-free meta-check fails if CI
# runs a scripts/ci gate the steps below do not mirror. The self-test runs first
# (a gate is only as trustworthy as its fixtures).
echo "=== verify↔CI parity self-test ===" && ./scripts/ci/check-verify-parity-test.sh
echo "=== verify.sh ↔ CI repo-gates parity ===" && ./scripts/ci/check-verify-parity.sh
echo "=== self-test runners self-test ===" && ./scripts/ci/check-selftest-runners-test.sh
echo "=== every scripts/ci self-test has a runner ===" && ./scripts/ci/check-selftest-runners.sh
echo "=== CI meta-gate coverage ===" && ./scripts/ci/ci-meta-gate-coverage-test.sh
echo "=== deploy checkout ref ===" && ./scripts/ci/deploy-checkout-ref-test.sh
echo "=== deploy migration gate coverage ===" && ./scripts/ci/deploy-migration-gate-coverage-test.sh
echo "=== migration follow-up gate ===" && ./scripts/ci/migration-followup-gate-test.sh
echo "=== deploy served-path smoke ===" && ./scripts/ci/deploy-served-path-smoke-test.sh
echo "=== Pages deploy branch label ===" && ./scripts/ci/pages-deploy-branch-test.sh
# The verifier image installs the Postman converter from a Dockerfile ARG whose
# default must equal the pin in scripts/dev/docs-postman.sh; the script refuses
# any other version and falls back to npx, so drift makes the image copy dead.
echo "=== Verifier image pins self-test ===" && ./scripts/ci/check-verify-image-pins-test.sh
echo "=== Verifier image pins ===" && ./scripts/ci/check-verify-image-pins.sh

# Best-effort awareness of main's CI health. ADVISORY ONLY: it needs
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
# Cost order, not topic order: each section here runs in under five seconds
# standalone (table in docs/contributing/local-verification.md); the rest take
# five seconds to several minutes. Nothing here changes WHAT any check asserts,
# WHICH run, or the ALL CHECKS PASSED / VERIFY INCOMPLETE verdict, only when each
# is reached. lint-shell-sigpipe.sh (2.5 s) is here so a shell edit that trips it
# is told in the first fifteen seconds, not after ten minutes of Go build.
#
# Two ordering facts that look like accidents:
#   * These lints run BEFORE `make fmt` rewrites anything. They judge the tree as
#     committed, which is what CI judges; none asserts on formatting.
#   * The three `go run` lints in scripts/ci (lint-golangci-config,
#     lint-openapi-urls, lint-pk-discriminators) are standalone programs (stdlib
#     plus yaml/jsonschema, no repo imports), so breakage in internal/ cannot
#     fail them. A cold Go build cache makes their first compile slower.
# The container verifier runs native arm64 on Apple-silicon Docker, and the
# syscall table there differs from amd64: dup2 has no arm64 entry, so a
# call that vets clean on amd64 CI and on the production host does not
# compile in that lane. Cross-vet the one package that touches raw
# syscalls, so the break surfaces here rather than in a Docker run.
echo "=== Vet (linux/arm64 cross) ===" && GOOS=linux GOARCH=arm64 go vet ./internal/pipeline/
echo "=== golangci config schema ===" && go run ./scripts/ci/lint-golangci-config
echo "=== Agents file ===" && ./scripts/ci/lint-agents-file.sh
echo "=== Agents file self-test ===" && ./scripts/ci/lint-agents-file-test.sh
echo "=== Repo budget ===" && ./scripts/ci/lint-repo-budget.sh
echo "=== Repo budget self-test ===" && ./scripts/ci/lint-repo-budget-test.sh
echo "=== Remote /tmp staging ===" && ./scripts/ci/lint-remote-tmp-staging.sh
echo "=== Remote /tmp staging self-test ===" && ./scripts/ci/lint-remote-tmp-staging-test.sh
echo "=== Actions pinning ===" && ./scripts/ci/lint-actions-pinning.sh
echo "=== Actions pinning self-test ===" && ./scripts/ci/lint-actions-pinning-test.sh
echo "=== pnpm version pin ===" && ./scripts/ci/lint-pnpm-version-pin.sh
echo "=== pnpm version pin self-test ===" && ./scripts/ci/lint-pnpm-version-pin-test.sh
# ci.yml's path-filter change-class computation, mirrored so a local run judges
# the same classification CI gates on. check-change-class.sh is a helper CI
# invokes with a class and file list; bare it is a usage error, so its self-test
# is the local mirror.
echo "=== Change-class computation self-test ===" && ./scripts/ci/check-change-class-test.sh
# Refuses a Dependabot PR that rides a go/toolchain or engines/packageManager
# bump under a dependency-patch label: that decision is deliberate, never
# automatic. A helper (CI passes the PR author login); its self-test is the mirror.
echo "=== Dependabot toolchain-bump guard self-test ===" && ./scripts/ci/check-dependabot-toolchain-bump-test.sh
# govulncheck must be built with the Go version go.mod declares, or a
# toolchain bump makes the vulnerability gate fail to parse the module graph
# instead of reporting on it.
echo "=== Go toolchain parity (govulncheck) ===" && ./scripts/ci/lint-go-toolchain-parity.sh
echo "=== Go toolchain parity self-test ===" && ./scripts/ci/lint-go-toolchain-parity-test.sh
# VERSIONS.md's pinned go-stellar-sdk tag must track go.mod's require line.
echo "=== SDK version pin self-test ===" && ./scripts/ci/lint-sdk-version-pin-test.sh
echo "=== SDK version pin ===" && ./scripts/ci/lint-sdk-version-pin.sh
echo "=== Dead scheduled-control detector self-test ===" && ./scripts/ci/check-scheduled-controls-test.sh
echo "=== Archive tier-D self-test ===" && ./scripts/ci/verify-archive-tier-d-test.sh
echo "=== Coverage floor self-test ===" && ./scripts/ci/coverage-floor-test.sh
echo "=== Imports ==="       && ./scripts/ci/lint-imports.sh
echo "=== Imports self-test ===" && ./scripts/ci/lint-imports-test.sh
echo "=== Protocol registry sync ===" && ./scripts/ci/lint-protocol-registry-sync.sh
echo "=== Lexicon ==="       && ./scripts/ci/lint-lexicon.sh
echo "=== Test patterns ===" && ./scripts/ci/lint-test-patterns.sh
echo "=== Test patterns self-test ===" && ./scripts/ci/lint-test-patterns-test.sh
echo "=== Deprecation removal versions ===" && ./scripts/ci/check-deprecations.sh
echo "=== Deprecation removal versions self-test ===" && bash scripts/ci/check-deprecations-test.sh
echo "=== i128/NUMERIC ===" && ./scripts/ci/lint-i128.sh
# gofumpt's doc-comment reformatter rewrites a doubled apostrophe to U+201D,
# so a comment quoting `entry_xdr != ''` stops saying what the filter is. The
# corruption is fmt-STABLE, which is why a clean `gofumpt -l` cannot stand in
# for this scan.
echo "=== Go typographic quotes ===" && python3 ./scripts/ci/lint-go-typographic-quotes.py
echo "=== Go typographic quotes self-test ===" && ./scripts/ci/lint-go-typographic-quotes-test.sh
echo "=== Migrations money ===" && ./scripts/ci/lint-migrations.sh
echo "=== Migrations back-numbering self-test ===" && ./scripts/ci/lint-migrations-backnumber-test.sh
echo "=== Migration backward-compat self-test ===" && ./scripts/ci/lint-migration-compat-test.sh
echo "=== Migration immutability ===" && ./scripts/ci/lint-migration-immutability.sh
echo "=== Migration immutability self-test ===" && ./scripts/ci/lint-migration-immutability-test.sh
echo "=== Migration header commands ===" && ./scripts/ci/lint-migration-commands.sh
echo "=== Migration header commands self-test ===" && ./scripts/ci/lint-migration-commands-test.sh
echo "=== Completeness-staleness calibration ===" && ./scripts/ci/lint-completeness-staleness.sh
echo "=== Source enablement (tracked ⇒ run, or declared) ===" && ./scripts/ci/lint-source-enablement.sh
echo "=== Source enablement self-test ===" && ./scripts/ci/lint-source-enablement-test.sh
echo "=== Pre-push integration-routing self-test ===" && ./scripts/ci/prepush-integration-required-test.sh
echo "=== Integration-shard partition self-test ===" && ./scripts/ci/integration-shard-test.sh
echo "=== Shell SIGPIPE (pipe-into-head) ===" && ./scripts/ci/lint-shell-sigpipe.sh
echo "=== Git fixture isolation ===" && ./scripts/ci/lint-git-fixture-isolation.sh
echo "=== YAML duplicate keys ===" && python3 ./scripts/ci/lint-yaml-duplicate-keys.py
echo "=== Memory-mappings probe self-test ===" && ./scripts/ci/memory-mappings-test.sh
echo "=== HTTP timeouts ===" && ./scripts/ci/lint-http-timeouts.sh
echo "=== HTTP timeouts self-test ===" && ./scripts/ci/lint-http-timeouts-test.sh
echo "=== API-key keyspace walk ===" && ./scripts/ci/lint-apikey-scan.sh
echo "=== API-key keyspace walk self-test ===" && ./scripts/ci/lint-apikey-scan-test.sh
echo "=== Unbounded latest-row reads ===" && python3 ./scripts/ci/lint-unbounded-latest-row.py
echo "=== Unbounded latest-row reads self-test ===" && ./scripts/ci/lint-unbounded-latest-row-test.sh
echo "=== Oneshot unit start bound (no ignored RuntimeMaxSec) ===" && ./scripts/ci/lint-healthcheck-oneshot-timeout.sh
echo "=== Oneshot unit start bound self-test ===" && ./scripts/ci/lint-healthcheck-oneshot-timeout-test.sh
echo "=== Deploy-baseline self-test ===" && ./scripts/ci/deploy-baseline-test.sh
echo "=== Deploy-protection self-test ===" && ./scripts/ci/check-deploy-protection-test.sh
echo "=== Production credentials behind an environment gate ===" && python3 ./scripts/ci/lint-deploy-credentials.py
echo "=== Production credentials lint self-test ===" && ./scripts/ci/lint-deploy-credentials-test.sh
echo "=== Main-CI-health decision-core self-test ===" && ./scripts/ci/check-main-ci-health-test.sh
echo "=== SLA-evidence decision-core + k6-weekly wiring self-test ===" && ./scripts/ci/check-sla-evidence-test.sh
echo "=== SLA proof renderer self-test ===" && ./scripts/ci/render-sla-proof-test.sh
echo "=== SLA proof from probe-aggregate self-test ===" && bash scripts/ops/sla-proof-from-probe-test.sh
echo "=== deploy/systemd authority ===" && bash ./scripts/ci/lint-deploy-systemd-authority.sh
echo "=== systemd flag-hatch word-split ===" && ./scripts/ci/lint-systemd-flag-hatch.sh
echo "=== systemd flag-hatch word-split self-test ===" && ./scripts/ci/lint-systemd-flag-hatch-test.sh
echo "=== systemd unit hardening ===" && ./scripts/ci/lint-systemd-hardening.sh
echo "=== systemd unit hardening self-test ===" && ./scripts/ci/lint-systemd-hardening-test.sh
echo "=== systemd EnvironmentFile= order ===" && ./scripts/ci/lint-systemd-envfile-order.sh
echo "=== systemd EnvironmentFile= order self-test ===" && ./scripts/ci/lint-systemd-envfile-order-test.sh
echo "=== Textfile-collector exposition ===" && ./scripts/ci/lint-textfile-exposition.sh
echo "=== Textfile-collector exposition self-test ===" && ./scripts/ci/lint-textfile-exposition-test.sh
# Registered in scripts/ci/textfile-producers.manifest, but the manifest's third
# column only proved the file existed, not that it ran: run the self-test.
echo "=== ch-schema-snapshot self-test ===" && ./scripts/ops/ch-schema-snapshot-test.sh
echo "=== ch-schema-drift self-test ===" && ./configs/ansible/roles/archival-node/files/ch-schema-drift-test.sh
echo "=== Ansible task lint (pipefail/bash, secret-on-argv) ===" && ./scripts/ci/lint-ansible-tasks.sh
echo "=== Ansible task lint self-test ===" && ./scripts/ci/lint-ansible-tasks-test.sh
echo "=== Ansible toolchain pin self-test ===" && ./scripts/ci/lint-ansible-toolchain-pin-test.sh
echo "=== Ansible toolchain pin (no inline version in a workflow) ===" && ./scripts/ci/lint-ansible-toolchain-pin.sh
echo "=== Ansible-drift decision-core self-test ===" && ./scripts/ci/check-ansible-drift-test.sh
echo "=== Ansible collection pin parity self-test ===" && ./scripts/ci/ansible-collection-pin-parity-test.sh
echo "=== Ansible collection pin parity wiring self-test ===" && ./scripts/ci/verify-ansible-pin-parity-wiring-test.sh
echo "=== run-heavy-job wrapper self-test ===" && ./scripts/ci/run-heavy-job-test.sh
echo "=== Deploy playbook jump/backup-gate lint ===" && ./scripts/ci/lint-deploy-playbook.sh
echo "=== EnvironmentFile verbatim-reader self-test ===" && ./scripts/ci/envfile-loader-test.sh
echo "=== verify-archive EnvironmentFile override-order self-test ===" && ./scripts/ci/verify-archive-env-order-test.sh
echo "=== Deploy workflow input-validation self-test ===" && ./scripts/ci/deploy-inputs-test.sh
echo "=== Migrate lock-timeout self-test ===" && ./scripts/ci/lint-migrate-lock-timeout-test.sh
echo "=== Deploy-probe parity self-test ===" && ./scripts/ci/lint-deploy-probe-parity-test.sh
echo "=== Wrangler build-cache tracking tripwire (T535) ===" && ./scripts/ci/check-no-wrangler-cache-tracked.sh
echo "=== Wrangler build-cache tracking tripwire self-test ===" && bash scripts/ci/check-no-wrangler-cache-tracked-test.sh
echo "=== Jinja template parse gate ===" && ./scripts/ci/lint-jinja-templates.sh
echo "=== Jinja template parse gate self-test ===" && ./scripts/ci/lint-jinja-templates-test.sh
echo "=== ClickHouse Prometheus endpoint self-test ===" && ./scripts/ci/clickhouse-exporter-test.sh
# The archival-node role configures ClickHouse in four task files and
# installs it in a fifth, so a host without ClickHouse must not hard-fail
# the whole role. Runs the role's own main.yml locally
# (~12s, --check for the log-discipline arms) — no hosts, no ClickHouse.
echo "=== Ansible ClickHouse host-gate self-test ===" && ./scripts/ci/ansible-clickhouse-host-gate-test.sh
echo "=== Ansible listing-sync pubnet gate ===" && ./scripts/ci/ansible-listing-sync-gate-test.sh
echo "=== Alertmanager apply-path parity ===" && ./scripts/ci/check-alertmanager-parity.sh
echo "=== Alertmanager apply-path parity self-test ===" && ./scripts/ci/check-alertmanager-parity-test.sh
echo "=== Alertmanager inhibit-rule families ===" && bash configs/alertmanager/inhibit-rules-test.sh
echo "=== pgBackRest backup wrapper self-test ===" && ./scripts/ci/pgbackrest-backup-test.sh
echo "=== API-smoke textfile self-test ===" && ./scripts/ci/smoke-textfile-test.sh
echo "=== Served-value harness scheduling + cadence ===" && ./scripts/ci/lint-served-value-cadence.sh
echo "=== Served-value harness scheduling self-test ===" && ./scripts/ci/lint-served-value-cadence-test.sh
echo "=== data-freshness watchdog self-test ===" && ./scripts/ci/data-freshness-test.sh
echo "=== TimescaleDB job/CAGG probe self-test ===" && ./scripts/ci/timescale-jobs-probe-test.sh
echo "=== galexie catchup probe self-test ===" && ./scripts/ci/galexie-catchup-probe-test.sh
echo "=== galexie archive contiguity self-test ===" && ./scripts/ci/galexie-archive-contiguity-test.sh
echo "=== stellar-core auto-upgrade self-test ===" && ./scripts/ci/stellar-core-auto-upgrade-test.sh
echo "=== galexie archive tip-lag self-test ===" && ./scripts/ci/galexie-archive-tip-lag-test.sh
# BASE_SHA-gated. This and lint-replay-plan.sh below take their comparison base
# from the environment and, with none set, print a skip line and exit 0, which
# made verify<->CI parity look honest while checking nothing (an undeclared
# .gitleaksignore entry shipped through a green local run and reddened main).
#
# Locally the analogue of CI's push base is the merge-base with the upstream
# tip, covering exactly the commits about to be pushed. Nothing unpushed gives an
# empty range and both gates pass trivially, which is correct: no growth to declare.
if [ -z "${BASE_SHA:-}" ] && git rev-parse -q --verify origin/main >/dev/null 2>&1; then
    BASE_SHA="$(git merge-base origin/main HEAD 2>/dev/null || true)"
    [ -n "$BASE_SHA" ] && export BASE_SHA && echo "verify: BASE_SHA=${BASE_SHA} (merge-base with origin/main)"
fi
echo "=== Baseline-growth tripwire ===" && ./scripts/ci/lint-baseline-growth.sh
echo "=== Restore-drill contract + abort-path tests ===" && bash scripts/ops/restore-drill-test.sh && bash scripts/ops/restore-drill-run-test.sh
echo "=== config-assertions live-guard tests ===" && bash scripts/ops/config-assertions_test.sh
echo "=== rederive-from driver tests ===" && bash scripts/ops/rederive-from-test.sh
# BASE_SHA-gated like lint-baseline-growth.sh: self-skips locally, real in CI.
echo "=== Replay-plan tripwire ===" && ./scripts/ci/lint-replay-plan.sh
echo "=== External channels ===" && ./scripts/ci/lint-external-channels.sh
echo "=== External channels self-test ===" && ./scripts/ci/lint-external-channels-test.sh
echo "=== Doc-pinned tests self-test ===" && ./scripts/ci/doc-pinned-tests-test.sh
# CI runs it only on go=false diffs; the full suite above already covers it here, so just list.
echo "=== Doc-pinned tests ===" && { [ -z "${BASE_SHA:-}" ] || ./scripts/ci/doc-pinned-tests.sh --list "$BASE_SHA"; }
echo "=== OpenAPI URLs (query discipline + served hosts) ===" && go run ./scripts/ci/lint-openapi-urls openapi/stellar-index.v1.yaml
echo "=== PK discriminators ===" && go run ./scripts/ci/lint-pk-discriminators
echo "=== derive_generation guard (INV-3) ===" && go run ./scripts/ci/lint-derive-generation-guard
echo "=== Comment history citations ===" && ./scripts/ci/lint-comments.sh
echo "=== Comment history citations self-test ===" && ./scripts/ci/lint-comments-test.sh
# Structural rule-file lint, pure Python (no promtool): catches the mis-indented
# rule class (alerts at group level → "field expr not found in type RuleGroup")
# that otherwise only CI's promtool job flags.
echo "=== Rule structure ===" && python3 ./scripts/ci/lint-rule-structure.py
# YAML-aware runbook guard: runbook_url must be an annotation,
# not a label, on every alert — else the Alertmanager Discord templates
# render no runbook link. Fails if a runbook_url regresses back into labels.
echo "=== Runbook annotations ===" && python3 ./scripts/ci/lint-runbook-annotations.py
# Secret-rendering ansible template tasks must set `diff: false` (or
# no_log) so `--check --diff` never prints vault material into scrollback
# or the weekly drift job's CI log.
echo "=== Ansible secret-diff ===" && python3 ./scripts/ci/lint-ansible-secret-diff.py
# Installed scripts reaching ClickHouse without a credential file
# (report-only until `default` is locked) + its self-test.
echo "=== ClickHouse client auth ===" && python3 ./scripts/ci/lint-ch-client-auth.py && ./scripts/ci/lint-ch-client-auth-test.sh
# The metric-refs SELF-test needs neither promtool nor the monitoring stack, so
# it runs outside the promtool branch (which would skip it on any machine that
# HAS promtool). CI's repo-gates job runs it, so verify.sh must too or
# check-verify-parity fails.
echo "=== Metric refs self-test ===" && ./scripts/ci/lint-metric-refs-test.sh
echo "=== Unit-failed baseline self-test ===" && ./scripts/ci/lint-unit-failed-baseline-test.sh
echo "=== ClickHouse ops-user contract self-test ===" && ./scripts/ops/ch-ops-user-test.sh
# lint-changed.sh is a dispatcher OVER the scripts/ci gates with no caller of its
# own, and check-verify-parity.sh enforces only the CI->verify direction, so run
# its self-test here explicitly.
echo "=== Changed-file dispatcher self-test ===" && ./scripts/dev/lint-changed-test.sh
echo "=== Doctor self-test ===" && ./scripts/dev/doctor-test.sh
echo "=== verify-r1-sync self-test ===" && ./scripts/dev/verify-r1-sync-test.sh
# Pins commit-identity-range.sh's new-branch/tag fallback (detached HEAD with
# the branch's own commit also present as a remote-tracking ref) against the
# regression that made the range come back empty and the job report "checking 0
# commit(s)" as a pass.
echo "=== Commit-identity range self-test ===" && ./scripts/ci/commit-identity-range-test.sh
# govulncheck. Graceful-skip when not installed locally —
# CI installs it via `make deps`. Mirrors the promtool pattern.
if command -v govulncheck >/dev/null 2>&1; then
    echo "=== Vuln ==="        && make vuln
else
    defer_check "Vuln" "govulncheck is not installed"
fi

# ── Generated-artifact drift: MUST run before any parallel lane starts ─────
#
# CI enforces three: docs/reference/api and examples/postman (`openapi` job) and
# web/explorer/src/api/types.ts (`web/explorer` job), each by regenerating and
# diffing. An OpenAPI change that regenerated two passed locally and reddened CI
# on the third.
#
# Regenerating here is deliberate: a local run should FIX drift so the operator
# commits it; the diff is still checked so the run is loud about changed files.
#
# Run before any lane because it rewrites web/explorer/src/api/types.ts, which
# lane c reads; a concurrent regenerate-while-read is the race the "Parallel
# phase" ordering rule forbids, and every lane now sees the regenerated tree.
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

# Config reference drift. docs/reference/config/README.md is generated from
# internal/config/config.go struct tags (`make docs-config`) and is not in the
# diff list above, so a removed tag would leave the doc describing a dead block.
# Needs only `go run`, so no node/npx gate.
echo "=== Generated config reference drift ==="
go run ./cmd/stellarindex-ops docs-config > docs/reference/config/README.md
if ! git diff --exit-code --stat -- docs/reference/config; then
    echo "⚠️  docs/reference/config/README.md was STALE and has been regenerated above."
    echo "    Commit this file — lint-docs.sh's config-sync check only catches the"
    echo "    reverse direction (a tag added but not documented), not this one."
    exit 1
fi

# ── Parallel phase (TIER 1b) ─────────────────────────────────────────────────
#
# Four lanes, grouped so none shares a file with another, which is what makes
# concurrent execution safe rather than merely fast:
#
#   a  doc lints            reads markdown + this file's own targets; writes nothing
#   b  Go build/vet/tests   the ONLY lane that writes a tracked file (`make fmt`)
#   c  web typecheck/build  pnpm/node only; never touches *.go
#   d  everything else      ops/ansible/migration/monitoring self-tests; none of it
#                           invokes go, invokes pnpm, or writes a tracked file
#
# Lane b (Format + Vet + Lint + Test + Integration build) is the ceiling, about a
# third of the serial total. Lane d keeps promtool's Monitoring, amtool's
# Alertmanager config and gitleaks' Secrets scans: none touches *.go or web/**.
#
# A lane runs in a background subshell, so it cannot write this script's
# `deferred_checks` or exit the parent: each keeps its own deferred-count file and
# exit code, collected after every lane finishes.
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
    echo "=== Alerts catalogue self-test ===" && ./scripts/ci/lint-alerts-catalog-test.sh
    echo "=== Alerts catalogue ===" && python3 ./scripts/ci/lint-alerts-catalog.py
    echo "=== Doc links ===" && ./scripts/ci/lint-doc-links.sh
    echo "=== Doc links self-test ===" && ./scripts/ci/lint-doc-links-test.sh
    echo "=== ADR refs ===" && ./scripts/ci/lint-adr-refs.sh
    echo "=== ADR refs self-test ===" && ./scripts/ci/lint-adr-refs-test.sh
}

lane_b() { # Go build/vet/unit tests
    echo "=== Format ==="        && make fmt
    echo "=== Vet ==="           && make vet
    echo "=== Lint ==="          && make lint
    echo "=== Test ==="          && make test
    # Compile-only: catches interface-extension breakage in build-tagged
    # integration adapters without testcontainers (Docker isn't always local;
    # real `make test-integration` lives outside verify).
    echo "=== Integration build ===" && make test-integration-build
}

lane_c() { # web typecheck/lint/test/build. Graceful-skip when pnpm isn't
           # installed locally — CI runs the same gate via the `web/explorer`
           # job, so a local skip just defers the check. The build catches
           # Next.js output: 'export' constraints (e.g. dynamic =
           # 'force-static' on sitemap/robots) that typecheck alone misses.
    echo "=== _redirects rule cap ===" && ./scripts/ci/check-redirects-cap.sh
    if command -v pnpm >/dev/null 2>&1 && [ -f web/explorer/pnpm-lock.yaml ]; then
        echo "=== Showcase typecheck ===" && make web-typecheck
        echo "=== Showcase lint ==="      && make web-lint
        # Runs the vitest suite, including safe-domain.test.ts (the isSafeHomeDomain /
        # isSafePublicImageUrl phishing + client-SSRF regression gate): a refactor
        # loosening it must not land green.
        # BUILD BEFORE TEST: nav-shell.built.test.ts asserts on the static export in
        # web/explorer/out, so with the build after it the suite graded a stale
        # `out/` (a five-day-old export reported `/rwa: no rwa/index.html in the
        # export` and empty <main> pages). A verdict that depends on leftover build
        # output reads green for an unbuilt tree and red for reasons unrelated to the diff.
        echo "=== Showcase build ==="     && \
            NEXT_PUBLIC_API_BASE_URL=http://api.local-stub.invalid make web-build >/dev/null
        echo "=== Showcase tests ==="     && make web-test
    else
        defer_check_lane "$LANEDIR/c.deferred" "Showcase" "pnpm or web/explorer/pnpm-lock.yaml is unavailable"
    fi
    # Dashboard SPA, same pnpm gate; skipped silently without a lockfile (fresh checkout).
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
    echo "=== Ansible Postgres DSN encoding self-test ===" && ./scripts/ci/ansible-postgres-dsn-test.sh
    echo "=== Ansible Postgres WAL archiving render self-test ===" && ./scripts/ci/ansible-postgres-archive-test.sh
    echo "=== Ansible nftables per-source SSH rate-limit self-test ===" && ./scripts/ci/ansible-ssh-rate-limit-test.sh
    echo "=== Ansible prometheus archival-host guard self-test ===" && ./scripts/ci/ansible-prometheus-host-guard-test.sh
    echo "=== Ansible exporter listen-address self-test ===" && ./scripts/ci/ansible-exporter-listen-address-test.sh
    echo "=== Ansible textfile-collector dir mode self-test ===" && ./scripts/ci/ansible-textfile-dir-mode-test.sh
    echo "=== Ansible ZFS ARC cap self-test ===" && ./scripts/ci/ansible-zfs-arc-cap-test.sh
    echo "=== Ansible Postgres log size-cap self-test ===" && ./scripts/ci/ansible-pg-logrotate-cap-test.sh
    echo "=== Ansible prometheus port var self-test ===" && ./scripts/ci/ansible-prometheus-port-var-test.sh
    echo "=== Ansible patroni etcd TLS self-test ===" && ./scripts/ci/ansible-patroni-etcd-tls-test.sh
    echo "=== Ansible node-exporter-collectors install self-test ===" && ./scripts/ci/ansible-node-exporter-collectors-install-test.sh
    echo "=== Ansible Caddy signing-key pipefail self-test ===" && ./scripts/ci/ansible-caddy-key-pipefail-test.sh
    echo "=== Ansible galexie-backfill-status empty-log self-test ===" && ./scripts/ci/ansible-galexie-backfill-status-test.sh
    echo "=== Promtail server listen-address self-test ===" && ./scripts/ci/promtail-listen-address-test.sh
    echo "=== Ansible README parity self-test ===" && ./scripts/ci/ansible-readme-parity-test.sh
    echo "=== Ansible env-file secret guard self-test ===" && ./scripts/ci/ansible-envfile-secret-guard-test.sh
    echo "=== Ansible pubnet example checksum parity self-test ===" && ./scripts/ci/ansible-pubnet-checksum-parity-test.sh
    echo "=== Ansible keepalived healthcheck self-test ===" && ./scripts/ci/ansible-keepalived-healthcheck-test.sh
    echo "=== HAProxy http-request timeout self-test ===" && ./scripts/ci/haproxy-http-request-timeout-test.sh
    # CI's repo-gates job runs these gate scripts too; verify.sh must mirror
    # them or it issues a green CI won't honour (enforced by the parity
    # check above). All are deterministic + network-free.
    echo "=== Migration backward-compat ===" && ./scripts/ci/lint-migration-compat.sh
    echo "=== Lake dedup (aggregating reads of duplicate-bearing archives) ===" && ./scripts/ci/lint-lake-dedup.sh
    echo "=== Lake dedup self-test ===" && ./scripts/ci/lint-lake-dedup-test.sh
    echo "=== ClickHouse apply scope (deploy/clickhouse is not a bootstrap manifest) ===" && ./scripts/ci/lint-ch-apply-scope.sh
    echo "=== ClickHouse apply scope self-test ===" && ./scripts/ci/lint-ch-apply-scope-test.sh
    echo "=== Shell SIGPIPE self-test ===" && ./scripts/ci/lint-shell-sigpipe-test.sh
    echo "=== Public-dataset drift-verdict self-test ===" && ./scripts/ci/check-public-dataset-test.sh
    echo "=== Fleet release-drift verdict self-test ===" && ./scripts/ci/check-fleet-release-drift-test.sh
    echo "=== zfs-snapshot job self-test ===" && ./scripts/ci/zfs-snapshot-test.sh
    echo "=== ordinal-rederive-chunks wrapper self-test ===" && ./scripts/ci/ordinal-rederive-chunks-test.sh
    # Pins WHICH datasets the job is given (the Galexie LCM archive the other two
    # tiers derive from had none); the behavioural twin above pins HOW it snapshots.
    # Renders the role template, so render arms skip without a local jinja2; CI fails closed.
    echo "=== zfs-snapshot dataset coverage ===" && ./scripts/ci/zfs-snapshot-coverage-test.sh
    # Migrations-sync self-test: structural half needs only python; the
    # behavioural half runs the task file with ansible and needs GNU tar on the
    # target (unarchive --diff). macOS ships bsdtar — point it at a container
    # via DEPLOY_SYNC_CONNECTION/DEPLOY_SYNC_HOST (see the script header) or
    # let CI's toolchain-gates job (ubuntu) run it. Graceful-skip only when the
    # tools are missing, same convention as promtool below.
    if command -v ansible-playbook >/dev/null 2>&1 && { [ -n "${DEPLOY_SYNC_CONNECTION:-}" ] || grep -q 'GNU tar' <<<"$(tar --version 2>/dev/null)"; }; then
        echo "=== Deploy migrations-sync self-test ===" && ./scripts/ci/deploy-sync-test.sh
    else
        defer_check_lane "$LANEDIR/d.deferred" "Deploy migrations-sync self-test" "needs ansible-playbook and GNU tar; use VERIFY_PROFILE=container on macOS"
    fi
    echo "=== Baseline-growth tripwire self-test ===" && ./scripts/ci/lint-baseline-growth-test.sh
    echo "=== Config-apply gate self-test ===" && ./scripts/ci/config-apply-gate-test.sh
    # Repo-gates self-tests need a verify.sh twin, or check-verify-parity
    # goes red on main for everyone.
    echo "=== Public-dataset drift decision-core self-test ===" && ./scripts/ci/check-public-dataset-test.sh
    echo "=== Replay-plan tripwire self-test ===" && ./scripts/ci/lint-replay-plan-test.sh
    echo "=== Verdict helpers self-test (oneshot waits, sentinel gates) ===" && bash scripts/ops/ops-verdict-test.sh
    echo "=== ClickHouse maintenance + Phase-D failure-path self-test ===" && bash scripts/ops/ch-maintenance-fail-closed-test.sh
    echo "=== Lake-dedup driver self-test ===" && bash deploy/clickhouse/lake-dedup-driver-test.sh
    echo "=== D3 current-state rebuild self-test (cutover coverage gate) ===" && bash scripts/ops/d3-lecur-v2-rebuild-test.sh
    # Prometheus rule files. Graceful-skip without promtool (CI installs it); the
    # Makefile target hard-fails, so verify.sh guards it to keep local runs working.
    if command -v promtool >/dev/null 2>&1; then
        echo "=== Monitoring ===" && make monitoring-check
    else
        defer_check_lane "$LANEDIR/d.deferred" "Monitoring" "promtool is not installed"
        # The dead-metric-ref guard needs no promtool, so run it even when
        # the promtool-dependent monitoring-check is skipped.
        echo "=== Metric refs ===" && ./scripts/ci/lint-metric-refs.sh
    fi
    # Alertmanager config: validates BOTH render branches of apply.sh (all URLs empty
    # → stub path through the block-stripper; all set → substitution path).
    # Graceful-skip without amtool; CI's monitoring-rules job never skips.
    if command -v amtool >/dev/null 2>&1; then
        echo "=== Alertmanager config ==="
        ALERTMANAGER_SECRETS=/dev/null bash configs/alertmanager/apply.sh --check-only
        AM_DUMMY_ENV=$(mktemp)
        printf 'HEALTHCHECKS_DEADMANSSWITCH_URL=https://hc-ping.com/x\nDISCORD_WEBHOOK_URL_PAGES=https://discord.com/api/webhooks/1/a\nDISCORD_WEBHOOK_URL_ALERTS=https://discord.com/api/webhooks/2/b\n' > "$AM_DUMMY_ENV"
        ALERTMANAGER_SECRETS="$AM_DUMMY_ENV" bash configs/alertmanager/apply.sh --check-only
        rm -f "$AM_DUMMY_ENV"
        # And the fail-closed guard's SELF-test: an empty URL must be refused at
        # apply time. Renders green either way without this (a 31-day outage
        # installed a receiver-less config through a fully-passing gate).
        bash configs/alertmanager/apply-test.sh
    else
        defer_check_lane "$LANEDIR/d.deferred" "Alertmanager config" "amtool is not installed"
    fi
    # gitleaks (secret scan), graceful-skip when absent. Two scans, because
    # neither subsumes the other:
    #
    #   --no-git  scans the WORKING TREE, including uncommitted edits: catches a
    #             fixture before you commit it. Cannot see history.
    #   (default) scans COMMITTED HISTORY, which is what CI runs (fetch-depth: 0).
    #             Cannot see uncommitted edits.
    #
    # A fixture committed then removed in a follow-up commit passes --no-git but
    # stays red in CI, since the introducing commit is still in the log. History
    # findings provably not credentials are exempted by fingerprint in
    # .gitleaksignore.
    if command -v gitleaks >/dev/null 2>&1; then
        echo "=== Secrets (gitleaks, working tree) ===" && \
            gitleaks detect --no-git --no-banner --redact --config .gitleaks.worktree.toml
        echo "=== Secrets (gitleaks, history — CI parity) ===" && \
            gitleaks detect --no-banner --redact --config .gitleaks.toml
        echo "=== Secrets allowlist scope self-test ===" && \
            ./scripts/ci/gitleaks-allowlist-scope-test.sh
    else
        defer_check_lane "$LANEDIR/d.deferred" "Secrets" "gitleaks is not installed"
    fi
}

# Lane CONCURRENCY is bounded by memory, not core count. Lane b runs golangci-lint
# (multi-GB) and lane c a Next.js build; `make prepush` runs inside Docker with a
# 7.65 GiB VM, where all four lanes OOM-killed golangci-lint (`make[1]: ***
# [Makefile:158: lint] Killed`, a bare SIGKILL that reads like a lint failure).
#
# Below the threshold the heavy lanes are separated, not the whole tier: a and d
# (doc lints; shell/python/ansible self-tests plus gitleaks, CPU-hungry but not
# memory-hungry) run together, then b alone, then c alone. VERIFY_LANES=1 forces
# that schedule and VERIFY_LANES=4 the parallel one.
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
    # b and c run one at a time but each as its own backgrounded job so `wait`
    # reports its real exit code: a function on the left of `||` runs with errexit
    # disabled, which hid a failing `make test`/`make lint` inside it.
    lane_b > "$LANEDIR/b.log" 2>&1 & pid_b=$!
    wait "$pid_b" || lane_rc_b=$?
    lane_c > "$LANEDIR/c.log" 2>&1 & pid_c=$!
    wait "$pid_c" || lane_rc_c=$?
fi

# Every lane's log, in full, in lane order; each lane is strictly sequential inside.
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

# A lane failure fails the run and names every failed lane, not just the first
# bash noticed.
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
