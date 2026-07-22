'use client';

import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import { Breadcrumbs, EmptyState, Skeleton } from '@/components/ui';

import { useLastPathSegment } from '@/lib/useLastPathSegment';
import { shortAssetText } from '@/components/AssetLink';
import type { Envelope } from '@/app/explorer-shared';

// Pair slug = `${base}~${quote}`. Kept local rather than imported from
// page.tsx, which is a server component — this file is 'use client'.
const PAIR_SEPARATOR = '~';

function decodePairSlug(slug: string): { base: string; quote: string } | null {
  const ix = slug.indexOf(PAIR_SEPARATOR);
  if (ix === -1) return null;
  const base = slug.slice(0, ix);
  const quote = slug.slice(ix + 1);
  if (!base || !quote) return null;
  return { base, quote };
}

interface PairPrice {
  price?: string | null;
  price_type?: string;
  observed_at?: string;
  window_seconds?: number;
}

/**
 * PairPathView — the runtime fallback for market pairs outside the
 * build-time pre-render (site-audit S1/S1b/S7).
 *
 * /markets/[pair] pre-renders the top 500 pairs by 24h USD volume at BUILD
 * time. Markets churn between deploys, so any pair that enters the ranking
 * afterwards hard-404'd — including pairs the /markets listing itself was
 * linking to (row 1 of its own table) and 2 of the 8 rows in /network's
 * "Top Stellar markets" widget, which ranks a different population
 * entirely (/v1/pools, on-chain only).
 *
 * This is deliberately NOT a bigger pre-render limit: the 404ing pairs
 * ranked 27, 51 and 100 in live data, well inside the existing 500. The
 * snapshot was stale, not small — which is why raising 100 -> 500 in the
 * 2026-05-08 audit did not hold.
 *
 * Served by functions/markets/[[path]].js; noindex, like the other
 * long-tail shells.
 */
export function PairPathView() {
  const raw = useLastPathSegment() ?? '';
  const decoded = decodePairSlug(decodeURIComponent(raw));

  const base = decoded?.base ?? '';
  const quote = decoded?.quote ?? '';
  const valid = Boolean(base && quote);

  const { data, isLoading, isError } = useQuery<PairPrice | null>({
    queryKey: ['/v1/price', base, quote],
    enabled: valid,
    retry: false,
    staleTime: 60_000,
    queryFn: async () =>
      (
        await apiGet<Envelope<PairPrice>>(
          `/v1/price?base=${encodeURIComponent(base)}&quote=${encodeURIComponent(quote)}`,
        )
      ).data,
  });

  const baseLabel = valid ? shortAssetText(base) : '';
  const quoteLabel = valid ? shortAssetText(quote) : '';
  const title = valid ? `${baseLabel} / ${quoteLabel}` : 'Pair';

  return (
    <div className="mx-auto max-w-7xl space-y-6 px-6 py-8">
      <header className="space-y-1">
        <Breadcrumbs
          items={[
            { label: 'Home', href: '/' },
            { label: 'Markets', href: '/markets' },
            { label: title },
          ]}
        />
        <h1 className="text-h1 font-semibold text-ink">{title}</h1>
        {valid && (
          <p className="text-sm text-ink-muted">
            Live pair detail, loaded from the API. This pair is outside the
            pre-rendered set — the data below is current.
          </p>
        )}
      </header>

      {!valid && (
        <EmptyState
          title="Unrecognised pair"
          description="A market URL looks like /markets/<base>~<quote>."
        />
      )}

      {valid && (
        <Panel
          title="Price"
          hint="Volume-weighted across every venue we index"
          source={asExample('/v1/price', { base, quote })}
        >
          {isLoading && <Skeleton className="h-20 w-full" />}
          {isError && (
            <p className="text-sm text-ink-muted">
              Price is unavailable for this pair right now.
            </p>
          )}
          {!isLoading && !isError && !data?.price && (
            <EmptyState
              title="No price for this pair"
              description="We have not observed a trade on this market recently."
            />
          )}
          {!isLoading && !isError && data?.price && (
            <div className="space-y-1">
              <div className="font-mono text-h2 text-ink">{data.price}</div>
              <p className="text-sm text-ink-muted">
                {quoteLabel} per {baseLabel}
                {data.observed_at ? ` · observed ${data.observed_at}` : ''}
              </p>
            </div>
          )}
        </Panel>
      )}

      {valid && (
        <p className="text-sm text-ink-muted">
          Looking for the full breakdown?{' '}
          <Link href="/markets" className="text-brand-600 hover:underline">
            Browse all markets
          </Link>
          .
        </p>
      )}
    </div>
  );
}
