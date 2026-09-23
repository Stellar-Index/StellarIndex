import Link from 'next/link';

import { Panel } from '@/components/reveal';
import { asExample } from '@/api/client';
import { formatSubunitPrice } from '@/lib/format';
import { Badge, Breadcrumbs, Container } from '@/components/ui';
import { type GlobalAssetView } from '../../../assets/catalogue';

/**
 * ExternalAssetDetailView — the rendered detail for one resolved
 * GlobalAssetView, shared by the build-time page and the runtime shell
 * (ExternalAssetPathView) so the two cannot drift.
 */
export function ExternalAssetDetailView({ view }: { view: GlobalAssetView }) {
  const priceNum = view.price_usd != null ? Number(view.price_usd) : null;
  const hasPrice =
    priceNum != null && Number.isFinite(priceNum) && priceNum > 0;
  const authorityLabel = view.price_authority
    ? (PRICE_AUTHORITY_LABELS[view.price_authority] ?? view.price_authority)
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
        <div className="text-brand-600 text-xs font-medium tracking-wider uppercase">
          External asset
        </div>
        <h1 className="text-h1 text-ink flex flex-wrap items-baseline gap-3 font-semibold">
          <span>{view.name}</span>
          <span className="text-ink-muted font-mono text-base">
            {view.ticker}
          </span>
          <ClassBadge cls={view.class} />
        </h1>
        {view.verified_issuer && (
          <p className="text-ink-body text-sm">
            Reference issuer:{' '}
            <span className="font-medium">{view.verified_issuer}</span>
          </p>
        )}
      </header>

      <Panel
        headingLevel={2}
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
          <span className="text-ink font-mono text-3xl tabular-nums">
            {hasPrice ? `$${formatHeadlinePrice(priceNum)}` : '—'}
          </span>
          <span className="text-ink-muted text-sm">USD</span>
          {authorityLabel && (
            <span
              className="bg-brand-50 text-brand-700 rounded-sm px-2 py-0.5 text-[11px] tracking-wider uppercase"
              title="How this price was derived"
            >
              {authorityLabel}
            </span>
          )}
        </div>
        {!hasPrice && (
          <p className="text-ink-muted text-sm">
            No live USD price is currently available for this asset from our
            off-chain feeds.
          </p>
        )}
        {view.price_sources && view.price_sources.length > 0 && (
          <p className="text-ink-muted text-xs">
            Sources:{' '}
            <span className="text-ink-body font-mono">
              {view.price_sources.join(', ')}
            </span>
          </p>
        )}
      </Panel>

      <Panel
        headingLevel={2}
        title="About"
        bodyClassName="space-y-3 text-sm text-ink-body"
      >
        {view.description && (
          <p className="leading-relaxed">{view.description}</p>
        )}
        <p className="text-ink-muted leading-relaxed">
          <span className="text-ink-body font-medium">{view.name}</span> is a
          non-Stellar{' '}
          {view.class === 'fiat' ? 'fiat currency' : 'reference asset'} tracked
          by Stellar Index for pricing — it is{' '}
          <span className="font-medium">not issued on Stellar</span>. We index
          it from off-chain venues and reference feeds so on-Stellar pairs (and
          the aggregated VWAP) have a fiat/reference anchor. Stellar-issued
          assets live on{' '}
          <Link
            href="/assets"
            className="text-brand-600 hover:text-brand-700 font-medium"
          >
            /assets
          </Link>
          .
        </p>
      </Panel>

      <p className="text-ink-muted text-sm">
        <Link
          href="/external/assets"
          className="text-brand-600 hover:text-brand-700 font-medium"
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
// on the Stellar detail page's VerifiedCurrencyView: plain-decimal
// significant digits for sub-milli prices (formatSubunitPrice, never
// scientific notation — UXP-26), 2dp for large, 6dp otherwise.
function formatHeadlinePrice(n: number): string {
  if (n < 0.001) return formatSubunitPrice(n);
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
