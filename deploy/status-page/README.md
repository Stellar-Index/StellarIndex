# Public status page — `status.stellarindex.io`

`status.stellarindex.io` (this Cloudflare Pages project,
`stellarindex-status`) is the explorer's `/status` page built on its own
project. It is hosted apart from the API and explorer so it stays up when they do
not. Live component health is at `stellarindex.io/status` — see
[`docs/operations/status-page-setup.md`](../../docs/operations/status-page-setup.md).

## What ships from the codebase

- [`web/explorer/src/app/status/`](../../web/explorer/src/app/status/)
  renders active and recent incidents from `internal/incidents/data`
  at build time; live panels fetch the API client-side.
- [`scripts/ci/status-pages-redirects.sh`](../../scripts/ci/status-pages-redirects.sh)
  rewrites `/` to `/status/` (200) and 301s `/incident/*` to `/status/incident/:splat`.

## Hosting

Cloudflare Pages project `stellarindex-status` with custom
domain `status.stellarindex.io`. Bootstrap config managed by
[`scripts/ops/cf-pages-bootstrap.sh`](../../scripts/ops/cf-pages-bootstrap.sh):
- Root: `web/explorer`
- Build command: `pnpm install --frozen-lockfile && pnpm build && bash ../../scripts/ci/status-pages-redirects.sh out`
- Output: `out`
- Env: `NODE_VERSION=20`, `PNPM_VERSION=10`, `NEXT_PUBLIC_API_BASE_URL`,
  `NEXT_PUBLIC_NETWORK=mainnet`, `EXPLORER_BUILD_OFFLINE=1`

## Why not cstate / Statuspage.io

cstate v6 broke our v5 config shape, and the v5 vendored copy
hit a chain of Hugo-template gotchas (pagination config,
date-format params, `_TEMPLATE.md` rendering as a real
incident). Maintaining a Hugo theme + a dozen cstate-specific
config knobs to render "ok/not ok + latency" wasn't a good
trade vs ~400 lines of TSX that read directly from
`/v1/status` and look polished by default.

Statuspage.io ($79+/month, ties incidents to a proprietary
platform) was rejected on the same make-vs-buy axis — the
data already exists in our Prometheus + Alertmanager pipeline.
