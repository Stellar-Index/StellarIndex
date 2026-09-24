# Public status page — `status.stellarindex.io`

`status.stellarindex.io` (this Cloudflare Pages project,
`stellarindex-status`) is now a **redirect-only stub**. The status
page itself moved to `stellarindex.io/status` (one site, unified
nav) — see [`docs/operations/status-page-setup.md`](../../docs/operations/status-page-setup.md)
for the live page.

## What ships from the codebase

- [`public/_redirects`](../../web/status/public/_redirects) 301s
  every path (`/*`) to `https://stellarindex.io/status/:splat`,
  preserving any incident deep-link.
- [`web/status/src/app/page.tsx`](../../web/status/src/app/page.tsx)
  is a static fallback (`StatusMovedPage`) for the rare case a
  visitor bypasses the edge redirect — it shows a "the status page
  has moved" message with a link, and
  [`RedirectToStatus.tsx`](../../web/status/src/app/RedirectToStatus.tsx)
  does a client-side `window.location.replace` to the same target,
  forwarding the current path/query/hash the same way the `:splat`
  rule does.

## Hosting

Cloudflare Pages project `stellarindex-status` with custom
domain `status.stellarindex.io`. Bootstrap config managed by
[`scripts/ops/cf-pages-bootstrap.sh`](../../scripts/ops/cf-pages-bootstrap.sh):
- Root: `web/status`
- Build command: `pnpm install --frozen-lockfile && pnpm build`
- Output: `out`
- Env: `NODE_VERSION=20`, `PNPM_VERSION=10`

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
