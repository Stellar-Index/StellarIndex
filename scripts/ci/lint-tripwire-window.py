#!/usr/bin/env python3
"""Reject an alert whose `for:` equals the window of the event function it
compares against zero (audit Q261).

THE DEFECT. An "any nonzero increase needs eyes" tripwire written as

    expr: increase(m[W]) > 0          # or rate()/irate()/changes()/resets()
    for:  W

cannot fire on the one case a tripwire exists for — a single isolated
increment. After an increment at t the expression is true only for
evaluations in (t, t+W]. The alert goes pending at the first of those and
needs the expression to STAY true for a further `for:`, so with
`for: W` the last true evaluation always lands before the pending period
elapses. The rule fires only while increments keep arriving: a
sustained-rate alert carrying a tripwire's description. Ten rules per tree
had the shape; five were tripwires by their own text ("Sensitive — any
nonzero increase", "A delivery hit the 15-attempt retry budget") and had
never been able to report a lone event. promtool could not see it, because
every case fed a ramp — the one input that does fire.

`rate(m[W]) > 0` is the same predicate (rate is increase divided by W), so
it is checked too, and so are irate/changes/resets, which are also non-zero
only while an event is inside the window.

WHAT IS FLAGGED. An alert rule whose `for:` EQUALS the range of any
`<fn>(...[W])` that the expression compares `> 0`, directly or through
enclosing aggregations (`sum by (x) (increase(...[W])) > 0`). A threshold
other than zero is a rate alert and is not this lint's business, and the
right-hand side of an `unless` is a suppressor, not a trigger.

WHY EQUALITY AND NOT `>=`. A `for:` LONGER than the window is just as
unable to fire on one event, but it does not read as a tripwire:
`rate(errors[5m]) > 0` with `for: 30m` says "has been failing continuously
for half an hour". Fifteen rules per tree are written that way (2026-09);
Q261 did not examine them and this lint leaves them alone. `for: W` beside
`[W]` is the shape that arises by accident — the window copied into `for:`
— and every rule Q261 found mute had it.

THE WAIVER. Some rules want exactly this behaviour: "drops are normal in
rare bursts, tell me when dropping is continuous". That is a decision, so it
is written down where the rule is — a line in the comment block directly
above `- alert:`:

    # lint-tripwire-window:sustained: <why one isolated event must not notify>

The reason is mandatory, waivers are counted in the output, and a waiver on
a rule that does not have the shape is itself a failure, so the exempt set
cannot outlive the rules it was granted to. Same idiom as
lint-go-typographic-quotes.py's `lint-quotes:ok: <reason>`.

USAGE
    lint-tripwire-window.py                 both shipped rule trees
    lint-tripwire-window.py DIR [DIR...]    those directories (fixtures)
    lint-tripwire-window.py --self-test     prove the detector detects

lint-rule-structure.py runs `--self-test` and then the default form, so this
gate rides every place that one is wired (lint-changed.sh, verify.sh, the
alert-rules CI job) and each run first demonstrates it can go red.
Exit 0 clean, 1 findings, 2 cannot run / nothing to check.
"""
import glob
import os
import re
import sys

try:
    import yaml
except ImportError:
    # Fail closed, as the sibling rule lints do: an exit 0 without PyYAML is
    # a pass over nothing.
    print("lint-tripwire-window: FAIL — PyYAML not available; cannot lint rule "
          "files (install pyyaml; refusing to pass vacuously)", file=sys.stderr)
    sys.exit(2)

DEFAULT_DIRS = ["deploy/monitoring/rules", "configs/prometheus/rules.r1"]
EVENT_FUNCS = ("increase", "rate", "irate", "changes", "resets")
WAIVER = "lint-tripwire-window:sustained:"
MIN_REASON_CHARS = 20

_UNIT_MS = {"ms": 1, "s": 1000, "m": 60_000, "h": 3_600_000,
            "d": 86_400_000, "w": 604_800_000, "y": 31_536_000_000}
_DUR_PART_RE = re.compile(r"(\d+)(ms|[smhdwy])")
_CALL_RE = re.compile(r"\b(%s)\s*\(" % "|".join(EVENT_FUNCS))
_RANGE_RE = re.compile(r"\[\s*([0-9a-z]+)\s*(?::[^\]]*)?\]")
# What may sit between the call's closing paren and the comparison: closing
# parens of enclosing aggregations and a trailing `by (...)`/`without (...)`.
_TAIL_RE = re.compile(
    r"(?:\s*\)|\s*(?:by|without)\s*\([^)]*\))*\s*>\s*0(?![0-9.eExX_])")
