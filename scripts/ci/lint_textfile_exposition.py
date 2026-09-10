#!/usr/bin/env python3
"""lint_textfile_exposition.py — every line a textfile-collector producer
writes must parse as Prometheus exposition format.

THE DEFECT THIS EXISTS FOR (r1, 2026-09-10).  The TimescaleDB probe built
its lock-convoy query as ``SET statement_timeout = '10s'; SELECT count(*),
…``.  psql prints the *command tag* for the SET — the bare word ``SET`` —
as its own stdout line, and ``-At`` does not suppress it.  The probe's
``while IFS='|' read -r convoyed worst blocked`` therefore read ``SET``
as its first field and wrote::

    stellarindex_pg_lock_convoy_backends SET

node_exporter does not skip an unparseable line.  It rejects the WHOLE
file, so ``node_textfile_scrape_error`` went to 1 and *every* family in
timescale_jobs.prom vanished — the three new convoy gauges and the 127
pre-existing ``stellarindex_timescale_*`` series, which have nothing to do
with the convoy work.  TimescaleDB job and CAGG health monitoring was dark
for ~20 minutes and nothing alerted, because the alerting that would have
noticed was itself in the file that stopped parsing.

Nothing caught it.  The probe's own self-test passed 34/34 with the defect
present: it stubs psql, so it only ever parsed replies that never contain
a command tag — a shape the real client always produces for a SET.  A test
that supplies its own well-formed input cannot see a malformed-input bug.

WHAT THIS GATE CHECKS.  All of it is static: CI has no Postgres, no
ClickHouse and no host.

  1. CENSUS PARITY.  Producers are discovered from the tree and reconciled
     with scripts/ci/textfile-producers.manifest — in both directions, and
     column by column: the .prom basenames a row claims must appear in the
     producer, and the self-test it names must exist.  A new producer is a
     registration event rather than a silent addition, which is what makes
     the rest of this a sweep instead of a sample.  Discovering zero
     producers is itself a failure, so it cannot pass vacuously.

  2. EXPOSITION GRAMMAR.  Every metric-sample line a producer emits is
     rendered with a canary token in place of each shell/Go/Jinja
     expansion and must then parse: valid metric name, well-formed and
     QUOTED label values, exactly one value field and an optional
     timestamp, nothing after them.  This catches the literal half of the
     class — a stray word, a missing value, an unclosed brace, a label
     interpolated with %s where %q was meant.

  3. HEADERS.  ``# TYPE`` must name a real Prometheus metric type, no
     family may be typed twice inside one unconditional emit run (see
     same_emit_run), and every emitted family carries its own HELP and
     TYPE in the producer that emits it.

  4. COMMAND-TAG HAZARD — the mechanism that actually bit.  A SQL client
     asked for tuples-only output still narrates anything that is not a
     row: psql prints a command tag per statement, and a non-final
     data-returning statement prints its rows too.  So a *single* command
     string handed to such a client must contain exactly ONE statement.
     Carry session settings out of band (``PGOPTIONS='-c
     statement_timeout=10s'``, which is what b96982c22 did) or issue a
     second invocation.  ``-q`` is NOT an accepted remedy: it suppresses
     command tags but not the rows of an extra SELECT.

WHAT THIS GATE DOES NOT CHECK, said plainly, because a gate whose reach is
assumed rather than stated is how the next one of these ships:

  - It cannot know whether ``$overdue`` holds a number at RUNTIME.  That
    is a property of psql's reply, not of the text.  Guarding it belongs
    to the producer (emit values through a numeric guard, and validate the
    rendered file before the atomic ``mv``) and is proven per-producer by
    a self-test driving the shipped bytes against a stub faithful enough
    to emit the malformed shapes a real client produces — see
    scripts/ci/timescale-jobs-probe-test.sh, whose runuser stub models
    psql's command-tag behaviour for exactly this reason.  The manifest's
    third column is the list of producers where no such test exists.

  - A duplicate ``# TYPE`` split across two separate ``{ … } >> "$TMP"``
    groups is not caught: two producers on main legitimately render one
    .prom from two mutually exclusive paths, and telling those apart
    needs control flow this does not have.  The narrow scope is
    deliberate — sound beats broad, because a lint that cries wolf is a
    lint someone disables.

  - Values built inside a helper the gate cannot see through (a Go
    ``writeGauge(name, help, value)``) carry no literal line to inspect.
    That shape is the fix, not a gap: it is where a value can be typed.

Env overrides (used by the self-test): TEXTFILE_LINT_ROOT, TEXTFILE_LINT_MANIFEST.

Exit code = number of findings (0 = clean).
"""

from __future__ import annotations

import io
import os
import re
import sys

