#!/usr/bin/env bash
set -euo pipefail

cd /workspace
git config --global --add safe.directory /workspace
git config --global --add safe.directory /history
export ANSIBLE_LOCAL_TEMP=/tmp/ansible-local
export ANSIBLE_REMOTE_TEMP=/tmp/ansible-remote
BASE_SHA="$(git rev-parse HEAD)"
export BASE_SHA
export VERIFY_FAIL_ON_SKIP=1

echo "=== Pinned verifier toolchain ==="
printf 'go=%s node=%s pnpm=%s golangci-lint=%s\n' \
  "$(go version | awk '{print $3}')" "$(node --version)" "$(pnpm --version)" "$(golangci-lint version --short 2>/dev/null || golangci-lint version | head -1)"

echo "=== Install locked frontend dependencies ==="
# The store location arrives as npm_config_store_dir from verify-container.sh.
# Both node_modules trees live in named volumes that outlive the container, so
# a store path that differs from the one recorded in an existing tree makes
# pnpm 10 ask before purging it, and with no TTY that question aborts the run
# with ERR_PNPM_ABORTED_REMOVE_MODULES_DIR_NO_TTY. Answer it up front.
pnpm config set confirm-modules-purge false
for app in web/explorer web/status; do
  [[ -f "$app/pnpm-lock.yaml" ]] || continue
  pnpm --dir "$app" install --frozen-lockfile
done

echo "=== Workflow security ==="
zizmor --min-confidence medium .github/workflows/

echo "=== Secrets (real committed history) ==="
gitleaks detect --source /history --no-banner --redact --config /workspace/.gitleaks.toml

make verify

if ! git diff --exit-code --stat; then
  echo "verify-container: FAIL: a formatter or generator changed committed HEAD" >&2
  exit 1
fi

echo "verify-container: ALL LINUX CHECKS PASSED"
