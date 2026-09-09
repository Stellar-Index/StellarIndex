#!/usr/bin/env bash
# lint-source-enablement-test.sh — fixtures for the "registered, tracked,
# never run" gate (scripts/ci/lint-source-enablement.sh).
#
# The gate's whole value is that it REDS on a shape nothing else notices, so
# every one of its six checks is proven here by mutation: build a consistent
# fixture repo, break exactly one thing, assert the gate names it. A gate whose
# extraction has silently stopped matching reports OK over an empty subject
# set — which is why the vacuity cases below (empty list, moved declaration,
# renamed block, deleted waiver key) are proven to FAIL rather than pass.
#
# The fixtures build a throwaway repo root and run the REAL script against it
# (the script derives its root from its own path), so a regression in
# lint-source-enablement.sh reds this test.
#
# Run: bash scripts/ci/lint-source-enablement-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SRC="$PWD/scripts/ci/lint-source-enablement.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

ROOT="$TMP/repo"
DEFAULTS="$ROOT/configs/ansible/roles/archival-node/defaults/main.yml"
INV_PUBNET="$ROOT/configs/ansible/inventory/r9.example.yml"
INV_TESTNET="$ROOT/configs/ansible/inventory/testnet.yml"
KNOWN_GO="$ROOT/internal/config/validate.go"
CATALOGUE_GO="$ROOT/internal/ops/chops/reconciliation_catalogue.go"
PROJECTOR_GO="$ROOT/internal/projector/registry.go"

pass=0
fail=0

# mkfixture — a consistent tree the gate must call OK.
#
# Eleven sources, all known, all enabled, nothing waived. The catalogue tracks
# every one of them plus a projector-unconditional `sep41_x` (the fixture's
# stand-in for the real sep41 domain), and spells two of its entries as
# `<pkg>.SourceName` so the symbolic-resolution path is exercised on the happy
# path as well as in the case that depends on it.
mkfixture() {
	rm -rf "$ROOT"
	mkdir -p "$ROOT/scripts/ci" \
		"$ROOT/configs/ansible/roles/archival-node/defaults" \
		"$ROOT/configs/ansible/inventory" \
		"$ROOT/internal/config" \
		"$ROOT/internal/ops/chops" \
		"$ROOT/internal/projector" \
		"$ROOT/internal/sources/kilo" \
		"$ROOT/internal/sources/sep41_x"
	cp "$SRC" "$ROOT/scripts/ci/lint-source-enablement.sh"

	cat >"$DEFAULTS" <<'YML'
---
stellar_network:         "pubnet"

stellarindex_enabled_sources:
  - "alpha"
  - "bravo"
  # a comment inside the block is legal and must not be read as an entry
  - "charlie"
  - "delta"
  - "echo"
  - "foxtrot"
  - "golf"
  - "hotel"
  - "india"
  - "juliett"
  - "kilo"

stellarindex_sources_not_yet_enabled: {}

stellarindex_backfill_from_ledger: 0
YML

	cat >"$KNOWN_GO" <<'GO'
package config

// KnownSources is the set of source names the indexer recognises.
var KnownSources = map[string]struct{}{
	"alpha":   {},
	"bravo":   {},
	"charlie": {},
	"delta":   {},
	"echo":    {},
	"foxtrot": {},
	"golf":    {},
	"hotel":   {},
	"india":   {},
	"juliett": {},
	"kilo":    {},
}
GO

	cat >"$CATALOGUE_GO" <<'GO'
package chops

import (
	"github.com/Stellar-Index/StellarIndex/internal/sources/kilo"
	sepx "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_x"
)

func buildReconciliationCatalogue() []reconSource {
	cat := []reconSource{
		{name: "alpha", genesis: 1},
		{name: "bravo", genesis: 2},
		{name: "charlie", genesis: 3},
		{name: "delta", genesis: 4},
		{name: "echo", genesis: 5},
		{name: "foxtrot", genesis: 6},
		{name: "golf", genesis: 7},
		{name: "hotel", genesis: 8},
		{name: "india", genesis: 9},
		{name: "juliett", genesis: 10},
		{
			name:    kilo.SourceName,
			genesis: 11,
		},
		{name: sepx.SourceName, genesis: 12},
	}
	return cat
}
GO

	cat >"$PROJECTOR_GO" <<'GO'
package projector

import (
	sepx "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_x"
)

func BuildRegistry() {
	for _, name := range []string{sepx.SourceName} {
		_ = name
	}
}
GO

	# One pubnet inventory that INHERITS the role default (the r1/r2/r3 shape)
	# and one non-pubnet inventory that legitimately overrides it.
	cat >"$INV_PUBNET" <<'YML'
all:
  vars:
    stellar_network:     "pubnet"
YML
	cat >"$INV_TESTNET" <<'YML'
all:
  vars:
    stellar_network:     "testnet"
    stellarindex_enabled_sources:
      - sdex
YML

	printf 'package kilo\n\nconst SourceName = "kilo"\n' >"$ROOT/internal/sources/kilo/events.go"
	printf 'package sep41_x\n\nconst SourceName = "sep41_x"\n' >"$ROOT/internal/sources/sep41_x/events.go"
}