# Roots scanned for producers.  scripts/ci is deliberately excluded: the
# gates and their fixtures quote exposition text in order to check it, and
# letting a checker satisfy its own census would make this a tautology.
SCAN_DIRS = ("configs", "scripts", "cmd", "internal", "deploy")
EXCLUDE_DIR_PREFIXES = ("scripts/ci/",)
EXCLUDE_SUFFIXES = (".md", ".json", ".lock", "_test.go")
EXCLUDE_SUBSTRINGS = ("-test.sh", "/testdata/")

# A file is a producer when it both mentions a textfile destination and
# renders exposition headers.  Both halves are needed: the rule files and
# runbooks talk about textfiles without writing one, and plenty of code
# writes files that are not textfiles.
RE_TEXTFILE = re.compile(r"textfile", re.IGNORECASE)
RE_HEADER_ANY = re.compile(r"#\s(?:HELP|TYPE)\s+\S")

# Prefixes that make a bare first token a metric name rather than a shell
# function or a YAML key.  Kept explicit so `emit_intent_metrics 1 0` and
# `update_cache: true` cannot be mistaken for samples.
METRIC_PREFIXES = (
    "stellarindex_",
    "galexie_",
    "node_",
    "pg_",
    "pgbackrest_",
    "ch_",
)

SHELLISH_SUFFIXES = (".sh", ".j2", ".yml", ".yaml", ".prom", ".bash")

# ── exposition grammar ──────────────────────────────────────────────────
RE_METRIC_NAME = re.compile(r"^[a-zA-Z_:][a-zA-Z0-9_:]*$")
RE_LABEL_NAME = re.compile(r"^[a-zA-Z_][a-zA-Z0-9_]*$")
RE_VALUE = re.compile(r"^[+-]?(?:[0-9]+\.?[0-9]*(?:[eE][+-]?[0-9]+)?|\.[0-9]+(?:[eE][+-]?[0-9]+)?|Inf|NaN)$")


def is_value(tok: str) -> bool:
    return tok == CANARY or bool(RE_VALUE.match(tok))
RE_HEADER = re.compile(r"^#\s+(HELP|TYPE)\s+([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\s+(.*))?$")
VALID_TYPES = {"counter", "gauge", "histogram", "summary", "untyped"}

# ── expansion canaries ──────────────────────────────────────────────────
# Every runtime expansion is replaced by CANARY before the line is parsed.
# The token is deliberately NOT a number: a metric name and a metric value
# have incompatible grammars, so a numeric canary would make every
# `fmt.Fprintf(w, "%s{…} %d\n", name, v)` look like an invalid name. The
# parser accepts CANARY in any position instead, which keeps the check on
# the STRUCTURE the producer controls (quoting, field count, brace
# balance) rather than on values it cannot know statically.
#
# Go's %q verb renders its own quotes, so it canaries to a QUOTED token —
# otherwise a correctly-quoted label value would read as an unquoted one.
CANARY = "__SI_CANARY__"
RE_GO_QUOTED_VERB = re.compile(r"%[-+ #0]*[0-9]*(?:\.[0-9]+)*q")
RE_EXPANSIONS = (
    re.compile(r"\$\(\([^)]*\)\)"),            # $(( arithmetic ))
    re.compile(r"\$\((?:[^()]|\([^()]*\))*\)"),  # $( cmd ) with one nesting level
    re.compile(r"\$\{[^{}]*\}"),                # ${var}, ${var:-0}, ${arr[$i]}
    re.compile(r"\$[A-Za-z_][A-Za-z0-9_]*"),    # $var
    re.compile(r"\$[0-9@*#?]"),                 # $1, $@, $#
    re.compile(r"\{\{[^{}]*\}\}"),              # {{ jinja }}
    re.compile(r"%[-+ #0]*[0-9]*(?:\.[0-9]+)*[a-zA-Z]"),  # printf/Go %s %d %.3f %v
)


def canary(text: str) -> str:
    """Render a template line as it would look with every expansion filled."""
    out = RE_GO_QUOTED_VERB.sub('"%s"' % CANARY, text)
    for _ in range(6):  # nested forms need a couple of passes to settle
        before = out
        for rx in RE_EXPANSIONS:
            out = rx.sub(CANARY, out)
        if out == before:
            break
    return out


# ── SQL statement counting ──────────────────────────────────────────────
RE_SQL_LEADING = re.compile(
    r"^\s*(?:--[^\n]*\n\s*)*"
    r"(SELECT|WITH|VALUES|TABLE|SHOW|SET|BEGIN|START|COMMIT|ROLLBACK|RESET|DISCARD|"
    r"INSERT|UPDATE|DELETE|CREATE|ALTER|DROP|TRUNCATE|GRANT|REVOKE|ANALYZE|VACUUM|"
    r"EXPLAIN|COPY|DO|CALL|REFRESH|LOCK|CHECKPOINT|COMMENT|PREPARE|EXECUTE)\b",
    re.IGNORECASE,
)


