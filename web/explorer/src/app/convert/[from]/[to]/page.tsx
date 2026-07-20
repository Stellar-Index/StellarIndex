import type { Metadata } from 'next';
import Link from 'next/link';
import { ArrowLeftRight } from 'lucide-react';

import { Breadcrumbs } from '@/components/ui';
import { SITE_OG_IMAGES, serializeJsonLd } from '@/lib/seo';
import { assetHrefFor } from '@/lib/fiat-slugs';
import { buildConvertParams } from '@/lib/convert-params';
import { ConvertPair } from './ConvertPair';
import { ConvertChart } from './ConvertChart';
import { API_BASE_URL } from '@/api/client';

const isCIStub =
  API_BASE_URL.includes('.invalid') || API_BASE_URL.includes('local-stub');

const BUILD_FETCH_TIMEOUT_MS = 8_000;

// Fallback majors so a brand-new build with no upstream still
// produces a meaningful matrix. Same set as /currencies/[ticker]'s
// fallback so the two routes stay aligned.
const FALLBACK_TICKERS = [
  'USD', 'EUR', 'GBP', 'JPY', 'CHF', 'CAD', 'AUD', 'CNY',
  'INR', 'BRL', 'MXN', 'ZAR', 'NZD', 'SGD', 'HKD', 'SEK',
];

// Common amounts to render as static "X = Y" snippets for SEO body
// content. Picks naturally-queried ladder values across orders of
// magnitude — Google search-volume tools show "1 X to Y", "100 X
// to Y", "1000 X to Y" all rank as distinct queries with non-trivial
// volume even for the same currency pair.
const SNIPPET_AMOUNTS = [1, 10, 100, 1000, 10000];

type Params = Promise<{ from: string; to: string }>;

// Local projection of the per-(from, to) data the SSR shell needs.
// Pre-F-1201 this carried a full cross_rates map (from→every ticker)
// from /v1/currencies/{from}; post-F-1201 we fetch one pair via
// /v1/price/batch and synthesize a {to: rate} singleton map so the
// existing render path stays unchanged.
interface CurrencyDetail {
  ticker: string;
  name: string;
  rate_usd: number; // 1 USD = N {from}
  inverse_usd: number; // 1 {from} = N USD
  cross_rates: Record<string, number>; // {to: 1 {from} = N {to}}
  published_at?: string;
  source?: string;
}

interface VerifiedCurrencyEntry {
  ticker: string;
  name?: string;
  class: string;
}

async function fetchTickers(): Promise<string[]> {
  if (isCIStub) return FALLBACK_TICKERS;
  // Migrated from /v1/currencies → /v1/assets/verified (rc.48 +
  // F-1201 audit-2026-05-12). Filter to class=fiat client-side.
  try {
    const res = await fetch(`${API_BASE_URL}/v1/assets/verified`, {
      signal: AbortSignal.timeout(BUILD_FETCH_TIMEOUT_MS),
    });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const env = (await res.json()) as { data: VerifiedCurrencyEntry[] };
    const tickers = (env.data ?? [])
      .filter((row) => row.class === 'fiat')
      .map((row) => row.ticker)
      .filter(Boolean);
    return tickers.length > 0 ? tickers : FALLBACK_TICKERS;
  } catch {
    return FALLBACK_TICKERS;
  }
}

// Hub-and-spoke: top-20 majors × all-110 currencies, both directions.
// 20 × 109 × 2 = ~4,360 pages — well inside Cloudflare Pages'
// 20,000-file/deploy ceiling once Next.js's .html/.meta/.rsc trio
// per route is counted. The full N×N (12k pages, ~36k files) blows
// the cap; this design captures the SEO surface that matters.
// HUB_TICKERS + the pair-builder live in @/lib/convert-params so the
// sitemap mirrors this exact set (no drift → no 404s in the sitemap).
export async function generateStaticParams() {
  return buildConvertParams(await fetchTickers());
}

