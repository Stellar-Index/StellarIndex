"""Every relative markdown link, and every in-repo anchor, must resolve.

A broken link in a runbook is an operational defect: the operator is
mid-incident, follows the pointer the alert gave them, and lands on a 404.
Twenty-two of these had accumulated before this gate existed, including four
runbooks referenced by live alerts that had never been written, and three ADRs
whose filenames had changed underneath the link.

FILE SET — tracked files PLUS untracked, non-ignored ones. Link TARGETS are
checked the same way: a target that exists locally but is gitignored counts as
missing, because a fresh clone will not have it. The tracked-only
version of this gate reported "OK across 581 files" while sixteen links were
broken in three files that had just been created and not yet staged: it was
blind to exactly the files being changed. CI checks out a tree where everything
is tracked, so tracked-only made verify.sh green while CI went red.

ANCHORS — a link to `../FOO.md#some-heading` is checked against the headings
FOO.md actually has, slugified the way GitHub does it.

Exclusions, both deliberate:
  * Code spans and fenced blocks are skipped. Markdown does not render a link
    inside backticks, and this repo's protocol tables are full of XDR notation
    like `Vec[Address](assets)` that looks exactly like one. An UNBALANCED
    fence count is reported rather than silently blinding the rest of a file.
  * `_template` / `*TEMPLATE*` files carry placeholder targets on purpose.
"""

import os
import re
import subprocess
import sys

FENCE = re.compile(r"^\s*(```|~~~)")
LINK = re.compile(r"\]\(([^)\s]+)\)")
HEAD = re.compile(r"^#{1,6}\s+(.*?)\s*$")


def _git(*args):
    out = subprocess.run(["git", *args], capture_output=True, text=True).stdout
    return [f for f in out.split("\0") if f]


def _ignored(path):
    """True if git ignores this path.

    A link target that exists on THIS machine but is gitignored does not
    exist for anyone else. Checking the filesystem alone made this gate pass
    locally and fail in CI on three links into `docs/archive/` and `notes/`,
    which are ignored — the same "local green, CI red" class the gate was
    written to kill, arrived at from the other direction.
    """
    return subprocess.run(["git", "check-ignore", "-q", path],
                          capture_output=True).returncode == 0


def _strip_spans(line):
    return re.sub(r"`[^`]*`", "", line)


def slug(text):
    """GitHub's heading slug: drop formatting and punctuation, lower, hyphenate."""
    t = re.sub(r"`([^`]*)`", r"\1", text)
    t = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", t)
    t = re.sub(r"[*_~]", "", t).strip().lower()
    t = re.sub(r"[^\w\s-]", "", t)
    # GitHub hyphenates each space individually and does NOT collapse runs, so
    # "work — the" (em dash stripped, two spaces left) becomes "work--the".
    # Collapsing here produced false "no such anchor" reports on every heading
    # containing a dash.
    return t.replace(" ", "-")


_anchors = {}


def anchors_of(path):
    if path in _anchors:
        return _anchors[path]
    try:
        text = open(path, encoding="utf-8").read()
    except Exception:
        _anchors[path] = None
        return None
    seen, out, infence = {}, set(), False
    for line in text.split("\n"):
        if FENCE.match(line):
            infence = not infence
            continue
        if infence:
            continue
        m = HEAD.match(line)
        if not m:
            continue
        s = slug(m.group(1))
        if not s:
            continue
        n = seen.get(s, 0)
        out.add(s if n == 0 else f"{s}-{n}")
        seen[s] = n + 1
    _anchors[path] = out
    return out


def main():
    files = sorted(
        set(_git("ls-files", "-z", "*.md"))
        | set(_git("ls-files", "-z", "--others", "--exclude-standard", "*.md"))
    )
    fails = []
    for path in files:
        base = os.path.basename(path)
        if base.startswith("_template") or "TEMPLATE" in base:
            continue
        try:
            lines = open(path, encoding="utf-8").read().split("\n")
        except Exception:
            continue
        if sum(1 for line in lines if FENCE.match(line)) % 2:
            fails.append((path, 0, "", "has an ODD number of code fences — everything after "
                                       "the stray fence goes unscanned, so this gate is blind to it"))
            continue
        infence = False
        for lineno, line in enumerate(lines, 1):
            if FENCE.match(line):
                infence = not infence
                continue
            if infence:
                continue
            for m in LINK.finditer(_strip_spans(line)):
                target = m.group(1)
                if target.startswith(("http://", "https://", "mailto:", "#")):
                    continue
                rel, _, frag = target.partition("#")
                if not rel:
                    continue
                dest = os.path.normpath(os.path.join(os.path.dirname(path), rel))
                if not os.path.exists(dest):
                    fails.append((path, lineno, target, "target does not exist"))
                    continue
                if _ignored(dest):
                    fails.append((path, lineno, target,
                                  "target is GITIGNORED — it exists on this machine but not in a "
                                  "fresh clone, so the link is broken for every other reader"))
                    continue
                if frag and dest.endswith(".md"):
                    have = anchors_of(dest)
                    if have is not None and slug(frag) not in have:
                        fails.append((path, lineno, target,
                                      f"{os.path.basename(dest)} has no heading anchor "
                                      f"#{slug(frag)}"))

    for path, lineno, target, why in fails:
        loc = f"{path}:{lineno}" if lineno else path
        arrow = f" -> {target}" if target else ""
        print(f"  \033[31mFAIL\033[0m {loc}{arrow} {why}")

    print()
    if fails:
        print(f"lint-doc-links: {len(fails)} failure(s) across {len(files)} markdown file(s)")
    else:
        print("lint-doc-links: OK — every relative link and in-repo anchor resolves across "
              f"{len(files)} markdown file(s)")
    return len(fails)


if __name__ == "__main__":
    sys.exit(main())
