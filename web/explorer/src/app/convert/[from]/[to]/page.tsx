import type { Metadata } from 'next';
import Link from 'next/link';
import { ArrowLeftRight } from 'lucide-react';

import { Container, PageHeader } from '@/components/ui';
import { SITE_OG_IMAGES } from '@/lib/seo';
import { assetHref, assetHrefFor } from '@/lib/fiat-slugs';
import {
  buildAssetConvertParams,
  buildConvertParams,
  type ConvertAsset,
} from '@/lib/convert-params';
import { formatPairPrice } from '@/lib/format';
import { ConvertPair } from './ConvertPair';
import { ConvertChart } from './ConvertChart';
import {
  ConvertAssetIdsProvider,
  ConvertLiveRate,
  ConvertSnippets,
} from './ConvertLive';
import { buildFetchData, isCIStub } from '@/lib/buildFetch';
import { fetchConvertCatalogue } from '../../tickers';
import { CURRENT_NETWORK } from '@/lib/networks';

type Params = Promise<{ from: string; to: string }>;

// Local projection of the per-(from, to) data the SSR shell needs.
// One pair comes from /v1/price/batch, synthesized into a {to: rate}
// singleton map so the render path stays map-shaped.
interface CurrencyDetail {
  ticker: string;
  name: string;
  cross_rates: Record<string, number>; // {to: 1 {from} = N {to}}
  // NOTE: no `source`/`published_at` here — neither endpoint this page
  // reads serves per-rate provenance, and declared-but-never-populated
  // fields would leave a permanently blank "Source: …" line. Re-add only when a real provenance field
  // exists on the wire.
}

// Hub-and-spoke: top-20 majors × all-110 currencies, both directions.
// 20 × 109 × 2 = ~4,360 pages — well inside Cloudflare Pages'
// 20,000-file/deploy ceiling once Next.js's .html/.meta/.rsc trio
// per route is counted. The full N×N (12k pages, ~36k files) blows
// the cap; this design captures the SEO surface that matters.
// Verified Stellar assets add one direction only: asset → each hub (≤50 × 20).
// HUB_TICKERS + the pair-builders live in @/lib/convert-params so the
// sitemap mirrors this exact set (no drift → no 404s in the sitemap).
export async function generateStaticParams() {
  // NOTE: with output:export a dynamic route may NOT return an empty param set
  // (Next errors "missing generateStaticParams"), so we can't 404 the whole
  // /convert tree by building zero pages. The route is instead hidden from the
  // Footer nav on the lean test nets (see Footer LEAN_HIDDEN_HREFS); its live
  // rate/chart fetches are gated below so a direct visit doesn't 404-storm.
  const { fiat, assets } = await fetchConvertCatalogue();
  return [
    ...buildConvertParams(fiat),
    ...buildAssetConvertParams(assets, fiat),
  ];
}

/** The verified asset a (upper-cased) path segment names, if any. */
async function convertAssetFor(
  segment: string,
): Promise<ConvertAsset | undefined> {
  const { assets } = await fetchConvertCatalogue();
  return assets.find((a) => a.ticker.toUpperCase() === segment);
}

// A verified asset's identity is the catalogue row; /v1/external/assets is
// keyed by bare ticker and must never stand in for a (code, issuer) asset.
async function fetchAssetDetail(
  asset: ConvertAsset,
  to: string,
): Promise<CurrencyDetail | null> {
  if (isCIStub) return null;
  const rows = await buildFetchData<
    Array<{ asset_id: string; price: string | null }>
  >(
    `/v1/price/batch?asset_ids=${encodeURIComponent(asset.assetId)}&quote=${encodeURIComponent(`fiat:${to}`)}`,
    { softFail: true, timeoutMs: 6_000, attempts: 2 },
  );
  const row = (rows ?? []).find((r) => r.asset_id === asset.assetId);
  const rate = row?.price ? Number(row.price) : 0;
  return {
    ticker: asset.ticker,
    name: asset.name,
    cross_rates: rate > 0 ? { [to]: rate } : {},
  };
}

// fetchDetail returns the SSR snapshot the converter shell needs:
// the {from} currency's identity (ticker / name) + the from→to
// rate via cross_rates[to].
//
// Two endpoints, fetched in parallel:
//
//   1. /v1/external/assets/{from} for the identity (ticker, name);
//      /v1/assets is Stellar-only and 404s a fiat ticker
//   2. /v1/price/batch?asset_ids=fiat:{from}&quote=fiat:{to} for
//      the singleton from→to rate (forward: 1 {from} in {to} units)
//
// The cross_rates map carries just the one entry (key = `to`)
// rather than every ticker the pre-rc.48 endpoint returned. The
// SSR shell only ever reads cross_rates[to], so the surface area
// is unchanged.
async function fetchDetail(
  from: string,
  to: string,
): Promise<CurrencyDetail | null> {
  if (isCIStub) return null;
  const [identity, priceRows] = await Promise.all([
    // Entity identity — fail-hard, same as fetchAssetDetail in
    // assets/[slug]/page.tsx: a persistent transport failure throws
    // and fails the build rather than baking a rate-less page.
    buildFetchData<{ ticker: string; name: string; price_usd?: string | null }>(
      `/v1/external/assets/${from.toUpperCase()}`,
    ),
    // The rate itself — softFail, like fetchPriceDirect's /v1/price
    // call: useConvertRate (ConvertLive.tsx) re-fetches this pair
    // client-side every 60s, so a cold/slow price/batch at build
    // time should degrade this one pair rather than abort the export.
    buildFetchData<Array<{ asset_id: string; price: string | null }>>(
      `/v1/price/batch?asset_ids=${encodeURIComponent(`fiat:${from.toUpperCase()}`)}&quote=${encodeURIComponent(`fiat:${to.toUpperCase()}`)}`,
      { softFail: true, timeoutMs: 6_000, attempts: 2 },
    ),
  ]);
  if (!identity) return null;
  const fromUSD = identity.price_usd ? Number(identity.price_usd) : 0;
  if (!(fromUSD > 0)) return null;
  const row = (priceRows ?? []).find(
    (r) => r.asset_id === `fiat:${from.toUpperCase()}`,
  );
  // Forward read: the price is already "1 {from} = ? {to}", the
  // direction the converter displays, so it is never inverted.
  const fromToRate = row?.price ? Number(row.price) : 0;
  return {
    ticker: identity.ticker,
    name: identity.name,
    cross_rates: fromToRate > 0 ? { [to.toUpperCase()]: fromToRate } : {},
  };
}

