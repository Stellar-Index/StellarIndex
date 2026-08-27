#!/usr/bin/env bash
# Local sequential quality checks — run this before every push.
#
# CI runs these jobs in parallel; verify.sh is the strictly-sequential
# local equivalent that surfaces failures one at a time. Pattern
# borrowed from loop-app/scripts/verify.sh.

set -euo pipefail

cd "$(dirname "$0")/../.."

# verify.sh ↔ CI parity (W5-ci-6). The #1 cause of "green locally, red in CI"
# is this gate drifting behind CI's import-checks job. This deterministic
# meta-check (no network) fails fast if CI runs a scripts/ci gate that the
# steps below do not mirror — so a lint added to CI obligates adding it here.
# The self-test runs first (the gate is only as trustworthy as its fixtures).
echo "=== verify↔CI parity self-test ===" && ./scripts/ci/check-verify-parity-test.sh
echo "=== verify.sh ↔ CI import-checks parity ===" && ./scripts/ci/check-verify-parity.sh

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

echo "=== Format ==="        && make fmt
echo "=== Vet ==="           && make vet
echo "=== Lint ==="          && make lint
echo "=== Docs ==="          && ./scripts/ci/lint-docs.sh
echo "=== Imports ==="       && ./scripts/ci/lint-imports.sh
echo "=== Imports self-test ===" && ./scripts/ci/lint-imports-test.sh
echo "=== Protocol registry sync ===" && ./scripts/ci/lint-protocol-registry-sync.sh
echo "=== Lexicon ==="       && ./scripts/ci/lint-lexicon.sh
echo "=== i128/NUMERIC ===" && ./scripts/ci/lint-i128.sh
echo "=== Migrations money ===" && ./scripts/ci/lint-migrations.sh
# CI's import-checks job runs these gate scripts too; verify.sh must mirror
# them or it issues a green CI won't honour (W5-ci-6, enforced by the parity
# check above). All are deterministic + network-free.
echo "=== Migration backward-compat ===" && ./scripts/ci/lint-migration-compat.sh
echo "=== Migration backward-compat self-test ===" && ./scripts/ci/lint-migration-compat-test.sh
echo "=== Migration immutability ===" && ./scripts/ci/lint-migration-immutability.sh
echo "=== Migration immutability self-test ===" && ./scripts/ci/lint-migration-immutability-test.sh
echo "=== Completeness-staleness calibration ===" && ./scripts/ci/lint-completeness-staleness.sh
echo "=== Deploy-protection self-test ===" && ./scripts/ci/check-deploy-protection-test.sh
echo "=== Main-CI-health decision-core self-test ===" && ./scripts/ci/check-main-ci-health-test.sh
echo "=== Ansible-drift decision-core self-test ===" && ./scripts/ci/check-ansible-drift-test.sh
echo "=== Baseline-growth tripwire self-test ===" && ./scripts/ci/lint-baseline-growth-test.sh
# BASE_SHA-gated: self-skips locally (no comparison base); runs for real in CI
# with the PR/push base. Invoked here to keep verify↔CI parity honest.
echo "=== Baseline-growth tripwire ===" && ./scripts/ci/lint-baseline-growth.sh
echo "=== OpenAPI URLs ===" && go run ./scripts/ci/lint-openapi-urls openapi/stellar-index.v1.yaml
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
# Prometheus rule files. Graceful-skip when promtool isn't
# installed locally — CI installs it explicitly. The Makefile
# target hard-fails on missing promtool; verify.sh wraps it with
# an existence check so local-dev `bash scripts/dev/verify.sh`
# keeps working without a full Prometheus install.
if command -v promtool >/dev/null 2>&1; then
    echo "=== Monitoring ===" && make monitoring-check
else
    echo "=== Monitoring (skipped — promtool not installed; install via 'brew install prometheus' or the Prometheus GH release) ==="
    # The dead-metric-ref guard needs no promtool, so run it even when
    # the promtool-dependent monitoring-check is skipped (F-1329).
    echo "=== Metric refs ===" && ./scripts/ci/lint-metric-refs.sh
