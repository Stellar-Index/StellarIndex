#!/usr/bin/env bash
# bootstrap-worktree.sh — make a fresh checkout or linked worktree able to
# pass scripts/dev/verify.sh, in one command, and report EVERY missing item
# in one pass rather than one per twelve-minute run.
#
# The cost of not having this, measured on 2026-09-07: two full verify.sh
# runs, each failing on a different missing install about ten minutes in and
# in a section unrelated to the change under test.
#
#   run 1  `openapi-typescript: command not found` — web/explorer had no
#          node_modules, so `make web-generate-api` died inside the
#          generated-artifact drift check.
#   run 2  `tsc: command not found` — web/status had no node_modules, so
#          `make status-typecheck` died. verify.sh gates that section on
#          web/status/pnpm-lock.yaml, which is TRACKED and therefore always
#          present; node_modules is not tracked and was not checked.
#
# Both are five-second questions. Nothing here is serial: the survey runs to
# completion and prints every gap before the first install.
#
# Usage:
#   scripts/dev/bootstrap-worktree.sh          survey, then install what it can
#   scripts/dev/bootstrap-worktree.sh --check  survey only, install nothing
#   make bootstrap-worktree
#
# Severity, and what the exit code means:
#
#   BLOCKING    scripts/dev/verify.sh HARD-FAILS without it. Exit is
#               non-zero while any of these is unresolved.
#   clearance   verify.sh defers the check and ends "VERIFY INCOMPLETE",
#               but scripts/dev/doctor.sh --profile native — which
#               `make prepush` and `scripts/dev/cut-release.sh` both run —
#               treats it as required. Reported with its install command;
#               does not fail this script.
#
# What is installed, and what deliberately is not:
#
#   - Missing pinned Go tools are installed by `make deps`, which owns the
#     pins. This script never repeats a version number.
#   - Missing node_modules are installed with the same
#     `pnpm --dir <app> install --frozen-lockfile` that scripts/dev/prepush.sh
#     runs, for BOTH web/explorer and web/status.
#   - A tool present at the WRONG version is reported, never silently
#     corrected: ~/go/bin is shared with every other checkout on the
#     machine, so downgrading one is a decision for the operator, and
#     `make deps` is the one command that makes them all match.
#
# Note that ~/go/bin is not on every shell's PATH. The Makefile calls
# gofumpt, goimports and golangci-lint through $(GOBIN) by absolute path,
# so they work regardless; this survey resolves them the same way rather
# than through `command -v`, which would report a false miss.

set -uo pipefail

cd "$(dirname "$0")/../.." || exit 2

CHECK_ONLY=0
case "${1:-}" in
    --check) CHECK_ONLY=1 ;;
    -h|--help)
        echo "usage: $0 [--check]" >&2
        exit 0
        ;;
    "") ;;
    *) echo "bootstrap-worktree: unknown argument '$1'" >&2; exit 2 ;;
esac

GO_BIN="$(go env GOPATH 2>/dev/null || true)/bin"
WEB_APPS="web/explorer web/status"

blocking_missing=()
clearance_missing=()
version_drift=()
install_go=0
install_apps=""

ok()    { printf '  ok        %-22s %s\n' "$1" "$2"; }
miss()  { printf '  MISSING   %-22s %s\n' "$1" "$2"; }
warn()  { printf '  warn      %-22s %s\n' "$1" "$2"; }

echo "bootstrap-worktree: $(pwd)"
echo ""
echo "── survey (every gap is listed before anything is installed) ─────────"

# ── 1. Interpreters and gate tools resolved on PATH ──────────────────────
#
# BLOCKING set: verify.sh runs these unconditionally and dies without them.
for tool in git make go python3 tar; do
    if path="$(command -v "$tool")"; then
        ok "$tool" "$path"
    else
        miss "$tool" "not on PATH"
        blocking_missing+=("$tool")
    fi
done