def strip_sql_noise(sql: str) -> str:
    """Remove comments and string/identifier literals so only structure remains."""
    out = []
    i = 0
    n = len(sql)
    while i < n:
        ch = sql[i]
        if ch == "-" and sql.startswith("--", i):
            j = sql.find("\n", i)
            i = n if j < 0 else j
            continue
        if ch == "/" and sql.startswith("/*", i):
            j = sql.find("*/", i + 2)
            i = n if j < 0 else j + 2
            continue
        if ch in "'\"":
            quote = ch
            i += 1
            while i < n:
                if sql[i] == quote:
                    # doubled quote is an escaped quote inside the literal
                    if i + 1 < n and sql[i + 1] == quote:
                        i += 2
                        continue
                    i += 1
                    break
                i += 1
            out.append(" ")
            continue
        out.append(ch)
        i += 1
    return "".join(out)


def statement_count(sql: str) -> int:
    """Top-level statements in a SQL string (a lone trailing `;` is not one)."""
    bare = strip_sql_noise(sql).strip()
    while bare.endswith(";"):
        bare = bare[:-1].rstrip()
    if not bare:
        return 0
    return 1 + bare.count(";")


# ── source-text helpers ─────────────────────────────────────────────────
def join_continuations(text: str) -> str:
    """Join shell backslash-newline continuations so one command is one line."""
    return re.sub(r"\\\n[ \t]*", " ", text)


def read_quoted(text: str, start: int) -> tuple[str, int]:
    """Read the quoted string opening at `start` (which must be the quote).

    Returns (content, index-after-closing-quote).  Handles backslash escapes
    inside double quotes and spans newlines, which is how the multi-line SQL
    in an ansible `content:` block is written.
    """
    quote = text[start]
    i = start + 1
    buf = []
    n = len(text)
    while i < n:
        ch = text[i]
        if quote == '"' and ch == "\\" and i + 1 < n:
            buf.append(text[i + 1])
            i += 2
            continue
        if quote == '"' and text.startswith("$(", i):
            # Step over a command substitution verbatim. Its own quotes do
            # NOT end the outer string — `echo "a $(echo "b")"` is one shell
            # word — and stopping at the inner quote silently truncates the
            # very line this gate is trying to parse.
            depth = 0
            j = i + 1
            while j < n:
                if text[j] == "(":
                    depth += 1
                elif text[j] == ")":
                    depth -= 1
                    if depth == 0:
                        j += 1
                        break
                j += 1
            buf.append(text[i:j])
            i = j
            continue
        if ch == quote:
            return "".join(buf), i + 1
        buf.append(ch)
        i += 1
    return "".join(buf), n


# Direct `-c "…"` / `-tAc "…"` / `--command "…"` forms.
RE_COMMAND_FLAG = re.compile(r"(?:^|\s)-(?:-command|[A-Za-z]*c)(?:\s+|=)(?=['\"])")
# A shell function that forwards its first argument to a SQL client.
RE_FUNC_DEF = re.compile(r"^[ \t]*([A-Za-z_][A-Za-z0-9_]*)[ \t]*\(\)[ \t]*\{?[ \t]*$", re.MULTILINE)
RE_SQL_CLIENT = re.compile(r"\bpsql\b|\$\{?PSQL\}?|\bclickhouse-client\b")
RE_TUPLES_ONLY = re.compile(
    r"\bpsql\b[^\n|;]*(?:\s-{1,2}(?:tuples-only|no-align)\b|\s-[A-Za-z]*[tA][A-Za-z]*\b)"
    r"|\$\{?PSQL\}?\"?[^\n|;]*\s-[A-Za-z]*[tA][A-Za-z]*\b"
    r"|\bclickhouse-client\b"
)
RE_HEREDOC = re.compile(r"<<-?\s*(['\"]?)([A-Za-z_][A-Za-z0-9_]*)\1")


