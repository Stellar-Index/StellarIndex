# Postman / Insomnia / Bruno collection

`stellar-index.postman_collection.json` is a Postman v2.1 collection
auto-generated from
[`openapi/stellar-index.v1.yaml`](../../openapi/stellar-index.v1.yaml).
The OpenAPI spec is the source of truth — regenerate after any
spec change with:

```sh
make docs-postman
```

This is the customer-facing canonical path. The docs-site build
pipeline (docs.stellarindex.io) regenerates its own copy at
build time; nothing else in the repo writes to this file.

## Importing

| Tool | How |
|------|-----|
| Postman | File → Import → drop the JSON file |
| Insomnia | Application menu → Import/Export → Import Data → From File |
| Bruno | File → Open Collection → select the JSON file |

## Setting variables

The collection ships with two collection-level variables:

- `baseUrl` — defaults to `https://api.stellarindex.io/v1` (note:
  the `/v1` prefix is part of the base URL, not the request
  paths). To hit a local indexer, override it to
  `http://localhost:3000/v1`.
- `bearerToken` — your API key (`sip_…`), empty by default. The
  collection carries bearer auth at the collection level, so every
  request sends `Authorization: Bearer {{bearerToken}}` once the
  variable is set. Public read endpoints work without it (at the
  lower anonymous rate limit); `/v1/account/*` requires it. Get a
  key from the dashboard at
  <https://stellarindex.io/dashboard/keys>, or by running the
  collection's `POST /v1/signup` request.

In Postman: Collections → Stellar Index API → Variables tab.
