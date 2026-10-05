"""ADR template, ADR decision immutability, and citation resolution.

1. TEMPLATE — every ADR not listed in lint-adr-refs.baseline has exactly the
   sections of docs/adr/_template.md, in order; Context is at most 3 lines; no
   dated amendment blockquotes (amendments are folded into Decision); at most
   150 lines; the median over template ADRs is at most 60 lines and, from ten
   ADRs on, p90 is at most 100. A baseline entry that already conforms fails,
   so the baseline only shrinks.
2. IMMUTABILITY — the Decision and Invariant sections of an Accepted template
   ADR hash to the value in docs/adr/decision-hashes.txt (whitespace-folded, so
   reflowing is allowed and rewording is not). `--record` adds missing entries
   and never overwrites one: a changed decision is a new ADR (README rule 2).
3. CITATIONS — every `ADR-NNNN` names an ADR file or an index row, every
   "AGENTS.md invariant N" names a [N] id in AGENTS.md, and every
   `docs/<path>.md#anchor` names a heading that exists. Shell self-tests
   (`*-test.sh`) are skipped: their citations are fixtures.

Usage: lint_adr_refs.py [--root DIR] [--record]
"""

import hashlib
import os
import re
import statistics
import subprocess
import sys

sys.dont_write_bytecode = True  # importing lint_doc_links must not leave __pycache__ in the tree
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from lint_doc_links import anchors_of, slug  # noqa: E402

SECTIONS = ["Context", "Decision", "Invariant", "Consequences", "Evidence"]
HASHED = ("Decision", "Invariant")
MAX_LINES, MEDIAN_LINES, P90_LINES, CONTEXT_LINES = 150, 60, 100, 3
AMENDMENT = re.compile(r"^>\s*\**\s*(amendment|reality note|update)\b", re.I)
ADR_REF = re.compile(r"\bADR-(\d{4})\b")
INV_REF = re.compile(r"AGENTS\.md invariant (\d+)", re.I)
ANCHOR_REF = re.compile(r"(?<![\w/.-])(docs/[\w./-]+\.md)#([\w-]+)")
INDEX_ROW = re.compile(r"^\|\s*\[?(\d{4})\b", re.M)
SCAN_EXT = (".go", ".md", ".yml", ".yaml", ".sql", ".sh", ".py", ".toml", ".j2", ".ts", ".tsx")
SKIP_DIRS = {".git", "node_modules", ".next", "vendor"}


def sections(lines):
    """[(name, body_lines)] for each '## ' heading, fences respected."""
    out, infence = [], False
    for line in lines:
        if line.lstrip().startswith(("```", "~~~")):
            infence = not infence
        if not infence and line.startswith("## "):
            out.append((line[3:].strip(), []))
        elif out:
            out[-1][1].append(line)
    return out


def front(lines, key):
    for line in lines[1:]:
        if line.strip() == "---":
            return None
        if line.startswith(key + ":"):
            return line.split(":", 1)[1].strip()
    return None


def decision_hash(secs):
    text = "\n".join(f"## {n}\n" + "\n".join(b) for n, b in secs if n in HASHED)
    return hashlib.sha256(" ".join(text.split()).encode()).hexdigest()


def template_fails(path, lines):
    secs = sections(lines)
    fails = []
    if [n for n, _ in secs] != SECTIONS:
        fails.append(f"sections are {[n for n, _ in secs]}, want {SECTIONS} (docs/adr/_template.md)")
    ctx = [b for n, b in secs if n == "Context"]
    if ctx and sum(1 for x in ctx[0] if x.strip()) > CONTEXT_LINES:
        fails.append(f"Context is longer than {CONTEXT_LINES} lines")
    for i, line in enumerate(lines, 1):
        if AMENDMENT.match(line):
            fails.append(f"line {i}: amendment blockquote; fold the correction into Decision")
    if len(lines) > MAX_LINES:
        fails.append(f"{len(lines)} lines, max {MAX_LINES}")
    return [f"{path}: {f}" for f in fails]


def scan_files(root):
    try:
        out = subprocess.run(["git", "-C", root, "ls-files", "-co", "--exclude-standard"],
                             capture_output=True, text=True, check=True).stdout.split("\n")
        return [p for p in out if p.endswith(SCAN_EXT) and os.path.isfile(os.path.join(root, p))]
    except (subprocess.CalledProcessError, FileNotFoundError):
        files = []
        for d, dirs, names in os.walk(root):
            dirs[:] = [x for x in dirs if x not in SKIP_DIRS]
            files += [os.path.relpath(os.path.join(d, n), root) for n in names if n.endswith(SCAN_EXT)]
        return files


