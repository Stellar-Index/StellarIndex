import type { Metadata } from 'next';
import Link from 'next/link';

import { Panel } from '@/components/reveal';
import { asExample, API_BASE_URL } from '@/api/client';
import { ogImageFor } from '@/lib/seo';
import { Badge, Breadcrumbs, Callout, Container } from '@/components/ui';
import { type GlobalAssetView } from '../../../assets/catalogue';

/**
 * /external/assets/[slug] — detail page for a NON-Stellar reference
 * asset (fiat currency or reference-only coin like BTC / ETH). The
 * Stellar-side counterpart lives at /assets/[slug]; this is the
 * external half of the LC-001 Stellar/external split.
 *
 * Backed by GET /v1/external/assets/{slug} (envelope `{data:
 * GlobalAssetView}`). It's a LEAN page: it renders only the
 * cross-chain headline (name / ticker / class + USD price +
 * authority) — none of the Stellar-specific tabs (issuer / holders /
 * liquidity / supply / SEP-1 / markets), which don't apply to an
 * asset that isn't issued on Stellar.
 */

// Static export hits every page once at build time. CI's stub
// hostname doesn't resolve, so bypass the network entirely when the
// URL looks like a CI placeholder (mirrors the Stellar page).
const isCIStub =
  API_BASE_URL.includes('.invalid') || API_BASE_URL.includes('local-stub');

// 8s per fetch, matching the Stellar detail page's build budget.
const BUILD_FETCH_TIMEOUT_MS = 8_000;

export async function generateStaticParams() {
  // Build-time fetch of the external listing so every fiat / reference
  // coin gets a pre-rendered route. A single canonical fallback keeps
  // the static export buildable when the API is unreachable (CI) —
  // Next refuses a dynamic route under output:'export' with zero
  // params. `btc` is a real reference-only catalogue slug, so the
  // fallback page renders real content rather than a stub.
  const fallback = [{ slug: 'btc' }];
  if (isCIStub) return fallback;
  try {
    const res = await fetch(`${API_BASE_URL}/v1/external/assets?limit=500`, {
      signal: AbortSignal.timeout(BUILD_FETCH_TIMEOUT_MS * 2),
    });
    if (!res.ok) return fallback;
    const env = (await res.json()) as { data?: { slug?: string }[] };
    const rows = env.data ?? [];
    const seen = new Set<string>();
    const out: { slug: string }[] = [];
    for (const row of rows) {
      if (row.slug && !seen.has(row.slug)) {
        seen.add(row.slug);
        out.push({ slug: row.slug });
      }
    }
    return out.length > 0 ? out : fallback;
  } catch {
    return fallback;
  }
}

type Params = Promise<{ slug: string }>;

// fetchExternalAsset reads GET /v1/external/assets/{slug} at build
// time. Returns null on any non-2xx / network error / CI stub so the
// page renders its not-found state (a Stellar asset 404s here — its
// detail lives on /assets/{slug}).
async function fetchExternalAsset(slug: string): Promise<GlobalAssetView | null> {
  if (isCIStub) return null;
  try {
    const res = await fetch(
      `${API_BASE_URL}/v1/external/assets/${encodeURIComponent(slug)}`,
      { signal: AbortSignal.timeout(BUILD_FETCH_TIMEOUT_MS) },
    );
    if (!res.ok) return null;
    const env = (await res.json()) as { data?: GlobalAssetView };
    return env.data ?? null;
  } catch {
    return null;
  }
}

export async function generateMetadata({
  params,
}: {
  params: Params;
}): Promise<Metadata> {
  const { slug } = await params;
  const view = await fetchExternalAsset(slug);
  const name = view?.name ?? view?.ticker ?? slug;
  const canonical = `https://stellarindex.io/external/assets/${view?.slug ?? slug}`;
  const description = `${name} is a non-Stellar reference asset tracked by Stellar Index for pricing (it isn't issued on Stellar). Live USD price and source authority.`;
  const title = `${name} price — external asset`;
  return {
    title,
    description,
    alternates: { canonical },
    openGraph: {
      title,
      description,
      url: canonical,
      type: 'website',
      images: [ogImageFor('assets', view?.slug ?? slug)],
    },
    twitter: {
      card: 'summary_large_image',
      title,
      description,
      images: [ogImageFor('assets', view?.slug ?? slug)],
    },
  };
}

