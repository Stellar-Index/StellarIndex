#!/usr/bin/env bash
# ch-rebuild-projected.sh — ADR-0034 Phase-4 clean-slate rebuild of the
# PROJECTED (soroban_events-derived) sources from the ClickHouse lake.
#
# Why clean-slate (not additive upsert): the live AMM/projected trades were
# written through the collision-era event_index=0 bug, so their op_index — and
# thus the trades PK (source,ledger,tx_hash,op_index,ts) — differs from the
# correct CH re-derivation. An additive -write DOUBLES rows (ON CONFLICT can't
# dedup mismatched keys). So per 1M-ledger window: DELETE the window's rows,
# then ch-rebuild -write re-derives them from CH with correct keys.
#
# Scoped to <=62.894M (the CH backfill tip) so the live tail the indexer is
# still writing stays untouched; the delete/rebuild range never overlaps the
# indexer's current writes, so ingestion keeps running. Append-logged.
#
# The DELETE is destructive, so five rules bound it (F075, RLT-380, RLT-381, #782):
#
#   1. Ask first. Each window runs `ch-rebuild -write -preflight` BEFORE its
#      DELETE: the same BackfillSafe / live-cursor / buffered-range refusals
#      the real run would hit, for the same range and sources. Anything short
#      of the verdict line — a refusal, a binary that predates -preflight,
#      silence — deletes nothing.
#   2. Delete only what that verdict says will be re-derived. The DELETE is
#      built per source from the verdict's `rederive=` list (see
#      window_delete_sql), in ONE transaction, and the SAME list is what
#      -write is then given. SRC narrows the run and the DELETE with it. A
#      source this script has no DELETE map for is refused outright: it can
#      only be upserted additively, which is `ch-rebuild` run directly, not
#      this script. sushiswap_v3 is not here because it is not BackfillSafe:
#      the gate refuses to rewrite it, so it must never be deleted.
#   3. Never forget an emptied window. $DIRTY gets `lo hi sources` before the
#      DELETE and loses it only after the re-derive succeeds — and a run that
#      cannot write that record deletes nothing. If the re-derive dies in
#      between, the window IS emptied until this script runs again: the next
#      run — whatever its SRC/FROM/TO — rebuilds every dirty window first, for
#      exactly the sources that were deleted, and refuses to go on if it
#      cannot.
#   4. Never let an emptied window be certified complete. $DIRTY is a file on
#      this box; the ADR-0033 completeness verdict cannot see it. So every
#      state that leaves a window emptied — a failed re-derive, an ambiguous
#      DELETE, a window still pending in $DIRTY at the start of a run —
#      FILES the range as a projection dirty window per deleted source, by
#      running the command it also prints:
#        ch-rebuild -from LO -to HI -sources <deleted> -record-dirty-window
#      compute-completeness then re-reconciles that range instead of carrying
#      its prior clean claim over it, and clears the obligation only with the
#      verdict that discharges it (F072). Nothing here ever retracts one: this
#      script's own re-derive is the CAUSE of the dirtiness, not evidence
#      against it, so one re-reconcile per incident is the price of the claim.
#      The command is printed as well as run, because the filing can itself
#      fail (no binary, PG down): the run then says COULD NOT FILE and the
#      printed line is what the operator re-runs (F075).
#   5. Never leave the aggregates on the old rows. Every continuous aggregate
#      over `trades` (prices_*, twap_*, the volume rollups) has a refresh
#      policy that looks back minutes to months, never this far, so once a
#      window's trades are rewritten this script refreshes them over it with
#      `trades-cagg-refresh`, which also fails if prices_1m then disagrees
#      with `trades` over sampled windows. $STALE gets `lo hi` before a DELETE that
#      touches trades and loses it only after that refresh succeeds; the
#      next run — whatever its SRC/FROM/TO — refreshes every $STALE line
#      (after rebuilding its dirty windows) before anything else.
#
# Done-state ($STATE) is per source: `source lo hi`. A bare window start is
# the pre-per-source format, written by the full-SRC run, and still reads as
# "done for every source" — skipping is the non-destructive reading of it.
#
# A SUCCESSFUL window files no projection dirty window, on purpose (#408):
# filing one per window for 8 sources over [50M,62.894M] would force the next
# nightly compute-completeness to re-reconcile ~12.9M ledgers × 8
# un-prefiltered sources — a likely timeout that takes out EVERY source's
# verdict, which is worse than the stale claim it fixes. Rule 4 buys the claim
# back only where the served tier is certainly wrong rather than merely
# un-re-verified: the window is EMPTY.
#
# The one gap no in-process handler can close: a kill -9 or power loss
# between the DELETE and the filing. The emptied window survives only in
# $DIRTY, which no verdict reads — so the NEXT run of this script files every
# $DIRTY line before it recovers anything. Until a run happens, /v1/coverage
# still carries its prior clean claim over that range; if the box stays down,
# file each $DIRTY line by hand:
#   stellarindex-ops-ch ch-rebuild -config CFG -from LO -to HI \
#     -sources <the $DIRTY line's sources> -record-dirty-window
#
# NOT in scope: sdex (op-derived, correctly keyed), external/band (not
# CH-event-derived), reflector/redstone (exact, no collision — nothing to
# clean-slate; re-derive them with ch-rebuild directly if ever needed).
#
# This is the ONE sanctioned ch-rebuild over projected domains (the replay
# decision rule in docs/architecture/ingest-pipeline.md points here):
# additive upserts cannot repair a wrong PK, so the window is DELETEd first.
# Since #333 `ch-rebuild -write` reads the live projector's cursor per
# source and refuses a range the live tail is still inside — the TO<=62.894M
# scoping above already satisfies it, EXCEPT for a source whose own
# projector cursor is lagging behind TO (e.g. a held blend_backstop catch-up).
# Fix the lag (projector-replay / projected-rebuild) rather than reaching for
# -allow-live-overlap.
#
# Exit: 0 complete · 1 a window was refused or failed, or a CAGG refresh
# failed (read the log) · 2 bad SRC/FROM/TO/WIN (nothing touched), or a
# corrupt line in $DIRTY or $STALE (lines listed before it may already have
# been handled).
#
# Run on r1: nohup setsid bash scripts/ops/ch-rebuild-projected.sh >/dev/null 2>&1 &
set -uo pipefail
# Read a systemd EnvironmentFile VERBATIM — never `.`/source it. Its
# values are unquoted (that is what systemd wants), so the shell would
# expand `$`, split on `;`/`&`/`|`/whitespace and eat quotes inside a
# secret: the services keep working while this path gets a mangled DSN
# (deploy-ansible-secrets-5). Same reader as run-heavy-job.sh.
# usage: load_env_file FILE [export]
load_env_file() {
  local line
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      [A-Za-z_]*=*)
        if [ "${2:-}" = export ]; then
          export "${line?}"
        else
          printf -v "${line%%=*}" '%s' "${line#*=}"
        fi
        ;;
    esac
  done < "$1"
}
load_env_file /etc/default/stellarindex-ops export
OPS=${OPS:-/usr/local/bin/stellarindex-ops-ch}
CFG=${CFG:-/etc/stellarindex.toml}
DSN="$STELLARINDEX_POSTGRES_DSN"
SRC=${SRC:-"aquarius,soroswap,phoenix,comet,blend,cctp,rozo,defindex"}
FROM=${FROM:-50000000}; TO=${TO:-62894000}; WIN=${WIN:-1000000}
STATE=${STATE:-/var/lib/ch-backfill/rebuild-done-windows.txt}
DIRTY=${DIRTY:-"$STATE.dirty"}
STALE=${STALE:-"$STATE.caggs"}
LOG=${LOG:-/var/log/ch-rebuild-projected.log}
mkdir -p "$(dirname "$STATE")"; touch "$STATE" "$DIRTY" "$STALE"
exec >>"$LOG" 2>&1

