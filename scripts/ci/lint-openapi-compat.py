#!/usr/bin/env python3
"""Fails when a change to openapi/stellar-index.v1.yaml breaks an existing integrator.

The docs, Postman and TS-type generators check that the spec and its artifacts agree.
None of them compares the spec with the one already published, so a removed path or
enum value regenerated cleanly and shipped. This compares base against head and reports:

  removed-operation   a path or method that existed is gone
  removed-schema      a named component schema is gone
  removed-property    a property an existing schema declared is gone
  removed-enum-value  an enum lost a value
  new-required-input  an existing operation gained a required parameter or request field

A deliberate break is declared in a commit message in the range:

    OpenAPI-Break: <reason>

Usage:
  BASE_SHA=<sha> lint-openapi-compat.py            # git mode: base revision vs working tree
  lint-openapi-compat.py --base-file A --head-file B [--messages-file M]   # file mode (self-test)
"""
import argparse
import os
import subprocess
import sys

import yaml

SPEC = "openapi/stellar-index.v1.yaml"
METHODS = ("get", "put", "post", "delete", "patch", "head", "options")
ZERO_SHA = "0" * 40


def walk(node, path=()):
    """Yields (path, dict) for every mapping. A parameter list is keyed by name and location, so
    reordering it is not a change; other lists are keyed by index."""
    if isinstance(node, dict):
        yield path, node
        for key, child in node.items():
            yield from walk(child, path + (str(key),))
    elif isinstance(node, list):
        for i, child in enumerate(node):
            key = str(i)
            if isinstance(child, dict) and "name" in child and "in" in child:
                key = f"{child['name']}:{child['in']}"
            yield from walk(child, path + (key,))


def at(doc, path):
    node = doc
    for key in path:
        if isinstance(node, dict):
            if key not in node:
                return None
            node = node[key]
        elif isinstance(node, list):
            found = None
            for i, child in enumerate(node):
                name = str(i)
                if isinstance(child, dict) and "name" in child and "in" in child:
                    name = f"{child['name']}:{child['in']}"
                if name == key:
                    found = child
                    break
            if found is None:
                return None
            node = found
        else:
            return None
    return node


def show(path):
    return "/".join(path)


def resolve(doc, node):
    """Follows a local $ref chain to the node it names."""
    seen = set()
    while isinstance(node, dict) and "$ref" in node:
        ref = node["$ref"]
        if ref in seen or not ref.startswith("#/"):
            return {}
        seen.add(ref)
        node = at(doc, tuple(ref[2:].split("/")))
    return node if isinstance(node, dict) else {}


def operations(doc):
    for path, item in (doc.get("paths") or {}).items():
        if not isinstance(item, dict):
            continue
        for method in METHODS:
            if isinstance(item.get(method), dict):
                yield (path, method), item, item[method]


def required_params(doc, item, op):
    out = set()
    for raw in (item.get("parameters") or []) + (op.get("parameters") or []):
        p = resolve(doc, raw)
        if p.get("required"):
            out.add(f"{p.get('name')} (in {p.get('in')})")
    return out


def request_schemas(doc, op):
    """Maps a stable name to every schema reachable from the operation's request body."""
    out = {}
    body = resolve(doc, op.get("requestBody") or {})
    stack = [
        (f"body[{ctype}]", (media or {}).get("schema") or {})
        for ctype, media in (body.get("content") or {}).items()
    ]
    while stack:
        name, schema = stack.pop()
        if isinstance(schema, dict) and "$ref" in schema:
            name = schema["$ref"]
        schema = resolve(doc, schema)
        if not schema or name in out:
            continue
        out[name] = schema
        for prop, child in (schema.get("properties") or {}).items():
            stack.append((f"{name}.{prop}", child))
        if isinstance(schema.get("items"), dict):
            stack.append((f"{name}[]", schema["items"]))
        for key in ("allOf", "oneOf", "anyOf"):
            for i, child in enumerate(schema.get(key) or []):
                stack.append((f"{name}.{key}[{i}]", child))
    return out


