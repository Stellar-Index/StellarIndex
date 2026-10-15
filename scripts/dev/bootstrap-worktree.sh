#!/usr/bin/env bash
# bootstrap-worktree.sh — make a fresh checkout or linked worktree able to
# pass scripts/dev/verify.sh in one command, and report EVERY missing item in
# one pass rather than one per twelve-minute run.
#
# Why: verify.sh fails ~10 minutes in on a missing install in a section
# unrelated to the change (web/explorer with no node_modules:
# `openapi-typescript: command not found`). verify.sh gates a web app on its
# TRACKED pnpm-lock.yaml, which is always present; node_modules is not
# tracked. The survey runs to completion before the first install.
#
# Usage:
#   scripts/dev/bootstrap-worktree.sh          survey, then install what it can
#   scripts/dev/bootstrap-worktree.sh --check  survey only, install nothing
#   make bootstrap-worktree
#
# Severity and exit code:
#   BLOCKING    verify.sh HARD-FAILS without it. Exit is non-zero while any
#               is unresolved.
#   clearance   verify.sh defers the check and ends "VERIFY INCOMPLETE", but
#               scripts/dev/doctor.sh --profile native (run by `make prepush`
#               and cut-release.sh) requires it. Reported with its install
#               command; does not fail this script.
#
# Installed:
#   - Missing pinned Go tools via `make deps`, which owns the pins (no
#     version numbers here).
#   - Missing node_modules via the `pnpm --dir <app> install --frozen-lockfile`
#     that prepush.sh runs, for web/explorer.
#   - A tool at the WRONG version is reported, never corrected: ~/go/bin is
#     shared with every checkout on the machine, so a downgrade is the
#     operator's call; `make deps` makes them all match.
#
# ~/go/bin is not on every PATH. The Makefile calls gofumpt, goimports and
# golangci-lint through $(GOBIN) by absolute path, so this survey resolves
# them the same way; `command -v` would report a false miss.

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
WEB_APPS="web/explorer"

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
# BLOCKING set: verify.sh runs these unconditionally.
for tool in git make go python3 tar; do
    if path="$(command -v "$tool")"; then
        ok "$tool" "$path"
    else
        miss "$tool" "not on PATH"
        blocking_missing+=("$tool")
    fi
done

# ── 2. Go tools the Makefile resolves through GOBIN ──────────────────────
# make fmt and make lint call these by absolute path out of $(GOBIN), so a
# `command -v` survey reports a false miss where PATH omits ~/go/bin. Resolve
# them where the Makefile does and compare against its pins.
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
# Only these three. govulncheck is NOT here: `make vuln` and verify.sh resolve
# it with `command -v`, so a copy in ${GO_BIN} that PATH cannot see is still a
# deferred check; an absolute-path survey would report "ok" for it wrongly.
survey_go_tool gofumpt       GOFUMPT_VERSION       blocking
survey_go_tool goimports     GOIMPORTS_VERSION     blocking
survey_go_tool golangci-lint GOLANGCI_LINT_VERSION blocking

# ── 3. Tools verify.sh defers on, and doctor --profile native requires ───
# Absent, verify.sh prints "(deferred: …)" and ends VERIFY INCOMPLETE (exit 1,
# by design) while doctor's native profile requires each.
# `make prepush` and cut-release.sh both run, treats each as required.
for tool in node npx pnpm jq govulncheck gitleaks promtool amtool ansible-playbook zizmor gh; do
    if path="$(command -v "$tool")"; then
        ok "$tool" "$path"
    else
        miss "$tool" "not on PATH — verify.sh defers its check; make prepush refuses"
        clearance_missing+=("$tool")
    fi
done

# govulncheck's pin matters once PATH sees it: `make audit` reinstalls at
# GOVULNCHECK_VERSION, so a newer copy is drift.
if govulncheck_path="$(command -v govulncheck)"; then
    govulncheck_pin="$(go_tool_pin GOVULNCHECK_VERSION)"
    govulncheck_have="$(go_tool_installed "$govulncheck_path")"
    if [ -n "$govulncheck_pin" ] && [ -n "$govulncheck_have" ] && [ "$govulncheck_have" != "$govulncheck_pin" ]; then
        warn "govulncheck-pin" "${govulncheck_path} is ${govulncheck_have}, Makefile pins ${govulncheck_pin}"
        version_drift+=("govulncheck ${govulncheck_have} != ${govulncheck_pin}")
    fi
fi

# GNU tar. macOS ships bsdtar, so the deploy migrations-sync self-test is
# deferred by design. Recorded, never a gap.
if grep -q 'GNU tar' <<<"$(tar --version 2>/dev/null)"; then
    ok "gnu-tar" "$(command -v tar)"
else
    warn "gnu-tar" "bsdtar — the deploy migrations-sync self-test stays deferred (by design on macOS)"
fi

# ── 4. node_modules in BOTH web apps ─────────────────────────────────────
# Check for the executable each gate invokes, not the directory: a
# half-finished install leaves node_modules present and the binary absent.
# A present install can still be stale: verify.sh fails when pnpm-lock.yaml is
# newer than node_modules/.modules.yaml, and points here.
web_app_stale() { # web_app_stale <dir>
    [ -f "$1/node_modules/.modules.yaml" ] && [ "$1/pnpm-lock.yaml" -nt "$1/node_modules/.modules.yaml" ]
}
survey_web_app() { # survey_web_app <dir> <bin-name> <gate>
    app="$1"; bin="$2"; gate="$3"
    if [ ! -f "${app}/pnpm-lock.yaml" ]; then
        ok "${app}" "no pnpm-lock.yaml — verify.sh skips this app entirely"
        return
    fi
    if [ ! -x "${app}/node_modules/.bin/${bin}" ]; then
        miss "${app}" "node_modules/.bin/${bin} absent — ${gate} fails with '${bin}: command not found'"
    elif web_app_stale "$app"; then
        miss "${app}" "node_modules is STALE — pnpm-lock.yaml is newer than the last install; verify.sh fails on it"
    else
        ok "${app}" "node_modules/.bin/${bin} present"
        return
    fi
    blocking_missing+=("${app}/node_modules")
    install_apps="${install_apps} ${app}"
}
survey_web_app web/explorer openapi-typescript "make web-generate-api (generated-artifact drift)"

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
    if [ ! -x "${app}/node_modules/.bin/${bin}" ]; then
        miss "$app" "node_modules/.bin/${bin} still absent"; remaining=$((remaining + 1))
    elif web_app_stale "$app"; then
        miss "$app" "node_modules still older than pnpm-lock.yaml"; remaining=$((remaining + 1))
    else ok "$app" "node_modules/.bin/${bin} present"; fi
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