// fetchDetail returns the SSR snapshot the converter shell needs:
// the {from} currency's identity (ticker / name) + the from→to
// rate via cross_rates[to].
//
// F-1201 migration: pre-rc.48 a single /v1/currencies/{from} call
// returned every cross-rate at once; rc.48 removed that route.
// We now hit two endpoints in parallel:
//
//   1. /v1/assets/{from} for the identity (ticker, name)
//   2. /v1/price/batch?asset_ids=fiat:{to}&quote=fiat:{from} for
//      the singleton from→to rate
//
// The cross_rates map carries just the one entry (key = `to`)
// rather than every ticker the pre-rc.48 endpoint returned. The
// SSR shell only ever reads cross_rates[to], so the surface area
// is unchanged.
async function fetchDetail(from: string, to: string): Promise<CurrencyDetail | null> {
  if (isCIStub) return null;
  try {
    const [identityRes, priceRes] = await Promise.all([
      fetch(`${API_BASE_URL}/v1/assets/${from.toUpperCase()}`, {
        signal: AbortSignal.timeout(BUILD_FETCH_TIMEOUT_MS),
      }),
      fetch(
        `${API_BASE_URL}/v1/price/batch?asset_ids=${encodeURIComponent(`fiat:${to.toUpperCase()}`)}&quote=${encodeURIComponent(`fiat:${from.toUpperCase()}`)}`,
        { signal: AbortSignal.timeout(BUILD_FETCH_TIMEOUT_MS) },
      ),
    ]);
    if (!identityRes.ok || !priceRes.ok) return null;
    const identityEnv = (await identityRes.json()) as {
      data: { ticker: string; name: string; price_usd?: string | null };
    };
    const priceEnv = (await priceRes.json()) as {
      data: Array<{ asset_id: string; price: string | null }>;
    };
    const v = identityEnv.data;
    if (!v) return null;
    const fromUSD = v.price_usd ? Number(v.price_usd) : 0;
    if (!(fromUSD > 0)) return null;
    const toRateRow = (priceEnv.data ?? []).find(
      (r) => r.asset_id === `fiat:${to.toUpperCase()}`,
    );
    // price/batch(asset_ids=fiat:{to}, quote=fiat:{from}) returns the
    // value of 1 {to} in {from} units (e.g. 1 EUR = 1.15 USD). The
    // converter displays "1 {from} = ? {to}", which is the INVERSE.
    // Pre-fix (audit 2026-06-19) this was shown un-inverted, so
    // /convert/USD/EUR read "1 USD = 1.15 EUR" — actually the EUR→USD
    // rate mislabeled. Invert here.
    const toInFromUnits = toRateRow?.price ? Number(toRateRow.price) : 0;
    const fromToRate = toInFromUnits > 0 ? 1 / toInFromUnits : 0;
    return {
      ticker: v.ticker,
      name: v.name,
      // rate_usd: 1 USD = N {from}  →  inverse of fromUSD (which
      // is 1 {from} = N USD)
      rate_usd: 1 / fromUSD,
      inverse_usd: fromUSD,
      cross_rates: fromToRate > 0 ? { [to.toUpperCase()]: fromToRate } : {},
    };
  } catch {
    return null;
  }
}

export async function generateMetadata({ params }: { params: Params }): Promise<Metadata> {
  const { from, to } = await params;
  const f = from.toUpperCase();
  const t = to.toUpperCase();
  const detail = await fetchDetail(f, t);
  const rate = detail?.cross_rates?.[t];
  const ratePart = rate != null
    ? ` 1 ${f} = ${formatRateForMeta(rate)} ${t}.`
    : '';
  return {
    title: `${f} to ${t} — live exchange rate + currency converter`,
    description: `Convert ${f} to ${t} at the live mid-market rate.${ratePart} Real-time forex rate, interactive converter, and ${f}/${t} cross-rates at common amounts (1, 10, 100, 1000, 10000).`,
    alternates: {
      canonical: `https://stellarindex.io/convert/${f}/${t}`,
    },
    openGraph: {
      title: `${f} to ${t} converter`,
      description: rate != null
        ? `1 ${f} = ${formatRateForMeta(rate)} ${t} — live forex rate.`
        : `Live ${f} to ${t} forex rate + converter.`,
      url: `https://stellarindex.io/convert/${f}/${t}`,
      type: 'website',
      images: SITE_OG_IMAGES,
    },
  };
}