def sql_call_sites(text: str) -> list[tuple[int, str]]:
    """Every SQL string this file hands to a client, as (line-number, sql).

    Anchored on the invocation rather than scanning for quotes at large: an
    unanchored quote scan over an ansible task file pairs quotes across
    unrelated shell and invents strings nobody ever runs.
    """
    sites: list[tuple[int, str]] = []
    joined = join_continuations(text)

    def lineno(idx: int) -> int:
        return joined.count("\n", 0, idx) + 1

    # (a) `-c "…"` and friends.
    for m in RE_COMMAND_FLAG.finditer(joined):
        q = m.end()
        if q < len(joined) and joined[q] in "\"'":
            sql, _ = read_quoted(joined, q)
            sites.append((lineno(q), sql))

    # (b) a helper function that forwards "$1" to a client, plus its call
    #     sites.  This is the shape the incident took: `q "<sql>"` where
    #     q() runs `psql -At -F'|' -c "$1"`.
    forwarders = []
    for m in RE_FUNC_DEF.finditer(joined):
        name = m.group(1)
        body = joined[m.end():m.end() + 2000]
        end = body.find("\n}")
        if end >= 0:
            body = body[:end]
        if RE_SQL_CLIENT.search(body) and re.search(r'-(?:-command|[A-Za-z]*c)\s+"\$[1@*]"', body):
            forwarders.append(name)
    for name in sorted(set(forwarders)):
        for m in re.finditer(r"^[ \t]*%s[ \t]+(?=['\"])" % re.escape(name), joined, re.MULTILINE):
            sql, _ = read_quoted(joined, m.end())
            sites.append((lineno(m.end()), sql))

    # (c) heredocs fed to a client.
    for m in RE_HEREDOC.finditer(joined):
        line_start = joined.rfind("\n", 0, m.start()) + 1
        line_end = joined.find("\n", m.start())
        line = joined[line_start: line_end if line_end >= 0 else len(joined)]
        if not RE_SQL_CLIENT.search(line):
            continue
        delim = m.group(2)
        body_start = (line_end + 1) if line_end >= 0 else len(joined)
        term = re.search(r"^[ \t]*%s[ \t]*$" % re.escape(delim), joined[body_start:], re.MULTILINE)
        body = joined[body_start: body_start + term.start()] if term else joined[body_start:]
        sites.append((lineno(body_start), body))

    return sites


def uses_tuples_only_client(text: str) -> bool:
    return bool(RE_TUPLES_ONLY.search(join_continuations(text)))


RE_SQL_EXPO_LITERAL = re.compile(r"'([a-zA-Z_:][a-zA-Z0-9_:]*[{ ][^']*)'")
RE_SQL_CLAUSE = re.compile(r"\b(FROM|WHERE|GROUP|ORDER|UNION|HAVING|LIMIT|WINDOW)\b", re.IGNORECASE)


def sql_composed_lines(sql: str) -> list[str]:
    """Exposition lines a query CONCATENATES and prints straight into the file.

    data-freshness.sh redirects psql's stdout into the textfile directly, so
    the exposition text is composed in SQL:

        SELECT 'stellarindex_data_freshness_age_seconds{domain="'||domain||
               '",source="'||src||'"} '||round(age)::text FROM f

    Nothing in the shell resembles a metric line, so a scan of the script
    text alone sees none of it — and this is the producer with the LARGEST
    exposure to the 2026-09-10 class, because every byte psql writes reaches
    the file unexamined. Each `||`-spliced expression becomes a canary and
    the result is checked like any other emitted line.
    """
    out: list[str] = []
    for m in RE_SQL_EXPO_LITERAL.finditer(sql):
        if not m.group(1).split("{", 1)[0].split()[0].startswith(METRIC_PREFIXES):
            continue
        rendered = [m.group(1)]
        pos = m.end()
        n = len(sql)
        while True:
            j = pos
            while j < n and sql[j] in " \t\n":
                j += 1
            if not sql.startswith("||", j):
                break
            j += 2
            while j < n and sql[j] in " \t\n":
                j += 1
            if j < n and sql[j] == "'":
                lit, pos = read_quoted(sql, j)
                rendered.append(lit)
                continue
            # A spliced expression, which may span lines: run to the next
            # top-level `||`, `,`, `;` or clause keyword. Parens and string
            # literals are stepped over, so `greatest(0, (SELECT t FROM tip)
            # - x)::text` is one expression, not four.
            depth = 0
            k = j
            while k < n:
                ch = sql[k]
                if ch == "'":
                    _, k = read_quoted(sql, k)
                    continue
                if ch == "(":
                    depth += 1
                elif ch == ")":
                    if depth == 0:
                        break
                    depth -= 1
                elif depth == 0:
                    if sql.startswith("||", k) or ch in ",;":
                        break
                    if RE_SQL_CLAUSE.match(sql, k):
                        break
                k += 1
            rendered.append(CANARY)
            pos = k
        out.append("".join(rendered))
    return out


# ── emitted-line extraction ─────────────────────────────────────────────
RE_ECHO = re.compile(r"^[ \t]*echo[ \t]+(?:-e[ \t]+|-n[ \t]+)*(.*)$")
RE_PRINTF = re.compile(r"^[ \t]*printf[ \t]+(?:--[ \t]+)?(.*)$")
RE_REDIRECT_TAIL = re.compile(r"[ \t]*(?:\d?>>?|\|)[^\n]*$")
RE_GO_STRING = re.compile(r'"((?:[^"\\]|\\.)*)"')


