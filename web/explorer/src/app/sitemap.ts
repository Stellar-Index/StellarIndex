import type { MetadataRoute } from 'next';

import { buildFetchData, requireRows } from '@/lib/buildFetch';
import { loadADRs } from '@/lib/adr';
import { routeAvailable } from '@/lib/network-routes';
import { CURRENT_NETWORK } from '@/lib/networks';
import { loadArchitectureDocs } from '@/lib/architecture';
import { loadBlogPosts } from '@/lib/blog';
import { loadOperationsDocs } from '@/lib/operations';
import { loadIncidents } from '@/lib/incidents';
import { fiatSlugFor } from '@/lib/fiat-slugs';
import { PROTOCOLS } from './protocols/registry';
import { buildConvertParams } from '@/lib/convert-params';

// Required for `output: 'export'` — sitemap is generated at build
// time and emitted as a static file. Same applies to robots.ts.
export const dynamic = 'force-static';

// Per-network origin. This was hardcoded to stellarindex.io, so a test-net
// build emitted a sitemap listing MAINNET urls — pointing crawlers at the
// production site from the test-net origin, and describing pages this
// deployment does not serve. Same hardcode class as the /network page
// reporting "Pubnet" on testnet.
const SITE_URL = CURRENT_NETWORK.explorerUrl;

/**
 * Build a sitemap URL that matches the canonical form the explorer
 * actually serves. With `trailingSlash: true` in next.config.js,
 * Next.js 308-redirects every non-trailing-slash URL to its
 * trailing-slash form. A sitemap full of redirect-targets is bad
 * SEO — Google penalises sitemaps that send crawlers through 308
 * hops, and the canonical form is already the trailing-slash one.
 *
 * `path` is the URL path relative to SITE_URL. Empty string is the
 * home page (`/`). Helper appends a `/` only when the path doesn't
 * already end with one (so a caller passing `/foo/` stays idempotent).
 */
function siteURL(path: string): string {
  if (path === '' || path === '/') return `${SITE_URL}/`;
  return path.endsWith('/') ? `${SITE_URL}${path}` : `${SITE_URL}${path}/`;
}

/**
 * sitemap.xml — generated at build time. Static pages are
 * enumerated explicitly; dynamic /assets/[slug] entries mirror
 * generateStaticParams: live API only, no seed fallback. The
 * status page now lives on this site at /status (+ per-incident
 * /status/incident/[slug] pages) and IS enumerated below; only the
 * /docs reference lives on a separate docs.stellarindex.io subdomain.
 */
