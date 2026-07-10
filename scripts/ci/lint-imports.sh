#!/usr/bin/env bash
# Import-boundary lint for Stellar Index.
#
# Enforces architectural layering rules that docs alone can't: a
# build-time check that certain package imports don't leak into
# production code paths. Adding a rule is a ~5-line amendment to
# the RULES array below.
#
# Current rules
# -------------
#
#   A. No `internal/stellarrpc` in production ingest.
#      Reason: stellar-rpc was removed from r1 on 2026-04-23; the
#      production ingest path is Galexie → ledgerstream →
#      dispatcher → decoder. See
#      docs/architecture/ingest-pipeline.md.
#      Allowed in:
#        - internal/stellarrpc/           (the package itself)
#        - internal/ops/                  (stellarindex-ops subcommand
#                                          packages, e.g. `rpc-probe`)
#        - scripts/dev/                   (fixture-capture scripts)
#        - internal/sources/*/decode.go   (imports `Event` type
#                                          only; type moves to a
#                                          neutral package in PR
#                                          165b, at which point
#                                          this allowlist entry
#                                          goes away)
#        - *_test.go                      (test files)
#
#   B. No go-stellar-sdk/xdr in production connectors.
#      Reason: ADR-0013 scopes the xdr dep to the internal/scval
#      wrapper. Connectors work with the scval re-exports
#      (ScVal, ScMapEntry aliases).
#      Allowed in:
#        - internal/scval/                (the wrapper)
#        - *_test.go                      (test fixtures often
#                                          construct raw xdr)
#
#   C. No Horizon imports, anywhere.
#      Reason: ADR-0001 rules Horizon out entirely. Preemptive.
#      Allowed in: nowhere.
#
#   P/*. Foundation-purity rules — pin leaf packages (canonical,
#      nettools, scale, version, cachekeys) to a maximum internal-
#      dependency set. See PURITY_RULES below.
#
#   L/*. Layering rules (D8 dependency-direction) — forbid upward /
#      sideways edges: pkg→internal (ADR-0005), sources→app layer,
#      storage→compute (grandfathered via baseline pending D8 M0-1),
#      internal/api imported outside api+cmd. See LAYERING_RULES.
#
#   S/storage-subpackage-only — internal/storage/ holds subpackages
#      only (timescale/ clickhouse/ redisclient/); no top-level
#      adapter files.
#
# Usage
# -----
#
#   scripts/ci/lint-imports.sh
#
# Exit 0 on clean, non-zero on any violation. Hooked into
# `make verify` and CI.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"

# Delegate to a small Python program — walking *.go files and
# parsing import blocks is easier in Python than in bash.
python3 - <<'PY'
import os
import re
import sys
from pathlib import Path

REPO = Path(".").resolve()

# ─── Rule definitions ────────────────────────────────────────────
#
# Each rule is a dict with:
#   name     — short identifier for the error line.
#   banned   — list of package paths that trigger the rule.
#   allow    — list of file-path prefix / glob predicates; if ANY
#              matches, the file is allowed to have the banned
#              import. Matching is substring-prefix for strings
#              starting with "/" or substring-match for globs with
#              "*". "_test.go" matches any file ending in that.
#   why      — human-readable reason; printed on violation.
# ---------------------------------------------------------------

