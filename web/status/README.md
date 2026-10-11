# web/status — standalone status page

`status.stellarindex.io` (Cloudflare Pages project `stellarindex-status`).
It renders the incident corpus in `internal/incidents/data/` at build time
and makes no API call, so it builds and publishes while the API and the
explorer are down. That is its job: the outage channel.

The loader is the explorer's `web/explorer/src/lib/incidents.ts`, imported
across the package (it uses only `node:` builtins). `next.config.mjs` sets the
repo root as the build root so that import resolves.

Live component health stays on the explorer at `stellarindex.io/status`.
`public/_redirects` sends `/incident/<slug>` deep links there.

Deploy: `gh workflow run status-page.yml --ref main`. Procedure:
`docs/operations/runbooks/sev-status-page-update.md`.