export default async function sitemap(): Promise<MetadataRoute.Sitemap> {
  const now = new Date().toISOString();

  const staticPages: MetadataRoute.Sitemap = [
    '',
    '/assets',
    '/rwa',
    '/markets',
    '/issuers',
    // Chain-explorer hubs (ADR-0038). Each is indexable,
    // canonical-tagged and content-rich, yet all seven were orphaned
    // from the sitemap — reachable only from the nav rail, so the whole
    // explorer half of the site was undiscoverable to a crawler that
    // started at sitemap.xml. Their per-entity children stay out on
    // purpose: those are unbounded long tails served as noindex shells.
    '/network',
    '/ledgers',
    '/transactions',
    '/operations',
    '/accounts',
    '/contracts',
    '/protocols',
    '/sources',
    '/diagnostics',
    '/dexes',
    '/lending',
    '/amm',
    '/yield',
    '/sdex',
    '/liquidity-pools',
    '/aggregators',
    '/oracles',
    // Same category-hub family as the entries above (a CategoryHub over
    // /v1/protocols), and indexable and canonical-tagged like them, but it
    // was the one member never added here. routeAvailable drops it on the
    // test nets, where there are no bridge deployments to list.
    '/bridges',
    '/research',
    '/methodology',
    '/docs',
    '/widgets',
    '/sdk',
    '/contact',
    '/changelog',
    '/insights',
    '/insights/creators',
    '/insights/sponsors',
    '/anomalies',
    '/divergences',
    '/mev',
    '/exchanges',
    // The external-asset hub. Its per-currency children are enumerated
    // below (currencyPages) and the rail links it beside /exchanges, but
    // the hub itself was never listed — so the one page that indexes the
    // whole external set was the only part of it a crawler never saw.
    '/external/assets',
    '/pricing',
    '/blog',
    '/company',
    '/careers',
    '/status',
    '/sla',
    // The two research index pages. /research cards the individual
    // narratives and runbooks but never linked its own indexes, so both
    // were crawl-dark as well as click-dark.
    '/research/architecture',
    '/research/operations',
    // NOTE: auth/app routes (/signin, /signup, /account) are deliberately
    // NOT listed — they're robots:noindex (no SEO value / private), and a
    // noindex URL in the sitemap is a Search Console error
    // ("Submitted URL marked 'noindex'").
    //
    // #328: filtered through the per-network route table before emission.
    // A test net's sitemap used to submit /markets, /anomalies,
    // /divergences, /mev and the rest of the pricing surface to Search
    // Console under the TEST NET's own canonical origin — pages that are
    // structurally empty there, i.e. thin content, indexed on purpose.
  ]
    .filter((path) => routeAvailable(path === '' ? '/' : path))
    .map((path) => ({
      url: siteURL(path),
      lastModified: now,
      changeFrequency: path === '' ? 'daily' : ('weekly' as const),
      priority: path === '' ? 1 : 0.7,
    }));

  // Research pages: ADRs + curated architecture narratives. Both
  // surfaces are static-export pre-rendered and stable enough to
  // be worth indexing — they are cited from /methodology and from
  // every PR description.
  const adrPages: MetadataRoute.Sitemap = loadADRs().map((adr) => ({
    url: siteURL(`/research/adr/${adr.id}`),
    lastModified: now,
    changeFrequency: 'monthly',
    priority: 0.5,
  }));
  const archPages: MetadataRoute.Sitemap = loadArchitectureDocs().map((d) => ({
    url: siteURL(`/research/architecture/${d.slug}`),
    lastModified: now,
    changeFrequency: 'monthly',
    priority: 0.6,
  }));
  const opsPages: MetadataRoute.Sitemap = loadOperationsDocs().map((d) => ({
    url: siteURL(`/research/operations/${d.slug}`),
    lastModified: now,
    changeFrequency: 'monthly',
    priority: 0.5,
  }));
  const blogPages: MetadataRoute.Sitemap = loadBlogPosts().map((p) => ({
    url: siteURL(`/blog/${p.slug}`),
    lastModified: now,
    changeFrequency: 'monthly',
    priority: 0.6,
  }));
  // Incident postmortems — one permanent page each under /status.
  const incidentPages: MetadataRoute.Sitemap = loadIncidents().map((inc) => ({
    url: siteURL(`/status/incident/${inc.slug}`),
    lastModified: now,
    changeFrequency: 'monthly',
    priority: 0.4,
  }));
  // Per-protocol verification pages — pre-rendered from the static PROTOCOLS
  // registry (generateStaticParams in protocols/[name]). These were orphaned
  // from the sitemap despite being indexable, content-rich hubs.
  // sdex's canonical surface is /sdex (already in staticPages);
  // /protocols/sdex 308-redirects there and must not be sitemapped.
  const protocolPages: MetadataRoute.Sitemap = PROTOCOLS.filter(
    (p) => p.name !== 'sdex',
  ).map((p) => ({
    url: siteURL(`/protocols/${p.name}`),
    lastModified: now,
    changeFrequency: 'daily',
    priority: 0.7,
  }));

  const [
    assetSlugs,
    issuerKeys,
    currencyTickers,
    marketPairs,
    sources,
    lendingPools,
  ] = await Promise.all([
    fetchCoinSlugs(),
    fetchIssuerKeys(),
    fetchCurrencyTickers(),
    fetchMarketPairs(),
    fetchSources(),
    fetchLendingPools(),
  ]);
  // Numeric-only asset codes ("9", "818") are legal on Stellar but
  // read as junk results in a search index — keep the pages, drop
  // them from the sitemap (they also render noindex).
  const indexableAssetSlugs = assetSlugs.filter((s) => !/^\d+$/.test(s));
  const assetPages: MetadataRoute.Sitemap = indexableAssetSlugs.map((slug) => ({
    url: siteURL(`/assets/${slug}`),
    lastModified: now,
    changeFrequency: 'daily',
    priority: 0.6,
  }));
  const issuerPages: MetadataRoute.Sitemap = issuerKeys.map((g) => ({
    url: siteURL(`/issuers/${g}`),
    lastModified: now,
    changeFrequency: 'weekly',
    priority: 0.5,
  }));
  // Per-currency detail pages live under /external/assets/{friendly-slug}
  // (2026-08-24 fiat de-duplication: /assets/{fiat} no longer exports and
  // 301s there). One entry per ticker — friendly form (us-dollar, …).
  const currencyPages: MetadataRoute.Sitemap = currencyTickers.map(
    (ticker) => ({
      url: siteURL(`/external/assets/${fiatSlugFor(ticker)}`),
      lastModified: now,
      changeFrequency: 'daily',
      priority: 0.7,
    }),
  );

  // Convert pages — high-intent "X to Y" queries, pre-rendered as the same
  // hub-and-spoke matrix the route builds (shared buildConvertParams over the
  // same fiat ticker set, so the sitemap can't list a pair that 404s). These
  // were orphaned from the sitemap despite being indexed.
  const convertPages: MetadataRoute.Sitemap = buildConvertParams(
    currencyTickers,
  ).map(({ from, to }) => ({
    url: siteURL(`/convert/${from}/${to}`),
    lastModified: now,
    changeFrequency: 'weekly',
    priority: 0.6,
  }));

  // Per-pair detail pages. The route is /markets/{base}~{quote},
  // URL-encoded once. We pre-render the top 100 by 24h volume at
  // build time (see markets/[pair]/page.tsx); listing them in the
  // sitemap surfaces the highest-traffic pairs to crawlers without
  // exploding the file (every Stellar pair × 100s of issuers would
  // be tens of thousands of URLs of mostly-empty content).
  const marketPages: MetadataRoute.Sitemap = marketPairs.map((slug) => ({
    url: siteURL(`/markets/${encodeURIComponent(slug)}`),
    lastModified: now,
    changeFrequency: 'daily',
    priority: 0.6,
  }));
  // Per-source / per-exchange / per-dex detail pages. Every
  // source registry entry has a /sources/{name} page; only
  // ClassExchange entries with subclass=cex|dex have user-facing
  // /exchanges/{name} or /dexes/{source} pages.
  //
  // Pre-fix the sitemap emitted /exchanges/{name} AND /dexes/{name}
  // for *every* source — including aggregators (coingecko, cmc),
  // oracles (band, redstone, reflector-*), authority-sanity (ecb)
  // and lending (blend) — which produced ~33 sitemap entries that
  // 404'd at the page level. Google penalises sitemaps that
  // contain known-broken URLs, so we now gate emission on the
  // source's class+subclass to match the page's
  // generateStaticParams (CEX_INFO / DEX_INFO maps).
  const sourcePages: MetadataRoute.Sitemap = [];
  for (const s of sources) {
    sourcePages.push({
      url: siteURL(`/sources/${s.name}`),
      lastModified: now,
      changeFrequency: 'weekly',
      priority: 0.5,
    });
    if (s.class === 'exchange' && s.subclass === 'cex') {
      sourcePages.push({
        url: siteURL(`/exchanges/${s.name}`),
        lastModified: now,
        changeFrequency: 'weekly',
        priority: 0.5,
      });
    }
    if (s.class === 'exchange' && s.subclass === 'dex') {
      sourcePages.push({
        url: siteURL(`/dexes/${s.name}`),
        lastModified: now,
        changeFrequency: 'weekly',
        priority: 0.5,
      });
    }
  }
  // Lending pools — Blend pool detail pages. Small set today
  // (~9 pools), so list every one at priority 0.5.
  const lendingPages: MetadataRoute.Sitemap = lendingPools.map((id) => ({
    url: siteURL(`/lending/${id}`),
    lastModified: now,
    changeFrequency: 'weekly',
    priority: 0.5,
  }));

  return [
    ...staticPages,
    ...blogPages,
    ...incidentPages,
    ...adrPages,
    ...archPages,
    ...opsPages,
    ...protocolPages,
    ...convertPages,
    ...assetPages,
    ...issuerPages,
    ...currencyPages,
    ...marketPages,
    ...sourcePages,
    ...lendingPages,
  ];
}

