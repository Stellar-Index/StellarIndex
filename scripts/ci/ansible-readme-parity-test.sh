#!/usr/bin/env bash
# ansible-readme-parity-test.sh — pins configs/ansible/README.md to the
# archival-node role it documents.
#
# Q252: the README's first-run bootstrap prescribed a --tags subset that
# skipped `users` (the account the galexie tasks chown to), redis,
# clickhouse, minio, the binaries, hardening and caddy, and it documented
# a `run_stellar_rpc` switch whose tasks had been deleted. Prose drifts
# from the role silently; this check makes that drift fail:
#   1. every tag the README passes to --tags/--skip-tags exists in the role;
#   2. the first-run bootstrap selects every tag tasks/main.yml imports
#      (plainest form: no --tags at all);
#   3. every run_* switch the README names is defined in defaults/main.yml
#      or read by the role's tasks.
# Arms B-E run the same checker against doctored READMEs, so a checker that
# stopped matching anything would fail here rather than pass vacuously.
#
# Run: bash scripts/ci/ansible-readme-parity-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1

README="configs/ansible/README.md"
ROLE="configs/ansible/roles/archival-node"

pass=0
fail=0

# Tags declared anywhere in the role, plus Ansible's special tags.
role_tags() {
  grep -hoE '^[[:space:]]*tags:[[:space:]]*\[[^]]*\]' "$ROLE"/tasks/*.yml "$ROLE"/handlers/*.yml |
    sed -E 's/.*\[([^]]*)\].*/\1/' | tr ',' '\n' | tr -d ' ' | grep -v '^$'
  printf '%s\n' all always never tagged untagged
}

# Tags on tasks/main.yml's own imports: the set a full bootstrap must run.
bootstrap_tags() {
  grep -hoE '^[[:space:]]*tags:[[:space:]]*\[[^]]*\]' "$ROLE/tasks/main.yml" |
    sed -E 's/.*\[([^]]*)\].*/\1/' | tr ',' '\n' | tr -d ' ' | grep -v -e '^$' -e '^always$' | sort -u
}

# Every archival-node.yml invocation in <readme>, backslash continuations
# joined, one per line, prefixed with the `## ` section it appears under.
invocations() {
  awk '
    /^## / { section = $0 }
    {
      line = $0
      if (cont != "") { line = cont " " line; cont = "" }
      if (line ~ /\\$/) { sub(/\\$/, "", line); cont = line; next }
      if (line ~ /ansible-playbook/ && line ~ /playbooks\/archival-node\.yml/) {
        print section "\t" line
      }
    }
  ' "$1"
}

# option_values <invocation> <flag> — comma-split values of --tags/--skip-tags.
option_values() {
  grep -oE -- "$2(=|[[:space:]]+)[^[:space:]]+" <<<"$1" |
    sed -E "s/^$2(=|[[:space:]]+)//" | tr ',' '\n' | grep -v '^$'
}

# check_readme <readme> — prints one line per violation; returns the count.
check_readme() {
  local readme="$1" n=0 known wanted section inv tag flag missing
  known="$(role_tags | sort -u)"
  wanted="$(bootstrap_tags)"

  while IFS=$'\t' read -r section inv; do
    [ -z "$inv" ] && continue
    for flag in --tags --skip-tags; do
      while IFS= read -r tag; do
        [ -z "$tag" ] && continue
        if ! grep -qxF -- "$tag" <<<"$known"; then
          echo "unknown tag '$tag' ($flag) in: $inv"
          n=$((n + 1))
        fi
      done < <(option_values "$inv" "$flag")
    done
    if [ "$section" = "## First-run bootstrap" ] && grep -qE -- '--tags' <<<"$inv"; then
      missing="$(comm -23 <(printf '%s\n' "$wanted") <(option_values "$inv" --tags | sort -u) | tr '\n' ' ')"
      if [ -n "$missing" ]; then
        echo "first-run bootstrap skips role tags: ${missing% }"
        n=$((n + 1))
      fi
    fi
  done < <(invocations "$readme")

  while IFS= read -r flag; do
    if ! grep -qE "^${flag}:" "$ROLE/defaults/main.yml" &&
      ! grep -rqw -- "$flag" "$ROLE/tasks"; then
      echo "README names $flag, which the role neither defines nor reads"
      n=$((n + 1))
    fi
  done < <(grep -oE '\brun_[a-z_]+' "$readme" | sort -u)

  return "$n"
}

# expect <label> <readme> <want: clean|dirty> [violation-substring]
expect() {
  local label="$1" readme="$2" want="$3" needle="${4:-}" out rc
  out="$(check_readme "$readme")"
  rc=$?
  if [ "$want" = clean ] && [ "$rc" -eq 0 ]; then
    echo "  ok   $label"
    pass=$((pass + 1))
  elif [ "$want" = dirty ] && [ "$rc" -gt 0 ] && grep -qF -- "$needle" <<<"$out"; then
    echo "  ok   $label"
    pass=$((pass + 1))
  else
    echo "  FAIL $label (want $want, got $rc violation(s)):" >&2
    printf '%s\n' "$out" | sed 's/^/       | /' >&2
    fail=$((fail + 1))
  fi
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

if [ "$(invocations "$README" | grep -c '^## First-run bootstrap')" -eq 0 ]; then
  echo "  FAIL no archival-node.yml invocation found under '## First-run bootstrap' in $README" >&2
  fail=$((fail + 1))
fi
expect "A: $README matches the archival-node role" "$README" clean

printf '## First-run bootstrap\n\nansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \\\n  --tags preflight,kernel,zfs,postgres,galexie,firewall,monitoring\n' >"$tmp/subset.md"
expect "B: a bootstrap tag subset is caught" "$tmp/subset.md" dirty "skips role tags: "

printf '## Running a subset\n\nansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --tags galexie --skip-tags restart\n' >"$tmp/unknown.md"
expect "C: a tag the role does not declare is caught" "$tmp/unknown.md" dirty "unknown tag 'restart'"

printf 'set run_stellar_core: true (and optionally run_stellar_rpc: true)\n' >"$tmp/flag.md"
expect "D: a run_* switch the role does not have is caught" "$tmp/flag.md" dirty "run_stellar_rpc"

printf '## First-run bootstrap\n\nansible-playbook -i inventory/r1.yml playbooks/archival-node.yml\n' >"$tmp/full.md"
expect "E: a plain full-role bootstrap is clean" "$tmp/full.md" clean

echo "ansible-readme-parity-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
