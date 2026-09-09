#!/usr/bin/env bash
# lint-source-enablement.sh — "registered, tracked, and never actually run".
#
# THE BUG CLASS, observed TWICE on production:
#
#   A protocol decoder is written, merged and deployed. It is registered at
#   every site the completeness verdict reads — projector registry, sink,
#   gated registry, the compute-completeness reconciliation catalogue,
#   gap-detector targets, sourcenet, the source metadata registry — so
#   /v1/coverage starts asserting a verdict about it. But the source is never
#   added to `stellarindex_enabled_sources` in
#   configs/ansible/roles/archival-node/defaults/main.yml, which is the only
#   path to `[ingestion] enabled_sources` in /etc/stellarindex.toml and
#   therefore the only thing that makes a projector run for it.
#
#   The result is silent by construction: the archive is complete, the lake
#   holds the events, /v1/coverage reports the source INCOMPLETE forever with
#   watermark_ledger stuck one below genesis, and the served tier is
#   permanently EMPTY. Nothing crashes, no unit fails, no alert names a cause.
#
#   1. sushiswap_v3 — decoder in 3f575923e, tracked by the completeness
#      surface from that day, absent from enabled_sources until 72ad4ad4d
#      (2026-09-09). It was the sole incomplete source of 21 and the standing
#      cause of stellarindex_completeness_incomplete. The omission was even
#      DECLARED, in prose, in the CHANGELOG entry that shipped it ("Not yet in
#      the r1 enabled_sources list") — a declaration in a 26,000-line file is
#      not a mechanism.
#   2. upshift (#503) — found hours after fixing the first, the same shape:
#      73/74 `upshift` string references in the deployed binaries, no
#      projector cursor (only a gap-detector-scan one), recognition_ok:false,
#      lake_complete:false, coverage_pct 0, watermark == genesis - 1.
#
# THE INVARIANT this gate enforces, in six parts. `KNOWN` is
# internal/config/validate.go's KnownSources — the authoritative whitelist of
# names `[ingestion] enabled_sources` accepts (docs/operations/self-hosting.md
# §5 says so, and config.Validate rejects anything else at boot). `ENABLED` is
# the pubnet ansible default. `WAIVED` is the declared not-yet-enabled set.
# `CATALOGUE` is the compute-completeness reconciliation catalogue, i.e. the
# set /v1/coverage publishes a verdict for. `ALWAYS_ON` is the set the
# projector registers WITHOUT an enabled_sources entry (the sep41 domain,
# F-1316 SKIP-SOLE-WRITER).
#
#   §1  KNOWN ⊆ ENABLED ∪ WAIVED   a runnable source is run, or declared not
#                                  to be. THIS is the two production defects.
#   §2  ENABLED ⊆ KNOWN            an enabled name config.Validate would
#                                  reject is a boot failure on the next apply
#   §3  WAIVED ∩ ENABLED = ∅       a name cannot be both run and declared unrun
#   §4  WAIVED ∩ CATALOGUE = ∅     THE TEETH. Waiving a source the completeness
#                                  machinery tracks re-creates the exact
#                                  defect: a permanently-red /v1/coverage row
#                                  nobody can act on. If we are not running it,
#                                  we must not publish a verdict about it.
#   §5  CATALOGUE ⊆ KNOWN ∪ ALWAYS_ON
#                                  a source tracked for completeness that no
#                                  operator can even name in enabled_sources is
#                                  §1 evaded — same permanent red
#   §6  WAIVED ⊆ KNOWN             a stale waiver for a source that no longer
#                                  exists hides the next one
#   §7  no PUBNET inventory sets stellarindex_enabled_sources
#                                  r1/r2/r3 take the list from the role
#                                  default, which is the only reason checking
#                                  that default says anything about what
#                                  pubnet runs. An inventory-level override
#                                  would shadow it and make §1 a verdict about
#                                  a list nobody applies.
#
# THE ESCAPE HATCH is deliberate, greppable, and lives on the ANSIBLE side:
#
#   stellarindex_sources_not_yet_enabled:
#     <source>: "<why it is written but not run>"
#
# — in the same file, immediately below the list it exempts, so enabling a
# source is a one-line move from one block to the other. It is on the ansible
# side ON PURPOSE: the whole bug is that someone edits six Go files and never
# opens the deployment default. This gate makes that file unavoidable,
# whichever answer is given. And §4 stops the hatch becoming the bug: you may
# defer RUNNING a decoder, you may not defer it while still CLAIMING a verdict.
#
# SCOPE. Only the PUBNET default is checked. The testnet/futurenet inventories
# deliberately carry `[sdex]` alone, and sourcenet.Applicable() reports every
# contract-anchored source as not-applicable there, so their coverage verdicts
# EXCLUDE those sources rather than reading permanently red. The gate asserts
# the defaults file still declares stellar_network: pubnet, so that reasoning
# cannot silently stop holding.
#
# NON-VACUITY. Every extracted set has a floor and every parse is strict: an
# unrecognised line inside any of the five blocks is a hard failure, not a
# skipped entry. A gate whose subject set silently emptied would report OK
# while checking nothing — this repo has been bitten by that shape more than
# once, so each extraction fails loudly instead.
#
# Run: bash scripts/ci/lint-source-enablement.sh
set -euo pipefail