type SitemapSource = {
  name: string;
  class: string;
  subclass: string;
};

// All six listings below go through buildFetch (src/lib/buildFetch.ts):
// bounded retry with backoff instead of one 5s shot, and — except where
// noted — fail-hard via requireRows so a persistent transport failure or
// an authoritative-empty listing fails the BUILD rather than quietly
// shipping a sitemap missing a whole URL family. A bare `catch { return
// [] }` here previously made a transient API blip indistinguishable from
// "there are truly zero rows", and Google silently drops the missing
// URLs rather than complaining.

async function fetchSources(): Promise<SitemapSource[]> {
  const rows = requireRows(
    await buildFetchData<{ name: string; class?: string; subclass?: string }[]>(
      '/v1/sources',
    ),
    '/v1/sources listing for sitemap',
  );
  return rows.map((s) => ({
    name: s.name,
    class: s.class ?? '',
    subclass: s.subclass ?? '',
  }));
}

async function fetchLendingPools(): Promise<string[]> {
  // Empty is tolerated here, same call as /lending/[pool]'s own
  // generateStaticParams: retry covers transport flakiness, but a
  // network with zero live pools right now is a real state, not a
  // broken one — there's no curated fallback to fall back to for the
  // sitemap the way that page has.
  //
  // /v1/lending/pools returns the pool contract address in the `pool`
  // field (matching the /lending/[pool] route's generateStaticParams).
  // Reading `contract_id` here (the field doesn't exist) emitted
  // `/lending/undefined` into the sitemap — a 404 Google penalises.
  const rows =
    (await buildFetchData<{ pool: string }[]>('/v1/lending/pools')) ?? [];
  return rows.map((p) => p.pool).filter(Boolean);
}

