#!/usr/bin/env bash
# preflight-deploy-gh556-test.sh — regression fixture for GH-556(b)/(c)/(d):
#
#   (b) `gh` missing must be a DECISION NEEDED (exit 1), not a soft note
#       that leaves the script exit 0 with the release unverified.
#   (c) a region with no manifest row must not name --refresh-manifest as
#       the remedy: it rewrites an existing row and cannot add one.
#   (d) configs/prometheus/rules.r1/ is applied automatically ONLY on r1
#       (deploy.yml's "Apply Prometheus rules (r1)" step is
#       `if: inputs.region == 'r1'`). Elsewhere it must stay SUBSTANTIVE.
#
# This builds a throwaway fixture repo (its own git history, manifest,
# playbook/workflow stubs, and a fake `ssh`/`gh` on PATH) so the real
# preflight-deploy.sh and the real config-apply-gate.sh run unmodified
# against it.
#
# Run: bash scripts/dev/preflight-deploy-gh556-test.sh
set -uo pipefail

unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
      GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_COMMON_DIR

cd "$(dirname "$0")/../.." || exit 1
REAL_SCRIPT="$PWD/scripts/dev/preflight-deploy.sh"
REAL_GATE="$PWD/scripts/ci/config-apply-gate.sh"

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
assert_not_contains() { # assert_not_contains <label> <haystack> <needle>
    if grep -qF -- "$3" <<<"$2"; then
        echo "FAIL: $1 — expected NOT to find: $3" >&2
        echo "--- actual output ---" >&2
        printf '%s\n' "$2" >&2
        fail=1
    fi
}

build_fixture() {
    mkdir -p "$TMP"/scripts/dev "$TMP"/scripts/ci \
             "$TMP"/configs/ansible/playbooks "$TMP"/.github/workflows \
             "$TMP"/configs/prometheus/rules.r1 \
             "$TMP"/cmd/stellarindex-api "$TMP"/migrations "$TMP"/bin

    cp "$REAL_SCRIPT" "$TMP/scripts/dev/preflight-deploy.sh"
    chmod +x "$TMP/scripts/dev/preflight-deploy.sh"
    cp "$REAL_GATE" "$TMP/scripts/ci/config-apply-gate.sh"
    chmod +x "$TMP/scripts/ci/config-apply-gate.sh"
    : > "$TMP/cmd/stellarindex-api/main.go"

    cat >"$TMP/configs/ansible/playbooks/deploy-binary.yml" <<'EOF'
cli_binaries:
  - stellarindex-migrate
EOF

    cat >"$TMP/.github/workflows/deploy.yml" <<'EOF'
        default: 'stellarindex-api'
EOF

    printf 'r1\tr1-host\tdeploy\t-\tstellarindex-api\t-\t2026-01-01T00:00:00Z\n' \
        > "$TMP/scripts/dev/region-binaries.tsv"
    printf 'testnet\ttestnet-host\tdeploy\t-\tstellarindex-api\t-\t2026-01-01T00:00:00Z\n' \
        >> "$TMP/scripts/dev/region-binaries.tsv"

    echo "groups: []" > "$TMP/configs/prometheus/rules.r1/alerts.yml"

    git -C "$TMP" init -q
    git -C "$TMP" config user.email test@example.com
    git -C "$TMP" config user.name test
    git -C "$TMP" add -A
    git -C "$TMP" commit -q -m v0.1.0
    git -C "$TMP" tag v0.1.0

    # v0.2.0: a SUBSTANTIVE (non-comment) change to the r1 rules overlay.
    cat >"$TMP/configs/prometheus/rules.r1/alerts.yml" <<'EOF'
groups:
  - name: example
    rules:
      - alert: Example
        expr: up == 0
EOF
    git -C "$TMP" -C . add configs/prometheus/rules.r1/alerts.yml
    git -C "$TMP" commit -q -m v0.2.0
    git -C "$TMP" tag v0.2.0
}

