'use client';

import { useMemo } from 'react';
import { useQuery } from '@tanstack/react-query';

import { apiGet } from '@/api/client';
import { CURRENT_NETWORK } from '@/lib/networks';

// Common amounts to render as static "X = Y" snippets for SEO body
// content. Picks naturally-queried ladder values across orders of
// magnitude — Google search-volume tools show "1 X to Y", "100 X
// to Y", "1000 X to Y" all rank as distinct queries with non-trivial
// volume even for the same currency pair.
const SNIPPET_AMOUNTS = [1, 10, 100, 1000, 10000];

/**
 * useConvertRate — the single live rate source for the whole
 * `/convert/[from]/[to]` page. The interactive widget (ConvertPair),
 * the header headline + inverse, and the common-amounts ladder all
 * call this hook with the SAME query key, so TanStack Query dedupes
 * them into ONE `/v1/price/batch` fetch and ONE shared 60s refresh
 * loop.
 *
 * W8 recon 10a: before this hook only ConvertPair re-fetched. Under
 * static export (`output: 'export'`) the header rate, inverse rate,
 * and ladder stayed frozen at the build-baked value while the copy
 * claimed "current mid-market rate … updates on each refresh tick" —
 * a static page asserting "current" over a stale number. Sharing this
 * hook makes every "current"-labelled element actually current: the
 * baked value paints first (fast first render + a number for crawlers),
 * then the client re-fetches on mount and every 60s (the RT-2
 * live-hydration pattern used by LiveAssetPrice / LivePairPrice).
 *
 * F-1201 migration (audit-2026-05-12): pre-rc.48 `/v1/currencies/{from}`
 * carried a cross_rates map for every currency in one RT; rc.48 removed
 * the route. We now hit `/v1/price/batch?asset_ids=fiat:{to}&quote=
 * fiat:{from}`, which returns the single-pair rate the converter uses.
 */
export function useConvertRate({
  from,
  to,
  initialRate,
  initialInverse,
}: {
  from: string;
  to: string;
  initialRate: number | null;
  initialInverse: number | null;
}): { rate: number | null; inverse: number | null; updatedAt: number } {
  const q = useQuery<number | null>({
    queryKey: ['/v1/price/batch', from, to, 'for-convert'],
    // No aggregator/FX on the lean test nets → /v1/price/batch is empty and the
    // 60s poll would 404-storm; the SSR-baked initialRate (also null there) is
    // used instead. The route is nav-hidden on those nets anyway.
    enabled: CURRENT_NETWORK.pricing,
    queryFn: async () => {
      const env = await apiGet<{
        data: Array<{ asset_id: string; price: string | null }>;
      }>(
        `/v1/price/batch?asset_ids=${encodeURIComponent(`fiat:${to}`)}&quote=${encodeURIComponent(`fiat:${from}`)}`,
        {},
      );
      const row = (env.data ?? []).find((r) => r.asset_id === `fiat:${to}`);
      // batch(asset_ids=fiat:{to}, quote=fiat:{from}) returns the value
      // of 1 {to} in {from} units; the page displays "1 {from} = ? {to}",
      // the INVERSE. Invert here so the live rate matches the SSR
      // `fromToRate` (page.tsx) rather than silently overwriting the
      // correct baked rate with the wrong direction (audit MONEY-2).
      const price = row?.price ? Number(row.price) : 0;
      return price > 0 ? 1 / price : null;
    },
    refetchInterval: 60_000,
    staleTime: 30_000,
  });

  const liveRate = q.data;
  const rate =
    liveRate != null && Number.isFinite(liveRate) ? liveRate : initialRate;
  const inverse = useMemo(
    () => (rate != null && rate > 0 ? 1 / rate : initialInverse),
    [rate, initialInverse],
  );

  return { rate, inverse, updatedAt: q.dataUpdatedAt };
}

/**
 * ConvertLiveRate — the header headline rate + its inverse, hydrated
 * LIVE off the shared query. Baked `initialRate`/`initialInverse` paint
 * first; the client swaps in the live rate on mount + every 60s.
 */
export function ConvertLiveRate({
  from,
  to,
  initialRate,
  initialInverse,
}: {
  from: string;
  to: string;
  initialRate: number | null;
  initialInverse: number | null;
}) {
  const { rate, inverse } = useConvertRate({
    from,
    to,
    initialRate,
    initialInverse,
  });
  return (
    <>
      {rate != null ? (
        <p className="text-ink font-mono text-2xl tabular-nums">
          1 {from} = {formatRate(rate)} {to}
        </p>
      ) : (
        <p className="text-ink-muted text-sm">Rate currently unavailable.</p>
      )}
      {inverse != null && (
        <p className="text-ink-body font-mono text-sm tabular-nums">
          1 {to} = {formatRate(inverse)} {from}
        </p>
      )}
    </>
  );
}

/**
 * ConvertSnippets — the "common amounts" SEO ladder, hydrated LIVE off
 * the shared query so every "= Y" value and the "current mid-market
 * rate" caption track the same live rate the widget shows. Baked
 * `initialRate` paints first.
 */
export function ConvertSnippets({
  from,
  to,
  initialRate,
  initialInverse,
}: {
  from: string;
  to: string;
  initialRate: number | null;
  initialInverse: number | null;
}) {
  const { rate } = useConvertRate({ from, to, initialRate, initialInverse });
  if (rate == null) return null;
  return (
    <section className="rounded-card border-line bg-surface border p-5">
      <h2 className="mb-4 text-lg font-semibold tracking-tight">
        {from} to {to} at common amounts
      </h2>
      <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
        {SNIPPET_AMOUNTS.map((amt) => (
          <div
            key={amt}
            className="bg-surface-muted flex items-baseline justify-between rounded-md px-3 py-2"
          >
            <span className="text-ink-body font-mono tabular-nums">
              {amt.toLocaleString('en-US')} {from}
            </span>
            <span className="text-ink font-mono font-medium tabular-nums">
              {formatRate(amt * rate)} {to}
            </span>
          </div>
        ))}
      </div>
      <p className="text-ink-muted mt-4 text-xs">
        All values calculated at the current mid-market rate of 1 {from} ={' '}
        {formatRate(rate)} {to}. Rates update on each forex-source refresh tick.
      </p>
    </section>
  );
}

function formatRate(n: number): string {
  if (!Number.isFinite(n)) return '—';
  if (Math.abs(n) >= 1000)
    return n.toLocaleString('en-US', { maximumFractionDigits: 2 });
  if (Math.abs(n) >= 1) return n.toFixed(4);
  if (Math.abs(n) >= 0.01) return n.toFixed(6);
  return n.toFixed(8);
}