_ALERT_LINE_RE = re.compile(r"^\s*-\s*alert:\s*(\S+)\s*$")


def parse_duration_ms(text):
    """Prometheus duration ("15m", "1h30m", "0") -> milliseconds, or None."""
    text = str(text).strip()
    if text in ("0", ""):
        return 0
    pos, total = 0, 0
    while pos < len(text):
        m = _DUR_PART_RE.match(text, pos)
        if not m:
            return None
        total += int(m.group(1)) * _UNIT_MS[m.group(2)]
        pos = m.end()
    return total


def strip_promql_comments(expr):
    return "\n".join(line.split("#", 1)[0] for line in expr.splitlines())


def _matching_paren(text, open_idx):
    depth = 0
    for i in range(open_idx, len(text)):
        if text[i] == "(":
            depth += 1
        elif text[i] == ")":
            depth -= 1
            if depth == 0:
                return i
    return -1


_STRING_RE = re.compile(r'"(?:[^"\\]|\\.)*"|\'(?:[^\'\\]|\\.)*\'')
_SETOP_RE = re.compile(r"\b(unless|and|or)\b")


def mask_unless_operands(expr):
    """Blank out the right-hand operand of every `unless`.

    `a unless rate(b[10m]) > 0` uses the comparison as a SUPPRESSOR: an
    isolated event on b silences the alert, it does not raise it, so it is
    not a tripwire and `for:` equal to that window is no defect
    (stellarindex_ingestion_duplicate_flood is the shipped example). and/
    unless share a precedence and associate left, `or` binds looser, so the
    operand runs to the next set operator at the same paren depth or to the
    paren that closes the enclosing group. Length is preserved so offsets
    into the original stay valid.
    """
    expr = _STRING_RE.sub(lambda m: '"' + "_" * (len(m.group(0)) - 2) + '"', expr)
    out = list(expr)
    depth, i, mask_depth = 0, 0, None
    while i < len(expr):
        ch = expr[i]
        if ch == "(":
            depth += 1
        elif ch == ")":
            depth -= 1
            if mask_depth is not None and depth < mask_depth:
                mask_depth = None
        else:
            m = _SETOP_RE.match(expr, i) if (i == 0 or not (expr[i - 1].isalnum() or expr[i - 1] == "_")) else None
            if m and (mask_depth is None or depth == mask_depth):
                mask_depth = depth if m.group(1) == "unless" else None
                i = m.end()
                continue
        if mask_depth is not None and ch not in "()\n":
            out[i] = " "
        i += 1
    return "".join(out)


def zero_compared_windows(expr):
    """[(func, window_text, window_ms)] for every event call compared `> 0`."""
    expr = mask_unless_operands(strip_promql_comments(str(expr)))
    out = []
    for m in _CALL_RE.finditer(expr):
        close = _matching_paren(expr, m.end() - 1)
        if close < 0:
            continue
        ranges = _RANGE_RE.findall(expr[m.end():close])
        if not ranges or not _TAIL_RE.match(expr, close + 1):
            continue
        # The function's own range is the LAST selector range before its
        # closing paren (an inner subquery range, if any, comes first).
        window = ranges[-1]
        ms = parse_duration_ms(window)
        if ms:
            out.append((m.group(1), window, ms))
    return out


def waivers_by_alert_index(text):
    """index of the i-th `- alert:` line -> waiver reason ('' = bare marker).

    Only the contiguous comment block directly above the alert counts, so a
    waiver cannot leak onto the next rule down.
    """
    lines = text.splitlines()
    found, idx = {}, 0
    for n, line in enumerate(lines):
        if not _ALERT_LINE_RE.match(line):
            continue
        k = n - 1
        while k >= 0 and lines[k].lstrip().startswith("#"):
            if WAIVER in lines[k]:
                reason = lines[k].split(WAIVER, 1)[1].strip()
                # A reason may wrap onto the comment lines that follow it.
                for j in range(k + 1, n):
                    reason += " " + lines[j].lstrip().lstrip("#").strip()
                found[idx] = reason.strip()
                break
            k -= 1
        idx += 1
    return found, idx