def unquote(arg: str) -> str | None:
    """First shell word of `arg`, unquoted; None when it is not a plain string."""
    arg = arg.strip()
    if not arg:
        return None
    if arg[0] in "\"'":
        body, _ = read_quoted(arg, 0)
        return body
    return RE_REDIRECT_TAIL.sub("", arg).strip()


def candidate_lines(path: str, text: str) -> list[tuple[int, str]]:
    """Exposition strings this file renders, as (line-number, rendered-line)."""
    out: list[tuple[int, str]] = []
    is_go = path.endswith(".go")
    is_shellish = path.endswith(SHELLISH_SUFFIXES)
    is_prom = path.endswith(".prom")
    # Raw lines are exposition text only inside a heredoc (or in a .prom
    # file). Outside one, a line beginning with `#` is a SHELL COMMENT, and
    # a comment that happens to start "# TYPE …" is prose, not a header —
    # reading it as one is precisely the cry-wolf failure this gate must
    # not have.
    heredoc_delim: str | None = None
    for n, raw in enumerate(text.split("\n"), 1):
        if is_shellish and not is_prom:
            if heredoc_delim is not None:
                if raw.strip() == heredoc_delim:
                    heredoc_delim = None
                    continue
            else:
                hd = RE_HEREDOC.search(raw)
                if hd:
                    heredoc_delim = hd.group(2)
            if heredoc_delim is None and raw.lstrip().startswith("#"):
                continue
        pieces: list[str] = []
        if is_go:
            for m in RE_GO_STRING.finditer(raw):
                lit = m.group(1)
                # An exposition line ALWAYS ends in a newline. Requiring one
                # keeps bare metric-name constants (`verifyArchiveMismatchMetric
                # = "stellarindex_verify_archive_mismatches_total"`) and flag
                # help strings out of the sample set — neither is a rendered
                # line, and treating them as one is how a lint earns its
                # reputation for crying wolf.
                if "\\n" not in lit:
                    continue
                pieces.append(lit.replace("\\n", "\n").replace('\\"', '"'))
        else:
            m = RE_ECHO.match(raw)
            if m:
                # No redirect-stripping before unquote(): a `|` inside a
                # command substitution (`… $(echo "…" | bc)`) is not a
                # pipeline, and cutting there truncates the emitted line.
                # unquote() strips the tail only on the unquoted branch.
                arg = unquote(m.group(1))
                if arg is not None:
                    pieces.append(arg)
            m = RE_PRINTF.match(raw)
            if m:
                arg = unquote(m.group(1))
                if arg is not None:
                    pieces.append(arg.replace("\\n", "\n"))
            if is_shellish and not pieces:
                pieces.append(raw)
        for piece in pieces:
            for line in piece.split("\n"):
                line = line.strip()
                if line:
                    out.append((n, line))
    return out


def declared_families(lines: list[tuple[int, str]]) -> tuple[dict[str, set[str]], list[tuple[int, str, str]]]:
    """(family -> {HELP,TYPE}) and the raw header records, from canaried lines."""
    families: dict[str, set[str]] = {}
    records: list[tuple[int, str, str]] = []
    for n, line in lines:
        if not line.startswith("#"):
            continue
        m = RE_HEADER.match(canary(line))
        if not m:
            continue
        kind, name, rest = m.group(1), m.group(2), (m.group(3) or "")
        families.setdefault(name, set()).add(kind)
        records.append((n, kind, name if kind == "HELP" else f"{name} {rest.strip()}"))
    return families, records


def family_of(first: str) -> str:
    """The family name inside an emitted line's first token.

    restore-drill.sh carries its label set in a variable
    (`stellarindex_restore_drill_failures${lbl}`), so after canarying the
    token reads `…_failures__SI_CANARY__`. That opaque tail is a label set
    this gate cannot see through; the family is the part before it.
    """
    bare = first.split("{", 1)[0]
    while bare.endswith(CANARY):
        bare = bare[: -len(CANARY)]
    return bare


def looks_like_sample(first: str, families: dict[str, set[str]]) -> bool:
    bare = family_of(first)
    if not bare or bare.endswith(":"):
        return False
    if bare in families:
        return True
    return bare.startswith(METRIC_PREFIXES) and bool(RE_METRIC_NAME.match(bare))


