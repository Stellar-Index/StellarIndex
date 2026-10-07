#!/usr/bin/env bash
# lint-go-toolchain-parity.sh — every actions/setup-go step in the
# workflow tree must resolve its Go version FROM go.mod, never from a
# hardcoded pin.
#
# The "govulncheck toolchain parity" concern this guards: if the `vuln`
# job's Go toolchain were pinned independently of go.mod, a
# language-version bump would make
# govulncheck analyse a NEWER module graph with an OLDER SDK, and the
# vulnerability gate would fail to parse instead of failing on an
# actual vulnerability — going dark exactly when the toolchain moves,
# which is the one moment it matters most.
#
# Every `actions/setup-go` step uses `go-version-file: go.mod`, so a
# go.mod bump carries every job (including `vuln`) without a second edit.
# This gate keeps it that way: nothing else stops a
# future edit from adding a `go-version: '1.25'` pin next to (or
# instead of) `go-version-file`, silently reintroducing the exact
# divergence Section D describes. This gate is that durability: it
# fails on any actions/setup-go step that lacks go-version-file: go.mod,
# or that also declares a hardcoded go-version (setup-go documents
# go-version as taking precedence when both are set, so a hardcoded
# value there defeats go-version-file even when the latter is present).
#
# Usage:
#   bash scripts/ci/lint-go-toolchain-parity.sh              # .github/workflows
#   bash scripts/ci/lint-go-toolchain-parity.sh <dir>...     # explicit roots
set -euo pipefail

cd "$(dirname "$0")/../.."

ROOTS=("${@:-.github/workflows}")

FILES=()
while IFS= read -r f; do
  FILES+=("$f")
done < <(find "${ROOTS[@]}" -type f \( -name '*.yml' -o -name '*.yaml' \) 2>/dev/null | sort)

if [[ "${#FILES[@]}" -eq 0 ]]; then
  echo "lint-go-toolchain-parity: FAIL — no workflow files under ${ROOTS[*]}; the gate would be vacuous" >&2
  exit 1
fi

STEPS=0
FAIL=0

for f in "${FILES[@]}"; do
  out="$(awk '
    function lead(s,    n) { n = match(s, /[^ ]/); return n ? n - 1 : length(s) }
    {
      line = $0
      if (line ~ /^[ \t]*$/) next
      ind = lead(line)
      if (in_step && ind <= step_indent) {
        if (!has_gvf) print lineno ": actions/setup-go step missing go-version-file: go.mod"
        if (has_gv)   print lineno ": actions/setup-go step pins a hardcoded go-version: (overrides go-version-file)"
        in_step = 0
      }
      if (line ~ /^[ \t]*-[ \t]*uses:[ \t]*actions\/setup-go@/) {
        step_indent = ind
        in_step = 1
        has_gvf = 0
        has_gv = 0
        lineno = NR
        count++
        next
      }
      if (in_step) {
        if (line ~ /go-version-file:[ \t]*go\.mod([ \t]|$)/) has_gvf = 1
        if (line ~ /^[ \t]*go-version:/) has_gv = 1
      }
    }
    END {
      if (in_step) {
        if (!has_gvf) print lineno ": actions/setup-go step missing go-version-file: go.mod"
        if (has_gv)   print lineno ": actions/setup-go step pins a hardcoded go-version: (overrides go-version-file)"
      }
      print "COUNT:" (count + 0)
    }
  ' "$f")"

  count_line="$(printf '%s\n' "$out" | grep '^COUNT:')"
  n="${count_line#COUNT:}"
  STEPS=$((STEPS + n))

  while IFS= read -r hit; do
    [ -z "$hit" ] && continue
    [[ "$hit" == COUNT:* ]] && continue
    lineno="${hit%%:*}"
    msg="${hit#*: }"
    echo "lint-go-toolchain-parity: $f:$lineno $msg"
    FAIL=$((FAIL + 1))
  done <<< "$out"
done

if [[ "$STEPS" -eq 0 ]]; then
  echo "lint-go-toolchain-parity: FAIL — no actions/setup-go step found across ${#FILES[@]} workflow file(s); the gate would be vacuous" >&2
  exit 1