# Fake `ssh`: unit enabled/active, so section 2 does not obscure the
# assertions below. --no-host is not used because the sidecar/baseline
# read runs the same for both regions and this keeps that path exercised.
write_fake_ssh() {
    cat >"$TMP/bin/ssh" <<'EOF'
#!/usr/bin/env bash
cmd="${*: -1}"
case "$cmd" in
    *"systemctl is-enabled"*)
        printf 'stellarindex-api\tservice\tenabled/active\n'
        ;;
    *)
        printf 'stellarindex-api\tv0.1.0\n'
        ;;
esac
exit 0
EOF
    chmod +x "$TMP/bin/ssh"
}

build_fixture
write_fake_ssh

# ── test 1: gh absent is a DECISION NEEDED, not a soft note ──────────────
# PATH deliberately excludes any `gh`.
out1="$(PATH="$TMP/bin:/usr/bin:/bin" "$TMP/scripts/dev/preflight-deploy.sh" \
    --region testnet --version v0.2.0 --no-host 2>&1)"
rc1=$?
echo "  test 1 (gh absent) exit code: $rc1"
assert_contains "gh-absent line is exact" "$out1" "DECISION NEEDED: gh is not installed"
if [ "$rc1" -eq 0 ]; then
    echo "FAIL: gh absent must block (exit 1), not report ready" >&2
    fail=1
fi

# ── test 2: r1 auto-applies rules.r1/ ────────────────────────────────────
cat >"$TMP/bin/gh" <<'EOF'
#!/usr/bin/env bash
if [ "$1" = release ] && [ "$2" = view ]; then echo 3; exit 0; fi
exit 1
EOF
chmod +x "$TMP/bin/gh"

out2="$(PATH="$TMP/bin:/usr/bin:/bin" "$TMP/scripts/dev/preflight-deploy.sh" \
    --region r1 --version v0.2.0 --no-host 2>&1)"
rc2=$?
echo "  test 2 (r1 auto-applied) exit code: $rc2"
assert_contains "rules.r1/ is auto-applied on r1" "$out2" "auto-applied"
assert_contains "the applied file is named" "$out2" "configs/prometheus/rules.r1/alerts.yml"
assert_not_contains "r1's auto-applied surface is not left SUBSTANTIVE" "$out2" "SUBSTANTIVE"
if [ "$rc2" -ne 0 ]; then
    echo "FAIL: r1 with the surface auto-applied must not report a decision outstanding (exit $rc2)" >&2
    echo "$out2" >&2
    fail=1
fi

# ── test 3: testnet gets no such exemption ───────────────────────────────
out3="$(PATH="$TMP/bin:/usr/bin:/bin" "$TMP/scripts/dev/preflight-deploy.sh" \
    --region testnet --version v0.2.0 --no-host 2>&1)"
rc3=$?
echo "  test 3 (testnet substantive) exit code: $rc3"
assert_contains "rules.r1/ stays SUBSTANTIVE off r1" "$out3" "SUBSTANTIVE"
assert_not_contains "testnet has no dispatch step that applies rules.r1/" "$out3" "auto-applied"
if [ "$rc3" -eq 0 ]; then
    echo "FAIL: testnet must report the rules.r1/ change as an outstanding decision (exit $rc3)" >&2
    echo "$out3" >&2
    fail=1
fi

# ── test 4: a missing row is not remedied by --refresh-manifest ──────────
# The fixture has no futurenet row. The refresh only rewrites an existing
# row, so the refusal must point at the hand-added row, not at itself.
out4="$(PATH="$TMP/bin:/usr/bin:/bin" "$TMP/scripts/dev/preflight-deploy.sh" \
    --region futurenet --version v0.2.0 --refresh-manifest 2>&1)"
rc4=$?
echo "  test 4 (missing row + --refresh-manifest) exit code: $rc4"
assert_contains "a missing row is added by hand" "$out4" "add one by hand"
assert_not_contains "--refresh-manifest is not offered as the whole remedy" "$out4" "re-derive it with --refresh-manifest"
if [ "$rc4" -ne 2 ]; then
    echo "FAIL: a region with no manifest row must exit 2 (got $rc4)" >&2
    fail=1
fi

if [ "$fail" -eq 0 ]; then
    echo "preflight-deploy-gh556-test: PASS"
    exit 0
fi
echo "preflight-deploy-gh556-test: FAIL"
exit 1
