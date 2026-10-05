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

ADR PATHS IN CODE SPANS — the first exception to skipping backticks. Runbooks
cite ADRs as `docs/adr/NNNN-slug.md` in backticks, which renders as no link,
so a renamed ADR left them dangling silently. ADR filenames are numbered and
never placeholders, so a backticked repo-root ADR path must resolve too.

GO IDENTIFIERS IN ADR CODE SPANS — an ADR that names `Type.Method`, `pkg.Func`
or `name()` in backticks is telling an implementer what to call. A span whose
final identifier appears in no .go file is a phantom: ADRs named
`source.HandledTopics()` and `sources.Source.TopicSymbols()`, neither of which
was ever built, and a reader implementing against them builds the wrong thing.
A phantom is corrected in the ADR text; where the original wording is kept
(an ADR not yet on the template), it is acknowledged in the same file with
`<!-- adr-absent-identifier: Name — reason -->`. A marker for a name that now
exists, or that no span in the file uses, is stale and fails.

Usage:
  lint_doc_links.py                every tracked + untracked markdown file
                                    (the default, unchanged)
  lint_doc_links.py FILE...        only these files as link SOURCES

Passing files scopes what gets SCANNED for outgoing links, never what a link
can resolve AGAINST: target existence, the gitignore check and anchor lookups
all still read the full working tree on demand, exactly as in the no-argument
form, so a fixture link to `../README.md#some-heading` is checked the same
way either way. Most of the cost here is one `git check-ignore` subprocess
per existing relative target (2000+ across 607 files) — scoping the source
set is what makes a single-file check cheap; it is not a shortcut on
resolution.
"""

import os
import re
import subprocess
import sys

FENCE = re.compile(r"^\s*(```|~~~)")
LINK = re.compile(r"\]\(([^)\s]+)\)")
HEAD = re.compile(r"^#{1,6}\s+(.*?)\s*$")
ADR_SPAN = re.compile(r"`(docs/adr/[0-9]{4}-[^`\s]+\.md)`")
GO_SPAN = re.compile(r"`((?:[A-Za-z_]\w*\.)*[A-Za-z_]\w*)(\(\))?`")
ABSENT_MARK = re.compile(r"<!--\s*adr-absent-identifier:(.*?)-->")
ABSENT_BODY = re.compile(r"^\s*([A-Za-z_]\w*)\s+(?:—|--)\s*\S")


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


def _adr_span_fails(path, lineno, line):
    fails = []
    for m in ADR_SPAN.finditer(line):
        target = m.group(1)
        if not os.path.exists(target):
            fails.append((path, lineno, target, "ADR path in a code span does not exist"))
        elif _ignored(target):
            fails.append((path, lineno, target, "ADR path in a code span is GITIGNORED"))
    return fails


def _go_span_names(line):
    """Identifiers a code span names as Go: a selector with an exported tail, or a call."""
    names = []
    for m in GO_SPAN.finditer(line):
        parts = m.group(1).split(".")
        if m.group(2) or (len(parts) > 1 and parts[-1][:1].isupper()):
            names.append(parts[-1])
    return names


def _existing_go_identifiers(names):
    """The subset of names that occur as a whole word in a tracked or untracked .go file."""
    if not names:
        return set(), None
    cmd = ["git", "grep", "--untracked", "-h", "-o", "-w", "-F"]
    for n in sorted(names):
        cmd += ["-e", n]
    res = subprocess.run(cmd + ["--", "*.go"], capture_output=True, text=True)
    if res.returncode not in (0, 1):
        return set(), res.stderr.strip() or f"git grep exited {res.returncode}"
    return set(res.stdout.split()), None


def _scan_adr(path, lines):
    """Go-shaped spans [(lineno, name)], markers {name: lineno}, and malformed-marker fails."""
    spans, marks, fails = [], {}, []
    infence = False
    for lineno, line in enumerate(lines, 1):
        for m in ABSENT_MARK.finditer(line):
            body = ABSENT_BODY.match(m.group(1))
            if body:
                marks[body.group(1)] = lineno
            else:
                fails.append((path, lineno, "", "malformed adr-absent-identifier marker — "
                                                 "write <!-- adr-absent-identifier: Name — reason -->"))
        if FENCE.match(line):
            infence = not infence
            continue
        if not infence:
            spans.extend((lineno, n) for n in _go_span_names(line))
    return spans, marks, fails


def adr_identifier_fails(adrs):
    """Fails for Go identifiers named in ADR code spans that exist nowhere in the tree.

    adrs maps each ADR path to its lines; one git grep resolves every name.
    """
    scanned = {p: _scan_adr(p, lines) for p, lines in adrs.items()}
    names = {n for spans, marks, _ in scanned.values() for n in [*(x for _, x in spans), *marks]}
    have, err = _existing_go_identifiers(names)
    if err:
        return [("docs/adr", 0, "", f"cannot resolve ADR identifiers against the tree: {err}")]
    fails = []
    for path, (spans, marks, bad) in scanned.items():
        fails.extend(bad)
        used = set()
        for lineno, name in spans:
            used.add(name)
            if name not in have and name not in marks:
                fails.append((path, lineno, name,
                              "Go identifier in an ADR code span exists in no .go file — correct it "
                              "in the ADR text, or mark the preserved original with "
                              "<!-- adr-absent-identifier: Name — reason -->"))
        for name, lineno in marks.items():
            if name in have:
                fails.append((path, lineno, name, "stale adr-absent-identifier marker — the "
                                                  "identifier now exists in the tree; remove it"))
            elif name not in used:
                fails.append((path, lineno, name, "stale adr-absent-identifier marker — no code "
                                                  "span in this file names it; remove it"))
    return fails


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
    argv = sys.argv[1:]
    if argv:
        # Explicit source list (e.g. from lint-changed.sh's diff). Scoping
        # is SOURCE-only: every target lookup below (os.path.exists,
        # _ignored, anchors_of) still resolves against the real filesystem,
        # unrestricted to this set, so a link out of one of these files into
        # an untouched file elsewhere in the tree is still checked in full.
        files = sorted(dict.fromkeys(argv))
        missing = [f for f in files if not os.path.isfile(f)]
        if missing:
            for f in missing:
                print(f"  \033[31mFAIL\033[0m {f} no such file — usage error, not skipped")
            print()
            print(f"lint-doc-links: {len(missing)} argument(s) named no such file")
            return len(missing)
    else:
        files = sorted(
            set(_git("ls-files", "-z", "*.md"))
            | set(_git("ls-files", "-z", "--others", "--exclude-standard", "*.md"))
        )
    fails = []
    adrs = {}
    scanned = 0
    for path in files:
        base = os.path.basename(path)
        if base.startswith("_template") or "TEMPLATE" in base:
            scanned += 1
            continue
        try:
            lines = open(path, encoding="utf-8").read().split("\n")
        except FileNotFoundError:
            # Went away mid-run (e.g. a concurrent process). Anything
            # else — a bad encoding, permissions — must NOT be absorbed
            # here: a bare `except Exception` once let one non-UTF-8 byte
            # skip a file's scan silently while the summary line still
            # reported it as scanned (GH-1255).
            continue
        scanned += 1
        if sum(1 for line in lines if FENCE.match(line)) % 2:
            fails.append((path, 0, "", "has an ODD number of code fences — everything after "
                                       "the stray fence goes unscanned, so this gate is blind to it"))
            continue
        if path.startswith("docs/adr/"):
            adrs[path] = lines
        infence = False
        for lineno, line in enumerate(lines, 1):
            if FENCE.match(line):
                infence = not infence
                continue
            if infence:
                continue
            fails.extend(_adr_span_fails(path, lineno, line))
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

    fails.extend(adr_identifier_fails(adrs))

    if scanned < len(files):
        fails.append(("lint-doc-links", 0, "",
                       f"only {scanned} of {len(files)} discovered markdown file(s) were scanned "
                       "— one or more went away mid-run"))

    for path, lineno, target, why in fails:
        loc = f"{path}:{lineno}" if lineno else path
        arrow = f" -> {target}" if target else ""
        print(f"  \033[31mFAIL\033[0m {loc}{arrow} {why}")

    print()
    if fails:
        print(f"lint-doc-links: {len(fails)} failure(s) across {scanned} markdown file(s)")
    else:
        print("lint-doc-links: OK — every relative link and in-repo anchor resolves across "
              f"{scanned} markdown file(s)")
    return len(fails)


if __name__ == "__main__":
    sys.exit(main())