def parse_sample(line: str) -> str | None:
    """None when `line` is a valid sample; otherwise the reason it is not."""
    name_part = line
    rest = ""
    if "{" in line:
        close = line.find("}")
        if close < 0:
            return "label set opened with `{` and never closed"
        name_part = line[: line.find("{")]
        labels = line[line.find("{") + 1: close]
        rest = line[close + 1:]
        for chunk in [c for c in split_labels(labels) if c.strip()]:
            if "=" not in chunk:
                return f"label {chunk.strip()!r} has no `=`"
            lname, lval = chunk.split("=", 1)
            if not RE_LABEL_NAME.match(lname.strip()):
                return f"invalid label name {lname.strip()!r}"
            lval = lval.strip()
            if not (len(lval) >= 2 and lval[0] == '"' and lval[-1] == '"'):
                return f"label value {lval!r} is not a quoted string"
    else:
        parts = line.split(None, 1)
        name_part = parts[0]
        rest = parts[1] if len(parts) > 1 else ""
    name_part = name_part.strip()
    while name_part.endswith(CANARY):
        name_part = name_part[: -len(CANARY)]
    if not name_part or not RE_METRIC_NAME.match(name_part):
        return f"invalid metric name {name_part!r}"
    fields = rest.split()
    if not fields:
        return "no value field (node_exporter rejects a bare metric name)"
    if not is_value(fields[0]):
        return f"value {fields[0]!r} is not a float"
    if len(fields) > 2:
        return f"{len(fields)} fields after the metric name; exposition allows a value and an optional timestamp"
    if len(fields) == 2 and not is_value(fields[1]):
        return f"timestamp {fields[1]!r} is not a number"
    return None


def split_labels(labels: str) -> list[str]:
    out, buf, in_str = [], [], False
    i = 0
    while i < len(labels):
        ch = labels[i]
        if in_str:
            if ch == "\\" and i + 1 < len(labels):
                buf.append(labels[i: i + 2])
                i += 2
                continue
            if ch == '"':
                in_str = False
            buf.append(ch)
        elif ch == '"':
            in_str = True
            buf.append(ch)
        elif ch == ",":
            out.append("".join(buf))
            buf = []
        else:
            buf.append(ch)
        i += 1
    out.append("".join(buf))
    return out


# ── discovery + manifest ────────────────────────────────────────────────
def discover(root: str) -> list[str]:
    found = []
    for d in SCAN_DIRS:
        base = os.path.join(root, d)
        if not os.path.isdir(base):
            continue
        for dirpath, _dirnames, filenames in os.walk(base):
            for name in sorted(filenames):
                path = os.path.join(dirpath, name)
                rel = os.path.relpath(path, root).replace(os.sep, "/")
                if rel.startswith(EXCLUDE_DIR_PREFIXES) or rel.endswith(EXCLUDE_SUFFIXES):
                    continue
                if any(s in rel for s in EXCLUDE_SUBSTRINGS):
                    continue
                try:
                    if os.path.getsize(path) > 4_000_000:
                        continue
                    text = io.open(path, encoding="utf-8", errors="ignore").read()
                except OSError:
                    continue
                if RE_TEXTFILE.search(text) and RE_HEADER_ANY.search(text):
                    found.append(rel)
    return sorted(found)


RE_BRANCH = re.compile(r"^[ \t]*(?:if|elif|else|fi|case|esac|;;|\}|\{|for|while|done|return|func\b)")


def same_emit_run(text: str, first_line: int, second_line: int) -> bool:
    """Do two header lines certainly execute in the same rendered file?

    Only "certainly" counts.  Two producers on main render the SAME .prom
    from two MUTUALLY EXCLUSIVE paths — ch-schema-drift.sh has a refusal
    block and a comparison block, internal/supply/textfile.go has a success
    and a failure writer — and each declares its families exactly once per
    path.  Judging that whole-file reports three duplicate TYPEs that can
    never both be written, and a lint that cries wolf is a lint someone
    disables.  So the check is scoped to a contiguous, unconditional run of
    emitted lines: no branch keyword between them, and no gap wide enough
    to have left the block.  That is sound (never a false positive) and
    partial (a duplicate split across two `{ … } >> "$TMP"` groups is not
    caught) — the whole-file guarantee comes from the producer validating
    its own output before the atomic `mv`, not from here.
    """
    lo, hi = sorted((first_line, second_line))
    if hi - lo > 24:
        return False
    body = text.split("\n")[lo:hi - 1]
    return not any(RE_BRANCH.match(line) for line in body)


def load_manifest(path: str) -> dict[str, tuple[str, str]]:
    """producer -> (output-spec, self-test-path-or-'-')."""
    entries: dict[str, tuple[str, str]] = {}
    for raw in io.open(path, encoding="utf-8"):
        line = raw.split("#", 1)[0].strip()
        if not line:
            continue
        parts = line.split()
        entries[parts[0]] = (
            parts[1] if len(parts) > 1 else "",
            parts[2] if len(parts) > 2 else "",
        )
    return entries


