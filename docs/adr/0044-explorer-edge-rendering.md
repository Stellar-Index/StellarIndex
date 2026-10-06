---
adr: 0044
title: Explorer rendering moves from static export to edge SSR
status: Accepted
date: 2026-07-03
supersedes: []
superseded_by: null
---

# ADR-0044: Explorer rendering moves from static export to edge SSR

## Context

The explorer is a Next.js static export on Cloudflare Pages, rendered once at build time. That runs into Cloudflare Pages' 20,000-file deploy ceiling (an upgrade once pushed the export past it and deploys failed while the site served a stale build), bakes build-time API errors into permanent pages, and leaves every entity outside the pre-rendered top N as a 404 or a noindex shell.

## Decision

Render the explorer at the edge on Cloudflare Workers via OpenNext (`@opennextjs/cloudflare`) with a cache policy per route family.

- Entity pages (assets, markets, issuers, contracts, tx, ledgers, accounts) render on demand against the live API and are edge-cached for 30 to 300 seconds, matching the API's closed-bucket cadence.
- Curated pages (home, hubs, research, blog, methodology) use a full-route cache with long TTLs, revalidated on deploy.
- OG images and JSON-LD render server-side with live data.
- Sitemap and `noindex` policy is an SEO decision, independent of what can be pre-rendered.

Migration is staged: spike, shadow deploy, cutover with the Pages project kept for one release as rollback, then deleting `generateStaticParams` top-N code, the Pages Function shells and the file-budget guard. Until cutover, static export stays the production path (`OPEN_NEXT=1` selects SSR in `web/explorer/next.config.mjs`).

## Invariant

- Production keeps the static-export path until the cutover stage lands: `output: 'export'` is the default in `web/explorer/next.config.mjs`, and SSR builds are opt-in via `OPEN_NEXT=1`.
- The SSR preview is checked by the site crawl battery before any cutover: `.github/workflows/cf-ssr-shadow.yml` runs `scripts/ci/site-crawl-check.sh`.

## Consequences

- No file ceiling, no baked-error pages, and one render path once cleanup finishes.
- A Workers runtime to operate, with request-level error alerting replacing build-time fail-hard.
- Stage 1 (spike) is done and Stage 2 (shadow deploy) is gated on a Workers-scoped Cloudflare token and traffic-modeled Workers cost numbers must be reviewed before cutover, so this is a plan in progress, not deployed state.
- Stage 1 findings Stage 2 must close: `dynamicParams = false` entity routes 404 under the Worker until flipped and the Pages Function shells deleted; `public/_headers` (CSP) does not cover Worker-rendered HTML; `buildFetch` fails hard, which is a bare 500 at request time; `staticAssetsIncrementalCache` is read-only, so real ISR needs an R2/KV cache, and Workers assets cap at 20,000 files; Worker SSR misses currently emit `Cache-Control: s-maxage=31536000` (the SSG default), which per-route-family `revalidate`/TTL work must fix before anything sits behind the real edge cache.
- Remaining Stage 2 items: port the OG Function (`workers-og`, satori + resvg-wasm) into a route handler and measure wasm bundle size; collapse the two wrangler configs (`wrangler.ssr.jsonc` beside the Pages `wrangler.toml`) into one at cutover; wire logpush/analytics into request-level error alerting.
- Rejected: shrinking the prerender sets (keeps every failure class), a static-plus-Worker hybrid (two render paths forever), self-hosting Next on r1 (couples explorer uptime to one box).

## Evidence

`web/explorer/open-next.config.ts`, `web/explorer/wrangler.ssr.jsonc`, `.github/workflows/cf-ssr-shadow.yml`.
