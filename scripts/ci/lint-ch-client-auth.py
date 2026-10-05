#!/usr/bin/env python3
"""Installed scripts must reach ClickHouse with a credential file.

INV-0802 step 2: CH's `default` user takes no password, and the plan is to
lock it once nothing uses it. A script that ansible installs into
/usr/local/{bin,sbin} and calls `clickhouse-client` without `-C` /
`--config-file`, or curls the HTTP port 8123 without `--netrc-file`,
authenticates as `default` and breaks on the lock. Those flags are also
the only way the credential stays off argv.

Scans every copy/template task under configs/ansible whose dest is in
/usr/local/bin or /usr/local/sbin, resolves its source (role files/,
templates/, or the repo's scripts/ via {{ playbook_dir }}) or its inline
content, and reports each call line without a credential flag. A flag
held in a variable (`CH_AUTH=(-C "$F")`, then `"${CH_AUTH[@]}"`) counts.

REPORT-ONLY for now: exits 0 with findings, and 2 when it scanned nothing
(a vacuous pass is the failure this guards against). The lock PR makes
findings fatal. Run from the repo root.
"""
import glob
import os
import re
import sys

try:
    import yaml
except ImportError:
    print("lint-ch-client-auth: FAIL — PyYAML not available; refusing to "
          "pass vacuously", file=sys.stderr)
    sys.exit(2)

ANSIBLE_DIR = "configs/ansible"
PLAYBOOK_DIR = os.path.join(ANSIBLE_DIR, "playbooks")
INSTALL_DIRS = ("/usr/local/bin/", "/usr/local/sbin/")
COPY_MODULES = {"copy", "ansible.builtin.copy", "template", "ansible.builtin.template"}

ASSIGN = re.compile(r"^\s*(?:local\s+|export\s+|readonly\s+|declare\s+\S+\s+)?([A-Za-z_]\w*)=(.*)$")
# curl's own -C is --continue-at, so each client has its own flag set.
NATIVE_AUTH = re.compile(r"(?:^|[\s(\"'])(?:-C|--config-file)(?:[\s=\"')]|$)")
HTTP_AUTH = re.compile(r"(?:^|[\s(\"'])--netrc-file(?:[\s=\"')]|$)")
NATIVE = re.compile(r"(?:^|[\s;&|({`]|\$\()clickhouse-client\b")
CURL = re.compile(r"(?:^|[\s;&|({`]|\$\()curl\b")
NOT_A_CALL = re.compile(r"\b(?:command\s+-v|which|type|hash)\s+clickhouse-client\b")
MESSAGE_WORDS = {"echo", "printf", "note", "log", "warn", "die", "fail", "err", "msg", "info", "say", "#"}


def tasks_in(node):
    if isinstance(node, list):
        for item in node:
            yield from tasks_in(item)
    elif isinstance(node, dict):
        yield node
        for key in ("block", "rescue", "always", "tasks", "pre_tasks", "post_tasks", "handlers"):
            if key in node:
                yield from tasks_in(node[key])


def role_dir_of(task_file):
    parts = task_file.split(os.sep)
    if "roles" in parts:
        i = parts.index("roles")
        return os.sep.join(parts[: i + 2])
    return None


def resolve_src(task_file, module, src):
    src = src.strip()
    if "{{" in src:
        if "playbook_dir" not in src:
            return None
        src = re.sub(r"\{\{\s*playbook_dir\s*\}\}", PLAYBOOK_DIR, src)
        if "{{" in src:
            return None
        return os.path.normpath(src)
    if os.path.isabs(src):
        return None  # a binary or a path on the target host
    role = role_dir_of(task_file)
    sub = "templates" if "template" in module else "files"
    for base in ([os.path.join(role, sub)] if role else []) + [os.path.dirname(task_file)]:
        cand = os.path.normpath(os.path.join(base, src))
        if os.path.isfile(cand):
            return cand
    return None


def installed_scripts():
    """Yield (label, text) for every script installed into /usr/local/{bin,sbin}."""
    files = sorted(set(
        glob.glob(os.path.join(ANSIBLE_DIR, "roles", "*", "tasks", "*.yml"))
        + glob.glob(os.path.join(ANSIBLE_DIR, "roles", "*", "handlers", "*.yml"))
        + glob.glob(os.path.join(ANSIBLE_DIR, "tasks", "*.yml"))
        + glob.glob(os.path.join(PLAYBOOK_DIR, "*.yml"))
    ))
    unresolved = []
    for tf in files:
        with open(tf, encoding="utf-8") as fh:
            try:
                doc = yaml.safe_load(fh)
            except yaml.YAMLError:
                continue
        for task in tasks_in(doc):
            for module in COPY_MODULES & set(task):
                args = task[module]
                if not isinstance(args, dict):
                    continue
                dest = str(args.get("dest", ""))
                if not dest.startswith(INSTALL_DIRS):
                    continue
                label = f"{tf} -> {dest}"
                if "content" in args:
                    yield label, str(args["content"])
                elif "src" in args:
                    path = resolve_src(tf, module, str(args["src"]))
                    if path is None:
                        if not str(args["src"]).startswith(("/", "{{")):
                            unresolved.append(label)
                        continue
                    with open(path, encoding="utf-8", errors="replace") as fh:
                        text = fh.read()
                    if text.startswith("\x7fELF"):
                        continue
                    yield f"{path} -> {dest}", text
    for label in unresolved:
        print(f"lint-ch-client-auth: note: could not resolve source for {label}", file=sys.stderr)


def logical_lines(text):
    buf, start = "", 0
    for n, raw in enumerate(text.splitlines(), 1):
        if not buf:
            start = n
        stripped = raw.rstrip()
        if stripped.endswith("\\"):
            buf += stripped[:-1] + " "
            continue
        yield start, buf + stripped
        buf = ""
    if buf:
        yield start, buf


def findings_in(text):
    lines = list(logical_lines(text))
    http_vars, native_auth_vars, http_auth_vars = set(), set(), set()
    for _, line in lines:
        m = ASSIGN.match(line)
        if m:
            name, value = m.groups()
            if ":8123" in value:
                http_vars.add(name)
            if NATIVE_AUTH.search(value):
                native_auth_vars.add(name)
            if HTTP_AUTH.search(value):
                http_auth_vars.add(name)

    def refs(line, names):
        return any(re.search(r"\$\{?" + re.escape(v) + r"\b", line) for v in names)

    out = []
    for n, line in lines:
        body = line.strip()
        if not body or body.startswith("#"):
            continue
        first = body.split()[0]
        if first in MESSAGE_WORDS:
            continue
        native_authed = bool(NATIVE_AUTH.search(body)) or refs(body, native_auth_vars)
        http_authed = bool(HTTP_AUTH.search(body)) or refs(body, http_auth_vars)
        if NATIVE.search(body) and not NOT_A_CALL.search(body) and not native_authed:
            out.append((n, "native", body))
        elif CURL.search(body) and (":8123" in body or refs(body, http_vars)) and not http_authed:
            out.append((n, "http", body))
    return out


def main():
    scanned, total = 0, 0
    for label, text in installed_scripts():
        scanned += 1
        for n, kind, body in findings_in(text):
            total += 1
            print(f"{label.split(' -> ')[0]}:{n}: {kind}: {body[:160]}  [installed as {label.split(' -> ')[1]}]")
    print(f"lint-ch-client-auth: {total} call(s) without a credential file in "
          f"{scanned} installed script(s) (report-only)")
    if scanned == 0:
        print("lint-ch-client-auth: FAIL — scanned no installed scripts", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