RULES = [
    {
        "name": "A/no-rpc-in-ingest",
        "banned": [
            "github.com/StellarIndex/stellar-index/internal/stellarrpc",
        ],
        "allow": [
            "internal/stellarrpc/",             # the package itself
            "internal/ops/",                    # stellarindex-ops subcommand packages (rpc-probe diag; D1 M1-5 split moved these out of cmd/stellarindex-ops/)
            "scripts/dev/",                     # fixture-capture
            "/decode.go",                       # source decode.go — uses Event type only (PR 165b will move)
            "/factory_seed.go",                 # cold-start factory state via simulateTransaction (PR 14) — not a runtime decoder
            "_test.go",                         # tests
        ],
        "why": (
            "stellar-rpc was removed from r1 2026-04-23; production ingest "
            "is Galexie → ledgerstream → dispatcher → decoder. See "
            "docs/architecture/ingest-pipeline.md."
        ),
    },
    {
        "name": "B/xdr-scoped-to-scval",
        "banned": [
            "github.com/stellar/go-stellar-sdk/xdr",
        ],
        "allow": [
            "internal/scval/",                  # the Soroban-event scval wrapper
            "internal/canonical/sac.go",        # SAC address derivation is a protocol-defined hash over xdr.Asset/HashIdPreimage (CAP-46) — xdr types ARE the spec here, no SCVal decoding (site-audit #40, 2026-07-03)
            "internal/ledgerstream/",           # transport layer — exposes xdr.LedgerCloseMeta
            "internal/dispatcher/",             # routes tx / events (PR 165b)
            "internal/pipeline/",               # shared ledger-meta plumbing (indexer + backfill)
            "internal/storage/clickhouse/",     # Tier-1 raw-lake structural decoder: walks LCM, stores raw XDR blobs (NOT SCVal decoding) (ADR-0034)
            "internal/xdrjson/",                # network-explorer classic-XDR→JSON decoder: decodes op bodies/keys/entries (NOT SCVal events) (ADR-0038)
            "internal/sources/sdex/",           # SDEX decodes non-SCVal xdr (classic ops) (PR 165c)
            "internal/sources/classicmovements/", # ADR-0047: reconstructs pre-P23 classic movements (Payment/CreateAccount) from raw xdr.Operation/OperationResult — same non-SCVal classic-op category as sdex
            "internal/sdexclaim/",              # shared ClaimAtom count/amount helpers (dispatcher + clickhouse); xdr.ClaimAtom only
            "internal/sources/accounts/",       # AccountEntry observer reads ledger-meta deltas (ADR-0021)
            "internal/sources/trustlines/",     # TrustlineEntry observer reads ledger-meta deltas (ADR-0022)
            "internal/sources/claimable_balances/", # ClaimableBalance observer reads ledger-meta deltas (ADR-0022)
            "internal/sources/liquidity_pools/", # LiquidityPool observer reads ledger-meta deltas (ADR-0022)
            "internal/sources/sac_balances/",   # SAC ContractData observer reads ledger-meta deltas (ADR-0022)
            "internal/sources/sep41_supply/",   # SEP-41 supply observer reads xdr.ScVal Value/Type discriminants (ADR-0023)
            "internal/sources/sep41_transfers/", # SEP-41 audit-trail decoder reads xdr.ScVal Value/Type discriminants (F-0021)
            "cmd/stellarindex-indexer/",         # glue: wires ledgerstream → dispatcher (PR 165d)
            "internal/ops/",                    # verify-decoders mirrors the indexer's ledger plumbing (D1 M1-5 split moved stellarindex-ops subcommands out of cmd/stellarindex-ops/)
            "internal/stellarrpc/",             # builds TransactionEnvelope XDR for simulateTransaction (not SCVal)
            "scripts/dev/",                     # diagnostic helpers (decode-scval pretty-prints raw XDR)
            "_test.go",                         # fixture construction
        ],
        "why": (
            "ADR-0013 scopes the xdr dependency away from Soroban "
            "event decoders — those go through internal/scval. "
            "Ledger-meta plumbing (ledgerstream, dispatcher, sdex) "
            "legitimately consumes non-SCVal xdr types."
        ),
    },
    {
        "name": "C/no-horizon",
        "banned": [
            "github.com/stellar/go/clients/horizonclient",
            "github.com/stellar/go/protocols/horizon",
            "github.com/stellar/go-stellar-sdk/clients/horizonclient",
            "github.com/stellar/go-stellar-sdk/protocols/horizon",
        ],
        "allow": [],
        "why": "ADR-0001 rules Horizon out entirely.",
    },
]

MODULE = "github.com/StellarIndex/stellar-index/"

# ─── Foundation-purity rules (D8 layering) ───────────────────────
#
# Each rule pins a FOUNDATION package to a maximum internal-dependency
# set: files under `prefix` (excluding _test.go) may import module-local
# packages ONLY from `allow_internal` (plus the package's own subtree).
# Go already forbids import CYCLES; this forbids the coupling that leads to
# them — it keeps the type/leaf layer safe to import from anywhere and stops
# foundation code from creeping upward into the app layers.
PURITY_RULES = [
    {
        "name": "P/canonical-foundation",
        "prefix": "internal/canonical/",
        "allow_internal": set(),
        "why": "internal/canonical is THE canonical-types foundation; it must import only stdlib/third-party (no other internal package) so it stays a universally-safe import and can never anchor a cycle.",
    },
    {
        "name": "P/nettools-leaf",
        "prefix": "internal/nettools/",
        "allow_internal": set(),
        "why": "internal/nettools is a stdlib-only leaf (the canonical SSRF guard); keep it dependency-free so any layer can import it.",
    },
    {
        "name": "P/external-scale-leaf",
        "prefix": "internal/sources/external/scale/",
        "allow_internal": set(),
        "why": "internal/sources/external/scale is a stdlib-only amount-scaling leaf; keep it dependency-free (every connector imports it).",
    },
    {
        "name": "P/version-leaf",
        "prefix": "internal/version/",
        "allow_internal": set(),
        "why": "internal/version is build-time info; a stdlib-only leaf.",
    },
    {
        "name": "P/cachekeys-layering",
        "prefix": "internal/cachekeys/",
        "allow_internal": {"internal/canonical"},
        "why": "internal/cachekeys is a low-level Redis-key builder; it may depend only on internal/canonical, never on a higher (api/storage/aggregate) layer.",
    },
]

