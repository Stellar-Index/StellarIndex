#!/usr/bin/env bash
# lint-replay-plan.sh — the "decoder changed, who replays history?" tripwire.
#
# A decoder or an asset allow-list defines what live ingestion RECORDS.
# Widening one changes what new rows look like from the moment the binary
# deploys — but every row already written stays as it was, and nothing in
# the deploy path replays the past. The served dataset then silently
# forks: recent history has the new shape, older history does not, and
# no gate notices because each row is individually valid.
#
# Widening a canonical set (e.g. fiat codes) starts live ingestion of the
# new members at once; without a replay their history is missing, and the
# gap surfaces only when a gate compares against the widened set. The
# change is correct; the omission is the plan — and a plan that lives in someone's head is not
# a plan the next operator can read.
#
# This gate makes the plan ship WITH the change. Any commit range that
# touches a watched path (a source decoder / event schema / feed
# registry, or a canonical asset allow-list) fails unless a commit
# message in the range carries an explicit, auditable trailer:
#
#     Replay-Plan: <what history is replayed, how, and by whom>
#
# or, when no already-served history is affected (e.g. a pure refactor,
# a new source with no rows yet, a decoder for an event that has never
# fired on mainnet):
#
#     Replay-Plan: none — <why no served history is affected>
#
# The trailer does not make a replay-free widening "allowed by default"
# — it makes it impossible to do SILENTLY. A reviewer (or the operator
# reading `git log`) sees the declaration next to the change, and a bare
# `Replay-Plan: none` without a reason does not count.
#
# Deliberately NOT a per-file gate, and deliberately WITHOUT the CID-1
# base-walk lint-baseline-growth.sh carries: decoders are a high-churn
# surface (a dozen PRs a week touch one), so one plan per range is the
# right granularity, and walking BASE_SHA back past every historical
# undeclared decoder commit on main would red every PR that merges after
# one. The failure mode this guards is a FORGOTTEN plan, not a hidden
# bypass — one honest run per range is enough.
#
# One thing it does NOT decide: which command. That is the replay decision
# rule in docs/architecture/ingest-pipeline.md, and a declared plan naming
# a projected source with `backfill` / `ch-rebuild` gets a WARNING here
# (never a failure — see warn_wrong_replay_command).
#
# Usage: BASE_SHA=<sha> ./scripts/ci/lint-replay-plan.sh
#   BASE_SHA — the comparison base (PR base sha, or the push event's
#              `before` sha). Unset/zero → check is skipped (first
#              push / manual local run without history context).
#        ./scripts/ci/lint-replay-plan.sh --list-projected
#   prints the projected source names the advisory checks against.
set -euo pipefail

cd "$(dirname "$0")/../.."

BASE_SHA="${BASE_SHA:-}"
ZERO_SHA="0000000000000000000000000000000000000000"

# Watched paths — the files whose content decides what ingestion writes.
# Git pathspecs: `*` crosses `/`, so internal/sources/*/ reaches both
# internal/sources/<src>/ and internal/sources/external/<venue>/. Test
# files are deliberately NOT watched (the trailing :(exclude) pathspec):
# a *_test.go change cannot alter a served row. Keep this list in step
# with the failure message below.
WATCHED=(
  # canonical asset allow-lists (ADR-0010 fiat, ADR-0014 crypto,
  # ADR-0028 RWA): what asset codes the pipeline will accept at all.
  'internal/canonical/asset_fiat.go'
  'internal/canonical/asset_crypto.go'
  'internal/canonical/asset_rwa.go'
  # per-source decoders (decode.go plus its siblings: aquarius
  # decode_rewards.go / decode_admin.go, blend decode_money_market.go),
  # event schemas, feed registries, and the external-venue pair
  # allow-lists (external/{coinbase,bitstamp,kraken,binance}/pairs.go —
  # the same widen-without-replay class as asset_fiat.go).
  'internal/sources/*/decode*.go'
  'internal/sources/*/events.go'
  'internal/sources/*/feeds.go'
  'internal/sources/*/pairs.go'
  # correlation / scaling / reconstruction logic that decides a
  # recorded value without living in a decode*.go file itself: the
  # soroswap Swap/Sync reserve correlation, redstone's price median,
  # classicmovements' liquidity-pool op decoders, sorobanevents'
  # multi-event reconstruction, and the CEX/oracle amount scalers.
  'internal/sources/*/consumer.go'
  'internal/sources/*/payload.go'
  'internal/sources/*/entrychanges.go'
  'internal/sources/*/reconstruct.go'
  'internal/sources/*/decimals.go'
  'internal/sources/*/scale.go'
  ':(exclude)*_test.go'
)

