#!/usr/bin/env bash
# lint-lexicon-test.sh — fixture tests for the non-slog-logger zero rule in
# scripts/ci/lint-lexicon.sh.
#
# The stdlib arm of that rule was `^[[:space:]]*"log"$`, which matches only
# a line that IS (with leading whitespace) exactly `"log"`. An aliased
# stdlib import (`l "log"`) is still the banned logger — slog is the only
# one — but read straight past it, so `import ( l "log" )` shipped GREEN
# next to a real `go.uber.org/zap` import that correctly went RED.
#
# ROOT/LEXICON_BASELINE let the gate run against an isolated fixture tree
# instead of the real repo, same convention as lint-apikey-scan.sh /
# lint-http-timeouts.sh.
#
# An "anchor" file plus a matching baseline keeps the RATCHET section
# (coin / positional-logger / variadic-option) quiet in every fixture: with
# none of those three patterns present anywhere under the fixture root,
# `grep -rl 'Coin' …` (and its two siblings) finds no match and exits 1,
# and under this script's `set -euo pipefail` that kills the WHOLE run
# before the zero-rule output below is even reached — a real, pre-existing
# gap the isolated-root case exposes but does not create (the live repo
# always has at least one match for each class, so it never fires there).
# Fixing that gap is out of scope here; the anchor sidesteps it instead.
#
# Run: bash scripts/ci/lint-lexicon-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-lexicon.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

# shellcheck source=scripts/ci/lib/test-expect.sh
. "$PWD/scripts/ci/lib/test-expect.sh"

ANCHOR_REL="internal/fixture/anchor.go"
printf 'coin %s\npositional-logger %s\nvariadic-option %s\n' \
  "$TMP/root/$ANCHOR_REL" "$TMP/root/$ANCHOR_REL" "$TMP/root/$ANCHOR_REL" > "$TMP/baseline"

# run <body> — a fixture tree with the ratchet anchor plus one file holding
# <body>, then the gate.
run() {
  rm -rf "$TMP/root"
  mkdir -p "$TMP/root/internal/fixture" "$TMP/root/cmd" "$TMP/root/pkg"
  cat > "$TMP/root/$ANCHOR_REL" <<'GO'
package fixture

import "log/slog"

type CoinBalance struct{}

func NewWithLogger(logger *slog.Logger) *Thing { return &Thing{} }

func NewWithOptions(opts ...Option) *Other { return &Other{} }
GO
  printf '%s\n' "$1" > "$TMP/root/internal/fixture/logger.go"
  OUT="$(LEXICON_BASELINE="$TMP/baseline" bash "$LINT" "$TMP/root" 2>&1)"
  RC=$?
}

run 'package fixture

import (
	"fmt"
	l "log"
)

func f() { l.Println(fmt.Sprintf("x")) }'
expect 'an aliased stdlib "log" import is caught' 1 'non-slog logger import'

run 'package fixture

import (
	"fmt"
	"log"
)

func f() { log.Println(fmt.Sprintf("x")) }'
expect 'a plain grouped unaliased "log" import is caught' 1 'non-slog logger import'

run 'package fixture

import _ "log"'
expect 'a blank stdlib "log" import is caught' 1 'non-slog logger import'

run 'package fixture

import "log"

func f() { log.Println("x") }'
expect 'a bare single-line import "log" is caught' 1 'non-slog logger import'

run 'package fixture

import (
	"fmt"
	"go.uber.org/zap"
)

func f() { _ = zap.NewNop(); _ = fmt.Sprintf("x") }'
expect 'a real non-stdlib logger (zap) is still caught' 1 'non-slog logger import'

run 'package fixture

import (
	"fmt"
	"log/slog"
)

func f() { slog.Info(fmt.Sprintf("x")) }'
expect 'slog itself passes clean' 0 'OK'

run 'package fixture

// this package imports a vendor path that merely ends in /log
import (
	"fmt"
	"github.com/foo/log"
)

func f() { log.Println(fmt.Sprintf("x")) }'
expect 'a third-party import path ending in /log is not a false positive' 0 'OK'

echo
echo "lint-lexicon-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ] || exit 1