cd "$(dirname "$0")/../.."

ANSIBLE_DEFAULTS=configs/ansible/roles/archival-node/defaults/main.yml
INVENTORY_DIR=configs/ansible/inventory
KNOWN_GO=internal/config/validate.go
CATALOGUE_GO=internal/ops/chops/reconciliation_catalogue.go
PROJECTOR_GO=internal/projector/registry.go
SOURCES_DIR=internal/sources

ENABLED_KEY=stellarindex_enabled_sources
WAIVER_KEY=stellarindex_sources_not_yet_enabled

# Floors for the non-vacuity guards. Deliberately well below today's counts
# (20 known / 19 enabled / 22 catalogue) — they exist to catch an extraction
# that has stopped matching, not to freeze the registry size.
MIN_KNOWN=10
MIN_ENABLED=10
MIN_CATALOGUE=10

rc=0

die() { # unrecoverable: the gate could not establish its subject set
	echo "lint-source-enablement: FAIL — $*" >&2
	exit 1
}

bullets() {
	local line
	while IFS= read -r line; do
		[ -n "$line" ] && echo "        - $line"
	done <<<"$1"
}

count() { printf '%s\n' "$1" | grep -c . || true; }

# set_minus A B — lines in A that are not in B. Blank-tolerant: an empty set
# is the empty string, which comm would otherwise read as one empty line.
set_minus() {
	comm -23 <(printf '%s\n' "$1" | grep -v '^$' | sort -u) \
		<(printf '%s\n' "$2" | grep -v '^$' | sort -u) || true
}

# set_intersect A B
set_intersect() {
	comm -12 <(printf '%s\n' "$1" | grep -v '^$' | sort -u) \
		<(printf '%s\n' "$2" | grep -v '^$' | sort -u) || true
}

# ─── 0. Load-bearing inputs ─────────────────────────────────────────
for f in "$ANSIBLE_DEFAULTS" "$KNOWN_GO" "$CATALOGUE_GO" "$PROJECTOR_GO"; do
	[ -f "$f" ] || die "required input missing: $f (a rename must red this gate, not hollow it out)"
done
[ -d "$SOURCES_DIR" ] || die "required input missing: $SOURCES_DIR/"
[ -d "$INVENTORY_DIR" ] || die "required input missing: $INVENTORY_DIR/ (§7 could not run)"

# ─── 1. The defaults file must still be the pubnet one ──────────────
if ! grep -qE '^stellar_network:[[:space:]]+"pubnet"' "$ANSIBLE_DEFAULTS"; then
	die "$ANSIBLE_DEFAULTS no longer declares stellar_network: \"pubnet\".
      This gate's premise is that the default role IS the pubnet deployment, where
      every contract-anchored source is applicable. Re-derive it before trusting a pass."
fi

