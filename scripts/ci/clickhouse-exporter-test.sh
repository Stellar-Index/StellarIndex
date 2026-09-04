#!/usr/bin/env bash
# clickhouse-exporter-test.sh — the ClickHouse Prometheus endpoint, end to
# end through the pieces that have to agree.
#
# WHY THIS EXISTS (r1, 2026-09-03): ClickHouse serves its own /metrics —
# there is no exporter package — but the stock config.xml ships the entire
# `<prometheus>` block INSIDE AN XML COMMENT. On r1 that meant nothing
# listening on 9363 and zero `ClickHouse*` series in Prometheus, so the
# lake behind every explorer read had no health signal at all, while the
# file on disk looked like it configured one. A commented-out block is the
# specific shape this test pins: it renders the role template and requires
# the element to be present in a real XML PARSE, which a comment is not.
#
# The other half is the seam. A template that renders port 9363, a task
# that installs it somewhere ClickHouse does not read, a scrape job on a
# different port, or a rule selecting a job nobody scrapes are each
# individually green and collectively dead. Four files have to agree on
# two values (the port and the job name); this asserts they do.
#
# Rendering needs a python with jinja2 + PyYAML. Same discovery as
# lint-jinja-templates.sh (ansible ships its own interpreter): fail-closed
# in CI, graceful skip locally. The structural half needs neither and
# always runs.
#
# Run: bash scripts/ci/clickhouse-exporter-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 2

ROLE=configs/ansible/roles/archival-node
TEMPLATE="$ROLE/templates/clickhouse-prometheus.xml.j2"
TASK="$ROLE/tasks/22-clickhouse-exporter.yml"
MAIN="$ROLE/tasks/main.yml"
DEFAULTS="$ROLE/defaults/main.yml"
SCRAPE=configs/prometheus/prometheus.r1.yml
RULES=(deploy/monitoring/rules/clickhouse.yml configs/prometheus/rules.r1/clickhouse.yml)

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

for f in "$TEMPLATE" "$TASK" "$MAIN" "$DEFAULTS" "$SCRAPE" "${RULES[@]}"; do
  [ -r "$f" ] || { echo "clickhouse-exporter-test: missing $f" >&2; exit 2; }
done

# ─── structural: the task installs the template where ClickHouse reads ──
echo "clickhouse-exporter-test: role wiring"

if grep -q 'src: clickhouse-prometheus.xml.j2' "$TASK"; then
  ok "task renders the template"
else
  bad "task does not render clickhouse-prometheus.xml.j2"
fi

# config.d/ is the only directory clickhouse-server merges drop-ins from.
if grep -q 'dest: /etc/clickhouse-server/config.d/si-prometheus.xml' "$TASK"; then
  ok "installs into /etc/clickhouse-server/config.d/"
else
  bad "drop-in destination is not under /etc/clickhouse-server/config.d/"
fi

# r1 sets run_clickhouse=false on purpose (its lake is hand-tended and
# 08-clickhouse.yml must never rebuild it) — and r1 is the host with no
# metrics. Gating this import on run_clickhouse would put the fix out of
# reach of the only host that needs it.
# The import block: from the line naming the task file to the next
# top-level `- name:` (or EOF). Landed in a variable first — no pipe into
# an early-exit consumer under pipefail (lint-shell-sigpipe.sh).
import_block=$(awk '/22-clickhouse-exporter.yml/ {f=1} f && /^- name:/ && !/22-clickhouse-exporter/ {f=0} f' "$MAIN")
case "$import_block" in
  *"when:"*"run_clickhouse"*)
    bad "main.yml gates the exporter import on run_clickhouse (false on r1 — the host with the gap)" ;;
  *)
    ok "exporter import is unconditional (reaches r1, where run_clickhouse is false)" ;;
esac

if grep -q 'clickhouse-exporter' "$MAIN"; then
  ok "import carries the clickhouse-exporter tag (tag-limited apply)"
else
  bad "no clickhouse-exporter tag — the operator cannot apply this alone"
fi

# ─── the render ─────────────────────────────────────────────────────────
PY=""
for cand in python3 /opt/homebrew/bin/python3; do
  if command -v "$cand" >/dev/null 2>&1 &&
     "$cand" -c 'import jinja2, yaml' >/dev/null 2>&1; then
    PY="$cand"; break
  fi
done
if [ -z "$PY" ] && command -v ansible-playbook >/dev/null 2>&1; then
  cand=$(head -1 "$(command -v ansible-playbook)" | sed 's|^#!||' | awk '{print $1}')
  if [ -x "$cand" ] && "$cand" -c 'import jinja2, yaml' >/dev/null 2>&1; then PY="$cand"; fi
fi

if [ -z "$PY" ]; then
  if [ "${CI:-}" = "true" ]; then
    echo "clickhouse-exporter-test: FAIL — no python3 with jinja2+PyYAML (required in CI)" >&2
    exit 1
  fi
  echo "  SKIP render checks (no python3 with jinja2+PyYAML locally; CI enforces)"