run() { OUT="$(bash "$ROOT/scripts/ci/lint-source-enablement.sh" 2>&1)"; RC=$?; }

report_fail() { # <case> <why>
	echo "FAIL: $1 — $2" >&2
	printf '%s\n' "  rc=$RC" >&2
	printf '%s\n' "$OUT" | sed 's/^/    /' >&2
	fail=$((fail + 1))
}

expect_ok() { # <case>
	if [ "$RC" -ne 0 ]; then
		report_fail "$1" "expected the gate to PASS, it exited $RC"
		return
	fi
	echo "ok: $1"
	pass=$((pass + 1))
}

expect_red() { # <case> <substring the message must contain>
	if [ "$RC" -eq 0 ]; then
		report_fail "$1" "expected the gate to FAIL, it exited 0"
		return
	fi
	if ! grep -qF -- "$2" <<<"$OUT"; then
		report_fail "$1" "failed, but the message never mentions '$2'"
		return
	fi
	echo "ok: $1"
	pass=$((pass + 1))
}

# ─── 0. The fixture as built is consistent ──────────────────────────
mkfixture
run
expect_ok 'a consistent tree passes'

# ─── §1 the production defect: known, tracked, never enabled ────────
#
# This is sushiswap_v3 (2026-09-09) and upshift (#503) reduced to a fixture:
# the decoder is in KnownSources and the completeness catalogue tracks it,
# but no enabled_sources entry exists, so no projector ever runs for it.
mkfixture
perl -0pi -e 's/^  - "delta"\n//m' "$DEFAULTS"
run
expect_red 'a known source missing from enabled_sources reds' 'delta'

# The message must name the deployment file the operator has to edit —
# a gate that says "drift" without naming the fix is a gate people mute.
run
expect_red 'the §1 message names the ansible default' 'stellarindex_enabled_sources'

# ─── §1 escape hatch: declared-not-yet-enabled is accepted ──────────
#
# Removing the catalogue entry (deferring the CLAIM as well as the run) plus a
# reasoned waiver is the one legitimate way to ship a decoder that is not run.
mkfixture
perl -0pi -e 's/^  - "delta"\n//m' "$DEFAULTS"
perl -0pi -e 's/^\t\t\{name: "delta", genesis: 4\},\n//m' "$CATALOGUE_GO"
perl -0pi -e 's/^stellarindex_sources_not_yet_enabled: \{\}/stellarindex_sources_not_yet_enabled:\n  delta: "decoder merged, awaiting the WASM audit before we serve it"/m' "$DEFAULTS"
run
expect_ok 'a declared, untracked, not-yet-enabled source passes'

# ─── §4 the hatch cannot re-create the defect ───────────────────────
#
# Same waiver, but the catalogue entry stays. /v1/coverage would keep
# publishing a verdict that can only ever read complete=false — precisely the
# state the two production occurrences sat in for weeks.
mkfixture
perl -0pi -e 's/^  - "delta"\n//m' "$DEFAULTS"
perl -0pi -e 's/^stellarindex_sources_not_yet_enabled: \{\}/stellarindex_sources_not_yet_enabled:\n  delta: "decoder merged, awaiting the WASM audit before we serve it"/m' "$DEFAULTS"
run
expect_red 'a waiver for a completeness-TRACKED source reds' 'completeness machinery still tracks'

# ─── §2 an enabled name the config would reject at boot ─────────────
mkfixture
perl -0pi -e 's/^  - "alpha"/  - "alpha"\n  - "typoed"/m' "$DEFAULTS"
run
expect_red 'an enabled source absent from KnownSources reds' 'typoed'

# ─── §3 enabled AND declared not-enabled ────────────────────────────
mkfixture
perl -0pi -e 's/^stellarindex_sources_not_yet_enabled: \{\}/stellarindex_sources_not_yet_enabled:\n  alpha: "stale note, we turned it on last month"/m' "$DEFAULTS"
run
expect_red 'a source both enabled and declared not-enabled reds' 'declared not-yet-enabled while ALSO being'

# ─── §5 tracked for completeness, impossible to enable ──────────────
#
# §1's evasion route: register the source with the completeness machinery but
# never add it to KnownSources. It cannot be named in enabled_sources at all
# (config.Validate rejects it), so its verdict is permanently red too.
mkfixture
perl -0pi -e 's/^\t\t\{name: "alpha", genesis: 1\},/\t\t{name: "alpha", genesis: 1},\n\t\t{name: "ghost", genesis: 13},/m' "$CATALOGUE_GO"
run
expect_red 'a catalogue entry outside KnownSources reds' 'ghost'

