#!/usr/bin/env bash
# galexie-archive-fill_test.sh — pins which MinIO identity each phase of
# galexie-archive-fill.sh uses under ansible's /etc/default: reads and the
# mirror go through the bucket-scoped writer alias (no delete), only the
# operator PARTIALS delete uses the separately named delete alias, an
# operator-set ARCHIVE_DEST keeps its own alias for both, and an unresolvable
# alias fails the run before it mirrors or deletes anything.
#
# Runs a copy of the real script with its root-owned paths and lock rewritten
# into a temp dir, beside the defaults file 07-galexie.yml renders, against a
# stub mc (and a GNU-xargs shim where the host's xargs lacks -a/-d), so it
# needs no root, MinIO or network.
#
# Run: bash configs/ansible/roles/archival-node/files/galexie-archive-fill_test.sh
set -uo pipefail

cd "$(dirname "$0")" || exit 1
SCRIPT="$PWD/galexie-archive-fill.sh"

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"
PART=FC42F7FF--62720000-62783999

# The copy under test: root-owned paths into $TMP, and the flock block
# dropped (fd-variable redirection needs bash 4.1; the lock is not under test).
# shellcheck disable=SC2016  # $LOCK and $lock_fd are literal script text
sed -e "s#/var/log/galexie-mirror.log#$TMP/mirror.log#" \
    -e "s#/run/lock/galexie-archive-fill.lock#$TMP/fill.lock#" \
    -e "s#/etc/default/galexie-archive-fill#$TMP/default#g" \
    -e "s#/var/lib/galexie-archive/hot-floor#$TMP/hot-floor#" \
    -e 's#^exec {lock_fd}>"\$LOCK"$#:#' \
    -e 's#^if ! flock -n "\$lock_fd"; then$#if false; then#' \
    "$SCRIPT" > "$TMP/fill.sh"
for want in "$TMP/mirror.log" "$TMP/fill.lock" "$TMP/default" "$TMP/hot-floor" '^:$' '^if false; then$'; do
  grep -q -- "$want" "$TMP/fill.sh" || { echo "FAIL: rewrite of the script copy missed '$want' — update this test"; exit 1; }
done

# /etc/default/galexie-archive-fill as ansible renders it (hot floor 0).
awk '/^- name: Template \/etc\/default\/galexie-archive-fill/ { t = 1 }
     t && /content: \|/ { c = 1; next }
     c && /^  [a-z]/ { exit }
     c { sub(/^      /, ""); print }' ../tasks/07-galexie.yml \
  | sed 's/{{ stellarindex_archive_hot_floor }}/0/' > "$TMP/default"
grep -q 'ARCHIVE_DEST=archivewriter/galexie-archive' "$TMP/default" \
  || { echo "FAIL: rendered defaults lack the writer ARCHIVE_DEST — update this test"; exit 1; }