# ─── 2. ENABLED — the pubnet enabled-sources list ───────────────────
#
# Strict reader: inside the block every line must be blank, a comment, or a
# scalar list item. Anything else (a nested mapping, a Jinja expression, a
# quoted string with a space) is REPORTED rather than skipped, because a
# skipped entry is exactly how this gate would go quietly vacuous.
read_yaml_list() { # <file> <key>
	awk -v key="$2" '
		$0 == key ":" { inb = 1; next }
		inb && /^[^[:space:]#]/ { inb = 0 }
		inb {
			if ($0 ~ /^[[:space:]]*$/) next
			if ($0 ~ /^[[:space:]]*#/) next
			if ($0 ~ /^  - "?[a-z0-9_-]+"?[[:space:]]*$/) {
				v = $0
				sub(/^  -[[:space:]]*"?/, "", v)
				sub(/"?[[:space:]]*$/, "", v)
				print "ITEM " v
				next
			}
			print "BAD " $0
		}
	' "$1"
}

enabled_raw=$(read_yaml_list "$ANSIBLE_DEFAULTS" "$ENABLED_KEY")
enabled_bad=$(printf '%s\n' "$enabled_raw" | sed -n 's/^BAD //p')
if [ -n "$enabled_bad" ]; then
	echo "lint-source-enablement: FAIL — $ENABLED_KEY in $ANSIBLE_DEFAULTS has line(s) this" >&2
	echo "      gate cannot parse. Refusing to check a list it only half-read:" >&2
	bullets "$enabled_bad" >&2
	exit 1
fi
ENABLED=$(printf '%s\n' "$enabled_raw" | sed -n 's/^ITEM //p' | sort -u)
n_enabled=$(count "$ENABLED")
[ "$n_enabled" -ge "$MIN_ENABLED" ] ||
	die "$ENABLED_KEY extracted only $n_enabled name(s) from $ANSIBLE_DEFAULTS (floor
      $MIN_ENABLED). The block moved or its shape changed, and this gate is checking nothing."

dupes=$(printf '%s\n' "$enabled_raw" | sed -n 's/^ITEM //p' | sort | uniq -d)
if [ -n "$dupes" ]; then
	echo "lint-source-enablement: FAIL — $ENABLED_KEY lists the same source twice:" >&2
	bullets "$dupes" >&2
	rc=1
fi

# ─── 3. WAIVED — the declared not-yet-enabled set ───────────────────
#
# Two legal shapes: `key: {}` (nothing waived) or a block mapping of
# `  <source>: "<reason>"`. The reason is MANDATORY — a bare name is a
# silence with extra steps, which is the thing this gate exists to end.
read_yaml_reason_map() { # <file> <key>
	awk -v key="$2" '
		$0 == key ": {}" { found = 1; next }
		$0 == key ":" { inb = 1; found = 1; next }
		inb && /^[^[:space:]#]/ { inb = 0 }
		inb {
			if ($0 ~ /^[[:space:]]*$/) next
			if ($0 ~ /^[[:space:]]*#/) next
			if ($0 ~ /^  [a-z0-9_-]+:[[:space:]]*"[^"]+"[[:space:]]*$/) {
				k = $0
				sub(/^  /, "", k)
				sub(/:.*$/, "", k)
				print "ITEM " k
				next
			}
			print "BAD " $0
		}
		END { if (!found) print "ABSENT" }
	' "$1"
}

waived_raw=$(read_yaml_reason_map "$ANSIBLE_DEFAULTS" "$WAIVER_KEY")
if grep -qx 'ABSENT' <<<"$waived_raw"; then
	die "$ANSIBLE_DEFAULTS has no \`$WAIVER_KEY\` key.
      It is the ONLY declared way to say \"this decoder exists but is deliberately not
      run\", and without it the escape hatch is silence again. Restore it
      (\`$WAIVER_KEY: {}\` when nothing is waived)."
fi
waived_bad=$(printf '%s\n' "$waived_raw" | sed -n 's/^BAD //p')
if [ -n "$waived_bad" ]; then
	echo "lint-source-enablement: FAIL — $WAIVER_KEY in $ANSIBLE_DEFAULTS has line(s) this" >&2
	echo "      gate cannot parse. Each entry is \`  <source>: \"<reason it is not run>\"\`," >&2
	echo "      and the reason is mandatory:" >&2
	bullets "$waived_bad" >&2
	exit 1
fi
WAIVED=$(printf '%s\n' "$waived_raw" | sed -n 's/^ITEM //p' | sort -u)

# ─── 4. KNOWN — config.KnownSources ─────────────────────────────────
known_raw=$(awk '
	/^var KnownSources = map\[string\]struct\{\}\{$/ { inb = 1; next }
	inb && /^\}/ { inb = 0 }
	inb {
		if ($0 ~ /^[[:space:]]*$/) next
		if ($0 ~ /^[[:space:]]*\/\//) next
		if ($0 ~ /^\t"[a-z0-9_-]+":[[:space:]]*\{\},$/) {
			v = $0
			sub(/^\t"/, "", v)
			sub(/".*$/, "", v)
			print "ITEM " v
			next
		}
		print "BAD " $0
	}
' "$KNOWN_GO")
known_bad=$(printf '%s\n' "$known_raw" | sed -n 's/^BAD //p')
if [ -n "$known_bad" ]; then
	echo "lint-source-enablement: FAIL — config.KnownSources in $KNOWN_GO has entries this" >&2
	echo "      gate cannot parse; it would silently check a subset:" >&2
	bullets "$known_bad" >&2
	exit 1
fi
KNOWN=$(printf '%s\n' "$known_raw" | sed -n 's/^ITEM //p' | sort -u)
n_known=$(count "$KNOWN")
[ "$n_known" -ge "$MIN_KNOWN" ] ||
	die "config.KnownSources extracted only $n_known name(s) from $KNOWN_GO (floor
      $MIN_KNOWN). The declaration moved, and this gate is checking nothing."

# ─── 5. CATALOGUE — what compute-completeness publishes a verdict for ──
#
# Entries spell their name either as a literal or as <pkg>.SourceName.
# Resolving the symbolic form matters: four of the twenty-two use it, and a
# grep that quietly dropped them would take the sep41 domain and two others
# out of §4/§5 with no visible change.
resolve_source_name() { # <go-file> <package-ident>
	local file="$1" ident="$2" path dir gf vals
	# `|| true` on every no-match-able grep: under `set -e` + pipefail an
	# unmatched grep aborts the assignment, and the caller's own
	# "extracted nothing" guard never runs — the gate would exit 1 with an
	# EMPTY message instead of naming what it could not read.
	path=$(grep -oE "^[[:space:]]*${ident} \"github\.com/[^\"]+\"" "$file" |
		grep -oE '"[^"]+"' | tr -d '"' | sort -u || true)
	if [ -z "$path" ]; then
		path=$(grep -oE "\"github\.com/[^\"]+/${ident}\"" "$file" | tr -d '"' | sort -u || true)
	fi
	[ -n "$path" ] || return 1
	[ "$(count "$path")" -eq 1 ] || return 1
	dir="${path##*/}"
	vals=""
	for gf in "$SOURCES_DIR/$dir"/*.go; do
		[ -f "$gf" ] || continue
		case "$gf" in *_test.go) continue ;; esac
		vals="${vals}$(grep -oE '[[:space:]]SourceName[[:space:]]*=[[:space:]]*"[a-z0-9_-]+"' "$gf" |
			grep -oE '"[a-z0-9_-]+"' | tr -d '"' || true)"$'\n'
	done
	vals=$(printf '%s' "$vals" | grep -v '^$' | sort -u)
	[ "$(count "$vals")" -eq 1 ] || return 1
	printf '%s\n' "$vals"
}

# The `({|<space>)name:` anchor keeps a field like `a_name:` out of the
# capture; both spellings the catalogue uses (`{name: "x"` at the head of an
# inline literal, and a tab-indented `name:` on its own line) are covered.
CATALOGUE=""
cat_tokens=$(grep -oE '(\{|[[:space:]])name:[[:space:]]+("[a-z0-9_-]+"|[a-z0-9_]+\.SourceName)' "$CATALOGUE_GO" |
	sed -E 's/^.*name:[[:space:]]+//' | sort -u || true)
[ -n "$cat_tokens" ] ||
	die "no reconSource entries found in $CATALOGUE_GO.
      The catalogue's shape changed and §4/§5 would check nothing."
while IFS= read -r tok; do
	[ -n "$tok" ] || continue
	case "$tok" in
	'"'*)
		CATALOGUE="${CATALOGUE}$(printf '%s' "$tok" | tr -d '"')"$'\n'
		;;
	*.SourceName)
		ident="${tok%.SourceName}"
		resolved=$(resolve_source_name "$CATALOGUE_GO" "$ident") ||
			die "cannot resolve ${tok} in $CATALOGUE_GO to a source name.
      Every catalogue entry must resolve, or §4/§5 silently stop covering it."
		CATALOGUE="${CATALOGUE}${resolved}"$'\n'
		;;
	esac
done <<<"$cat_tokens"
CATALOGUE=$(printf '%s' "$CATALOGUE" | grep -v '^$' | sort -u || true)
n_catalogue=$(count "$CATALOGUE")
[ "$n_catalogue" -ge "$MIN_CATALOGUE" ] ||
	die "the reconciliation catalogue extracted only $n_catalogue name(s) from
      $CATALOGUE_GO (floor $MIN_CATALOGUE). The extraction has stopped matching."

# ─── 6. ALWAYS_ON — projector sources with no enabled_sources entry ──
#
# BuildRegistry registers the sep41 domain unconditionally (F-1316
# SKIP-SOLE-WRITER: the dispatcher cedes it to the projector, and the sep41
# names are not in KnownSources so they can never legally appear in
# enabled_sources). Those are the only catalogue entries §5 may excuse, and the
# excuse is read from the code that grants it rather than hardcoded here.
ALWAYS_ON=""
always_tokens=$(grep -oE 'range \[\]string\{[a-z0-9_]+\.SourceName(,[[:space:]]*[a-z0-9_]+\.SourceName)*\}' "$PROJECTOR_GO" |
	grep -oE '[a-z0-9_]+\.SourceName' | sort -u || true)
[ -n "$always_tokens" ] ||
	die "no unconditional projector source list found in $PROJECTOR_GO.
      §5 would then demand every catalogue entry be in KnownSources, which is the right
      direction for the wrong reason — re-aim the extraction first."
while IFS= read -r tok; do
	[ -n "$tok" ] || continue
	ident="${tok%.SourceName}"
	resolved=$(resolve_source_name "$PROJECTOR_GO" "$ident") ||
		die "cannot resolve ${tok} in $PROJECTOR_GO to a source name."
	ALWAYS_ON="${ALWAYS_ON}${resolved}"$'\n'
done <<<"$always_tokens"
ALWAYS_ON=$(printf '%s' "$ALWAYS_ON" | grep -v '^$' | sort -u || true)

# ─── §1 KNOWN ⊆ ENABLED ∪ WAIVED ────────────────────────────────────
unrun=$(set_minus "$KNOWN" "$(printf '%s\n%s\n' "$ENABLED" "$WAIVED")")
if [ -n "$unrun" ]; then
	echo "lint-source-enablement: FAIL — source(s) the code can run that NOTHING runs:" >&2
	bullets "$unrun" >&2
	cat >&2 <<EOF

      Each is in config.KnownSources (so the decoder shipped) but is absent from
      $ENABLED_KEY in
      $ANSIBLE_DEFAULTS,
      the only path to \`[ingestion] enabled_sources\` in /etc/stellarindex.toml.
      No projector will ever run for it: the lake fills, /v1/coverage reports the
      source INCOMPLETE forever with watermark_ledger stuck one below genesis, and
      the served tier stays empty. That is the sushiswap_v3 (2026-09-09) and
      upshift (#503) shape exactly.

      Fix it one of two ways, both of them edits to the ansible defaults:
        - RUN IT: add the name to $ENABLED_KEY.
        - DECLARE IT: add \`<source>: "<why>"\` under $WAIVER_KEY
          AND remove its reconSource entry from
          $CATALOGUE_GO, because a source
          we do not run must not publish a verdict about itself.
EOF
	rc=1
fi

# ─── §2 ENABLED ⊆ KNOWN ─────────────────────────────────────────────
unknown=$(set_minus "$ENABLED" "$KNOWN")
if [ -n "$unknown" ]; then
	echo "lint-source-enablement: FAIL — $ENABLED_KEY names source(s) config.KnownSources" >&2
	echo "      does not know. config.Validate rejects these at boot, so the next apply" >&2
	echo "      would take the indexer down:" >&2
	bullets "$unknown" >&2
	rc=1
fi

# ─── §3 WAIVED ∩ ENABLED = ∅ ────────────────────────────────────────
both=$(set_intersect "$WAIVED" "$ENABLED")
if [ -n "$both" ]; then
	echo "lint-source-enablement: FAIL — source(s) declared not-yet-enabled while ALSO being" >&2
	echo "      enabled. One of the two statements is stale and a reader cannot tell which:" >&2
	bullets "$both" >&2
	rc=1
fi

# ─── §4 WAIVED ∩ CATALOGUE = ∅ ──────────────────────────────────────
tracked_waived=$(set_intersect "$WAIVED" "$CATALOGUE")
if [ -n "$tracked_waived" ]; then
	echo "lint-source-enablement: FAIL — source(s) declared not-yet-enabled that the" >&2
	echo "      completeness machinery still tracks:" >&2
	bullets "$tracked_waived" >&2
	cat >&2 <<EOF

      This is the original defect wearing a waiver. compute-completeness will keep
      writing a completeness_snapshots row for each, /v1/coverage will keep
      publishing it, and it can only ever read complete=false, coverage_pct=0,
      watermark_ledger=genesis-1 — a permanently red row nobody can act on, which
      teaches every reader to ignore the board.

      Deferring a source means deferring the CLAIM too: remove its reconSource
      entry from $CATALOGUE_GO
      until the day it is enabled.
EOF
	rc=1
fi

# ─── §5 CATALOGUE ⊆ KNOWN ∪ ALWAYS_ON ───────────────────────────────
unrunnable=$(set_minus "$CATALOGUE" "$(printf '%s\n%s\n' "$KNOWN" "$ALWAYS_ON")")
if [ -n "$unrunnable" ]; then
	echo "lint-source-enablement: FAIL — source(s) tracked for completeness that no operator" >&2
	echo "      can enable:" >&2
	bullets "$unrunnable" >&2
	cat >&2 <<EOF

      They have a reconSource entry in
      $CATALOGUE_GO — so /v1/coverage
      publishes a verdict — but they are neither in config.KnownSources (which
      config.Validate checks $ENABLED_KEY against, so naming one there
      fails the indexer at boot) nor registered unconditionally by the projector.
      The verdict can therefore never go green. Add the name to KnownSources and to
      $ENABLED_KEY, or drop the catalogue entry.
EOF
	rc=1
fi

# ─── §6 WAIVED ⊆ KNOWN ──────────────────────────────────────────────
stale=$(set_minus "$WAIVED" "$KNOWN")
if [ -n "$stale" ]; then
	echo "lint-source-enablement: FAIL — stale entry in $WAIVER_KEY — config.KnownSources" >&2
	echo "      has no such source, so the waiver excuses nothing and hides the next one:" >&2
	bullets "$stale" >&2
	rc=1
fi

# ─── §7 no pubnet inventory shadows the role default ────────────────
#
# The whole gate is a statement about one list, and it is only a statement
# about what PUBNET runs because r1/r2/r3 inherit that list from the role
# default rather than setting their own. testnet/futurenet DO override it
# (they run `[sdex]` alone, and sourcenet reports every contract-anchored
# source as not-applicable there, so their coverage verdicts EXCLUDE those
# sources instead of reading permanently red) — which is why the discriminator
# here is the inventory's own stellar_network and not its filename.
shadowed=""
n_pubnet_inv=0
for inv in "$INVENTORY_DIR"/*.yml; do
	[ -f "$inv" ] || continue
	grep -qE '^[[:space:]]*stellar_network:[[:space:]]+"pubnet"' "$inv" || continue
	n_pubnet_inv=$((n_pubnet_inv + 1))
	if grep -qE '^[[:space:]]*stellarindex_enabled_sources:' "$inv"; then
		shadowed="${shadowed}${inv}"$'\n'
	fi
done
if [ "$n_pubnet_inv" -eq 0 ]; then
	die "no pubnet inventory found under $INVENTORY_DIR/.
      Every check above is a claim about the list PUBNET runs, and with no pubnet
      inventory to inherit it there is nothing to make that claim about."
fi
if [ -n "$shadowed" ]; then
	echo "lint-source-enablement: FAIL — pubnet inventory file(s) override" >&2
	echo "      $ENABLED_KEY:" >&2
	bullets "$shadowed" >&2
	cat >&2 <<EOF

      A pubnet host that carries its own list no longer inherits the role default,
      so every check above becomes a verdict about a list that host does not apply —
      this gate would go green while the source it is protecting is still unrun there.
      Keep the pubnet list in $ANSIBLE_DEFAULTS
      (ADR-0015: the source fleet is region-INVARIANT by design). Non-pubnet
      inventories may override it; sourcenet excludes those sources from their
      verdicts rather than reporting them permanently incomplete.
EOF
	rc=1
fi

if [ "$rc" -eq 0 ]; then
	echo "lint-source-enablement: OK — $n_known known source(s): $n_enabled enabled," \
		"$(count "$WAIVED") declared not-yet-enabled; $n_catalogue tracked by" \
		"compute-completeness, $(count "$ALWAYS_ON") of them projector-unconditional;" \
		"$n_pubnet_inv pubnet inventory file(s) inherit the list."
fi
exit "$rc"