def main(argv):
    root = argv[argv.index("--root") + 1] if "--root" in argv else os.getcwd()
    record = "--record" in argv
    adr_dir = os.path.join(root, "docs/adr")
    hash_path = os.path.join(adr_dir, "decision-hashes.txt")
    base_path = os.path.join(root, "scripts/ci/lint-adr-refs.baseline")
    read = lambda p: open(p, encoding="utf-8").read()  # noqa: E731
    fails = []

    baseline = set()
    if os.path.exists(base_path):
        baseline = {x.strip() for x in read(base_path).split("\n") if x.strip() and not x.startswith("#")}
    hashes = {}
    if os.path.exists(hash_path):
        for x in read(hash_path).split("\n"):
            if x.strip() and not x.startswith("#"):
                num, h = x.split()
                hashes[num] = h

    adrs = sorted(f for f in os.listdir(adr_dir) if re.match(r"\d{4}-.*\.md$", f))
    for f in baseline - set(adrs):
        fails.append(f"lint-adr-refs.baseline: {f} does not exist; remove it")
    tmpl = os.path.join(adr_dir, "_template.md")
    fails += template_fails("docs/adr/_template.md", read(tmpl).split("\n")[:-1])

    sizes, new_hashes = [], {}
    for f in adrs:
        rel, lines = f"docs/adr/{f}", read(os.path.join(adr_dir, f)).split("\n")[:-1]
        tf = template_fails(rel, lines)
        if f in baseline:
            if not tf:
                fails.append(f"{rel}: on the template now; remove it from lint-adr-refs.baseline")
            continue
        fails += tf
        sizes.append(len(lines))
        if tf or front(lines, "status") != "Accepted":
            continue
        num, h = f[:4], decision_hash(sections(lines))
        if num not in hashes:
            if record:
                new_hashes[num] = h
            else:
                fails.append(f"{rel}: no recorded Decision/Invariant hash; run "
                             "scripts/ci/lint-adr-refs.sh --record in the PR that adopts the template")
        elif hashes[num] != h:
            fails.append(f"{rel}: Decision or Invariant changed; they are immutable once Accepted "
                         "(docs/adr/README.md rule 1). A changed decision is a new ADR (rule 2)")
    if sizes and statistics.median(sizes) > MEDIAN_LINES:
        fails.append(f"docs/adr: template ADRs median {statistics.median(sizes)} lines, max {MEDIAN_LINES}")
    if len(sizes) >= 10:
        p90 = sorted(sizes)[int(len(sizes) * 0.9) - 1]
        if p90 > P90_LINES:
            fails.append(f"docs/adr: template ADRs p90 {p90} lines, max {P90_LINES}")

    numbers = {f[:4] for f in adrs} | set(INDEX_ROW.findall(read(os.path.join(adr_dir, "README.md"))))
    agents = os.path.join(root, "AGENTS.md")
    invariants = set(re.findall(r"\*\*\[(\d+)\]\*\*|^#+ .*invariant (\d+)", read(agents), re.M)) if os.path.exists(agents) else set()
    invariants = {a or b for a, b in invariants}
    for rel in (p for p in scan_files(root) if not p.endswith("-test.sh")):
        try:
            text = read(os.path.join(root, rel))
        except (UnicodeDecodeError, OSError):
            continue
        for i, line in enumerate(text.split("\n"), 1):
            for n in ADR_REF.findall(line):
                if n not in numbers:
                    fails.append(f"{rel}:{i}: ADR-{n} has no file in docs/adr and no index row")
            for n in INV_REF.findall(line):
                if n not in invariants:
                    fails.append(f"{rel}:{i}: AGENTS.md has no invariant [{n}]")
            for doc, frag in ANCHOR_REF.findall(line):
                have = anchors_of(os.path.join(root, doc))
                if have is None:
                    fails.append(f"{rel}:{i}: {doc} does not exist")
                elif slug(frag) not in have:
                    fails.append(f"{rel}:{i}: {doc} has no heading #{frag}")

    if new_hashes:
        merged = {**hashes, **new_hashes}
        with open(hash_path, "w", encoding="utf-8") as fh:
            fh.write("# ADR-number sha256(Decision+Invariant), written by lint-adr-refs.sh --record\n")
            fh.writelines(f"{k} {merged[k]}\n" for k in sorted(merged))
        print(f"lint-adr-refs: recorded {len(new_hashes)} hash(es) in docs/adr/decision-hashes.txt")
    for f in fails:
        print(f"FAIL {f}")
    if fails:
        print(f"lint-adr-refs: {len(fails)} failure(s)")
        return 1
    print(f"lint-adr-refs: OK ({len(adrs) - len(baseline)} template ADR(s), {len(baseline)} pending in baseline)")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