# ─── Layering rules (D8 dependency-direction) ────────────────────
#
# Importer-side boundary rules: non-test files whose path starts with
# any prefix in `importers` ("" = every file) may NOT import module-
# local packages matching `forbid`, unless the file also matches an
# `exempt_importers` prefix. Complements PURITY_RULES (which pin a
# package's maximum dependency set); these forbid specific upward /
# sideways edges per the D8 dependency-direction map
# (docs/maintainability-audit-2026-07-01/D8-dependency-direction.md).
# Known-legacy violations are grandfathered via lint-imports.baseline
# and must shrink monotonically.
LAYERING_RULES = [
    {
        "name": "L/pkg-purity",
        "importers": ["pkg/"],
        "exempt_importers": [],
        "forbid": ["internal"],
        "why": "ADR-0005: pkg/ is the public SemVer-stable surface; it must never import internal/ (Go would refuse for external consumers — this keeps in-repo builds honest too).",
    },
    {
        "name": "L/sources-app-purity",
        "importers": ["internal/sources/"],
        "exempt_importers": [],
        "forbid": [
            "internal/api",
            "internal/storage",
            "internal/platform",
            "internal/pipeline",
            "internal/projector",
            "internal/aggregate",
        ],
        "why": "Decoders/connectors sit below the app layer: a source package must not reach api/storage/platform/pipeline/projector/aggregate (D8 rule 3 — the most load-bearing direction in the ingest architecture).",
    },
    {
        "name": "L/storage-below-compute",
        "importers": ["internal/storage/"],
        "exempt_importers": [],
        "forbid": [
            "internal/aggregate",
            "internal/divergence",
            "internal/supply",
            "internal/sources",
        ],
        "why": "Storage is the persistence tier below compute (D8 rule 4). Today it imports upward because persisted domain structs live in aggregate/supply/sources packages (D8 M0-1) — those edges are grandfathered in lint-imports.baseline; do not add new ones, and shrink the baseline as structs move to a neutral home.",
    },
    {
        "name": "L/api-scope",
        "importers": [""],
        "exempt_importers": ["internal/api/", "cmd/", "scripts/", "test/"],
        "forbid": ["internal/api"],
        "why": "internal/api is the top of the serving stack: only the api tree itself and binaries may import it (D8 rule 5).",
    },
]

# ─── Walker ──────────────────────────────────────────────────────

IMPORT_RE = re.compile(r'^\s*(?:[a-zA-Z_][a-zA-Z0-9_]*\s+)?"([^"]+)"')


def file_allowed(rule, rel_path):
    """True if rel_path matches any allowlist predicate for the rule."""
    for pred in rule["allow"]:
        if pred in rel_path:
            return True
    return False


def imports_in(path):
    """Yield import paths from a .go file by parsing the import block."""
    try:
        text = path.read_text()
    except (OSError, UnicodeDecodeError):
        return
    in_block = False
    for line in text.splitlines():
        stripped = line.strip()
        if stripped.startswith("import ("):
            in_block = True
            continue
        if in_block:
            if stripped == ")":
                in_block = False
                continue
            m = IMPORT_RE.match(line)
            if m:
                yield m.group(1)
        elif stripped.startswith("import "):
            # Single-import form: `import "x"` or `import alias "x"`.
            m = IMPORT_RE.match(stripped[len("import "):])
            if m:
                yield m.group(1)


SKIP_DIRS = {".git", "vendor", ".discovery-repos", "node_modules", ".claude"}


def walk_go_files(root):
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
        for fn in filenames:
            if fn.endswith(".go"):
                yield Path(dirpath) / fn


# ─── Main ───────────────────────────────────────────────────────

BASELINE_PATH = REPO / "scripts/ci/lint-imports.baseline"


def load_baseline():
    """Return a set of (rule_name, rel_path) pairs from the baseline file.

    Empty set if the baseline doesn't exist — the lint runs strict.
    """
    if not BASELINE_PATH.exists():
        return set()
    allowed = set()
    for raw in BASELINE_PATH.read_text().splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        parts = line.split(None, 1)
        if len(parts) != 2:
            print(f"baseline: malformed line: {raw!r}", file=sys.stderr)
            continue
        allowed.add((parts[0], parts[1]))
    return allowed


