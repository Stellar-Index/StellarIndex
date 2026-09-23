#!/usr/bin/env bash
# preflight-deploy-baseline-test.sh — regression fixture for RLT-047 / #556(a):
# preflight-deploy.sh's config-apply baseline must be the lowest version
# across EVERY deployed-versions sidecar on the host, exactly as
# deploy.yml's "Capture the host's live version" step computes it —
# not just the binaries this region's manifest currently deploys.
#
# A binary excluded from a region's deployable set (unit disabled/masked)
# can still carry a sidecar from an earlier deploy. Scoping the baseline
# read to the manifest set drops that sidecar, computes a baseline that is
# too recent, and can miss a config-surface change the deploy.yml gate
# would still fail on — a false green in the tool that exists to predict
# that gate.
#
# This builds a throwaway fixture repo (its own git history, manifest,
# playbook and workflow stubs, and a fake `ssh`/`gh` on PATH) so the real
# preflight-deploy.sh runs unmodified against it.
#
# Run: bash scripts/dev/preflight-deploy-baseline-test.sh
set -uo pipefail

unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
      GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_COMMON_DIR

cd "$(dirname "$0")/../.." || exit 1
REAL_SCRIPT="$PWD/scripts/dev/preflight-deploy.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail=0
assert_contains() { # assert_contains <label> <haystack> <needle>
    if ! grep -qF -- "$3" <<<"$2"; then
        echo "FAIL: $1 — expected to find: $3" >&2
        echo "--- actual output ---" >&2
        printf '%s\n' "$2" >&2
        fail=1
    fi
}

build_fixture() {
    mkdir -p "$TMP"/scripts/dev "$TMP"/scripts/ci \
             "$TMP"/configs/ansible/playbooks "$TMP"/.github/workflows \
             "$TMP"/cmd/stellarindex-api "$TMP"/migrations "$TMP"/bin

    cp "$REAL_SCRIPT" "$TMP/scripts/dev/preflight-deploy.sh"
    chmod +x "$TMP/scripts/dev/preflight-deploy.sh"
    : > "$TMP/cmd/stellarindex-api/main.go"

    cat >"$TMP/configs/ansible/playbooks/deploy-binary.yml" <<'EOF'
cli_binaries:
  - stellarindex-migrate
EOF

    cat >"$TMP/.github/workflows/deploy.yml" <<'EOF'
        default: 'stellarindex-api'
EOF

    cat >"$TMP/scripts/ci/config-apply-gate.sh" <<'EOF'
#!/usr/bin/env bash
set -uo pipefail
SURFACES=(
    'configs/foo/bar.yml'
)
case "$1" in
    --payload)
        git diff "$2" "$3" -- "$4"
        ;;
    --ddl-objects)
        exit 1
        ;;
    *)
        ver="$1"; ack="$2"; base="$3"
        changed="$(git diff --name-only "$base" "$ver" -- "${SURFACES[@]}")"
        if [ -z "$changed" ]; then
            echo "no config-surface changes"
            exit 0
        fi
        if [ "$ack" = "true" ]; then
            echo "acknowledged"
            exit 0
        fi
        echo "::error::changed config surface(s) SUBSTANTIVELY: $changed"
        exit 1
        ;;
esac
EOF
    chmod +x "$TMP/scripts/ci/config-apply-gate.sh"

    printf 'testnet\ttestnet-host\tdeploy\t-\tstellarindex-api\t-\t2026-01-01T00:00:00Z\n' \
        > "$TMP/scripts/dev/region-binaries.tsv"

    # Fake `gh`: a readable release with assets, so section 1 is clean and
    # does not obscure the baseline assertion this fixture is about.
    cat >"$TMP/bin/gh" <<'EOF'
#!/usr/bin/env bash
if [ "$1" = release ] && [ "$2" = view ]; then echo 3; exit 0; fi
exit 1
EOF
    chmod +x "$TMP/bin/gh"

    # Fake `ssh`: routes on the remote command text.
    #   - "systemctl is-enabled"      -> section 2 unit-state derivation
    #   - "stellarindex-migrate) continue" -> the fixed baseline read (every
    #     sidecar on the host, migrate excluded) — v0.1.0 (aggregator,
    #     NOT in this region's manifest) is the oldest, v0.3.0 (api) is not.
    #   - anything else               -> the manifest-scoped sidecar read
    #     used for the section-2 display table (api only, at VERSION).
    cat >"$TMP/bin/ssh" <<'EOF'
#!/usr/bin/env bash
cmd="${*: -1}"
case "$cmd" in
    *"systemctl is-enabled"*)
        printf 'stellarindex-api\tservice\tenabled/active\n'
        ;;
    *"stellarindex-migrate) continue"*)
        printf 'v0.1.0\n'
        printf 'v0.3.0\n'
        ;;
    *)
        printf 'stellarindex-api\tv0.3.0\n'
        ;;
esac
exit 0
EOF
    chmod +x "$TMP/bin/ssh"

    git -C "$TMP" init -q
    git -C "$TMP" config user.email test@example.com
    git -C "$TMP" config user.name test
    git -C "$TMP" add -A
    git -C "$TMP" commit -q -m v0.1.0
    git -C "$TMP" tag v0.1.0

    mkdir -p "$TMP/configs/foo"
    echo "value: original" > "$TMP/configs/foo/bar.yml"
    git -C "$TMP" add configs/foo/bar.yml
    git -C "$TMP" commit -q -m v0.2.0
    git -C "$TMP" tag v0.2.0

    # v0.3.0 (the deploying tag) carries no further change to bar.yml — the
    # regression is that the drift was introduced in v0.1.0..v0.2.0 and
    # must still surface when diffing from a v0.1.0 baseline.
    git -C "$TMP" commit -q --allow-empty -m v0.3.0
    git -C "$TMP" tag v0.3.0
}

build_fixture

out="$(PATH="$TMP/bin:$PATH" "$TMP/scripts/dev/preflight-deploy.sh" --region testnet --version v0.3.0 2>&1)"
rc=$?

echo "  exit code: $rc"
assert_contains "true baseline is the oldest sidecar on the host (v0.1.0), not the manifest-scoped one (v0.3.0)" \
    "$out" "baseline: v0.1.0"
assert_contains "the config surface substantively changed between the true baseline and the deploying tag" \
    "$out" "configs/foo/bar.yml"
assert_contains "an outstanding decision is reported rather than a false green" \
    "$out" "DECISION NEEDED"
if [ "$rc" -eq 0 ]; then
    echo "FAIL: exit 0 — a false green: the config drift the gate would still fail on was not reported" >&2
    fail=1
fi

if [ "$fail" -eq 0 ]; then
    echo "preflight-deploy-baseline-test: PASS"
    exit 0
fi
echo "preflight-deploy-baseline-test: FAIL"
exit 1