async function fetchMarketPairs(): Promise<string[]> {
  // Match the per-pair generateStaticParams cap (500) so the
  // sitemap doesn't undercount the routes we actually
  // pre-render. Pre-2026-05-08 this was 100 in both places —
  // bumped together so Google sees the same surface that
  // returns 200.
  const rows = requireRows(
    await buildFetchData<{ base: string; quote: string }[]>(
      '/v1/markets?limit=500&order_by=volume_24h_usd_desc',
    ),
    '/v1/markets listing for sitemap',
  );
  return rows.map((m) => `${m.base}~${m.quote}`);
}

async function fetchCurrencyTickers(): Promise<string[]> {
  // Migrated from /v1/currencies → /v1/assets/verified (rc.48 +
  // F-1201 audit-2026-05-12). The new endpoint returns the full
  // verified-currency catalogue with `class` ∈ {crypto, stablecoin,
  // fiat}; filter to fiat client-side so the sitemap only includes
  // the fiat tickers (which is what the per-currency converter
  // pages cover).
  const rows = requireRows(
    await buildFetchData<Array<{ ticker: string; class: string }>>(
      '/v1/assets/verified',
    ),
    '/v1/assets/verified listing for sitemap',
  );
  return rows.filter((row) => row.class === 'fiat').map((row) => row.ticker);
}

async function fetchIssuerKeys(): Promise<string[]> {
  const rows = requireRows(
    await buildFetchData<{ g_strkey: string }[]>('/v1/issuers?limit=100'),
    '/v1/issuers listing for sitemap',
  );
  return rows.map((i) => i.g_strkey);
}

async function fetchCoinSlugs(): Promise<string[]> {
  // /v1/assets returns rows with `slug` populated when sourced
  // from the coins reader (rc.47 lift). Fall back to `asset_id`
  // for any row without a friendly slug — those routes are still
  // pre-rendered under /assets/{asset_id}.
  const rows = requireRows(
    await buildFetchData<{ slug?: string; asset_id?: string }[]>(
      '/v1/assets?limit=500',
    ),
    '/v1/assets listing for sitemap',
  );
  return rows.map((d) => d.slug || d.asset_id || '').filter(Boolean);
}