def main():
    baseline = load_baseline()
    observed = set()  # (rule_name, rel_path) pairs we actually hit
    regressions = []  # violations NOT in baseline — real fails

    # Structural check: internal/storage/ is subpackage-only
    # (timescale/ clickhouse/ redisclient/) — no top-level adapter
    # files (CLAUDE.md repo map; ADR-0034 tiering). A stray
    # internal/storage/*.go is the start of a new grab-bag layer.
    strays = sorted(
        p.name for p in (REPO / "internal/storage").glob("*.go"))
    if strays:
        for name in strays:
            print(f"import-lint [S/storage-subpackage-only] ❌ "
                  f"internal/storage/{name} — internal/storage must "
                  "contain only subpackages (timescale/ clickhouse/ "
                  "redisclient/); put the adapter in its subpackage.",
                  file=sys.stderr)
        sys.exit(1)

    for go_file in walk_go_files(REPO):
        rel = str(go_file.relative_to(REPO))
        imports = list(imports_in(go_file))
        for rule in RULES:
            for imp in imports:
                if imp not in rule["banned"]:
                    continue
                if file_allowed(rule, rel):
                    continue
                pair = (rule["name"], rel)
                observed.add(pair)
                if pair not in baseline:
                    regressions.append((rule, rel, imp))

        # Foundation-purity: files DIRECTLY in a pinned package dir may import
        # only module-local packages in the rule's allow set. Scoped to the
        # immediate directory (not the subtree) so a distinct subpackage
        # (e.g. internal/canonical/discovery) isn't held to the parent's
        # purity. Tests are exempt.
        # Layering: importer-side forbidden edges (subtree-scoped,
        # tests exempt — integration tests legitimately cross layers).
        if not rel.endswith("_test.go"):
            for lrule in LAYERING_RULES:
                if not any(rel.startswith(p) for p in lrule["importers"]):
                    continue
                if any(rel.startswith(p) for p in lrule["exempt_importers"]):
                    continue
                for imp in imports:
                    if not imp.startswith(MODULE):
                        continue
                    local = imp[len(MODULE):]
                    if not any(local == f or local.startswith(f + "/")
                               for f in lrule["forbid"]):
                        continue
                    pair = (lrule["name"], rel)
                    observed.add(pair)
                    if pair not in baseline:
                        regressions.append((lrule, rel, imp))

        rel_dir = os.path.dirname(rel) + "/"
        if not rel.endswith("_test.go"):
            for prule in PURITY_RULES:
                if rel_dir != prule["prefix"]:
                    continue
                own = prule["prefix"].rstrip("/")
                for imp in imports:
                    if not imp.startswith(MODULE):
                        continue
                    local = imp[len(MODULE):]
                    if local == own or local.startswith(own + "/"):
                        continue  # own package / subpackage
                    if any(local == a or local.startswith(a + "/")
                           for a in prule["allow_internal"]):
                        continue
                    pair = (prule["name"], rel)
                    observed.add(pair)
                    if pair not in baseline:
                        regressions.append((prule, rel, imp))

    # Stale baseline entries — no longer violating. Force shrink.
    stale = baseline - observed

    if regressions:
        for rule, rel, imp in regressions:
            print(f"import-lint [{rule['name']}] ❌ {rel} imports {imp}",
                  file=sys.stderr)
        print("", file=sys.stderr)
        seen = set()
        for rule, _rel, _imp in regressions:
            if rule["name"] in seen:
                continue
            seen.add(rule["name"])
            print(f"  {rule['name']}: {rule['why']}", file=sys.stderr)
        print("", file=sys.stderr)
        print(f"import-lint: FAIL ({len(regressions)} NEW violation(s) — "
              "not in scripts/ci/lint-imports.baseline).",
              file=sys.stderr)
        print("Either (a) fix the import, (b) justify adding the file to "
              "the allowlist in lint-imports.sh, or (c) add to "
              "lint-imports.baseline only with a PR note citing why.",
              file=sys.stderr)
        sys.exit(1)

    if stale:
        for rule_name, rel in sorted(stale):
            print(f"import-lint [{rule_name}] ⚠ baseline stale: {rel} "
                  "no longer violates — remove from "
                  "scripts/ci/lint-imports.baseline.",
                  file=sys.stderr)
        print("", file=sys.stderr)
        print(f"import-lint: FAIL ({len(stale)} stale baseline entry/ies). "
              "Shrink the baseline monotonically as the rework lands.",
              file=sys.stderr)
        sys.exit(1)

    total_baseline = len(observed)
    if total_baseline:
        print(f"✅ import-lint passed. {total_baseline} known-legacy "
              "violation(s) grandfathered via lint-imports.baseline.")
    else:
        print("✅ import-lint passed.")


main()
PY