# ── 2. Go tools the Makefile resolves through GOBIN ──────────────────────
#
# make fmt and make lint call these by absolute path out of $(GOBIN), so a
# `command -v` survey reports a false miss on any machine whose PATH omits
# ~/go/bin — which is most of them. Resolve them where the Makefile does,
# and compare against the Makefile's own pins.
go_tool_pin() { # go_tool_pin <make-variable>
    var="$1"
    line="$(grep -E "^${var} *:?= *v" Makefile)"
    printf '%s' "${line##*= }"
}
go_tool_installed() { # go_tool_installed <path>
    go version -m "$1" 2>/dev/null | awk '$1 == "mod" { print $3 }'
}
survey_go_tool() { # survey_go_tool <tool> <make-pin-variable> <severity>
    tool="$1"; pin_var="$2"; severity="$3"
    pin="$(go_tool_pin "$pin_var")"
    if [ ! -x "${GO_BIN}/${tool}" ]; then
        miss "$tool" "absent from ${GO_BIN} (pin ${pin:-unknown}) — 'make deps' installs it"
        if [ "$severity" = blocking ]; then blocking_missing+=("$tool"); else clearance_missing+=("$tool"); fi
        install_go=1
        return
    fi
    have="$(go_tool_installed "${GO_BIN}/${tool}")"
    if [ -n "$pin" ] && [ -n "$have" ] && [ "$have" != "$pin" ]; then
        warn "$tool" "${GO_BIN}/${tool} is ${have}, Makefile pins ${pin}"
        version_drift+=("${tool} ${have} != ${pin}")
    else
        ok "$tool" "${GO_BIN}/${tool} ${have:-(version unreadable)}"
    fi
}
# Only these three. govulncheck is deliberately NOT here: `make vuln` and
# verify.sh both resolve it with `command -v`, so a copy sitting in
# ${GO_BIN} that PATH cannot see is still a deferred check — surveying it
# by absolute path would report "ok" for something verify.sh skips.
survey_go_tool gofumpt       GOFUMPT_VERSION       blocking
survey_go_tool goimports     GOIMPORTS_VERSION     blocking
survey_go_tool golangci-lint GOLANGCI_LINT_VERSION blocking

# ── 3. Tools verify.sh defers on, and doctor --profile native requires ───
#
# Absent, verify.sh prints "(deferred: …)" and ends VERIFY INCOMPLETE with
# exit 1 — by design, not a bug — while doctor's native profile, which
# `make prepush` and cut-release.sh both run, treats each as required.
for tool in node npx pnpm jq govulncheck gitleaks promtool amtool ansible-playbook zizmor gh; do
    if path="$(command -v "$tool")"; then
        ok "$tool" "$path"
    else
        miss "$tool" "not on PATH — verify.sh defers its check; make prepush refuses"
        clearance_missing+=("$tool")
    fi
done

# govulncheck's pin still matters once PATH can see it — the Makefile's
# `audit` target reinstalls at GOVULNCHECK_VERSION, so a newer copy is drift.
if govulncheck_path="$(command -v govulncheck)"; then
    govulncheck_pin="$(go_tool_pin GOVULNCHECK_VERSION)"
    govulncheck_have="$(go_tool_installed "$govulncheck_path")"
    if [ -n "$govulncheck_pin" ] && [ -n "$govulncheck_have" ] && [ "$govulncheck_have" != "$govulncheck_pin" ]; then
        warn "govulncheck-pin" "${govulncheck_path} is ${govulncheck_have}, Makefile pins ${govulncheck_pin}"
        version_drift+=("govulncheck ${govulncheck_have} != ${govulncheck_pin}")
    fi
fi

# GNU tar. macOS ships bsdtar, so the deploy migrations-sync self-test is
# deferred here by design (verify.sh's own comment says so). Recorded, never
# treated as a gap.
if grep -q 'GNU tar' <<<"$(tar --version 2>/dev/null)"; then
    ok "gnu-tar" "$(command -v tar)"
else
    warn "gnu-tar" "bsdtar — the deploy migrations-sync self-test stays deferred (by design on macOS)"
fi

# ── 4. node_modules in BOTH web apps ─────────────────────────────────────
#
# This is the pair that cost the two runs. The check is for the executable
# each gate actually invokes, not for the directory: a half-finished install
# leaves node_modules present and the binary absent.
survey_web_app() { # survey_web_app <dir> <bin-name> <gate>
    app="$1"; bin="$2"; gate="$3"
    if [ ! -f "${app}/pnpm-lock.yaml" ]; then
        ok "${app}" "no pnpm-lock.yaml — verify.sh skips this app entirely"
        return
    fi
    if [ -x "${app}/node_modules/.bin/${bin}" ]; then
        ok "${app}" "node_modules/.bin/${bin} present"
    else
        miss "${app}" "node_modules/.bin/${bin} absent — ${gate} fails with '${bin}: command not found'"
        blocking_missing+=("${app}/node_modules")
        install_apps="${install_apps} ${app}"
    fi
}
survey_web_app web/explorer openapi-typescript "make web-generate-api (generated-artifact drift)"
survey_web_app web/status   tsc                "make status-typecheck"

