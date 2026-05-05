<!-- GENERATED FILE - DO NOT EDIT. Source: openapi/rates-engine.v1.yaml -->
---
title: Generated API reference
last_verified: 2026-04-28
status: generated
---

# API reference

GENERATED FILE — do not edit by hand. Source of truth:
[`openapi/rates-engine.v1.yaml`](../../../openapi/rates-engine.v1.yaml).

The rendered reference is [`index.html`](index.html). Open it
directly in a browser, or serve via GitHub Pages.

To regenerate: `make docs-api`. CI verifies the rendered output
is in sync with the spec on every PR that touches either side.

## Postman collection

`make docs-postman` produces `docs/reference/api/postman-collection.json`
— a Postman v2.1 collection generated from the same OpenAPI source
of truth. The file is gitignored because the upstream converter
samples enum/oneOf values randomly when generating example bodies,
so two consecutive runs produce different bytes (the JSON
imports correctly into Postman either way; the diff is just noise).

Operators / customers regenerate locally:

```sh
make docs-postman
# → docs/reference/api/postman-collection.json (Postman v2.1 schema)
# Import into Postman: File → Import → drop the .json
```

A future enhancement (post-launch) wires this into release.yml so
each tagged release ships a `postman-collection.json` artifact on
the GitHub Release page — bypasses the local-Node-required step
and lets customers grab a stable, versioned collection per release.
