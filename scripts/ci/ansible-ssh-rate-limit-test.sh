#!/usr/bin/env bash
# ansible-ssh-rate-limit-test.sh — every ruleset that owns /etc/nftables.conf
# must rate-limit NEW SSH connections per source address, never through one
# shared bucket.
#
# `tcp dport 22 ... ct state new limit rate 30/minute accept` is a single
# token bucket for the whole rule: one internet scanner at ~1 SYN/s drains it
# and every other new SSH connection (the operator, the deploy runner) falls
# through to `policy drop`, before sshd or fail2ban ever sees it. r1 has port
# 22 world-open, so that lever is available to anyone.
#
# Renders each writer's real Jinja source (the template or inline
# `content:` block of every role task that writes the file) and
# asserts: no port-22 accept carries a bare `limit`, and the per-source
# `update @ssh_ratelimit_v*` drop precedes the accept, against a bounded,
# expiring dynamic set.
set -euo pipefail

cd "$(dirname "$0")/../.." || exit 1

# Same interpreter discovery as prometheus-firewall-ipv6-test.sh: a plain
# python3 rarely has jinja2, but ansible-playbook ships its own.
PY=""
for cand in python3 /opt/homebrew/bin/python3; do
  if command -v "$cand" >/dev/null 2>&1 &&
    "$cand" -c 'import jinja2' >/dev/null 2>&1; then
    PY="$cand"
    break
  fi
done
if [ -z "$PY" ] && command -v ansible-playbook >/dev/null 2>&1; then
  cand=$(head -1 "$(command -v ansible-playbook)" | sed 's|^#!||' | awk '{print $1}')
  if [ -x "$cand" ] && "$cand" -c 'import jinja2' >/dev/null 2>&1; then PY="$cand"; fi
fi
if [ -z "$PY" ]; then
  if [ "${CI:-}" = "true" ]; then
    echo "ansible-ssh-rate-limit-test: FAIL — no python3 with jinja2 (required in CI)" >&2
    exit 1
  fi
  echo "ansible-ssh-rate-limit-test: SKIP (no python3 with jinja2 locally; CI enforces)"
  exit 0
fi

"$PY" - <<'PYEOF'
import glob
import re
import sys

import jinja2

ROLES = "configs/ansible/roles"

VARS = dict(
    # r1's shape: SSH world-open (inventory/r1.yml), plus a v6 CIDR so the
    # roles that mirror a v6 SSH set render their v6 rule too.
    allowed_ssh_cidrs=["0.0.0.0/0"],
    internal_cidrs=["10.0.0.0/8"],
    public_allow_ports=[
        {"proto": "tcp", "port": 22, "comment": "ssh"},
        {"proto": "tcp", "port": 443, "comment": "caddy-https"},
    ],
    internal_allow_ports=[{"proto": "tcp", "port": 5432, "comment": "postgres"}],
)
for prefix in ("prometheus", "loki", "patroni", "redis", "haproxy", "sentinel"):
    VARS[prefix + "_allowed_ssh_cidrs"] = ["0.0.0.0/0", "2001:db8:2::/64"]
    VARS[prefix + "_internal_cidrs"] = ["10.0.0.0/8", "2001:db8:1::/64"]


class Lenient(jinja2.Undefined):
    # Role-specific port vars are irrelevant to the SSH rule; render them
    # as a placeholder rather than enumerating every role's defaults.
    def __str__(self):
        return "1"

    def __iter__(self):
        return iter(())


def writer_source(path):
    src = open(path).read()
    if "dest: /etc/nftables.conf" not in src:
        return None
    m = re.search(r"content: \|\n((?:(?:      .*)?\n)+)", src)
    if m:
        return "\n".join(l[6:] if l.startswith("      ") else l for l in m.group(1).splitlines())
    t = re.search(r"src: (\S+)\s*\n\s*dest: /etc/nftables.conf", src)
    if not t:
        print(f"  FAIL — {path}: writes /etc/nftables.conf from neither `content:` nor `src:`", file=sys.stderr)
        sys.exit(1)
    role_dir = path.split("/tasks/")[0]
    return open(f"{role_dir}/templates/{t.group(1)}").read()


sources = {}
for path in sorted(glob.glob(ROLES + "/*/tasks/*.yml")):
    body = writer_source(path)
    if body is not None:
        sources[path] = body

env = jinja2.Environment(undefined=Lenient)
failed = False


def check(cond, msg):
    global failed
    print(("  ok — " if cond else "  FAIL — ") + msg, file=sys.stdout if cond else sys.stderr)
    failed = failed or not cond


# The census itself is part of the claim: a writer this glob misses is a
# writer this test silently stops covering.
check(len(sources) >= 6, f"found {len(sources)} /etc/nftables.conf writers (expected >= 6)")

for path, src in sources.items():
    out = env.from_string(src).render(**VARS)
    ssh = [l.strip() for l in out.splitlines() if re.search(r"\btcp dport 22\b", l)]
    check(bool(ssh), f"{path}: renders at least one port-22 rule")
    for fam, saddr in (("v4", "ip saddr"), ("v6", "ip6 saddr")):
        accepts = [l for l in ssh if f"{saddr} @ssh_allowed_{fam}" in l and l.split(" comment ")[0].endswith("accept")]
        if not accepts:
            continue
        for a in accepts:
            check(" limit " not in a, f"{path}: {fam} SSH accept has no shared-bucket limit: {a}")
        rl = [
            l for l in ssh
            if f"update @ssh_ratelimit_{fam} {{ {saddr} limit rate over 30/minute burst 10 packets }} drop" in l
        ]
        check(
            len(rl) == 1 and ssh.index(rl[0]) < ssh.index(accepts[0]),
            f"{path}: {fam} per-source SSH limit drop precedes the accept",
        )
        decl = re.search(r"set ssh_ratelimit_%s \{([^}]*)\}" % fam, out, re.S)
        body = decl.group(1) if decl else ""
        check(
            decl is not None
            and re.search(r"flags dynamic,timeout", body)
            and re.search(r"\btimeout \d+[smh]", body)
            and re.search(r"\bsize \d+", body),
            f"{path}: ssh_ratelimit_{fam} is a bounded, expiring dynamic set",
        )

if failed:
    sys.exit(1)
print("ansible-ssh-rate-limit-test: PASS")
PYEOF