# ── checks ──────────────────────────────────────────────────────────────
def check_rendered_file(path: str) -> int:
    """Validate a RENDERED .prom file (not a template). Findings count.

    `--check-file` exists so a producer's self-test can assert on the bytes
    it actually wrote using a parser written from the exposition grammar,
    rather than re-implementing one per test (which would only prove the
    copy agrees with itself) or depending on promtool being installed on
    the runner.
    """
    fails = 0
    seen_type: dict[str, int] = {}
    lines = 0
    for n, raw in enumerate(io.open(path, encoding="utf-8", errors="replace"), 1):
        line = raw.rstrip("\n")
        if not line.strip():
            continue
        lines += 1
        if line.startswith("#"):
            m = RE_HEADER.match(line)
            if not m:
                continue  # a plain comment is legal in exposition text
            kind, name, rest = m.group(1), m.group(2), (m.group(3) or "")
            if kind == "TYPE":
                if rest.strip().lower() not in VALID_TYPES:
                    fails += 1
                    print(f"{path}:{n}: `# TYPE {name} {rest.strip()}` is not a metric type", file=sys.stderr)
                if name in seen_type:
                    fails += 1
                    print(
                        f"{path}:{n}: second `# TYPE` for {name} (first at line {seen_type[name]}) "
                        "— Prometheus rejects the whole file",
                        file=sys.stderr,
                    )
                seen_type[name] = n
            continue
        why = parse_sample(line)
        if why is not None:
            fails += 1
            print(f"{path}:{n}: {why}\n    {line}", file=sys.stderr)
    if lines == 0:
        print(f"{path}: FAIL — no content; refusing to call an empty file valid", file=sys.stderr)
        return 1
    print(f"lint-textfile-exposition: {path}: {lines} line(s), {fails} finding(s).")
    return fails