else
  echo "clickhouse-exporter-test: template render"
  "$PY" - "$TEMPLATE" "$DEFAULTS" <<'PY_RENDER_EOF'
import sys
import xml.etree.ElementTree as ET

import jinja2
import yaml

template_path, defaults_path = sys.argv[1], sys.argv[2]

with open(template_path, encoding="utf-8") as fh:
    src = fh.read()
with open(defaults_path, encoding="utf-8") as fh:
    defaults = yaml.safe_load(fh) or {}

# StrictUndefined: the template must need nothing the role does not
# already declare. A `| default()` chain that silently swallowed a typo'd
# variable name would otherwise render a plausible file with the wrong
# port on every host.
env = jinja2.Environment(
    trim_blocks=True, lstrip_blocks=False, keep_trailing_newline=True,
    undefined=jinja2.StrictUndefined,
)
tmpl = env.from_string(src)

fails = []


def check(cond, label):
    print(("  ok   " if cond else "  FAIL ") + label)
    if not cond:
        fails.append(label)


def render(**overrides):
    ctx = dict(defaults)
    ctx.update(overrides)
    return tmpl.render(**ctx)


try:
    out = render()
except jinja2.UndefinedError as exc:
    print("  FAIL renders against the role defaults alone: %s" % exc)
    sys.exit(1)
check(True, "renders against the role defaults alone")

try:
    root = ET.fromstring(out)
except ET.ParseError as exc:
    print("  FAIL rendered drop-in is well-formed XML: %s" % exc)
    sys.exit(1)
check(True, "rendered drop-in is well-formed XML")

# THE assertion. ElementTree drops comments, so a `<prometheus>` block
# inside `<!-- ... -->` — r1's measured state, config.xml lines 1151-1158
# — finds nothing here. Rendering "correct-looking" text is not the bar;
# being a live element is.
node = root.find("prometheus")
check(node is not None, "the <prometheus> block is a live element, not commented out")
if node is None:
    sys.exit(1)


def text(child):
    el = node.find(child)
    return None if el is None else (el.text or "").strip()


check(text("port") == str(defaults["clickhouse_prometheus_port"]),
      "port is the role default (%s)" % defaults["clickhouse_prometheus_port"])
check(text("endpoint") == defaults["clickhouse_prometheus_endpoint"],
      "endpoint is the role default (%s)" % defaults["clickhouse_prometheus_endpoint"])
# All three collectors on: the alerts read system.events
# (ClickHouseProfileEvents_*), and metrics/asynchronous_metrics are what
# a responder triages with once paged.
for child in ("metrics", "events", "asynchronous_metrics"):
    check(text(child) == "true", "%s collector is enabled" % child)

# The values are variables, not decoration: an inventory override has to
# reach the rendered file, or a per-host port is a lie in the docs.
over = ET.fromstring(render(clickhouse_prometheus_port=19363,
                            clickhouse_prometheus_endpoint="/ch-metrics"))
onode = over.find("prometheus")
check(onode is not None and (onode.findtext("port") or "").strip() == "19363",
      "an inventory port override reaches the rendered file")
check(onode is not None and (onode.findtext("endpoint") or "").strip() == "/ch-metrics",
      "an inventory endpoint override reaches the rendered file")

sys.exit(1 if fails else 0)
PY_RENDER_EOF
  rc=$?
  if [ "$rc" -eq 0 ]; then
    pass=$((pass + 1))
  else
    fail=$((fail + 1))
  fi
fi

# ─── the seam: port + job name agree across four files ──────────────────
echo "clickhouse-exporter-test: scrape + rule seam"

PORT=$(awk '/^clickhouse_prometheus_port:/ {print $2}' "$DEFAULTS")
if [ -n "$PORT" ]; then
  ok "role declares clickhouse_prometheus_port ($PORT)"
else
  bad "no clickhouse_prometheus_port in $DEFAULTS"
  PORT="__unset__"
fi

# A scrape job on a port nothing serves is the same dead layer as an alert
# on a metric nothing emits (F-1329). The port must sit INSIDE the
# `clickhouse` job's block — from its `job_name:` line to the next
# `job_name:` — not merely somewhere in the file, or a clickhouse job on
# the wrong port beside any other job on 9363 would pass. Exact job name:
# `clickhouse_something` is not this job.
job_block=$(awk '/^[[:space:]]*-[[:space:]]*job_name:[[:space:]]*clickhouse[[:space:]]*$/ {f=1; print; next} /^[[:space:]]*-[[:space:]]*job_name:/ {f=0} f' "$SCRAPE")
case "$job_block" in
  *"localhost:$PORT"*)
    ok "prometheus.r1.yml scrapes job 'clickhouse' on localhost:$PORT (inside the job block)" ;;
  *)
    bad "prometheus.r1.yml has no 'clickhouse' job on localhost:$PORT" ;;
esac

for r in "${RULES[@]}"; do
  if grep -q 'job="clickhouse"' "$r"; then
    ok "$r selects job=\"clickhouse\""
  else
    bad "$r does not select job=\"clickhouse\" — the rules read a job nobody scrapes"
  fi
done

printf 'clickhouse-exporter-test: %d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
