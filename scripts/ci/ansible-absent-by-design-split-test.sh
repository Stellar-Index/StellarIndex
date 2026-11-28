#!/usr/bin/env bash
# ansible-absent-by-design-split-test.sh — pins how one inventory list,
# stellarindex_sources_absent_by_design, reaches its two consumers:
#   - "<domain>/<source>" names only into data-freshness.service, whose
#     script rejects any name it does not know (a bare completeness name
#     there would void the whole freshness declaration);
#   - bare names only into every compute-completeness*.service, as
#     COMPLETENESS_ABSENT_BY_DESIGN (run-compute-completeness.sh passes it as
#     -absent-by-design). A unit missing it would rewrite a declared verdict red.
#
# Renders the real templates with a local ansible-playbook run; no hosts.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROLE="$PWD/configs/ansible/roles/archival-node"
UNITS="compute-completeness compute-completeness-sdex compute-completeness-sep41"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

if ! command -v ansible-playbook >/dev/null; then
  echo "ansible-absent-by-design-split-test: FAIL — ansible-playbook not on PATH (this test must not pass vacuously)" >&2
  exit 1
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

render() { # $1 = extra-vars JSON, $2 = output dir
  mkdir -p "$2"
  {
    printf -- '---\n- hosts: localhost\n  connection: local\n  gather_facts: false\n'
    printf '  vars_files:\n    - "%s/defaults/main.yml"\n  tasks:\n' "$ROLE"
    for u in data-freshness $UNITS; do
      printf '    - ansible.builtin.template:\n        src: "%s/templates/systemd/%s.service.j2"\n        dest: "%s/%s.service"\n        mode: "0644"\n' \
        "$ROLE" "$u" "$2" "$u"
    done
  } > "$TMP/fixture.yml"
  (cd "$TMP" && ANSIBLE_LOCALHOST_WARNING=false ANSIBLE_INVENTORY_UNPARSED_WARNING=false \
    ansible-playbook -i localhost, fixture.yml -e region_id=ci -e "$1" 2>&1)
}

check() { # $1 = label, $2 = dir, $3 = want freshness value, $4 = want completeness value
  local got
  got="$(grep '^Environment="DATA_FRESHNESS_ABSENT_BY_DESIGN=' "$2/data-freshness.service")"
  if [ "$got" = "Environment=\"DATA_FRESHNESS_ABSENT_BY_DESIGN=$3\"" ]; then
    ok "$1: data-freshness.service gets only the domain/source names ('$3')"
  else
    bad "$1: data-freshness.service rendered: $got"
  fi
  for u in $UNITS; do
    got="$(grep '^Environment="COMPLETENESS_ABSENT_BY_DESIGN=' "$2/$u.service")"
    if [ "$got" = "Environment=\"COMPLETENESS_ABSENT_BY_DESIGN=$4\"" ]; then
      ok "$1: $u.service gets only the bare completeness names ('$4')"
    else
      bad "$1: $u.service rendered: ${got:-no COMPLETENESS_ABSENT_BY_DESIGN line}"
    fi
  done
}

if out="$(render '{}' "$TMP/default")"; then
  check "role default" "$TMP/default" "" ""
else
  bad "rendering with the role default failed: $(tail -5 <<<"$out")"
fi
if out="$(render '{"stellarindex_sources_absent_by_design": ["sep1/issuers", "sdex", "sep41_supply/supply_flows", "soroswap"]}' "$TMP/mixed")"; then
  check "mixed list" "$TMP/mixed" "sep1/issuers sep41_supply/supply_flows" "sdex,soroswap"
else
  bad "rendering a mixed list failed: $(tail -5 <<<"$out")"
fi

# shellcheck disable=SC2016  # the literal "$COMPLETENESS_ABSENT_BY_DESIGN" in the script is what is matched
if grep -q -- '-absent-by-design "\$COMPLETENESS_ABSENT_BY_DESIGN"' "$ROLE/files/run-compute-completeness.sh"; then
  ok "run-compute-completeness.sh passes COMPLETENESS_ABSENT_BY_DESIGN as -absent-by-design"
else
  bad "run-compute-completeness.sh does not pass COMPLETENESS_ABSENT_BY_DESIGN to compute-completeness"
fi

echo "ansible-absent-by-design-split-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