fi

if [[ "$FAIL" -gt 0 ]]; then
  echo
  echo "lint-go-toolchain-parity: FAIL — $FAIL actions/setup-go step(s) do not resolve their Go version from go.mod."
  echo "  Use: go-version-file: go.mod   (and do not also set go-version:)"
  exit 1
fi

# ── Container Go pin ───────────────────────────────────────────────────────
# A Dockerfile cannot read go.mod, so its `FROM golang:X.Y.Z` is a SECOND,
# independent declaration of the toolchain — the same divergence this gate
# forbids in workflows, in the one place `go-version-file` cannot reach.
# It has drifted in both directions before, and the container is what
# `make prepush` runs, so a stale pin silently grades the
# push with a different compiler than CI and production use.
DOCKER_DIR="${DOCKER_DIR:-docker}"
DOCKERFILES=()
while IFS= read -r df; do
  DOCKERFILES+=("$df")
done < <(find "$DOCKER_DIR" -type f \( -name 'Dockerfile*' -o -name '*.Dockerfile' \) 2>/dev/null | sort)

if [[ "${#DOCKERFILES[@]}" -eq 0 ]]; then
  echo "lint-go-toolchain-parity: FAIL — no Dockerfile found under $DOCKER_DIR/; the container-pin check would be vacuous" >&2
  exit 1
fi

want="$(awk '/^toolchain[ \t]+go/ { sub(/^go/, "", $2); print $2; exit }' go.mod)"
if [[ -z "$want" ]]; then
  want="$(awk '/^go[ \t]+[0-9]/ { print $2; exit }' go.mod)"
fi
if [[ -z "$want" ]]; then
  echo "lint-go-toolchain-parity: FAIL — go.mod declares neither a toolchain nor a go version" >&2
  exit 1
fi
PINS=0
for df in "${DOCKERFILES[@]}"; do
  while IFS= read -r hit; do
    [ -z "$hit" ] && continue
    ln="${hit%%:*}"
    tag="${hit#*:}"
    PINS=$((PINS + 1))
    ver="${tag%%@*}"
    # Compare on major.minor.patch when the tag carries one, else major.minor.
    case "$ver" in
      "$want"|"$want"-*)                   ;;
      "${want%.*}"|"${want%.*}"-*)         ;;
      *)
        echo "lint-go-toolchain-parity: $df:$ln pins golang:$tag but go.mod resolves to $want"
        FAIL=$((FAIL + 1))
        ;;
    esac
    # A minor tag like 1.26-alpine floats across patch releases, so without
    # a digest the compiler the image builds with is not fixed at all.
    case "$tag" in
      *@sha256:*) ;;
      *)
        echo "lint-go-toolchain-parity: $df:$ln pins golang:$tag by tag only; pin it as image:tag@sha256:<index digest>"
        FAIL=$((FAIL + 1))
        ;;
    esac
  done < <(grep -nE '^[[:space:]]*FROM[[:space:]]+golang:' "$df" 2>/dev/null \
           | sed -E 's/^([0-9]+):[[:space:]]*FROM[[:space:]]+golang:([^[:space:]]+).*/\1:\2/')
done
if [[ "$PINS" -eq 0 ]]; then
  echo "lint-go-toolchain-parity: FAIL — 0 container Go pin(s) across ${#DOCKERFILES[@]} Dockerfile(s); the container-pin check would be vacuous" >&2
  exit 1
fi
if [[ "$FAIL" -gt 0 ]]; then
  echo
  echo "lint-go-toolchain-parity: FAIL — a container Go pin disagrees with go.mod or is not digest-pinned."
  echo "  The container is what \`make prepush\` runs; a stale pin grades the push"
  echo "  with a different compiler than CI and production use (see F-1240)."
  exit 1
fi
echo "lint-go-toolchain-parity: OK — $PINS container Go pin(s) across ${#DOCKERFILES[@]} Dockerfile(s) match go.mod ($want), digest-pinned"

echo "lint-go-toolchain-parity: OK — $STEPS actions/setup-go step(s) in ${#FILES[@]} workflow file(s), all resolve from go.mod"
