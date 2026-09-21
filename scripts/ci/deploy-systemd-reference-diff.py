#!/usr/bin/env python3
"""Diff the systemd DIRECTIVES two unit files share (RLT-434).

lint-deploy-systemd-authority.sh classifies a file as REFERENCE the moment
a same-named .j2 exists in the role's templates/systemd/ — it never looks
at what either file actually says. Two units that agree on nothing but
their filename pass silently, which is how RestartSec, User and
VERIFY_ARCHIVE_MAX_RUNTIME drifted between deploy/systemd/*.service
(documentation) and the .j2 that is actually deployed.

This does not diff the files textually — headers, comments and Jinja
templating make that noisy by construction. It parses `Key=Value`
directives out of each (Environment=NAME=VALUE keyed by NAME, so unrelated
env vars don't collide) and reports every key that is present, literal
(no `{{ }}`/`{% %}`), and non-templated on BOTH sides but disagrees. A key
present on only one side is not reported: adding a directive to the
deployed template without back-porting it to the reference copy is a
one-sided drift the header already tells operators to expect, not the
silent same-key contradiction this checks for.
"""
import sys


def directives(path):
    out = {}
    section = None
    with open(path) as fh:
        for raw in fh:
            line = raw.strip()
            if not line or line.startswith("#") or line.startswith(";"):
                continue
            if line.startswith("[") and line.endswith("]"):
                section = line
                continue
            if "=" not in line:
                continue
            key, _, val = line.partition("=")
            key, val = key.strip(), val.strip()
            if "{{" in val or "{%" in val:
                continue
            if key == "Environment" and "=" in val:
                name, _, v = val.partition("=")
                out[(section, "Environment:" + name.strip())] = v.strip()
            else:
                out[(section, key)] = val
    return out


def diverged(ref_path, j2_path):
    ref, j2 = directives(ref_path), directives(j2_path)
    for key in sorted(set(ref) & set(j2)):
        if ref[key] != j2[key]:
            _, name = key
            yield f'{name} = "{ref[key]}" in {ref_path} vs "{j2[key]}" in {j2_path}'


if __name__ == "__main__":
    if len(sys.argv) != 3:
        print("usage: deploy-systemd-reference-diff.py <reference.service> <template.j2>", file=sys.stderr)
        sys.exit(2)
    found = list(diverged(sys.argv[1], sys.argv[2]))
    for line in found:
        print(line)
    sys.exit(1 if found else 0)