# The sources window_delete_sql has a DELETE map for. Pinned against the
# reconciliation catalogue's table ownership by
# internal/ops/chops/ch_rebuild_projected_script_scope_test.go.
KNOWN_SOURCES="aquarius soroswap phoenix comet blend cctp rozo defindex"
TRADE_SOURCES="aquarius soroswap phoenix comet"

refuse() { echo "REFUSED: $* — nothing was touched"; exit 2; }
# A positive decimal with no leading zero: these reach SQL and $(( )).
is_ledger() { case "$1" in ''|0*|*[!0-9]*) return 1 ;; esac; return 0; }
in_words() { case " $2 " in *" $1 "*) return 0 ;; esac; return 1; }  # NAME "a b c"
in_csv() { case ",$2," in *",$1,"*) return 0 ;; esac; return 1; }    # NAME "a,b,c"
is_source_csv() { case "$1" in ''|*[!a-z0-9_,-]*|,*|*,|*,,*) return 1 ;; esac; return 0; }
has_trade_source() {  # CSV → true if any of it writes `trades`
  local s names
  IFS=, read -r -a names <<<"$1"
  for s in "${names[@]}"; do in_words "$s" "$TRADE_SOURCES" && return 0; done
  return 1
}

# unknown_in CSV → prints the first name with no DELETE map, if any.
unknown_in() {
  local s names
  IFS=, read -r -a names <<<"$1"
  for s in "${names[@]}"; do
    in_words "$s" "$KNOWN_SOURCES" || { printf '%s' "$s"; return 0; }
  done
}