def compare(base, head):
    problems = []

    base_ops = {key: (item, op) for key, item, op in operations(base)}
    head_ops = {key: (item, op) for key, item, op in operations(head)}
    for (path, method), (b_item, b_op) in sorted(base_ops.items()):
        if (path, method) not in head_ops:
            problems.append(f"removed-operation: {method.upper()} {path}")
            continue
        h_item, h_op = head_ops[(path, method)]
        label = f"{method.upper()} {path}"
        for name in sorted(required_params(head, h_item, h_op) - required_params(base, b_item, b_op)):
            problems.append(f"new-required-input: {label} now requires parameter {name}")
        b_body = resolve(base, b_op.get("requestBody") or {})
        h_body = resolve(head, h_op.get("requestBody") or {})
        if h_body.get("required") and not b_body.get("required"):
            problems.append(f"new-required-input: {label} now requires a request body")
        b_schemas = request_schemas(base, b_op)
        for name, h_schema in sorted(request_schemas(head, h_op).items()):
            if name not in b_schemas:
                continue
            added = set(h_schema.get("required") or []) - set(b_schemas[name].get("required") or [])
            for field in sorted(added):
                problems.append(f"new-required-input: {label} request {name} now requires `{field}`")

    base_schemas = (base.get("components") or {}).get("schemas") or {}
    head_schemas = (head.get("components") or {}).get("schemas") or {}
    for name in sorted(set(base_schemas) - set(head_schemas)):
        problems.append(f"removed-schema: components/schemas/{name}")

    for path, b_node in walk(base):
        h_node = at(head, path)
        if not isinstance(h_node, dict):
            continue
        if isinstance(b_node.get("properties"), dict) and isinstance(h_node.get("properties"), dict):
            for prop in sorted(set(b_node["properties"]) - set(h_node["properties"])):
                problems.append(f"removed-property: {show(path)} lost `{prop}`")
        if isinstance(b_node.get("enum"), list) and isinstance(h_node.get("enum"), list):
            gone = [v for v in b_node["enum"] if v not in h_node["enum"]]
            for value in gone:
                problems.append(f"removed-enum-value: {show(path)} lost `{value}`")
    return problems


def git(*args):
    return subprocess.run(["git", *args], capture_output=True, text=True, check=False)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base-file")
    ap.add_argument("--head-file")
    ap.add_argument("--messages-file")
    args = ap.parse_args()

    log = ""
    if args.base_file or args.head_file:
        if not (args.base_file and args.head_file):
            ap.error("--base-file and --head-file go together")
        with open(args.base_file, encoding="utf-8") as f:
            base = yaml.safe_load(f)
        with open(args.head_file, encoding="utf-8") as f:
            head = yaml.safe_load(f)
        if args.messages_file:
            with open(args.messages_file, encoding="utf-8") as f:
                log = f.read()
    else:
        os.chdir(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))
        base_sha = os.environ.get("BASE_SHA", "")
        if not base_sha or base_sha == ZERO_SHA:
            print("lint-openapi-compat: no BASE_SHA — skipping (nothing to compare against).")
            return 0
        if git("cat-file", "-e", f"{base_sha}^{{commit}}").returncode != 0:
            print(f"lint-openapi-compat: BASE_SHA {base_sha} not in local history — skipping.")
            return 0
        shown = git("show", f"{base_sha}:{SPEC}")
        if shown.returncode != 0:
            print(f"lint-openapi-compat: {SPEC} does not exist at {base_sha[:12]} — nothing to compare.")
            return 0
        base = yaml.safe_load(shown.stdout)
        with open(SPEC, encoding="utf-8") as f:
            head = yaml.safe_load(f)
        log = git("log", "--format=%B", f"{base_sha}..HEAD").stdout

    # A trailer with no reason declares nothing.
    declared = any(
        line.lower().startswith("openapi-break:") and line.split(":", 1)[1].strip()
        for line in log.splitlines()
    )
    problems = compare(base or {}, head or {})
    if not problems:
        print("lint-openapi-compat: OK — no published path, schema, property or enum value removed, no new required input")
        return 0
    for p in problems:
        print(f"  {p}")
    if declared:
        print(f"lint-openapi-compat: OK — {len(problems)} breaking change(s), declared by an OpenAPI-Break trailer")
        return 0
    print(
        f"lint-openapi-compat: FAIL — {len(problems)} breaking change(s) to {SPEC}.\n"
        "  Keep the old shape (add, do not remove), or declare the break in a commit message:\n"
        "      OpenAPI-Break: <reason>",
        file=sys.stderr,
    )
    return 1


if __name__ == "__main__":
    sys.exit(main())