# changed_watched <base> <head> — the watched paths that differ between
# <base> and <head> (three-dot: the PR's own changes, not drift on main).
changed_watched() {
  local base="$1" head="$2"
  git diff --name-only "${base}...${head}" -- "${WATCHED[@]}"
}

# has_replay_plan <log-body>
# True iff <log-body> carries a Replay-Plan trailer with a substantive
# value: non-empty, and not a bare `none` (a "none" must give its reason).
# Both greps read from here-strings, not a pipeline, so `grep -q`
# closing its input early can never surface as a SIGPIPE'd writer under
# `set -o pipefail` — the trap lint-baseline-growth.sh documents.
has_replay_plan() {
  local body="$1" trailers
  trailers="$(grep -iE '^Replay-Plan:[[:space:]]*\S' <<<"$body" || true)"
  [[ -n "$trailers" ]] || return 1
  # Drop bare `none` (optionally followed by punctuation/whitespace only).
  trailers="$(grep -viE '^Replay-Plan:[[:space:]]*none[[:space:]]*[-—:.]*[[:space:]]*$' <<<"$trailers" || true)"
  [[ -n "$trailers" ]]
}

# ── advisory: does the declared plan name the RIGHT command? ───────────
# A projected source (ADR-0031/0032: written by internal/projector and
# nothing else) is replayed with projector-replay / projected-rebuild.
# `backfill` is a MinIO walk that invariant 8 rules out and that writes
# ZERO rows on a gated projected source; `ch-rebuild` is the
# non-projected pass (and its event pass over a projected domain is a
# second writer). The rule lives in docs/architecture/ingest-pipeline.md
# ("The replay decision rule"); this is a WARNING, never a failure — the
# gate's job is to make the plan exist, and only a human knows whether an
# unusual command is the clean-slate exception.
#
# The source names are DERIVED from the SourceSpec registry, never listed
# here: a list here would be the same drift this whole gate exists to
# prevent. A spec is projected iff it has a non-nil Projector (AGENTS.md
# invariant 7), either as a `{ Name: …, Projector: … }` literal or through
# a `func <helper>(name …) SourceSpec` whose body sets both. Each
# `<pkg>.<Const>` name resolves through the import block to that package's
# string constant. Anything the parser cannot resolve is an error, not a
# skip: a derivation that silently drops names checks nothing.
SPEC_REGISTRY=internal/pipeline/source_spec.go

