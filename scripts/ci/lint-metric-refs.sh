#!/usr/bin/env bash
# lint-metric-refs.sh - guard against the "dead alert layer".
#
# Every `stellarindex_*` metric token in a Prometheus rule `expr:` must resolve to an emitter: a
# `Name:` field in internal/obs or a literal in internal/ cmd/ scripts/ configs (textfile .prom
# emitters included). A rule selecting a metric nothing produces can never fire. Every `job=` in
# an expr must also name a job_name in that tree's scrape config.
#
# Only the alert->producer direction is enforced. The reverse (emitted but never alerted) is an
# advisory pass: most metrics are dashboard-only, and a hard fail would need an exception list as
# large as the registry.
#
# Scope:
#   - Only `stellarindex_*` is enforced; node_/pg_/redis_/pgbackrest_ come from third-party
#     exporters not in this repo (see EXTERNAL_OK, documentation only).
#   - Tokens only inside a `job=` matcher are scrape-job names, not metrics.
#   - Recording rules use the `stellarindex:foo:5m` colon style, which the token regex skips.
#   - KNOWN_INERT lists references kept on purpose that cannot fire (no producer yet). Add a
#     producer = remove the entry; inventing a name with no producer is not allowed.
#
# Exit 1 if any dead reference remains, else 0.

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root" || exit 1

RULE_DIRS=(
  "deploy/monitoring/rules"
  "configs/prometheus/rules.r1"
)

# Scrape config each rule tree deploys beside (index-aligned with RULE_DIRS). The trees use
# different job names, and a selector over an undefined job matches nothing.
SCRAPE_CONFIGS=(
  "configs/ansible/roles/prometheus/templates/prometheus.yml.j2"
  "configs/prometheus/prometheus.r1.yml"
)

# Directories scanned for emitters. apply.sh is named as a file so its self-test cannot stand in
# for an emitter.
EMITTER_PATHS=(internal cmd scripts configs/healthchecks configs/ansible/roles/archival-node/files configs/alertmanager/apply.sh)

# Emitters that live as inline `content:` blocks in task YAML, invisible to the .go/.sh/.prom grep.
# Scoped to tasks/handlers: scanning all of configs/ would let a rule file satisfy its own
# reference.
ANSIBLE_TASK_PATHS=(
  configs/ansible/roles/archival-node/tasks
  configs/ansible/roles/archival-node/handlers
)

# Deliberately-inert references: kept in the rule files (with an INERT comment) but with no
# producer. Keep in lockstep with those comment blocks.
KNOWN_INERT=(
  # stellar.yml — no archive-publish-error counter is wired to Prometheus.
  stellarindex_stellar_archive_publish_errors_total
  # stellar.yml - stellar-core / stellar-rpc metrics come from an exporter not deployed on r1.
  stellarindex_stellar_core_last_ledger_time_unix
  stellarindex_stellar_core_peer_count
  stellarindex_stellar_rpc_latest_ledger_age_seconds
)

# Third-party exporter metrics referenced by exprs; documentation only (not enforced).
# shellcheck disable=SC2034 # documentation-only array, not read by the script
EXTERNAL_OK=(
  pg_up pg_replication_lag_seconds pg_locks_count pg_settings_max_locks_per_transaction
  pg_settings_max_connections pg_stat_activity_count
  redis_up redis_connected_slaves redis_memory_used_bytes redis_memory_max_bytes
  redis_evicted_keys_total redis_rdb_last_bgsave_status
  pgbackrest_backup_since_last_completion_seconds
  pgbackrest_backup_info
  node_zfs_zpool_state node_filesystem_avail_bytes node_filesystem_size_bytes
  node_cpu_seconds_total node_memory_MemTotal_bytes node_memory_MemAvailable_bytes
  node_vmstat_pswpout
  # The textfile collector publishes each file's mtime; the stack probe writes no run stamp, so this
  # is the only thing that ages when its timer stops.
  node_textfile_mtime_seconds
  # Packaged nvme collector textfile (nvme.prom), one series per device.
  nvme_temperature_celsius nvme_num_err_log_entries_total
  # clickhouse-server's own Prometheus endpoint (:9363); system.events is ClickHouseProfileEvents_<Name>.
  ClickHouseProfileEvents_FailedQuery ClickHouseProfileEvents_RejectedInserts
  up
)

in_list() {
  local needle="$1"; shift
  local x
  for x in "$@"; do [[ "$x" == "$needle" ]] && return 0; done
  return 1
}

