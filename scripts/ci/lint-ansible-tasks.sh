#!/usr/bin/env bash
# lint-ansible-tasks.sh — two structural guards over configs/ansible/**.
#
# Rule 1: pipefail-needs-bash. `ansible.builtin.shell` runs its body via
#   /bin/sh, which is dash on every host this role targets (Ubuntu noble).
#   dash < 0.5.13 rejects `set -o pipefail` ("set: Illegal option -o
#   pipefail", rc=2) BEFORE the first real statement, so the task fails and
#   the play aborts on every host. The tree has hit this three times
#   (deploy-binary.yml 2026-07-18, 10-observability.yml, and 04-users.yml
#   added 2026-08-27 — the latter aborted every full archival-node apply at
#   step 04, before postgres/galexie/services). Any shell task whose body
#   sets pipefail MUST declare `executable: /bin/bash`. Scripts shipped via
#   `copy: content:` carry their own shebang and are not shell tasks, so
#   they are out of scope.
#
# Rule 2: secret-on-argv. A vault secret interpolated into `argv:` / `cmd:`
#   of a command/shell task is visible to every local uid in
#   /proc/<pid>/cmdline (+ ps, + auditd execve records) for the life of the
#   process; `no_log: true` hides it from Ansible's OWN output only. Feed
#   secrets via `stdin:` or `environment:` instead. Same rule for shipped
#   shell scripts: `mc alias set` / `mc admin user add` must take the
#   secret on stdin, not as a positional argument (galexie-append.sh ran
#   the argv form on every galexie restart).
#
# Both rules are line-oriented (awk), deliberately conservative, and
# self-tested by lint-ansible-tasks-test.sh. Exit code = number of
# findings (0 = clean). Prints a self-accounting line so a scan of zero
# files cannot pass silently.

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root" || exit 1

ANSIBLE_DIR="${ANSIBLE_DIR:-configs/ansible}"

if [ ! -d "$ANSIBLE_DIR" ]; then
  echo "lint-ansible-tasks: FAIL — $ANSIBLE_DIR not found (gate must not pass vacuously)" >&2
  exit 1
fi

# (bash 3.2 on macOS has no mapfile — fill the arrays with read loops.)
yml_files=()
while IFS= read -r f; do yml_files+=("$f"); done < <(
  find "$ANSIBLE_DIR" -type f \( -name '*.yml' -o -name '*.yaml' \) ! -path '*/inventory/*' | sort)
sh_files=()
while IFS= read -r f; do sh_files+=("$f"); done < <(
  find "$ANSIBLE_DIR" -type f \( -name '*.sh' -o -name '*.sh.j2' \) | sort)

if [ "${#yml_files[@]}" -eq 0 ]; then
  echo "lint-ansible-tasks: FAIL — no task files found under $ANSIBLE_DIR (gate must not pass vacuously)" >&2
  exit 1
fi

# Grandfathered violations (shrink-only; CS-098 lint-baseline-growth.sh
# watches scripts/ci/*.baseline). Format: `<rule>\t<path>` per line. A
# baselined (rule, path) pair is reported but not counted; a baseline entry
# with NO matching finding is STALE and fails the gate so the list can only
# shrink.
BASELINE="${BASELINE:-scripts/ci/lint-ansible-tasks.baseline}"
baseline_entries=()
if [ -f "$BASELINE" ]; then
  while IFS= read -r l; do
    case "$l" in ''|'#'*) continue ;; esac
    baseline_entries+=("$l")
  done < "$BASELINE"
fi
baseline_seen=""

findings=0
report=""   # accumulated finding lines: "<file>:<line>: <rule>: …"