fi
# The metric-refs SELF-test (does the guard still detect a dead ref?)
# needs neither promtool nor the monitoring stack, so it runs
# unconditionally — outside the promtool branch above, which would
# otherwise skip it on any machine that HAS promtool. CI's import-checks
# job runs it, so verify.sh must too or check-verify-parity fails.
echo "=== Metric refs self-test ===" && ./scripts/ci/lint-metric-refs-test.sh
# govulncheck (F-0057). Graceful-skip when not installed locally —
# CI installs it via `make deps`. Mirrors the promtool pattern.
if command -v govulncheck >/dev/null 2>&1; then
    echo "=== Vuln ==="        && make vuln
else
    echo "=== Vuln (skipped — govulncheck not installed; install via 'go install golang.org/x/vuln/cmd/govulncheck@latest') ==="
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
        gitleaks detect --no-git --no-banner --redact --config .gitleaks.toml
    echo "=== Secrets (gitleaks, history — CI parity) ===" && \
        gitleaks detect --no-banner --redact --config .gitleaks.toml
else
    echo "=== Secrets (skipped — gitleaks not installed; install via 'brew install gitleaks') ==="
fi
# Generated-artifact drift. CI enforces three of these — docs/reference/api
# and examples/postman in the `openapi` job, web/explorer/src/api/types.ts in
# the `web/explorer` job — each by regenerating and diffing. verify.sh ran
# none of them, so an OpenAPI change that regenerated two of the three
# passed local gate and reddened CI on the third (2026-07-25).
#
# Regenerating here is deliberate: unlike CI, a local run should FIX the
# drift rather than just report it, so the operator commits the result. The
# diff is still checked so the run is loud about having changed files.
if command -v node >/dev/null 2>&1 && command -v npx >/dev/null 2>&1; then
    echo "=== Generated API reference + Postman drift ==="
    ./scripts/dev/docs-api.sh >/dev/null
    ./scripts/dev/docs-postman.sh >/dev/null
    if ! git diff --exit-code --stat -- docs/reference/api examples/postman; then
        echo "⚠️  Generated artifacts were STALE and have been regenerated above."
        echo "    Commit these files — CI fails on exactly this diff."
        exit 1
    fi
else
    echo "=== Generated-artifact drift (skipped — node/npx not installed) ==="
fi
echo "=== Test ==="          && make test
# Compile-only: catches interface-extension breakage in
# build-tagged integration adapters without spinning testcontainers.
# Real `make test-integration` lives outside verify because Docker
# isn't always available locally.
echo "=== Integration build ===" && make test-integration-build
# Showcase typecheck + lint + build. Graceful-skip when pnpm
# isn't installed locally — CI runs the same gate via the
# `web/explorer` job, so a local skip just defers the check.
# The build catches Next.js output: 'export' constraints
# (e.g. dynamic = 'force-static' on sitemap/robots) that
# typecheck alone misses.
if command -v pnpm >/dev/null 2>&1 && [ -f web/explorer/pnpm-lock.yaml ]; then
    echo "=== Showcase typecheck ===" && make web-typecheck
    echo "=== Showcase lint ==="      && make web-lint
    # The vitest suite ran in NEITHER CI nor this gate (cold audit
    # 2026-08-04): 209 tests across 49 files, all green and all dead —
    # including safe-domain.test.ts, the isSafeHomeDomain /
    # isSafePublicImageUrl phishing + client-SSRF regression gate, and
    # the AGT-06 stale-flag regression. A refactor loosening any of
    # them would have landed green.
    echo "=== Showcase tests ==="     && make web-test
    echo "=== Showcase build ==="     && \
        NEXT_PUBLIC_API_BASE_URL=http://api.local-stub.invalid make web-build >/dev/null
else
    echo "=== Showcase (skipped — pnpm not installed; install via 'brew install pnpm' or 'corepack enable') ==="
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
echo ""
echo "✅ ALL CHECKS PASSED"