def main() -> int:
    argv = sys.argv[1:]
    if argv and argv[0] == "--check-file":
        if len(argv) != 2:
            print("usage: lint_textfile_exposition.py --check-file <rendered.prom>", file=sys.stderr)
            return 1
        return check_rendered_file(argv[1])

    root = os.environ.get("TEXTFILE_LINT_ROOT") or os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
    manifest_path = os.environ.get("TEXTFILE_LINT_MANIFEST") or os.path.join(root, "scripts/ci/textfile-producers.manifest")
    fails = 0

    found = discover(root)
    if not found:
        print(
            "lint-textfile-exposition: FAIL — discovered NO textfile-collector producers under "
            f"{'/'.join(SCAN_DIRS)}. The discovery pattern has drifted; this gate must not pass vacuously.",
            file=sys.stderr,
        )
        return 1

    if not os.path.isfile(manifest_path):
        print(f"lint-textfile-exposition: FAIL — manifest not found: {manifest_path}", file=sys.stderr)
        return 1
    manifest = load_manifest(manifest_path)

    # ── 1. census parity ────────────────────────────────────────────────
    for rel in found:
        if rel not in manifest:
            fails += 1
            print(
                f"lint-textfile-exposition: {rel}: NEW textfile-collector producer, not in "
                f"{os.path.relpath(manifest_path, root)}.\n"
                "  Add it, with the .prom file it writes and whether a self-test drives its shipped\n"
                "  bytes. Every family in one .prom shares that file's fate: one unparseable line and\n"
                "  node_exporter drops all of them (r1 2026-09-10).",
                file=sys.stderr,
            )
    for rel in sorted(manifest):
        if rel not in found:
            fails += 1
            print(
                f"lint-textfile-exposition: {rel}: listed in "
                f"{os.path.relpath(manifest_path, root)} but no longer discovered as a producer "
                "— delete the entry, or restore the file.",
                file=sys.stderr,
            )
            continue
        outputs, selftest = manifest[rel]
        if not outputs or not selftest:
            fails += 1
            print(
                f"lint-textfile-exposition: {rel}: manifest row needs all three columns "
                "(producer, output, self-test-or-'-').",
                file=sys.stderr,
            )
            continue
        producer_text = io.open(os.path.join(root, rel), encoding="utf-8", errors="ignore").read()
        for out in outputs.split(","):
            if out.endswith(".prom") and out not in producer_text:
                fails += 1
                print(
                    f"lint-textfile-exposition: {rel}: manifest claims it writes {out}, but that "
                    "name does not appear in the file. A stale output column hides which families "
                    "share a fate — fix the column, or use `operator-supplied` if the path now "
                    "arrives as a flag.",
                    file=sys.stderr,
                )
        if selftest != "-" and not os.path.isfile(os.path.join(root, selftest)):
            fails += 1
            print(
                f"lint-textfile-exposition: {rel}: manifest names self-test {selftest}, which does "
                "not exist. Point it at the test that drives this producer's shipped bytes, or "
                "set `-` to record that nothing does.",
                file=sys.stderr,
            )

    # ── 2-4. per-producer checks ────────────────────────────────────────
    checked_lines = 0
    checked_sql = 0
    for rel in found:
        text = io.open(os.path.join(root, rel), encoding="utf-8", errors="ignore").read()
        lines = candidate_lines(rel, text)
        families, headers = declared_families(lines)

        # 3a. `# TYPE` well-formedness, and no family typed twice inside one
        #     unconditional emit run (see same_emit_run for why the scope is
        #     that narrow rather than whole-file).
        seen_type: dict[str, int] = {}
        for n, kind, payload in headers:
            if kind != "TYPE":
                continue
            name = payload.split(None, 1)[0]
            if CANARY in name:
                continue  # `# TYPE %s gauge` — the name is a runtime value
            mtype = (payload.split(None, 1)[1] if " " in payload else "").strip().lower()
            if mtype and mtype not in VALID_TYPES:
                fails += 1
                print(
                    f"lint-textfile-exposition: {rel}:{n}: `# TYPE {name} {mtype}` is not a Prometheus "
                    f"metric type ({'/'.join(sorted(VALID_TYPES))}).",
                    file=sys.stderr,
                )
            prev = seen_type.get(name)
            if prev is not None and same_emit_run(text, prev, n):
                fails += 1
                print(
                    f"lint-textfile-exposition: {rel}:{n}: second `# TYPE` line for {name} "
                    f"(first at line {prev}), in the same emit run. Prometheus rejects the whole "
                    "file on a duplicate TYPE, taking every other family in it with it.",
                    file=sys.stderr,
                )
            seen_type[name] = n

        emitted: set[str] = set()
        for n, line in lines:
            if line.startswith("#"):
                continue
            rendered = canary(line)
            first = rendered.split(None, 1)[0] if rendered.split() else ""
            if not first or not looks_like_sample(first, families):
                continue
            checked_lines += 1
            emitted.add(family_of(first))
            why = parse_sample(rendered)
            if why is not None:
                fails += 1
                print(
                    f"lint-textfile-exposition: {rel}:{n}: emitted line does not parse as Prometheus "
                    f"exposition — {why}.\n"
                    f"    rendered: {rendered}\n"
                    "  node_exporter rejects the ENTIRE .prom file on one bad line.",
                    file=sys.stderr,
                )

        # ── 4. SQL: composed exposition, and the command-tag hazard ─────
        tuples_only = uses_tuples_only_client(text)
        for n, sql in sql_call_sites(text):
            if not RE_SQL_LEADING.match(sql):
                continue
            checked_sql += 1

            # 4a. lines the query concatenates and prints straight into the
            #     textfile (data-freshness.sh). Same grammar, different door.
            for composed in sql_composed_lines(sql):
                checked_lines += 1
                emitted.add(family_of(composed.split(None, 1)[0]))
                why = parse_sample(composed)
                if why is not None:
                    fails += 1
                    print(
                        f"lint-textfile-exposition: {rel}:{n}: this query composes an exposition "
                        f"line that does not parse — {why}.\n"
                        f"    rendered: {composed}\n"
                        "  psql's stdout is redirected into the .prom, so this text IS the file.",
                        file=sys.stderr,
                    )

            # 4b. the command-tag hazard.
            count = statement_count(sql)
            if tuples_only and count > 1:
                fails += 1
                head = " ".join(sql.split())[:96]
                print(
                    f"lint-textfile-exposition: {rel}:{n}: {count} SQL statements in one command "
                    "string handed to a tuples-only client.\n"
                    f"    sql: {head}…\n"
                    "  The client narrates every statement that is not the rows you want: psql prints\n"
                    "  a command tag (`SET`, `BEGIN`, `UPDATE 3`) on stdout and `-At` does not suppress\n"
                    "  it, so the caller's field parser reads that word as data and writes it as a\n"
                    "  metric value. That is r1 2026-09-10: `stellarindex_pg_lock_convoy_backends SET`\n"
                    "  made node_exporter reject the whole file, taking 127 unrelated\n"
                    "  stellarindex_timescale_* series off the host for ~20 minutes.\n"
                    "  Fix: carry session settings out of band —\n"
                    "    PGOPTIONS=\"-c statement_timeout=10s\" psql … -c \"<one statement>\"\n"
                    "  or issue a second invocation. `-q` is not enough: it hides command tags but not\n"
                    "  the rows of an extra SELECT.",
                    file=sys.stderr,
                )

        # ── 3b. every emitted family is declared in the producer that
        #        emits it. Runs last so SQL-composed families count too.
        for name in sorted(emitted):
            missing = {"HELP", "TYPE"} - families.get(name, set())
            if missing:
                fails += 1
                print(
                    f"lint-textfile-exposition: {rel}: emits {name} but the producer renders no "
                    f"`# {'` / `# '.join(sorted(missing))}` line for it.",
                    file=sys.stderr,
                )

    print(
        f"lint-textfile-exposition: {len(found)} producers checked "
        f"({len(manifest)} in manifest), {checked_lines} emitted metric lines, "
        f"{checked_sql} SQL command strings, {fails} finding(s)."
    )
    return fails


if __name__ == "__main__":
    sys.exit(min(main(), 250))