def lint_text(path, text):
    """-> (errors[], n_alerts, n_compared, n_waived)."""
    errors = []
    try:
        doc = yaml.safe_load(text)
    except yaml.YAMLError as e:
        return [f"{path}: YAML parse error: {e}"], 0, 0, 0
    alerts = []
    for g in (doc or {}).get("groups") or []:
        for r in (g or {}).get("rules") or []:
            if isinstance(r, dict) and "alert" in r:
                alerts.append(r)
    waivers, n_lines = waivers_by_alert_index(text)
    if n_lines != len(alerts):
        # A waiver is matched to its rule by position. If the raw scan and
        # the parser disagree about how many alerts there are, that mapping
        # is unsound — refuse rather than waive the wrong rule.
        return [f"{path}: found {n_lines} `- alert:` line(s) but the YAML has "
                f"{len(alerts)} alert rule(s); cannot place waivers reliably"], \
            len(alerts), 0, 0
    n_compared = n_waived = 0
    for i, r in enumerate(alerts):
        name = r.get("alert")
        for_ms = parse_duration_ms(r.get("for", "0"))
        if for_ms is None:
            errors.append(f"{path}: alert '{name}': cannot parse `for: {r.get('for')}`")
            continue
        windows = zero_compared_windows(r.get("expr", ""))
        n_compared += len(windows)
        hits = [(f, w) for (f, w, ms) in windows if for_ms and for_ms == ms]
        if i in waivers:
            if not hits:
                errors.append(
                    f"{path}: alert '{name}': carries `{WAIVER}` but has no "
                    f"`for:` == event-window shape to waive — remove the stale waiver")
            elif len(waivers[i]) < MIN_REASON_CHARS:
                errors.append(
                    f"{path}: alert '{name}': `{WAIVER}` needs a reason (why one "
                    f"isolated event must not notify); a bare marker waives nothing")
            else:
                n_waived += 1
            continue
        for func, window in hits:
            errors.append(
                f"{path}: alert '{name}': `for: {r.get('for')}` equals the window of "
                f"`{func}(...[{window}]) > 0`. One isolated increment keeps that "
                f"expression true for only {window}, so the pending period can never "
                f"complete and the alert cannot fire on a single event. If any "
                f"increment needs eyes use `for: 0m` (a shorter non-zero `for:` only "
                f"delays it); if only a CONTINUING condition should notify, say so "
                f"above the rule: `# {WAIVER} <reason>`")
    return errors, len(alerts), n_compared, n_waived


def lint_dirs(dirs):
    errors, files, alerts, compared, waived = [], 0, 0, 0, 0
    for d in dirs:
        if not os.path.isdir(d):
            errors.append(f"{d}: rule directory does not exist — a moved directory "
                          f"would otherwise make this lint a pass over nothing")
            continue
        paths = sorted(glob.glob(os.path.join(d, "*.yml")))
        if not paths:
            errors.append(f"{d}: matched zero *.yml files — nothing to lint")
            continue
        for path in paths:
            with open(path, encoding="utf-8") as fh:
                e, a, c, w = lint_text(path, fh.read())
            errors += e
            files += 1
            alerts += a
            compared += c
            waived += w
    return errors, files, alerts, compared, waived


# ───────────────────────────── self-test ─────────────────────────────
def _rule(name, expr, for_, comment=""):
    body = f"      - alert: {name}\n        expr: |\n"
    body += "".join(f"          {line}\n" for line in expr.splitlines())
    if for_ is not None:
        body += f"        for: {for_}\n"
    return comment + body


def _doc(*rules):
    return "groups:\n  - name: g\n    rules:\n" + "".join(rules)


_WAIVED = ("      # lint-tripwire-window:sustained: bursts are normal, only "
           "continuous dropping notifies\n")
