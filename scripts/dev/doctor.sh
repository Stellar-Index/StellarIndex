#!/usr/bin/env bash
# Report whether this machine can run each local verification profile.
# Capability is explicit: a weaker machine gets a useful check, but never a
# misleading claim that CI-equivalent checks ran.
set -euo pipefail

cd "$(dirname "$0")/../.."

profile="${VERIFY_PROFILE:-auto}"
if [[ "${1:-}" == "--profile" ]]; then
  profile="${2:-}"
elif [[ $# -ne 0 ]]; then
  echo "usage: $0 [--profile portable|native|container|full|auto]" >&2
  exit 2
fi

case "$profile" in
  portable|native|container|full|auto) ;;
  *) echo "doctor: unknown profile '$profile'" >&2; exit 2 ;;
esac

failures=0
warnings=0

ok() { printf '  ok    %-18s %s\n' "$1" "$2"; }
bad() { printf '  FAIL  %-18s %s\n' "$1" "$2" >&2; failures=$((failures + 1)); }
warn() { printf '  warn  %-18s %s\n' "$1" "$2"; warnings=$((warnings + 1)); }

need() {
  local name="$1"
  if command -v "$name" >/dev/null 2>&1; then
    ok "$name" "$(command -v "$name")"
  else
    bad "$name" "not found"
  fi
}

docker_ready() {
  command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1
}

if [[ "$profile" == "auto" ]]; then
  if docker_ready; then
    profile="container"
  else
    profile="native"
  fi
  echo "doctor: auto selected profile=$profile"
fi

echo "Local verification doctor (profile=$profile)"
for tool in git bash make go; do need "$tool"; done

if [[ "$profile" == "portable" || "$profile" == "native" ]]; then
  need python3
  go_bin="$(go env GOPATH 2>/dev/null)/bin"
  for tool in gofumpt goimports golangci-lint; do
    if [[ -x "$go_bin/$tool" ]]; then
      ok "$tool" "$go_bin/$tool"
    else
      bad "$tool" "missing from $go_bin; run make deps"
    fi
  done
fi

if [[ "$profile" == "native" ]]; then
  for tool in govulncheck gitleaks node npx pnpm jq promtool amtool ansible-playbook zizmor; do
    need "$tool"
  done
  node_major="$(node --version 2>/dev/null | sed -E 's/^v([0-9]+).*/\1/' || true)"
  if [[ "$node_major" == "22" ]]; then ok "node-version" "major 22"; else warn "node-version" "CI uses major 22; found ${node_major:-unknown}"; fi
  pnpm_version="$(pnpm --version 2>/dev/null || true)"
  if [[ "$pnpm_version" == "10.33.0" ]]; then ok "pnpm-version" "$pnpm_version"; else warn "pnpm-version" "want 10.33.0; found ${pnpm_version:-unknown}"; fi
fi

if [[ "$profile" == "container" || "$profile" == "full" ]]; then
  if docker_ready; then
    ok docker "daemon reachable ($(docker context show 2>/dev/null || echo default))"
  else
    bad docker "CLI or daemon unavailable"
  fi
fi

if [[ "$profile" == "portable" ]]; then
  warn scope "portable checks are useful feedback, not pre-push clearance"
fi

if (( failures > 0 )); then
  echo "doctor: FAIL ($failures missing requirement(s), $warnings warning(s))" >&2
  exit 1
fi
echo "doctor: PASS profile=$profile ($warnings warning(s))"