# Print the body of every rule `expr:` region, `#` comments removed. A region ends at the next
# sibling key (for/labels/annotations/- alert/- record).
expr_lines() {
  awk '
    function strip_hash(s) {
      # drop `# ...` trailing YAML/PromQL comments inside the expr region
      # so a metric NAMED ONLY in a comment (e.g. `# TODO wire
      # stellarindex_foo`) is not enforced as a live reference. PromQL has
      # no `#` operator, so this never truncates a real selector.
      sub(/#.*/, "", s)
      return s
    }
    # A block scalar may be literal (`|`) or folded (`>`), with an optional chomping indicator.
    /^[[:space:]]*expr:[[:space:]]*([|>][-+]?)?[[:space:]]*$/ { inexpr=1; next }
    /^[[:space:]]*expr:/ { print strip_hash(substr($0, index($0,"expr:")+5)); next }
    inexpr {
      if ($0 ~ /^[[:space:]]*(for|labels|annotations):/ || $0 ~ /^[[:space:]]*-[[:space:]]+(alert|record):/) { inexpr=0; next }
      print strip_hash($0)
    }
  ' "$1"
}

# Pull stellarindex_* tokens from `expr:` regions, with job= matcher bodies removed first.
extract_expr_tokens() {
  expr_lines "$1" \
    | sed -E 's/job[[:space:]]*=~?[[:space:]]*"[^"]*"//g' \
    | grep -oE 'stellarindex_[a-zA-Z0-9_]+' || true
}

# Print every scrape-job name a rule file's exprs select. Only positive matchers count; a regex
# alternative with metacharacters cannot be resolved statically and is skipped.
extract_expr_jobs() {
  expr_lines "$1" \
    | grep -oE '(^|[^a-zA-Z0-9_])job[[:space:]]*=~?[[:space:]]*"[^"]*"' \
    | sed -E 's/^[^"]*"//; s/"$//' \
    | tr '|' '\n' \
    | grep -E '^[A-Za-z0-9_:-]+$' || true
}

# Print every job_name a scrape config defines (a job under a jinja `{% if %}` still counts).
scrape_job_names() {
  sed -nE 's/^[[:space:]]*-?[[:space:]]*job_name:[[:space:]]*"?([A-Za-z0-9_.-]+)"?.*/\1/p' "$1"
}

self_rel="scripts/ci/lint-metric-refs.sh"

# Emit an emitter file with comments blanked, so a metric named only in a comment (`// TODO`,
# `# HELP`) does not count as emitted. Go is scanned with string and rune literals kept whole, so
# `//` inside "https://..." opens no comment.
strip_comments() {
  case "$1" in
    *.go)        perl -0pe 's{("(?:\\.|[^"\\\n])*"|`[^`]*`|\x27(?:\\.|[^\x27\\\n])*\x27)|//[^\n]*|/\*.*?\*/}{defined $1 ? $1 : " "}gse' "$1" ;;
    *.sh|*.prom) sed -E 's:#.*$::' "$1" ;;
    *)           cat "$1" ;;
  esac
}

is_emitted() {
  # True if the token is a literal on a non-comment line of an emitter file. Excludes this script
  # (its KNOWN_INERT array would self-match); narrow with grep -rlF first, then strip comments.
  local tok="$1" f
  while IFS= read -r f; do
    [[ "$f" == "$self_rel" ]] && continue
    grep -qF -e "$tok" <(strip_comments "$f") && return 0
  done < <(grep -rlF --include="*.go" --include="*.sh" --include="*.prom" -e "$tok" "${EMITTER_PATHS[@]}" 2>/dev/null)

  # Inline ansible probes: `#` is the comment marker in the embedded shell bodies too.
  while IFS= read -r f; do
    [[ "$f" == "$self_rel" ]] && continue
    grep -qF -e "$tok" <(sed -E 's:#.*$::' "$f") && return 0
  done < <(grep -rlF --include="*.yml" --include="*.yaml" -e "$tok" "${ANSIBLE_TASK_PATHS[@]}" 2>/dev/null)
  return 1
}

dead=0