# ── Rule 1 + Rule 2 over task files ─────────────────────────────────────
# A "task block" starts at a `- name:` (or an unnamed `- module:`) list item
# and runs to the next one. Within a block we track the module and, by
# indentation, which top-level task key (argv/cmd/environment/stdin/…) the
# current line belongs to.
for f in "${yml_files[@]}"; do
  out="$(awk -v file="$f" '
    function flush() {
      if (is_shell && pipefail_line && !bash_exec) {
        printf("%s:%d: pipefail-needs-bash: ansible.builtin.shell body sets pipefail (line %d) but no `executable: /bin/bash` — dash rejects `-o pipefail` (task: %s)\n",
               file, task_line, pipefail_line, task_name)
        n++
      }
      is_shell = 0; is_cmd = 0; pipefail_line = 0; bash_exec = 0; task_line = 0; task_name = ""
      key = ""; subkey = ""; key_indent = -1
    }
    function is_module(k) {
      return (k == "ansible.builtin.shell" || k == "shell" || k == "ansible.builtin.command" || k == "command")
    }
    BEGIN { n = 0; flush() }
    {
      line = $0
      if (match(line, /^[ \t]*- (name:|[a-z_.]+:)/)) {
        flush()
        task_line = NR
        indent = match(line, /[^ \t]/) - 1
        key_indent = indent + 2
        if (line ~ /^[ \t]*- name:/) { task_name = line; sub(/^[ \t]*- name:[ \t]*/, "", task_name) }
        else { task_name = "(unnamed)" }
      }
      if (task_line == 0) next
      if (line ~ /^[ \t]*#/) next
      indent = match(line, /[^ \t]/) - 1
      # task-level key (argv/stdin/environment sit one level BELOW the
      # module key, so they are tracked as subkey)
      if (indent == key_indent && line ~ /^[ \t]*[a-z_.]+:/) {
        key = line; sub(/^[ \t]*/, "", key); sub(/:.*/, "", key); subkey = ""
      } else if (indent == key_indent - 2 && line ~ /^[ \t]*- [a-z_.]+:/) {
        key = line; sub(/^[ \t]*- /, "", key); sub(/:.*/, "", key); subkey = ""
      } else if (indent == key_indent + 2 && line ~ /^[ \t]*[a-z_]+:/) {
        subkey = line; sub(/^[ \t]*/, "", subkey); sub(/:.*/, "", subkey)
      }
      if (key == "ansible.builtin.shell" || key == "shell") is_shell = 1
      if (key == "ansible.builtin.command" || key == "command") is_cmd = 1
      # The command BODY is: the module line itself / its folded-scalar
      # continuation (subkey == ""), `cmd:` or `argv:` items.
      in_body = is_module(key) && (subkey == "" || subkey == "cmd" || subkey == "argv" || subkey == "_raw_params")
      if (is_shell && in_body && !pipefail_line \
          && (line ~ /^[ \t]*set[ \t]+[-+][A-Za-z]*o[A-Za-z]*[ \t]+pipefail/ || line ~ /^[ \t]*set[ \t]+.*-o[ \t]+pipefail/)) pipefail_line = NR
      if (line ~ /^[ \t]*executable:[ \t]*\/bin\/bash[ \t]*$/) bash_exec = 1
      if ((is_shell || is_cmd) && in_body \
          && line ~ /\{\{[^}]*(password|secret|_pass\b|token|api_key)[^}]*\}\}/) {
        printf("%s:%d: secret-on-argv: vault value interpolated into the command body (visible in /proc/<pid>/cmdline; no_log hides it from Ansible output only) — feed it via `stdin:` or `environment:` (task: %s)\n",
               file, NR, task_name)
        n++
      }
    }
    END { flush(); exit n }
  ' "$f")"
  [ -n "$out" ] && report="${report}${out}"$'\n'
done

# ── Rule 2 over shipped shell scripts ───────────────────────────────────
# Join backslash continuations so `mc alias set live "$URL" \
#   "$KEY" "$SECRET"` is judged as one command.
for f in ${sh_files[@]+"${sh_files[@]}"}; do
  out="$(awk -v file="$f" '
    { buf = buf $0; ln = (ln ? ln : NR)
      if ($0 ~ /\\$/) { sub(/\\$/, "", buf); next }
      # secret var AFTER the mc verb = positional argument; a secret piped
      # in BEFORE it (`printf … "$SECRET" | mc alias set …`) is the fix.
      if (buf !~ /^[ \t]*#/ \
          && buf ~ /mc[ \t]+(alias[ \t]+set|admin[ \t]+user[ \t]+add)[ \t][^|;&]*\$\{?[A-Za-z_]*(SECRET|PASSWORD|_PASS\b)/) {
        printf("%s:%d: secret-on-argv: `mc` given a secret as a positional argument (visible in /proc/<pid>/cmdline) — pipe it on stdin instead\n", file, ln)
        n++
      }
      buf = ""; ln = 0 }
    END { exit n }
  ' "$f")"
  [ -n "$out" ] && report="${report}${out}"$'\n'
done

# ── Apply the baseline ──────────────────────────────────────────────────
while IFS= read -r line; do
  [ -n "$line" ] || continue
  path="${line%%:*}"
  rule="$(printf '%s' "$line" | sed -E 's/^[^:]+:[0-9]+: ([a-z-]+):.*/\1/')"
  key="${rule}	${path}"
  baselined=0
  for e in ${baseline_entries[@]+"${baseline_entries[@]}"}; do
    if [ "$e" = "$key" ]; then baselined=1; baseline_seen="${baseline_seen}${key}"$'\n'; break; fi
  done
  if [ "$baselined" -eq 1 ]; then
    echo "baselined (shrink-only, ${BASELINE}): $line"
  else
    echo "$line"
    findings=$((findings + 1))
  fi
done <<< "$report"

for e in ${baseline_entries[@]+"${baseline_entries[@]}"}; do
  if ! grep -Fxq -- "$e" <<< "$baseline_seen"; then
    echo "STALE baseline entry (violation no longer present — delete it from ${BASELINE}): ${e}"
    findings=$((findings + 1))
  fi
done

echo "lint-ansible-tasks: scanned ${#yml_files[@]} task files + ${#sh_files[@]} shell scripts under $ANSIBLE_DIR — $findings finding(s)"
exit "$findings"