# ─── §6 a stale waiver ──────────────────────────────────────────────
mkfixture
perl -0pi -e 's/^stellarindex_sources_not_yet_enabled: \{\}/stellarindex_sources_not_yet_enabled:\n  removed_last_year: "kept long after the package was deleted"/m' "$DEFAULTS"
run
expect_red 'a waiver for a source KnownSources has never heard of reds' 'removed_last_year'

# ─── the waiver must carry a reason ─────────────────────────────────
#
# A bare name is silence with extra steps — the exact thing the hatch exists
# to replace.
mkfixture
perl -0pi -e 's/^  - "delta"\n//m' "$DEFAULTS"
perl -0pi -e 's/^stellarindex_sources_not_yet_enabled: \{\}/stellarindex_sources_not_yet_enabled:\n  delta:/m' "$DEFAULTS"
run
expect_red 'a waiver with no reason reds' 'the reason is mandatory'

# ─── deleting the hatch is not a way to pass ────────────────────────
mkfixture
perl -0pi -e 's/^stellarindex_sources_not_yet_enabled: \{\}\n\n//m' "$DEFAULTS"
run
expect_red 'removing the waiver key entirely reds' 'the escape hatch is silence again'

# ─── NON-VACUITY: an empty subject set must FAIL, never pass ────────
mkfixture
perl -0pi -e 's/^  - "[a-z]+"\n//mg' "$DEFAULTS"
run
expect_red 'an emptied enabled_sources list reds instead of passing vacuously' 'floor'

mkfixture
perl -0pi -e 's/^var KnownSources = map\[string\]struct\{\}\{$/var KnownSourcesRenamed = map[string]struct{}{/m' "$KNOWN_GO"
run
expect_red 'a moved/renamed KnownSources declaration reds' 'checking nothing'

mkfixture
perl -0pi -e 's/\bname:/nombre:/g' "$CATALOGUE_GO"
run
expect_red 'a catalogue whose entry shape moved reds' 'no reconSource entries found'

mkfixture
perl -0pi -e 's/for _, name := range \[\]string\{sepx\.SourceName\}/for _, name := range []string{"sep41_x"}/m' "$PROJECTOR_GO"
run
expect_red 'a projector always-on list the gate can no longer read reds' 'no unconditional projector source list'

# ─── the pubnet premise is asserted, not assumed ────────────────────
mkfixture
perl -0pi -e 's/^stellar_network:         "pubnet"/stellar_network:         "testnet"/m' "$DEFAULTS"
run
expect_red 'a non-pubnet default role reds rather than being checked anyway' 'stellar_network'

# ─── a list item the reader cannot parse is reported, not skipped ───
#
# Silently dropping an unrecognised line is how a gate goes half-blind: the
# entry vanishes from ENABLED and §1 then reports a source as unrun that is in
# fact listed (or, worse, the reverse).
mkfixture
perl -0pi -e 's/^  - "alpha"/  - { name: "alpha", weight: 3 }/m' "$DEFAULTS"
run
expect_red 'an unparseable enabled_sources line reds' 'cannot parse'

# ─── duplicates in the enabled list ─────────────────────────────────
mkfixture
perl -0pi -e 's/^  - "alpha"/  - "alpha"\n  - "alpha"/m' "$DEFAULTS"
run
expect_red 'a duplicated enabled source reds' 'lists the same source twice'

# ─── symbolic <pkg>.SourceName entries are RESOLVED, not dropped ────
#
# Four of the real catalogue's twenty-two entries name their source as
# `<pkg>.SourceName`. If the resolution quietly returned nothing, those four
# would fall out of §4/§5 with no visible change — so prove the gate sees the
# resolved name by breaking a source that only ever appears symbolically.
mkfixture
perl -0pi -e 's/^  - "kilo"\n//m' "$DEFAULTS"
run
expect_red 'a symbolically-named catalogue source is resolved and checked' 'kilo'

# An unresolvable ident must red rather than shrink the subject set.
mkfixture
rm -f "$ROOT/internal/sources/kilo/events.go"
run
expect_red 'an unresolvable <pkg>.SourceName reds' 'cannot resolve'

# ─── §7 the checked list must be the one pubnet actually applies ────
#
# A non-pubnet inventory overriding the list is normal and must stay green
# (the fixture ships one). A PUBNET inventory doing it shadows the role
# default, and every check above would then be a verdict about a list that
# host does not apply.
mkfixture
printf '    stellarindex_enabled_sources:\n      - "alpha"\n' >>"$INV_PUBNET"
run
expect_red 'a pubnet inventory overriding the list reds' 'r9.example.yml'

# The reverse must NOT fire: only pubnet inventories are constrained.
mkfixture
run
expect_ok 'a non-pubnet inventory may override the list'

# With no pubnet inventory at all there is nothing the gate speaks for.
mkfixture
rm -f "$INV_PUBNET"
run
expect_red 'no pubnet inventory reds rather than certifying a list nobody applies' 'no pubnet inventory found'

echo "----"
echo "lint-source-enablement-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