# window_delete_sql CSV LO HI → the DELETE batch for exactly those sources.
# Every argument has been validated (is_source_csv + KNOWN_SOURCES, is_ledger)
# before it gets here. The trades IN list is the ceiling of what this script
# may ever delete from trades; the ANY list narrows it to this run.
window_delete_sql() {
  local csv="$1" lo="$2" hi="$3" s names trade_csv=""
  IFS=, read -r -a names <<<"$csv"
  for s in "${names[@]}"; do
    in_words "$s" "$TRADE_SOURCES" && trade_csv="${trade_csv:+$trade_csv,}$s"
  done
  echo "BEGIN;"
  if [ -n "$trade_csv" ]; then
    echo "DELETE FROM trades WHERE source IN ('aquarius','soroswap','phoenix','comet') AND source = ANY (string_to_array('$trade_csv', ',')) AND ledger BETWEEN $lo AND $hi;"
  fi
  for s in "${names[@]}"; do
    case "$s" in
      aquarius)
        echo "DELETE FROM aquarius_rewards_events WHERE ledger BETWEEN $lo AND $hi;"
        echo "DELETE FROM aquarius_admin WHERE ledger BETWEEN $lo AND $hi;"
        echo "DELETE FROM aquarius_protocol_fee WHERE ledger BETWEEN $lo AND $hi;"
        echo "DELETE FROM aquarius_kill_switches WHERE ledger BETWEEN $lo AND $hi;" ;;
      soroswap)
        echo "DELETE FROM soroswap_skim_events WHERE ledger BETWEEN $lo AND $hi;"
        echo "DELETE FROM soroswap_liquidity WHERE ledger BETWEEN $lo AND $hi;" ;;
      phoenix)
        echo "DELETE FROM phoenix_liquidity WHERE ledger BETWEEN $lo AND $hi;"
        echo "DELETE FROM phoenix_stake_events WHERE ledger BETWEEN $lo AND $hi;"
        echo "DELETE FROM phoenix_initialize WHERE ledger BETWEEN $lo AND $hi;"
        echo "DELETE FROM phoenix_admin_events WHERE ledger BETWEEN $lo AND $hi;" ;;
      comet) echo "DELETE FROM comet_liquidity WHERE ledger BETWEEN $lo AND $hi;" ;;
      cctp) echo "DELETE FROM cctp_events WHERE ledger BETWEEN $lo AND $hi;" ;;
      rozo) echo "DELETE FROM rozo_events WHERE ledger BETWEEN $lo AND $hi;" ;;
      defindex)
        echo "DELETE FROM defindex_flows WHERE ledger BETWEEN $lo AND $hi;"
        echo "DELETE FROM defindex_fees WHERE ledger BETWEEN $lo AND $hi;" ;;
      blend)
        echo "DELETE FROM blend_auctions WHERE ledger BETWEEN $lo AND $hi;"
        echo "DELETE FROM blend_positions WHERE ledger BETWEEN $lo AND $hi;"
        echo "DELETE FROM blend_emissions WHERE ledger BETWEEN $lo AND $hi;"
        echo "DELETE FROM blend_admin WHERE ledger BETWEEN $lo AND $hi;" ;;
    esac
  done
  echo "COMMIT;"
}