export async function generateMetadata({
  params,
}: {
  params: Params;
}): Promise<Metadata> {
  const { from, to } = await params;
  const t = to.toUpperCase();
  const asset = await convertAssetFor(from.toUpperCase());
  const f = asset?.ticker ?? from.toUpperCase();
  const detail = asset
    ? await fetchAssetDetail(asset, t)
    : await fetchDetail(f, t);
  const rate = detail?.cross_rates?.[t];
  const path = `/convert/${f.toUpperCase()}/${t}`;
  const ratePart =
    rate != null ? ` 1 ${f} = ${formatPairPrice(rate)} ${t}.` : '';
  return {
    title: `${f} to ${t} — live exchange rate + currency converter`,
    description: `Convert ${f} to ${t} at the live mid-market rate.${ratePart} Real-time forex rate, interactive converter, and ${f}/${t} cross-rates at common amounts (1, 10, 100, 1000, 10000).`,
    alternates: {
      canonical: `${CURRENT_NETWORK.explorerUrl}${path}`,
    },
    openGraph: {
      title: `${f} to ${t} converter`,
      description:
        rate != null
          ? `1 ${f} = ${formatPairPrice(rate)} ${t} — live forex rate.`
          : `Live ${f} to ${t} forex rate + converter.`,
      url: `${CURRENT_NETWORK.explorerUrl}${path}`,
      type: 'website',
      images: SITE_OG_IMAGES,
    },
  };
}

export default async function ConvertPage({ params }: { params: Params }) {
  const { from, to } = await params;
  const t = to.toUpperCase();
  const asset = await convertAssetFor(from.toUpperCase());
  const f = asset?.ticker ?? from.toUpperCase();
  const fromHref = asset ? assetHref(asset.slug) : assetHrefFor(f);
  // Only asset → fiat is baked; the landing picker prices the reverse.
  const reverseHref = asset
    ? `/convert/?${new URLSearchParams({ from: t, to: f })}`
    : `/convert/${t}/${f}`;
  // The landing reads its pair from the query string on load, so that hop is a
  // full navigation rather than a client transition.
  const ReverseLink = asset ? 'a' : Link;

  const detail = asset
    ? await fetchAssetDetail(asset, t)
    : await fetchDetail(f, t);
  const rate = detail?.cross_rates?.[t] ?? null;
  const inverse = rate != null && rate > 0 ? 1 / rate : null;

  return (
    <ConvertAssetIdsProvider ids={asset ? { [f]: asset.assetId } : {}}>
      <Container className="space-y-6 py-8 [&>*]:max-w-4xl">
        {/* The converter sits under the "from" currency's detail page. */}
        <PageHeader
          breadcrumbs={[
            { label: 'Home', href: '/' },
            { label: 'Assets', href: '/assets' },
            { label: f, href: fromHref },
            { label: `${f} → ${t}` },
          ]}
          title={`${f} to ${t}`}
          meta={detail?.name ? `${detail.name} → ${t}` : undefined}
        />
        {/* The baked rate paints first; the client swaps in the live one. */}
        <ConvertLiveRate
          from={f}
          to={t}
          initialRate={rate}
          initialInverse={inverse}
        />

        <ConvertPair
          from={f}
          to={t}
          initialRate={rate}
          initialInverse={inverse}
        />

        <ConvertChart from={f} to={t} />

        {/* Common-amounts ladder hydrates LIVE off the same shared query
          so its "= Y" values and "current mid-market rate" caption track
          the same live rate as the widget (W8 recon 10a). */}
        <ConvertSnippets
          from={f}
          to={t}
          initialRate={rate}
          initialInverse={inverse}
        />

        <section className="flex flex-wrap gap-2 text-sm">
          <ReverseLink
            href={reverseHref}
            className="border-line bg-surface text-ink-body hover:border-brand-500 hover:text-brand-600 inline-flex items-center gap-1.5 rounded-md border px-3 py-2"
          >
            <ArrowLeftRight className="h-3.5 w-3.5" />
            Convert {t} to {f} instead
          </ReverseLink>
          <Link
            href={fromHref}
            className="border-line bg-surface text-ink-body hover:border-brand-500 hover:text-brand-600 inline-flex items-center rounded-md border px-3 py-2"
          >
            {asset ? `${f} details` : `${f} cross-rates`}
          </Link>
          <Link
            href={assetHrefFor(t)}
            className="border-line bg-surface text-ink-body hover:border-brand-500 hover:text-brand-600 inline-flex items-center rounded-md border px-3 py-2"
          >
            {t} cross-rates
          </Link>
        </section>
      </Container>
    </ConvertAssetIdsProvider>
  );
}