# Stub mc: logs every call; an alias outside $MC_ALIASES does not resolve.
# The AWS bucket holds one partition of 3 objects; the archive holds none.
cat > "$TMP/bin/mc" <<'EOF'
#!/usr/bin/env bash
echo "$*" >> "$MC_CALLS"
known() { case " $MC_ALIASES " in *" ${1%%/*} "*) return 0 ;; esac; return 1; }
case "$1" in
  alias) [ "$2" = list ] && known "$3"; exit ;;
  ls)
    target="${*: -1}"
    known "$target" || { echo "mc: <ERROR> alias not found" >&2; exit 1; }
    case "$target" in
      aws-public/*/pubnet/) echo "[2026-09-30 00:00:00 UTC]     0B $PART/" ;;
      aws-public/*) printf 'obj1\nobj2\nobj3\n' ;;
    esac ;;
  mirror | rm) known "${*: -1}" || exit 1 ;;
esac
exit 0
EOF
chmod +x "$TMP/bin/mc"

# GNU xargs shim (-r -a FILE -d '\n' -P N -n 1 CMD...) for hosts without it.
if ! xargs -r -a /dev/null -d '\n' true 2>/dev/null; then
  cat > "$TMP/bin/xargs" <<'EOF'
#!/usr/bin/env bash
file=
while [ $# -gt 0 ]; do
  case "$1" in
    -r) shift ;;
    -a) file=$2; shift 2 ;;
    -d | -P | -n) shift 2 ;;
    *) break ;;
  esac
done
rc=0
while IFS= read -r line; do "$@" "$line" || rc=123; done < "$file"
exit "$rc"
EOF
  chmod +x "$TMP/bin/xargs"
fi

# run <aliases> [VAR=value...] — run the copy; sets rc, fills $TMP/{stderr,calls}.
run() {
  local aliases=$1; shift
  : > "$TMP/calls"
  env PATH="$TMP/bin:$PATH" MC_CALLS="$TMP/calls" MC_ALIASES="$aliases" PART="$PART" \
    TMPDIR="$TMP" PARTIAL_CHECK_WINDOW=1 "$@" \
    bash "$TMP/fill.sh" > "$TMP/stdout" 2> "$TMP/stderr"
  rc=$?
}

echo "1. timer run: every archive call goes through the writer alias"
run "aws-public local archivewriter"
if [ "$rc" -eq 0 ]; then ok "exit 0"; else bad "exit $rc: $(tail -3 "$TMP/stderr")"; fi
if grep -q "^mirror .* archivewriter/galexie-archive/$PART/\$" "$TMP/calls"; then ok "mirror writes via archivewriter"
else bad "no mirror to archivewriter/galexie-archive/$PART/"; fi
if grep -q 'local/' "$TMP/calls"; then bad "used the root alias: $(grep -m1 'local/' "$TMP/calls")"
else ok "no call through the local (root) alias"; fi
if grep -q '^rm ' "$TMP/calls"; then bad "timer run deleted"; else ok "no delete"; fi

echo "2. writer alias missing: fail before any mirror"
run "aws-public local"
if [ "$rc" -ne 0 ]; then ok "exit $rc"; else bad "exit 0 with no working writer alias"; fi
if grep -q "archivewriter" "$TMP/stderr"; then ok "error names the alias"; else bad "error does not name archivewriter"; fi
if grep -q '^mirror ' "$TMP/calls"; then bad "mirrored anyway"; else ok "no mirror"; fi

echo "3. PARTIALS with an unconfigured delete alias: fail before any rm"
run "aws-public local archivewriter" PARTIALS="$PART" ARCHIVE_DELETE_ALIAS=nosuchalias
if [ "$rc" -ne 0 ]; then ok "exit $rc"; else bad "exit 0 with no delete alias"; fi
if grep -q '^rm ' "$TMP/calls"; then bad "ran rm: $(grep -m1 '^rm ' "$TMP/calls")"; else ok "no rm"; fi

echo "4. PARTIALS: delete via the delete alias, mirror via the writer"
run "aws-public local archivewriter" PARTIALS="$PART"
if [ "$rc" -eq 0 ]; then ok "exit 0"; else bad "exit $rc: $(tail -3 "$TMP/stderr")"; fi
if grep -q "^rm --recursive --force local/galexie-archive/$PART/\$" "$TMP/calls"; then ok "rm via local"
else bad "no rm via local/galexie-archive/$PART/"; fi
if grep -q "^mirror .* archivewriter/galexie-archive/$PART/\$" "$TMP/calls"; then ok "mirror via archivewriter"
else bad "no mirror to archivewriter"; fi

echo "5. operator-set ARCHIVE_DEST: delete and mirror both stay on that store"
run "aws-public local archivewriter other" PARTIALS="$PART" ARCHIVE_DEST=other/galexie-archive
if [ "$rc" -eq 0 ]; then ok "exit 0"; else bad "exit $rc: $(tail -3 "$TMP/stderr")"; fi
if grep -q "^rm --recursive --force other/galexie-archive/$PART/\$" "$TMP/calls"; then ok "rm via other"
else bad "no rm via other/galexie-archive/$PART/"; fi
if grep -qE '(local|archivewriter)/' "$TMP/calls"; then bad "left the operator's store: $(grep -m1 -E '(local|archivewriter)/' "$TMP/calls")"
else ok "no call outside other/"; fi

echo
echo "passed: $pass  failed: $fail"
[ "$fail" -eq 0 ]