source_done() { grep -qx "$2" "$STATE" || grep -qxF "$1 $2 $3" "$STATE"; }  # SRC LO HI

# pending_sources LO HI → the $SRC sources not yet done for this window.
pending_sources() {
  local s names out=""
  IFS=, read -r -a names <<<"$SRC"
  for s in "${names[@]}"; do
    source_done "$s" "$1" "$2" || out="${out:+$out,}$s"
  done
  printf '%s' "$out"
}

mark_dirty() { grep -qxF "$1 $2 $3" "$DIRTY" || echo "$1 $2 $3" >> "$DIRTY"; }  # LO HI CSV

# dirty_window_note LO HI CSV — rule 4. $DIRTY is a file on this box and the
# ADR-0033 completeness verdict cannot see it, so an emptied window needs a
# SECOND record: a projection dirty window, which makes the next
# compute-completeness re-reconcile the range instead of carrying its prior
# clean claim over it. Printed by file_dirty_window, which then runs it.
dirty_window_note() {
  printf '%s\n' "TELL THE VERDICT [$1,$2] sources=$3 — until this is filed, /v1/coverage keeps carrying its prior clean claim over an EMPTY window. File it with:" \
    "  $OPS ch-rebuild -config $CFG -from $1 -to $2 -sources $3 -record-dirty-window" \
    "compute-completeness clears it only with the verdict that discharges it; this script never retracts one."
}
# file_dirty_window LO HI CSV — rule 4, carried out rather than suggested.
# Printing the command left the obligation on an operator reading this log,
# so between a failed re-derive and someone's attention /v1/coverage kept
# certifying an EMPTY window complete (F075). The note still goes to the log
# first: it is the fallback when the filing below cannot be made, and what an
# operator greps for. Non-fatal by itself — every caller either exits
# non-zero anyway or is on its way to the recovery that removes the hole.
file_dirty_window() {
  dirty_window_note "$1" "$2" "$3"
  $OPS ch-rebuild -config "$CFG" -from "$1" -to "$2" -sources "$3" -record-dirty-window && return 0
  echo "COULD NOT FILE the projection dirty window [$1,$2] sources=$3 — until the command above is run by hand, /v1/coverage keeps carrying its prior clean claim over this range."
  return 1
}

drop_line() {  # FILE LINE
  local rc=0
  grep -vxF "$2" "$1" > "$1.tmp" || rc=$?
  # grep -v exits 1 when nothing is left, which is the usual case.
  if [ "$rc" -gt 1 ]; then echo "cannot rewrite $1 (grep rc=$rc) — the window will be redone next run"; exit 1; fi
  mv "$1.tmp" "$1"
}
clear_dirty() { drop_line "$DIRTY" "$1 $2 $3"; }  # LO HI CSV

mark_stale() { grep -qxF "$1 $2" "$STALE" || echo "$1 $2" >> "$STALE"; }  # LO HI

# refresh_window LO HI — rule 5. Idempotent, so a retry only costs time.
refresh_window() {
  echo "--- window [$1,$2] CAGG REFRESH $(date -u) ---"
  $OPS trades-cagg-refresh -config "$CFG" -from "$1" -to "$2" \
    || { echo "CAGG REFRESH FAILED [$1,$2] — trades are re-derived, but every continuous aggregate over them still serves the pre-repair rows. Recorded in $STALE; the next run refreshes it first. By hand: $OPS trades-cagg-refresh -config $CFG -from $1 -to $2"
         exit 1; }
  drop_line "$STALE" "$1 $2"
}

