#!/usr/bin/env python3
"""Emit the set of unit files the role genuinely installs FROM deploy/systemd.

The shell version asked two INDEPENDENT questions — "does `- <unit>`
appear as a list item in any task file" and "does the string
`deploy/systemd/{{ item }}` appear in any task file". The second is a
repo-wide constant (one unrelated task satisfies it for everything), so
classification collapsed to the first, which matches any YAML list item
— including an `ansible.builtin.systemd` ENABLE loop that installs
nothing. A unit enabled but never copied therefore passed as
AUTHORITATIVE: the exact footgun this lint exists to name.

Here both questions are asked of the SAME task: the task must copy or
template a src under deploy/systemd, and the unit must be in THAT task's
own loop.
"""
import sys, glob, yaml

def loop_items(task):
    for key in ("loop", "with_items"):
        v = task.get(key)
        if isinstance(v, list):
            for it in v:
                if isinstance(it, str):
                    yield it.strip()

def src_of(task):
    for mod in ("ansible.builtin.copy", "copy",
                "ansible.builtin.template", "template"):
        spec = task.get(mod)
        if isinstance(spec, dict):
            return str(spec.get("src", ""))
    return ""

installed = set()
for path in sorted(glob.glob(sys.argv[1] + "/tasks/*.yml")):
    try:
        doc = yaml.safe_load(open(path)) or []
    except yaml.YAMLError:
        continue
    if not isinstance(doc, list):
        continue
    for task in doc:
        if not isinstance(task, dict):
            continue
        if "deploy/systemd" not in src_of(task):
            continue
        for item in loop_items(task):
            installed.add(item)

for u in sorted(installed):
    print(u)
