#!/usr/bin/env python3
"""Refuse a YAML mapping that declares the same key twice.

WHY THIS EXISTS (2026-09-08). A commit added a second `pre_tasks:` key to
`configs/ansible/playbooks/archival-node.yml`. YAML keeps the LAST
occurrence and discards the earlier one, so the play silently lost its
`Confirm Ubuntu 22.04 or 24.04 LTS` guard — the file still contained the
task, and the playbook still ran, and nothing anywhere reported that the
guard had stopped executing. It was found days later by an unrelated
`--syntax-check`, after several applies had run without it.

That is the dangerous shape: the change LOOKS purely additive in review
(a new block appears in the diff) while silently deleting a sibling. No
parser complains, because duplicate keys are legal YAML — `safe_load`
just resolves them last-one-wins. The only way to catch it is to look for
the duplicate before the loader collapses it.

Scope is the YAML that decides what runs on a host or in CI: ansible,
GitHub workflows, and the Prometheus rule trees. Config-with-duplicates
in a fixture is not interesting; config-with-duplicates in a playbook
means a task nobody can see is not running.

Usage:
    lint-yaml-duplicate-keys.py [path ...]     # default: the scoped set
Exit 0 clean, 1 on a duplicate, 2 on a usage/parse error.
"""

from __future__ import annotations

import sys
from pathlib import Path

try:
    import yaml
except ImportError:  # pragma: no cover - environment guard
    print("lint-yaml-duplicate-keys: PyYAML not installed; skipping", file=sys.stderr)
    sys.exit(0)

# Directories whose YAML decides host or CI behaviour. A duplicate key in
# a test fixture is deliberate often enough that flagging it would train
# people to ignore this gate.
SCOPED = (
    "configs/ansible",
    "configs/prometheus",
    "configs/alertmanager",
    ".github/workflows",
    "deploy/monitoring",
)

SKIP_PARTS = {"node_modules", ".git", "vendor", "testdata", "fixtures"}


class DuplicateKeyError(Exception):
    def __init__(self, key: str, line: int, first_line: int) -> None:
        self.key = key
        self.line = line
        self.first_line = first_line
        super().__init__(key)


class _DupCheckLoader(yaml.SafeLoader):
    """SafeLoader that raises instead of silently keeping the last key."""


def _mapping_with_dup_check(loader: yaml.Loader, node: yaml.Node, deep: bool = False):
    seen: dict[str, int] = {}
    for key_node, _ in node.value:
        key = loader.construct_object(key_node, deep=deep)
        if not isinstance(key, str):
            continue
        line = key_node.start_mark.line + 1
        if key in seen:
            raise DuplicateKeyError(key, line, seen[key])
        seen[key] = line
    return loader.construct_mapping(node, deep=deep)


_DupCheckLoader.add_constructor(
    yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _mapping_with_dup_check
)


def check(path: Path) -> str | None:
    """Return a problem string, or None when the file is clean."""
    try:
        text = path.read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError) as exc:
        return f"{path}: unreadable ({exc})"
    try:
        # load_all: a playbook may be a multi-document stream.
        for _ in yaml.load_all(text, Loader=_DupCheckLoader):
            pass
    except DuplicateKeyError as dup:
        return (
            f"{path}:{dup.line}: duplicate key '{dup.key}' "
            f"(first declared at line {dup.first_line}) — YAML keeps the LAST "
            f"occurrence, so everything under the first one is silently discarded"
        )
    except yaml.YAMLError:
        # A file that does not parse at all is another gate's problem
        # (ansible --syntax-check, promtool, actionlint). Reporting it
        # here would duplicate their message with less context.
        return None
    return None


def candidates(argv: list[str]) -> list[Path]:
    root = Path(__file__).resolve().parents[2]
    if argv:
        return [Path(a) for a in argv]
    out: list[Path] = []
    for scope in SCOPED:
        base = root / scope
        if not base.is_dir():
            continue
        for p in base.rglob("*.y*ml"):
            if SKIP_PARTS & set(p.parts):
                continue
            out.append(p)
    return sorted(out)


def main(argv: list[str]) -> int:
    files = candidates(argv)
    if not files:
        print("lint-yaml-duplicate-keys: no YAML in scope — refusing to pass vacuously", file=sys.stderr)
        return 2
    problems = [msg for msg in (check(f) for f in files) if msg]
    for msg in problems:
        print(f"lint-yaml-duplicate-keys: {msg}", file=sys.stderr)
    if problems:
        print(
            f"lint-yaml-duplicate-keys: FAIL — {len(problems)} duplicate key(s). "
            "A duplicate reads as an addition in review while deleting its sibling.",
            file=sys.stderr,
        )
        return 1
    print(f"lint-yaml-duplicate-keys: OK — {len(files)} file(s), no duplicate mapping keys")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