# run_window LO HI CSV MODE — MODE is `normal` or `recover`. In recover mode
# CSV is what an earlier run DELETED, so all of it must be re-derivable now.
run_window() {
  local lo="$1" hi="$2" want="$3" mode="$4" pf verdict rederive s names bad skipped="" sql
  echo "--- window [$lo,$hi] PREFLIGHT sources=$want $(date -u) ---"
  pf=$($OPS ch-rebuild -config "$CFG" -from "$lo" -to "$hi" -sources "$want" -write -preflight) \
    || { echo "PREFLIGHT REFUSED [$lo,$hi] — nothing was deleted for this window"; exit 1; }
  verdict=$(sed -n '/^ch-rebuild: preflight ok \[/p' <<<"$pf")
  case "$verdict" in
    ''|*$'\n'*) echo "PREFLIGHT gave no single verdict [$lo,$hi] (stdout: '$pf') — nothing was deleted for this window"; exit 1 ;;
  esac
  echo "$verdict"
  rederive=${verdict##* rederive=}

  # The verdict is the DELETE set, so it is held to the same rules as SRC —
  # and it may only ever be NARROWER than what was asked.
  if [ -n "$rederive" ]; then
    is_source_csv "$rederive" || { echo "PREFLIGHT verdict carries a malformed rederive list '$rederive' — nothing was deleted"; exit 1; }
    bad=$(unknown_in "$rederive")
    [ -z "$bad" ] || { echo "PREFLIGHT says '$bad' would be re-derived and this script has no DELETE map for it — nothing was deleted"; exit 1; }
    IFS=, read -r -a names <<<"$rederive"
    for s in "${names[@]}"; do
      in_csv "$s" "$want" || { echo "PREFLIGHT says '$s' would be re-derived but only '$want' was asked for — nothing was deleted"; exit 1; }
    done
  fi
  IFS=, read -r -a names <<<"$want"
  for s in "${names[@]}"; do
    in_csv "$s" "$rederive" || skipped="${skipped:+$skipped,}$s"
  done
  if [ -n "$skipped" ]; then
    if [ "$mode" = recover ]; then
      echo "CANNOT RECOVER [$lo,$hi]: '$skipped' was DELETED by an earlier run and ch-rebuild no longer re-derives it under this config. The window stays recorded in $DIRTY; nothing was done. Fix the config/binary and re-run."
      exit 1
    fi
    echo "window [$lo,$hi] SKIPPED sources=$skipped — ch-rebuild would not re-derive them under this config, so they were NOT deleted and are NOT marked done"
  fi
  if [ -z "$rederive" ]; then
    echo "window [$lo,$hi] nothing to re-derive — nothing deleted"
    return 0
  fi

  # Rule 3 is only a rule if the record is CHECKED: an unwritable state dir
  # or a full disk would otherwise lose the marker and delete anyway, and
  # nothing would rebuild the window.
  mark_dirty "$lo" "$hi" "$rederive" \
    || { echo "CANNOT RECORD [$lo,$hi] sources=$rederive in $DIRTY — nothing was deleted for this window. Fix the state directory (space, permissions) and re-run."; exit 1; }
  if has_trade_source "$rederive"; then
    mark_stale "$lo" "$hi" \
      || { echo "CANNOT RECORD [$lo,$hi] in $STALE — nothing was deleted for this window. Fix the state directory (space, permissions) and re-run."; exit 1; }
  fi
  sql=$(window_delete_sql "$rederive" "$lo" "$hi")
  echo "--- window [$lo,$hi] DELETE sources=$rederive $(date -u) ---"
  echo "$sql"
  # One transaction: psql autocommits per statement otherwise, and a failure
  # part-way would leave the earlier tables emptied. ON_ERROR_STOP quits at
  # the first error, before COMMIT, and the open transaction rolls back. The
  # dirty marker stays regardless: a failure ON the COMMIT is ambiguous, and
  # redoing a window that turned out intact costs only time.
  psql "$DSN" -v ON_ERROR_STOP=1 <<<"$sql" \
    || { echo "DELETE FAILED [$lo,$hi] — one transaction, so rolled back unless the failure was the COMMIT itself. Recorded in $DIRTY; the next run redoes this window first."
         # Ambiguous by construction: a failure ON the COMMIT deleted the
         # rows. So the verdict is told the range may be emptied — a
         # spurious window costs one re-reconcile, the other way round
         # costs a certified hole.
         file_dirty_window "$lo" "$hi" "$rederive"
         exit 1; }

  echo "--- window [$lo,$hi] REBUILD sources=$rederive $(date -u) ---"
  $OPS ch-rebuild -config "$CFG" -from "$lo" -to "$hi" -sources "$rederive" -write \
    || { echo "REBUILD FAILED [$lo,$hi] — WINDOW LEFT EMPTIED for sources=$rederive. Recorded in $DIRTY. Re-run this script: it rebuilds this window first, for exactly these sources, whatever SRC/FROM/TO it is given. Do not hand-edit $STATE or $DIRTY."
         file_dirty_window "$lo" "$hi" "$rederive"
         exit 1; }

  IFS=, read -r -a names <<<"$rederive"
  for s in "${names[@]}"; do echo "$s $lo $hi" >> "$STATE"; done
  clear_dirty "$lo" "$hi" "$rederive"
  if has_trade_source "$rederive"; then refresh_window "$lo" "$hi"; fi
  echo "window [$lo,$hi] DONE sources=$rederive $(date -u)"
}