# shellcheck disable=SC2016  # awk program, not a shell expansion
SPEC_AWK='
function field(s, key,    v) {
  if (!match(s, "(^|[{ \t,])" key ":[ \t]*[^ \t,}]+")) return ""
  v = substr(s, RSTART, RLENGTH)
  sub("^.*" key ":[ \t]*", "", v)
  return v
}
function emit(tok,    a) {
  if (tok ~ /^"[^"]+"$/) { gsub(/"/, "", tok); print "LIT " tok; return }
  if (tok !~ /^[A-Za-z0-9_]+\.[A-Za-z0-9_]+$/) { print "ERR line " FNR ": unrecognised Name expression: " tok; return }
  split(tok, a, ".")
  if (imp[a[1]] == "") { print "ERR line " FNR ": " a[1] " is not an internal/ import"; return }
  print "REF " imp[a[1]] " " a[2] " " tok
}
FNR == NR {
  if ($0 ~ /^import \(/) { inimp = 1; next }
  if (inimp && $0 ~ /^\)/) { inimp = 0; next }
  if (inimp && $0 ~ /"[^"]+"/) {
    n = split($0, f, /[ \t]+/); k = (f[1] == "") ? 2 : 1
    path = f[n]; gsub(/"/, "", path)
    alias = (f[k] ~ /^"/) ? path : f[k]; sub(/.*\//, "", alias)
    dir = path; if (sub(/^.*\/internal\//, "internal/", dir)) imp[alias] = dir
    next
  }
  if ($0 ~ /^func [A-Za-z_][A-Za-z0-9_]*\([A-Za-z_][A-Za-z0-9_]* .*\) SourceSpec \{/) {
    h = $0; sub(/^func /, "", h); p = h; sub(/\(.*/, "", h)
    sub(/^[^(]*\(/, "", p); sub(/ .*/, "", p); param[h] = p; inh = 1; next
  }
  if (inh && $0 ~ /^}/) { inh = 0; next }
  if (inh) {
    pv = field($0, "Projector"); if (pv != "" && pv != "nil") projhelper[h] = 1
    if (field($0, "Name") == param[h]) namedby[h] = 1
  }
  next
}
/^var specs = \[\]SourceSpec\{/ { inspecs = 1; seen = 1; depth = 1; next }
!inspecs { next }
{
  line = $0; sub(/\/\/.*/, "", line)
  start = (depth == 1 && line ~ /^[ \t]*\{/)
  if (start) { nm = ""; proj = 0; inentry = 1 }
  if (inentry && (depth == 2 || start)) {
    v = field(line, "Name"); if (v != "") nm = v
    v = field(line, "Projector"); if (v != "" && v != "nil") proj = 1
  }
  if (depth == 1 && !start && line ~ /^[ \t]*[A-Za-z_][A-Za-z0-9_]*\(/) {
    h = line; sub(/^[ \t]*/, "", h); sub(/\(.*/, "", h)
    if (h in projhelper) {
      if (!(h in namedby)) print "ERR line " FNR ": helper " h " does not set Name from its first parameter"
      else { arg = line; sub(/^[^(]*\(/, "", arg); sub(/[,)].*/, "", arg); emit(arg) }
    }
  }
  o = gsub(/\{/, "{", line); c = gsub(/\}/, "}", line); depth += o - c
  if (inentry && depth == 1) {
    inentry = 0
    if (proj) { if (nm == "") print "ERR line " FNR ": projected spec with no Name"; else emit(nm) }
  }
  if (depth <= 0) inspecs = 0
}
END { if (!seen) print "ERR no `var specs = []SourceSpec{` block" }
'

# projector_source_names — one projected source name per line, sorted.
# Returns 1 with the reason on stderr when the registry cannot be read.
projector_source_names() {
  local reg="$SPEC_REGISTRY" parsed kind dir const tok gf vals names=""
  [[ -r "$reg" ]] || { echo "$reg is missing" >&2; return 1; }
  parsed="$(awk "$SPEC_AWK" "$reg" "$reg")"
  while read -r kind dir const tok; do
    case "$kind" in
      LIT) names="${names}${dir}"$'\n' ;;
      ERR) echo "$reg: $dir $const $tok" >&2; return 1 ;;
      REF)
        vals=""
        for gf in "$dir"/*.go; do
          [[ -f "$gf" && "$gf" != *_test.go ]] || continue
          vals="${vals}$(grep -oE "(^|[^A-Za-z0-9_])${const}[[:space:]]*=[[:space:]]*\"[^\"]+\"" "$gf" |
            grep -oE '"[^"]+"' | tr -d '"' || true)"$'\n'
        done
        vals="$(grep -v '^$' <<<"$vals" | LC_ALL=C sort -u || true)"
        if [[ -z "$vals" || "$vals" == *$'\n'* ]]; then
          echo "$reg: cannot resolve $tok to one string constant in $dir" >&2
          return 1
        fi
        names="${names}${vals}"$'\n'
        ;;
    esac
  done <<<"$parsed"
  grep -v '^$' <<<"$names" | LC_ALL=C sort -u || true
}

# warn_wrong_replay_command <trailer-lines> <projected-names>
warn_wrong_replay_command() {
  local trailers="$1" names="$2" count line name
  count="$(grep -c . <<<"$names" || true)"
  echo "lint-replay-plan: cross-checked the declared plan against $count projector source name(s)"
  while IFS= read -r line; do
    [[ -n "$line" ]] || continue
    grep -qE '(^|[[:space:]])(backfill|ch-rebuild)([[:space:]]|$)' <<<"$line" || continue
    while IFS= read -r name; do
      [[ -n "$name" ]] || continue
      grep -qE "(^|[[:space:]=,'\"])${name}([[:space:]=,'\"]|$)" <<<"$line" || continue
      echo "  WARNING: the plan replays projected source '${name}' with backfill/ch-rebuild."
      echo "           Projected sources have ONE writer (invariant 7): use"
      echo "           'projector-replay -source ${name}' (rewind) or"
      echo "           'projected-rebuild -source ${name} ... -write' (bulk)."
      echo "           See docs/architecture/ingest-pipeline.md — The replay decision rule."
    done <<<"$names"
  done <<<"$trailers"
}

if [[ "${1:-}" == "--list-projected" ]]; then
  projector_source_names
  exit
fi

if [[ -z "$BASE_SHA" || "$BASE_SHA" == "$ZERO_SHA" ]]; then
  echo "lint-replay-plan: no BASE_SHA — skipping (nothing to diff against)."
  exit 0
fi
if ! git cat-file -e "${BASE_SHA}^{commit}" 2>/dev/null; then
  echo "lint-replay-plan: BASE_SHA ${BASE_SHA} not in local history — skipping." \
       "(checkout fetch-depth too shallow?)"
  exit 0
fi

# Derived on every run, not only when a plan is declared, so the PR that
# moves or reshapes the registry is the one that goes red.
if ! projected="$(projector_source_names)" || [[ -z "$projected" ]]; then
  echo "lint-replay-plan: FAIL — derived no projected source names from $SPEC_REGISTRY."
  echo "  The registry moved or changed shape. Re-aim projector_source_names in"
  echo "  scripts/ci/lint-replay-plan.sh; an empty set would let the wrong-command"
  echo "  advisory pass every plan while checking nothing."
  exit 1
fi

changed="$(changed_watched "$BASE_SHA" HEAD)"
if [[ -z "${changed//[[:space:]]/}" ]]; then
  echo "lint-replay-plan: no decoder / asset allow-list change in range — nothing to declare."
  exit 0
fi

# Range log body, captured into a variable rather than piped into grep -q
# (see has_replay_plan for the SIGPIPE-under-pipefail reason).
log_body="$(git log --format=%B "${BASE_SHA}..HEAD")"

if has_replay_plan "$log_body"; then
  echo "lint-replay-plan: decoder / asset allow-list change declared its replay plan:"
  declared="$(grep -iE '^Replay-Plan:' <<<"$log_body")"
  # shellcheck disable=SC2001  # per-LINE indent of a multi-line variable;
  # ${var//…} has no line anchor, so the suggested parameter expansion
  # cannot express this (see lint-docs.sh for the same annotation).
  sed 's/^/  /' <<<"$declared"
  warn_wrong_replay_command "$declared" "$projected"
  exit 0
fi

echo "UNDECLARED REPLAY PLAN — watched paths changed in ${BASE_SHA}..HEAD:"
while IFS= read -r f; do
  [[ -n "$f" ]] && echo "  ~ $f"
done <<<"$changed"

cat <<'EOF2'

lint-replay-plan: FAIL — a decoder or asset allow-list changed and no
commit in the range states what happens to already-served history.

A decoder / allow-list change alters what live ingestion RECORDS from the
moment it deploys, but nothing replays the rows written before it. On
2026-08-27 commit e17288bd widened the fiat allow-list (32→132 codes):
ingestion started recording 4 new currencies, nobody replayed history,
and 190,228 served rows were missing for a day — found only when a stale
gate binary was upgraded (2026-08-28). The plan must ship WITH the change.

Add a trailer to a commit message in the range:

    Replay-Plan: <what history is replayed, how, and by whom>

The command comes from the replay decision rule in
docs/architecture/ingest-pipeline.md ("The replay decision rule") — NOT
from memory. A projected source (a SourceSpec with a Projector in
internal/pipeline/source_spec.go) replays with projector-replay or
projected-rebuild; `backfill` is a MinIO walk and is never the answer.

e.g.

    Replay-Plan: stellarindex-ops projector-replay -source reflector-fx -from 61602787 on r1 after deploy; 4 new codes (see fx_quotes)

If NO already-served history is affected (pure refactor, source with no
rows yet, event that has never fired on mainnet), say so — and say why;
a bare `none` does not count:

    Replay-Plan: none — refactor only; decoded output byte-identical (golden test unchanged)

Watched: internal/canonical/asset_{fiat,crypto,rwa}.go and
internal/sources/*/{decode*,events,feeds,pairs}.go (not *_test.go).
EOF2
exit 1