# ── 5. Verdict on the survey ─────────────────────────────────────────────
echo ""
echo "── survey result ────────────────────────────────────────────────────"
printf '  blocking gaps:  %d\n' "${#blocking_missing[@]}"
printf '  clearance gaps: %d\n' "${#clearance_missing[@]}"
printf '  version drift:  %d\n' "${#version_drift[@]}"

if [ "$CHECK_ONLY" -eq 1 ]; then
    echo ""
    echo "--check: nothing installed."
    [ "${#blocking_missing[@]}" -eq 0 ] || exit 1
    exit 0
fi

# ── 6. Install ───────────────────────────────────────────────────────────
if [ "$install_go" -eq 1 ] || [ -n "$install_apps" ]; then
    echo ""
    echo "── install ──────────────────────────────────────────────────────────"
fi
if [ "$install_go" -eq 1 ]; then
    echo "  make deps (owns the pins)"
    if ! make deps; then
        echo "  make deps FAILED" >&2
    fi
fi
for app in $install_apps; do
    echo "  pnpm --dir ${app} install --frozen-lockfile"
    if ! command -v pnpm >/dev/null 2>&1; then
        echo "  pnpm is not installed, so ${app} cannot be bootstrapped here" >&2
        continue
    fi
    if ! pnpm --dir "$app" install --frozen-lockfile; then
        echo "  pnpm install FAILED for ${app}" >&2
    fi
done

# ── 7. Re-survey what was installed, and name what is left ───────────────
echo ""
echo "── after install ────────────────────────────────────────────────────"
remaining=0
for tool in gofumpt goimports golangci-lint; do
    if [ -x "${GO_BIN}/${tool}" ]; then ok "$tool" "${GO_BIN}/${tool}"
    else miss "$tool" "still absent — run 'make deps' and read its output"; remaining=$((remaining + 1)); fi
done
for app in $WEB_APPS; do
    [ -f "${app}/pnpm-lock.yaml" ] || continue
    case "$app" in
        web/explorer) bin=openapi-typescript ;;
        *)            bin=tsc ;;
    esac
    if [ -x "${app}/node_modules/.bin/${bin}" ]; then ok "$app" "node_modules/.bin/${bin} present"
    else miss "$app" "node_modules/.bin/${bin} still absent"; remaining=$((remaining + 1)); fi
done
for tool in git make go python3 tar; do
    command -v "$tool" >/dev/null 2>&1 || { miss "$tool" "not on PATH — install it before running verify.sh"; remaining=$((remaining + 1)); }
done

if [ "${#version_drift[@]}" -gt 0 ]; then
    echo ""
    echo "  version drift against the Makefile pins (not corrected here — ~/go/bin"
    echo "  is shared with every other checkout on this machine; 'make deps' makes"
    echo "  them all match):"
    for d in "${version_drift[@]}"; do echo "    ${d}"; done
fi

if [ "${#clearance_missing[@]}" -gt 0 ]; then
    echo ""
    echo "  not needed by verify.sh, required by 'make prepush' (doctor --profile native):"
    for t in "${clearance_missing[@]}"; do
        case "$t" in
            govulncheck) echo "    govulncheck        make deps" ;;
            node|npx)    echo "    ${t}$(printf '%*s' $((18 - ${#t})) '')brew install node" ;;
            pnpm)        echo "    pnpm               brew install pnpm" ;;
            jq)          echo "    jq                 brew install jq" ;;
            gitleaks)    echo "    gitleaks           brew install gitleaks" ;;
            promtool)    echo "    promtool           brew install prometheus" ;;
            amtool)      echo "    amtool             brew install alertmanager" ;;
            ansible-playbook) echo "    ansible-playbook   brew install ansible" ;;
            zizmor)      echo "    zizmor             brew install zizmor" ;;
            gh)          echo "    gh                 brew install gh" ;;
            *)           echo "    ${t}" ;;
        esac
    done
fi

echo ""
if [ "$remaining" -gt 0 ]; then
    echo "bootstrap-worktree: INCOMPLETE — ${remaining} blocking gap(s) remain; verify.sh will fail on them."
    exit 1
fi
echo "bootstrap-worktree: this worktree can run scripts/dev/verify.sh."
exit 0