echo "=== ch-rebuild-projected START $(date -u) [$FROM,$TO] sources=$SRC ==="
if ! { is_ledger "$FROM" && is_ledger "$TO" && is_ledger "$WIN"; }; then
  refuse "FROM/TO/WIN must be positive integers (got FROM='$FROM' TO='$TO' WIN='$WIN')"
fi
is_source_csv "$SRC" || refuse "SRC is not a comma-separated source list: '$SRC'"
bad=$(unknown_in "$SRC")
[ -z "$bad" ] || refuse "SRC names '$bad', which this script has no DELETE map for (it knows: $KNOWN_SOURCES). It would be upserted additively with nothing deleted — run ch-rebuild directly for that"

# $DIRTY is the only local record that a window was emptied, so a run that
# cannot both read it and append to it may not delete anything (rule 3). The
# probe is the real operation — an append — because the state directory may
# be missing, full, read-only, or occupied by something that is not a file.
if ! { : >> "$DIRTY" && [ -f "$DIRTY" ] && [ -r "$DIRTY" ]; }; then
  refuse "\$DIRTY ($DIRTY) is not a readable, appendable file — it is the only record that a window was emptied, and without it a DELETE could be forgotten"
fi
if ! { : >> "$STALE" && [ -f "$STALE" ] && [ -r "$STALE" ]; }; then
  refuse "\$STALE ($STALE) is not a readable, appendable file — it is the only record that a window's continuous aggregates still hold the pre-repair trades"
fi

# ── recovery first: windows an earlier run emptied and did not rebuild ──
dirty_lines=()
while IFS= read -r line || [ -n "$line" ]; do
  [ -n "$line" ] && dirty_lines+=("$line")
done < "$DIRTY"
if [ "${#dirty_lines[@]}" -gt 0 ]; then
  echo "=== RECOVERY: ${#dirty_lines[@]} window(s) were emptied by an earlier run and not rebuilt ==="
  for line in "${dirty_lines[@]}"; do
    read -r dlo dhi dsrcs extra <<<"$line"
    if ! { is_ledger "${dlo:-}" && is_ledger "${dhi:-}" && is_source_csv "${dsrcs:-}" && [ -z "${extra:-}" ] && [ -z "$(unknown_in "$dsrcs")" ]; }; then
      echo "REFUSED: corrupt line in $DIRTY: '$line' — an emptied window may be recorded there; repair it by hand before re-running"; exit 2
    fi
    file_dirty_window "$dlo" "$dhi" "$dsrcs" \
      || echo "recovering [$dlo,$dhi] anyway — a recovery that succeeds removes the hole itself, and one that fails files the window again"
    run_window "$dlo" "$dhi" "$dsrcs" recover
  done
fi

# ── then the refreshes an earlier run owed (rule 5) ──
stale_lines=()
while IFS= read -r line || [ -n "$line" ]; do
  [ -n "$line" ] && stale_lines+=("$line")
done < "$STALE"
for line in ${stale_lines[@]+"${stale_lines[@]}"}; do  # bash 3.2 + set -u: empty "${a[@]}" is unbound
  read -r slo shi extra <<<"$line"
  if ! { is_ledger "${slo:-}" && is_ledger "${shi:-}" && [ -z "${extra:-}" ]; }; then
    echo "REFUSED: corrupt line in $STALE: '$line' — repair it by hand before re-running"; exit 2
  fi
  refresh_window "$slo" "$shi"
done

w=$FROM
while [ "$w" -le "$TO" ]; do
  hi=$((w+WIN-1)); [ "$hi" -gt "$TO" ] && hi=$TO
  todo=$(pending_sources "$w" "$hi")
  [ -z "$todo" ] || run_window "$w" "$hi" "$todo" normal
  w=$((w+WIN))
done
echo "=== ch-rebuild-projected COMPLETE $(date -u) ==="