SELF_TESTS = [
    # (label, yaml, expected number of errors, expected number waived)
    ("increase[W] > 0 with for: W is rejected",
     _doc(_rule("a", "increase(m_total[15m]) > 0", "15m")), 1, 0),
    ("the rate() twin under sum() is rejected (the api.yml webhook shape)",
     _doc(_rule("a", 'sum(rate(m_total{outcome="exhausted"}[1h])) > 0', "1h")), 1, 0),
    ("sum by (x) (increase(...)) > 0 is rejected",
     _doc(_rule("a", "sum by (source) (increase(m_total[10m])) > 0", "10m")), 1, 0),
    ("the trailing `by (x)` aggregation form is rejected",
     _doc(_rule("a", "sum(increase(m_total[10m])) by (source) > 0", "10m")), 1, 0),
    ("for: LONGER than the window reads as 'continuously for' and passes",
     _doc(_rule("a", "rate(m_total[5m]) > 0", "30m")), 0, 0),
    ("`> 0` on the right of `unless` is a suppressor, not a tripwire",
     _doc(_rule("a", "sum by (s) (rate(d_total[10m])) > 0.5\nunless on (s)\n"
                     "sum by (s) (rate(n_total[10m])) > 0", "10m")), 0, 0),
    ("the shape on the LEFT of `unless` is still rejected",
     _doc(_rule("a", "increase(m_total[10m]) > 0\nunless on (s) up == 1", "10m")), 1, 0),
    ("an `or` arm after an `unless` operand is live again, and rejected",
     _doc(_rule("a", "g > 1 unless rate(n_total[5m]) > 0\nor\n"
                     "increase(m_total[10m]) > 0", "10m")), 1, 0),
    ("a label VALUE containing ` unless ` masks nothing",
     _doc(_rule("a", 'increase(m_total{reason="drop unless retried"}[10m]) > 0', "10m")), 1, 0),
    ("mixed-unit equality (60m vs 1h) is rejected",
     _doc(_rule("a", "increase(m_total[1h]) > 0", "60m")), 1, 0),
    ("the shape inside an `or` arm is rejected",
     _doc(_rule("a", "increase(m_total[15m]) > 0\nor\n"
                     "(m_total > 0 unless m_total offset 15m)", "15m")), 1, 0),
    ("changes() > 0 is rejected",
     _doc(_rule("a", "changes(m[30m]) > 0", "30m")), 1, 0),
    ("for: 0m passes",
     _doc(_rule("a", "increase(m_total[15m]) > 0", "0m")), 0, 0),
    ("an absent for: passes",
     _doc(_rule("a", "increase(m_total[15m]) > 0", None)), 0, 0),
    ("for: shorter than the window passes",
     _doc(_rule("a", "increase(m_total[1h]) > 0", "15m")), 0, 0),
    ("a non-zero threshold is a rate alert, not this lint's business",
     _doc(_rule("a", "rate(m_total[5m]) > 0.1", "5m")), 0, 0),
    ("`> 0.5` is not `> 0`",
     _doc(_rule("a", "increase(m_total[5m]) > 0.5", "5m")), 0, 0),
    ("`== 0` (a stall alert) passes",
     _doc(_rule("a", "increase(m_total[10m]) == 0", "10m")), 0, 0),
    ("a PromQL comment mentioning the shape is not the shape",
     _doc(_rule("a", "# was increase(m_total[5m]) > 0\nm_gauge > 3", "5m")), 0, 0),
    ("a waiver with a reason waives, and is counted",
     _doc(_rule("a", "increase(m_total[10m]) > 0", "10m", _WAIVED)), 0, 1),
    ("a bare waiver with no reason waives nothing",
     _doc(_rule("a", "increase(m_total[10m]) > 0", "10m",
                "      # lint-tripwire-window:sustained:\n")), 1, 0),
    ("a waiver does not leak onto the NEXT rule",
     _doc(_rule("a", "increase(m_total[10m]) > 0", "10m", _WAIVED),
          _rule("b", "increase(n_total[10m]) > 0", "10m")), 1, 1),
    ("a waiver separated from its rule by a blank line does not count",
     _doc(_rule("a", "increase(m_total[10m]) > 0", "10m", _WAIVED + "\n")), 1, 0),
    ("a stale waiver on a rule without the shape is rejected",
     _doc(_rule("a", "increase(m_total[10m]) > 0", "0m", _WAIVED)), 1, 0),
]


def self_test():
    failed = 0
    for label, text, want_err, want_waived in SELF_TESTS:
        errors, _alerts, _compared, waived = lint_text("<self-test>", text)
        if len(errors) == want_err and waived == want_waived:
            continue
        failed += 1
        print(f"  SELF-TEST FAIL — {label}: want {want_err} error(s)/{want_waived} "
              f"waived, got {len(errors)}/{waived}", file=sys.stderr)
        for e in errors:
            print(f"      | {e}", file=sys.stderr)
    if failed:
        print(f"lint-tripwire-window: self-test FAILED ({failed} of "
              f"{len(SELF_TESTS)} cases)", file=sys.stderr)
        return 1
    print(f"lint-tripwire-window: self-test OK — {len(SELF_TESTS)} of "
          f"{len(SELF_TESTS)} cases")
    return 0


def main(argv):
    if argv[:1] == ["--self-test"]:
        return self_test()
    dirs = argv or DEFAULT_DIRS
    errors, files, alerts, compared, waived = lint_dirs(dirs)
    for e in errors:
        print(f"  {e}")
    tally = (f"{alerts} alert rule(s) in {files} file(s) over {len(dirs)} "
             f"dir(s); {compared} zero-compared event window(s), {waived} waived "
             f"as sustained signals")
    if errors:
        print(f"lint-tripwire-window: {len(errors)} problem(s) — {tally}",
              file=sys.stderr)
        return 1
    if alerts == 0:
        print(f"lint-tripwire-window: FAIL — checked no alert rules ({tally}); "
              f"refusing to pass over nothing", file=sys.stderr)
        return 2
    print(f"lint-tripwire-window: OK — {tally}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
