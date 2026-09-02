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
# That is exactly what happened on 2026-08-27/28: commit e17288bd widened
# internal/canonical/asset_fiat.go from 32 to 132 fiat codes. Live
# ingestion began recording 4 additional currencies immediately; nobody
# replayed history for them, and 190,228 served rows were missing for a
# day. The gap only surfaced once a stale gate binary was upgraded and
# started comparing against the widened set. The change was correct; the
# omission was the plan — and a plan that lives in someone's head is not
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
set -euo pipefail

cd "$(dirname "$0")/../.."

BASE_SHA="${BASE_SHA:-}"
ZERO_SHA="0000000000000000000000000000000000000000"

if [[ -z "$BASE_SHA" || "$BASE_SHA" == "$ZERO_SHA" ]]; then
  echo "lint-replay-plan: no BASE_SHA — skipping (nothing to diff against)."
  exit 0
fi
if ! git cat-file -e "${BASE_SHA}^{commit}" 2>/dev/null; then
  echo "lint-replay-plan: BASE_SHA ${BASE_SHA} not in local history — skipping." \
       "(checkout fetch-depth too shallow?)"
  exit 0
fi

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
# `set -o pipefail` — the trap lint-baseline-growth.sh (PR #38) documents.
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
# The source names are DERIVED from the projector registry (resolving
# each `case <pkg>.Source<X>:` through that package's constant), never
# listed here — a list here would be the same drift this whole gate
# exists to prevent. The count is printed so a derivation that silently
# resolves nothing is visible rather than reading as "all clear".
projector_source_names() {
  local reg=internal/projector/registry.go pkg const
  [[ -r "$reg" ]] || return 0
  grep -oE 'case [a-z0-9_]+\.Source[A-Za-z]*:' "$reg" |
    sed -E 's/^case //; s/:$//' |
    while IFS=. read -r pkg const; do
      # `<Const> = "<value>"` in the source package the case refers to.
      grep -rhoE "${const}[[:space:]]*=[[:space:]]*\"[^\"]+\"" "internal/sources/${pkg}" 2>/dev/null |
        sed -E 's/.*"([^"]+)".*/\1/'
    done | LC_ALL=C sort -u
}

# warn_wrong_replay_command <trailer-lines>
warn_wrong_replay_command() {
  local trailers="$1" names count line name
  names="$(projector_source_names)"
  count="$(grep -c . <<<"$names" || true)"
  echo "lint-replay-plan: cross-checked the declared plan against $count projector source name(s)"
  [[ "$count" -gt 0 ]] || return 0
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
  sed 's/^/  /' <<<"$declared"
  warn_wrong_replay_command "$declared"
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
from memory. A projected source (anything with a case in
internal/projector/registry.go) replays with projector-replay or
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