export default async function ExternalAssetDetailPage({
  params,
}: {
  params: Params;
}) {
  const { slug } = await params;
  const view = await fetchExternalAsset(slug);

  if (!view) {
    return (
      <Container className="space-y-8 py-8 sm:py-10">
        <header className="space-y-3">
          <Breadcrumbs
            items={[
              { label: 'Home', href: '/' },
              { label: 'External assets', href: '/external/assets' },
              { label: slug },
            ]}
          />
          <h1 className="text-h1 font-semibold text-ink">{slug}</h1>
        </header>
        <Callout tone="warn" title="External asset not found">
          <p>
            We don&apos;t track an external (non-Stellar) asset with the slug{' '}
            <code className="font-mono">{slug}</code>. If this is a
            Stellar-issued asset, its detail lives on{' '}
            <Link href="/assets" className="font-medium underline">
              /assets
            </Link>
            .
          </p>
          <p className="mt-2">
            <Link
              href="/external/assets"
              className="font-medium text-brand-600 hover:text-brand-700"
            >
              ← External assets
            </Link>
          </p>
        </Callout>
      </Container>
    );
  }

  const priceNum = view.price_usd != null ? Number(view.price_usd) : null;
  const hasPrice =
    priceNum != null && Number.isFinite(priceNum) && priceNum > 0;
  const authorityLabel = view.price_authority
    ? PRICE_AUTHORITY_LABELS[view.price_authority] ?? view.price_authority
    : null;

  return (
    <Container className="space-y-8 py-8 sm:py-10">
      <header className="space-y-3">
        <Breadcrumbs
          items={[
            { label: 'Home', href: '/' },
            { label: 'External assets', href: '/external/assets' },
            { label: view.name || view.ticker },
          ]}
        />
        <div className="text-xs font-medium uppercase tracking-wider text-brand-600">
          External asset
        </div>
        <h1 className="flex flex-wrap items-baseline gap-3 text-h1 font-semibold text-ink">
          <span>{view.name}</span>
          <span className="font-mono text-base text-ink-muted">
            {view.ticker}
          </span>
          <ClassBadge cls={view.class} />
        </h1>
        {view.verified_issuer && (
          <p className="text-sm text-ink-body">
            Reference issuer:{' '}
            <span className="font-medium">{view.verified_issuer}</span>
          </p>
        )}
      </header>

      <Panel
        title="Price"
        hint={
          view.price_as_of
            ? `As of ${new Date(view.price_as_of).toISOString().replace('T', ' ').slice(0, 19)} UTC`
            : undefined
        }
        source={asExample('/v1/external/assets/{slug}', { slug: view.slug })}
        bodyClassName="space-y-3"
      >
        <div className="flex flex-wrap items-baseline gap-4">
          <span className="font-mono text-3xl tabular-nums text-ink">
            {hasPrice ? `$${formatHeadlinePrice(priceNum)}` : '—'}
          </span>
          <span className="text-sm text-ink-muted">USD</span>
          {authorityLabel && (
            <span
              className="rounded-sm bg-brand-50 px-2 py-0.5 text-[11px] uppercase tracking-wider text-brand-700"
              title="How this price was derived"
            >
              {authorityLabel}
            </span>
          )}
        </div>
        {!hasPrice && (
          <p className="text-sm text-ink-muted">
            No live USD price is currently available for this asset from our
            off-chain feeds.
          </p>
        )}
        {view.price_sources && view.price_sources.length > 0 && (
          <p className="text-xs text-ink-muted">
            Sources:{' '}
            <span className="font-mono text-ink-body">
              {view.price_sources.join(', ')}
            </span>
          </p>
        )}
      </Panel>

      <Panel title="About" bodyClassName="space-y-3 text-sm text-ink-body">
        {view.description && (
          <p className="leading-relaxed">{view.description}</p>
        )}
        <p className="leading-relaxed text-ink-muted">
          <span className="font-medium text-ink-body">{view.name}</span> is a
          non-Stellar {view.class === 'fiat' ? 'fiat currency' : 'reference asset'}{' '}
          tracked by Stellar Index for pricing — it is{' '}
          <span className="font-medium">not issued on Stellar</span>. We index it
          from off-chain venues and reference feeds so on-Stellar pairs (and the
          aggregated VWAP) have a fiat/reference anchor. Stellar-issued assets
          live on{' '}
          <Link
            href="/assets"
            className="font-medium text-brand-600 hover:text-brand-700"
          >
            /assets
          </Link>
          .
        </p>
      </Panel>

      <p className="text-sm text-ink-muted">
        <Link
          href="/external/assets"
          className="font-medium text-brand-600 hover:text-brand-700"
        >
          ← External assets
        </Link>
      </p>
    </Container>
  );
}

// Human-readable labels for GlobalAssetView.price_authority — the tier
// of ComputeGlobalPrice's fallback chain that produced the headline.
const PRICE_AUTHORITY_LABELS: Record<string, string> = {
  vwap_native: 'Native VWAP',
  aggregator_avg: 'Aggregator average',
  triangulated: 'Triangulated',
};

// formatHeadlinePrice mirrors the GlobalAssetView headline formatting
// on the Stellar detail page's VerifiedCurrencyView: exponential for
// sub-milli prices, 2dp for large, 6dp otherwise.
function formatHeadlinePrice(n: number): string {
  if (n < 0.001) return n.toExponential(3);
  return n.toFixed(n >= 100 ? 2 : 6);
}

// ClassBadge renders the asset-class pill (Fiat / Crypto / Stablecoin),
// tone-matched to the /assets directory table so the same class looks
// the same everywhere.
function ClassBadge({ cls }: { cls?: string }) {
  if (!cls) {
    return <Badge>reference</Badge>;
  }
  const tone: 'warn' | 'ok' | 'brand' =
    cls === 'fiat' ? 'warn' : cls === 'stablecoin' ? 'ok' : 'brand';
  const label =
    cls === 'fiat' ? 'Fiat' : cls === 'stablecoin' ? 'Stablecoin' : 'Crypto';
  return <Badge tone={tone}>{label}</Badge>;
}