for dir in "${RULE_DIRS[@]}"; do
  [[ -d "$dir" ]] || { echo "lint-metric-refs: missing rule dir: $dir" >&2; exit 2; }
  while IFS= read -r -d '' f; do
    # Unique tokens for this file.
    while IFS= read -r tok; do
      [[ -z "$tok" ]] && continue
      if in_list "$tok" "${KNOWN_INERT[@]}"; then continue; fi
      if is_emitted "$tok"; then continue; fi
      echo "DEAD-REF: $f references '$tok' but nothing emits it (not in KNOWN_INERT)."
      dead=$((dead + 1))
    done < <(extract_expr_tokens "$f" | sort -u)
  done < <(find "$dir" -maxdepth 1 -name '*.yml' -print0)
done

for i in "${!RULE_DIRS[@]}"; do
  dir="${RULE_DIRS[$i]}"
  cfg="${SCRAPE_CONFIGS[$i]}"
  [[ -f "$cfg" ]] || { echo "lint-metric-refs: missing scrape config: $cfg" >&2; exit 2; }
  defined_jobs=" $(scrape_job_names "$cfg" | tr '\n' ' ')"
  while IFS= read -r -d '' f; do
    while IFS= read -r job; do
      [[ -z "$job" || "$defined_jobs" == *" $job "* ]] && continue
      echo "DEAD-JOB: $f selects job=\"$job\" but $cfg defines no such scrape job."
      dead=$((dead + 1))
    done < <(extract_expr_jobs "$f" | sort -u)
  done < <(find "$dir" -maxdepth 1 -name '*.yml' -print0)
done

# Reverse guard: a KNOWN_INERT entry that gained a producer must be promoted to a live ref.
for tok in "${KNOWN_INERT[@]}"; do
  if is_emitted "$tok"; then
    echo "STALE-INERT: '$tok' is in KNOWN_INERT but now HAS an emitter — drop it from the list and use the live metric." >&2
    dead=$((dead + 1))
  fi
done

# Metrics deliberately emitted with no alert; drop an entry when it gains one.
UNALERTED_OK=(
)

# Producer -> alert direction (advisory, does not affect exit code). The alerted set is built
# once, not per emitted token (re-parsing took 66s+). A newline-joined string, not `declare -A`,
# which bash 3.2 on macOS rejects.
alerted_tokens="$(
  for dir in "${RULE_DIRS[@]}"; do
    [[ -d "$dir" ]] || continue
    while IFS= read -r -d '' f; do
      extract_expr_tokens "$f"
    done < <(find "$dir" -maxdepth 1 -name '*.yml' -print0)
  done | sort -u
)"

is_alerted() {
  grep -qxF -e "$1" <<<"$alerted_tokens"
}

# Every stellarindex_* literal on a non-comment line of an emitter file or inline ansible probe.
collect_emitted_tokens() {
  local f
  while IFS= read -r f; do
    [[ "$f" == "$self_rel" ]] && continue
    strip_comments "$f" | grep -oE 'stellarindex_[a-zA-Z0-9_]+'
  done < <(grep -rlE --include="*.go" --include="*.sh" --include="*.prom" \
    'stellarindex_' "${EMITTER_PATHS[@]}" 2>/dev/null)
  while IFS= read -r f; do
    [[ "$f" == "$self_rel" ]] && continue
    sed -E 's:#.*$::' "$f" | grep -oE 'stellarindex_[a-zA-Z0-9_]+'
  done < <(grep -rlE --include="*.yml" --include="*.yaml" \
    'stellarindex_' "${ANSIBLE_TASK_PATHS[@]}" 2>/dev/null)
}

unalerted=0
while IFS= read -r tok; do
  [[ -z "$tok" ]] && continue
  in_list "$tok" ${UNALERTED_OK[@]+"${UNALERTED_OK[@]}"} && continue
  is_alerted "$tok" && continue
  echo "UNALERTED (advisory): '$tok' is emitted but no rule expr references it yet."
  unalerted=$((unalerted + 1))
done < <(collect_emitted_tokens | sort -u)

if [[ "$dead" -eq 0 ]]; then
  echo "lint-metric-refs: OK — every stellarindex_* expr token resolves to an emitter or a documented KNOWN_INERT entry."
else
  echo "lint-metric-refs: FAIL — $dead dead/stale metric reference(s)." >&2
fi
if [[ "$unalerted" -gt 0 ]]; then
  echo "lint-metric-refs: $unalerted stellarindex_* metric(s) have no alert reference (advisory only, does not fail the gate)."
fi
# exit 1, not exit "$dead": the status truncates mod 256, so exactly 256 dead refs exited 0.
[[ "$dead" -eq 0 ]] || exit 1