export default async function ConvertPage({ params }: { params: Params }) {
  const { from, to } = await params;
  const f = from.toUpperCase();
  const t = to.toUpperCase();

  const detail = await fetchDetail(f, t);
  const rate = detail?.cross_rates?.[t] ?? null;
  const inverse = rate != null && rate > 0 ? 1 / rate : null;

  // Schema.org BreadcrumbList — Home → Currencies → <from> → <from> to <to>.
  // Four-step path because the converter sits one level below the
  // currency-detail page in the IA (the "from" currency owns the conversion).
  const breadcrumbLD = {
    '@context': 'https://schema.org',
    '@type': 'BreadcrumbList',
    itemListElement: [
      { '@type': 'ListItem', position: 1, name: 'Home', item: 'https://stellarindex.io' },
      { '@type': 'ListItem', position: 2, name: 'Assets', item: 'https://stellarindex.io/assets' },
      { '@type': 'ListItem', position: 3, name: f, item: `https://stellarindex.io${assetHrefFor(f)}` },
      { '@type': 'ListItem', position: 4, name: `${f} to ${t}`, item: `https://stellarindex.io/convert/${f}/${t}` },
    ],
  };

  return (
    <div className="mx-auto max-w-4xl space-y-6 px-6 py-8">
      <script
        type="application/ld+json"
        dangerouslySetInnerHTML={{ __html: serializeJsonLd(breadcrumbLD) }}
      />
      <Breadcrumbs
        items={[
          { label: 'Home', href: '/' },
          { label: 'Assets', href: '/assets' },
          { label: f, href: assetHrefFor(f) },
          { label: `${f} → ${t}` },
        ]}
      />

      <header className="space-y-3 border-b border-line pb-5">
        <h1 className="text-3xl font-semibold tracking-tight">
          {f} to {t}
          {detail?.name && (
            <span className="ml-3 text-base font-normal text-ink-muted">
              {detail.name} → {t}
            </span>
          )}
        </h1>
        {rate != null ? (
          <p className="text-2xl font-mono tabular-nums text-ink">
            1 {f} = {formatRate(rate)} {t}
          </p>
        ) : (
          <p className="text-sm text-ink-muted">Rate currently unavailable.</p>
        )}
        {inverse != null && (
          <p className="text-sm font-mono tabular-nums text-ink-body">
            1 {t} = {formatRate(inverse)} {f}
          </p>
        )}
        {detail?.source && (
          <p className="text-xs text-ink-muted">
            Source: {detail.source}
            {detail.published_at && ` · published ${formatDate(detail.published_at)}`}
          </p>
        )}
      </header>

      <ConvertPair from={f} to={t} initialRate={rate} initialInverse={inverse} />

      <ConvertChart from={f} to={t} />

      {rate != null && (
        <section className="rounded-card border border-line bg-surface p-5">
          <h2 className="mb-4 text-lg font-semibold tracking-tight">
            {f} to {t} at common amounts
          </h2>
          <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
            {SNIPPET_AMOUNTS.map((amt) => (
              <div
                key={amt}
                className="flex items-baseline justify-between rounded-md bg-surface-muted px-3 py-2"
              >
                <span className="font-mono tabular-nums text-ink-body">
                  {amt.toLocaleString()} {f}
                </span>
                <span className="font-mono tabular-nums font-medium text-ink">
                  {formatRate(amt * rate)} {t}
                </span>
              </div>
            ))}
          </div>
          <p className="mt-4 text-xs text-ink-muted">
            All values calculated at the current mid-market rate of 1 {f} = {formatRate(rate)} {t}.
            Rates update on each {detail?.source ?? 'forex source'} refresh tick.
          </p>
        </section>
      )}

      <section className="flex flex-wrap gap-2 text-sm">
        <Link
          href={`/convert/${t}/${f}`}
          className="inline-flex items-center gap-1.5 rounded-md border border-line bg-surface px-3 py-2 text-ink-body hover:border-brand-500 hover:text-brand-600"
        >
          <ArrowLeftRight className="h-3.5 w-3.5" />
          Convert {t} to {f} instead
        </Link>
        <Link
          href={assetHrefFor(f)}
          className="inline-flex items-center rounded-md border border-line bg-surface px-3 py-2 text-ink-body hover:border-brand-500 hover:text-brand-600"
        >
          {f} cross-rates
        </Link>
        <Link
          href={assetHrefFor(t)}
          className="inline-flex items-center rounded-md border border-line bg-surface px-3 py-2 text-ink-body hover:border-brand-500 hover:text-brand-600"
        >
          {t} cross-rates
        </Link>
      </section>
    </div>
  );
}

function formatRate(n: number): string {
  if (!Number.isFinite(n)) return '—';
  if (Math.abs(n) >= 1000) return n.toLocaleString(undefined, { maximumFractionDigits: 2 });
  if (Math.abs(n) >= 1) return n.toFixed(4);
  if (Math.abs(n) >= 0.01) return n.toFixed(6);
  return n.toFixed(8);
}

function formatRateForMeta(n: number): string {
  if (!Number.isFinite(n)) return '—';
  if (Math.abs(n) >= 100) return n.toFixed(2);
  if (Math.abs(n) >= 1) return n.toFixed(4);
  return n.toFixed(6);
}

function formatDate(iso: string): string {
  try {
    return new Date(iso).toLocaleDateString(undefined, {
      year: 'numeric', month: 'short', day: 'numeric',
    });
  } catch {
    return iso;
  }
}
