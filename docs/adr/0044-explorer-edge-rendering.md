---
adr: 0044
title: Explorer rendering moves from static export to edge SSR
status: Accepted
date: 2026-07-03
supersedes: []
---

# ADR-0044: Explorer rendering moves from static export to edge SSR

Deciders: @ash (accepted 2026-07-04; Workers paid pricing approved in
principle — traffic-modeled cost numbers to be reviewed before the
stage-3 cutover). Supersedes the `output: 'export'` decision embedded
in the explorer since Phase 8 (never ADR'd; this made the choice
explicit before replacing it).

## Context

The explorer is a Next.js static export deployed to Cloudflare Pages:
every page is rendered once at build time, and dynamic entities are
covered by a mix of pre-rendered top-N pages (`generateStaticParams`)
and CF Pages Function shells that fetch client-side.

The 2026-07-03 site audit found this architecture at fault for three
independent production failures — not one-off bugs, but consequences
of rendering at build time:

1. **The 20,000-file ceiling.** Cloudflare Pages hard-caps deployments
   at 20k files. The Next 15→16 upgrade pushed our export from ~9k to
   36,106 files (8 segment-prefetch files per page) and **every deploy
   silently failed for nine days** while the site served a stale build.
   A prune restored deploys (S-024), but the ceiling is structural: we
   index ~191K assets and growing; any ambition to give entities real
   pages collides with a fixed file budget.
2. **Bake-time poisoning.** The issuer pages served "Issuer not found"
   for MONTHS-old top-100 issuers (S-003, the Circle bug) because the
   build-time fetch timed out and the error branch was rendered INTO
   the static HTML. A build-time hiccup becomes a permanent lie. The
   fail-hard buildFetch contract (2026-07-02) stops new poison but by
   design turns API blips into failed deploys — trading one failure
   mode for another. Rendering at request time is the only shape where
   neither failure mode exists.
3. **Staleness between deploys.** Entity data baked at build time
   (prices in titles, market caps, protocol stats) is only as fresh as
   the last deploy. When deploys froze, the drift was site-wide and
   invisible.

SEO is the fourth driver: today only the pre-rendered top-N get
crawlable content; the long tail (every other issuer, market pair,
contract, account) serves either a hard 404 or a noindex client-shell.
The audit's crawl found the site linking to its own non-prerendered
entities (sources → BTC/EUR market pages, assets → non-top-100
issuers) as hard 404s.

## Decision

Migrate the explorer from `output: 'export'` to **server-side
rendering at the edge on Cloudflare Workers via OpenNext**
(`@opennextjs/cloudflare`), with per-route-family cache policy:

- **Entity pages** (assets, markets, issuers, contracts, tx, ledgers,
  accounts): rendered on demand against the live API, edge-cached with
  short TTLs (30–300s by family, matching the API's own closed-bucket
  cadence). Every entity gets a real, crawlable, current page — no
  top-N cutoffs, no fallback-shell split, no baked errors.
- **Curated/static pages** (home, hubs, research, blog, methodology):
  full-route cache with long TTLs, revalidated on deploy — economically
  identical to today's static files.
- **OG images and JSON-LD** render server-side with live data (the CF
  Pages Functions for OG already do this; they fold into the Worker).
- **Indexing policy decouples from rendering**: what's in the sitemap
  and what's `noindex` becomes a pure SEO decision (value-tiered, as
  today), no longer welded to what could be pre-rendered.

## Consequences

Positive:
- No file limit; page-count scales with the index, not the deploy.
- The bake-poisoning class (S-003) and deploy-freeze staleness class
  (S-024) become structurally impossible; an API outage degrades to
  stale-while-revalidate edge cache or per-request error pages that
  heal on the next request.
- One render path (the SSG top-N / client-shell long-tail split and
  its `functions/*/[[path]].js` shells are deleted).
- Strictly better SEO ceiling: crawlable, current content for any
  entity we choose to index.

Negative / costs:
- Workers runtime cost per uncached request (mitigated: edge cache +
  the API's own CDN; the curated core stays effectively static).
- A server runtime to operate (cold starts, Workers limits, OpenNext
  adapter maintenance) where today there is only a file upload.
- Build-time fail-hard guarantees are replaced by runtime observability
  — needs request-level error alerting on the Worker (wired into the
  existing Prometheus/Alertmanager stack via CF analytics or logpush).

## Migration plan (staged, each stage shippable)

1. **Spike**: OpenNext build of the current app; verify routes, RSC,
   `next/font`, the redirects file, and Worker size limits. Exit
   criterion: the full route inventory renders under `wrangler dev`.
2. **Shadow deploy**: Worker on a preview hostname; the audit's crawl
   suite (route inventory × entity sampling) runs against it and must
   beat the static site's pass rate.
3. **Cutover**: apex routes to the Worker; CF Pages project retained
   as instant rollback for one release cycle.
4. **Cleanup**: delete `generateStaticParams` top-N machinery where it
   only existed to dodge the file cap, the CF Pages Function shells,
   the file-budget guard and segment-prune workflow steps; re-tier the
   sitemap by SEO value.

Until stage 3 lands, the static-export path stays fully supported
(the S-024 prune keeps deploys under the cap).

## Alternatives considered

- **Keep static export, shrink prerender sets**: keeps every failure
  class, just further from the cliff; the audit showed the cliff is
  silent when hit.
- **Hybrid (static core + Worker for entities)**: two render paths and
  two failure surfaces forever; the curated core under SSR with long
  TTLs is already cost-equivalent to static.
- **Self-host Next on r1 behind the CDN**: couples explorer uptime to
  a single origin box and adds an ops surface; the edge runtime is
  the better fit for a read-only rendering tier (ADR-0002 keeps
  self-hosting for data, not for stateless render).
