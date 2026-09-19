'use client';

import { useMemo } from 'react';
import { useQuery } from '@tanstack/react-query';

import { apiGet } from '@/api/client';
import type { components } from '@/api/types';
import { formatRelative } from '@/lib/format';
import { CURRENT_NETWORK } from '@/lib/networks';

type PriceBatchEnvelope = components['schemas']['PriceBatchEnvelope'];
type PriceType = components['schemas']['Price']['price_type'];

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
 *
 * RLT-384 (audit-2026-09-18): this hook used to type the batch response
 * as `{data: Array<{asset_id, price}>}` and return `q.dataUpdatedAt` as
 * its "updated" stamp, which erased the price envelope and told three
 * lies with it. `/v1/price/batch` OMITS a row it will not price (a
 * withheld or never-observed pair), so a `null` rate was
 * indistinguishable from a successful read and the BUILD-baked
 * `initialRate` silently took its place — under `output: 'export'` that
 * number can be weeks old — while `dataUpdatedAt` (the instant the fetch
 * resolved) stamped it "Updated 3s ago". And a `peg` row, which is the
 * operator's standing 1:1 declaration rather than an observation, read
 * as an observed market rate. The envelope is now carried whole:
 * freshness comes from the row's own `observed_at`, the declared
 * `price_type` is reported, and an omitted row is an explicit outcome
 * the UI labels rather than a silent fallback.
 */

/**
 * ConvertRateRead — the outcome of ONE batch read for the converter's
 * pair. Deliberately a discriminated union rather than `number | null`:
 * "the API answered and omitted this pair" and "the API answered with a
 * rate" are different money facts, and a nullable number is exactly the
 * sentinel that let the baked rate be served as the live one.
 */
type ConvertRateRead =
  | {
      outcome: 'priced';
      rate: number;
      observedAt: string | null;
      priceType: PriceType | null;
      stale: boolean;
    }
  | { outcome: 'omitted' };

export interface ConvertRate {
  /** The rate on screen: the live one, or the last published (baked) one. */
  rate: number | null;
  inverse: number | null;
  /**
   * When the displayed rate was OBSERVED (RFC 3339), straight off the
   * row. Null whenever the number on screen is not a live observation —
   * never a fetch/render timestamp standing in for one.
   */
  observedAt: string | null;
  /** How the displayed rate was derived, as the API declares it. */
  priceType: PriceType | null;
  /** The API flagged this read stale (one requested id ⇒ this row). */
  stale: boolean;
  /**
   * The API has spoken and did NOT price this pair, so the number on
   * screen is the last published one rather than the current rate.
   */
  showingLastPublished: boolean;
}

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
}): ConvertRate {
  const q = useQuery<ConvertRateRead>({
    queryKey: ['/v1/price/batch', from, to, 'for-convert'],
    // No aggregator/FX on the lean test nets → /v1/price/batch is empty and the
    // 60s poll would 404-storm; the SSR-baked initialRate (also null there) is
    // used instead. The route is nav-hidden on those nets anyway.
    enabled: CURRENT_NETWORK.pricing,
    queryFn: async () => {
      const env = await apiGet<PriceBatchEnvelope>(
        `/v1/price/batch?asset_ids=${encodeURIComponent(`fiat:${to}`)}&quote=${encodeURIComponent(`fiat:${from}`)}`,
        {},
      );
      const row = (env.data ?? []).find((r) => r.asset_id === `fiat:${to}`);
      // batch(asset_ids=fiat:{to}, quote=fiat:{from}) returns the value
      // of 1 {to} in {from} units; the page displays "1 {from} = ? {to}",
      // the INVERSE. Invert here so the live rate matches the SSR
      // `fromToRate` (page.tsx) rather than silently overwriting the
      // correct baked rate with the wrong direction (audit MONEY-2).
      const price = row?.price != null ? Number(row.price) : 0;
      if (row == null || !(price > 0)) return { outcome: 'omitted' };
      return {
        outcome: 'priced',
        rate: 1 / price,
        // A row without a stamp claims no freshness rather than
        // borrowing one — the whole point of RLT-384.
        observedAt: row.observed_at ?? null,
        priceType: row.price_type ?? null,
        // `flags.stale` is the OR over returned rows; this request asks
        // for exactly one id, so the OR is this row.
        stale: Boolean(env.flags?.stale),
      };
    },
    refetchInterval: 60_000,
    staleTime: 30_000,
  });

  const priced = q.data?.outcome === 'priced' ? q.data : null;
  const rate =
    priced != null && Number.isFinite(priced.rate) ? priced.rate : initialRate;
  const inverse = useMemo(
    () => (rate != null && rate > 0 ? 1 / rate : initialInverse),
    [rate, initialInverse],
  );

  return {
    rate,
    inverse,
    observedAt: priced?.observedAt ?? null,
    priceType: priced?.priceType ?? null,
    stale: priced?.stale ?? false,
    // An in-flight (or disabled) query has not refused anything yet, so
    // the baked paint is not yet a claim about the live rate; a settled
    // one that produced no price is.
    showingLastPublished:
      priced == null && rate != null && (q.isSuccess || q.isError),
  };
}

/**
 * rateBasis — the one line that says what the displayed number actually
 * is: when it was observed, how it was derived, and whether it is a live
 * rate at all. Null when the read has not settled, in which case nothing
 * is claimed. Shared by the header caption and the widget so the two can
 * never disagree about the same number.
 */
export function rateBasis(r: ConvertRate): string | null {
  if (r.showingLastPublished)
    return 'Rate unavailable — showing the last published rate';
  const parts: string[] = [];
  if (r.observedAt != null) {
    parts.push(`Updated ${formatRelative(r.observedAt)}`);
  }
  // Only ever name a basis the row itself declared.
  if (r.priceType != null) parts.push(basisLabel(r.priceType));
  if (r.stale) parts.push('stale');
  return parts.length > 0 ? parts.join(' · ') : null;
}

function basisLabel(t: PriceType): string {
  switch (t) {
    case 'peg':
      return 'declared 1:1 peg, not an observed market rate';
    case 'last_trade':
      return 'last trade';
    case 'twap':
      return 'TWAP';
    default:
      return 'mid-market VWAP';
  }
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
  const read = useConvertRate({ from, to, initialRate, initialInverse });
  const { rate, inverse } = read;
  const basis = rateBasis(read);
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
      {basis != null && (
        <p className="text-ink-muted text-[11px] tracking-wider uppercase">
          {basis}
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
  const read = useConvertRate({ from, to, initialRate, initialInverse });
  const { rate, observedAt, showingLastPublished } = read;
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
      {/* The caption must not call a number "current" when the API
          declined to price the pair and the figure on screen is the
          build-baked one (RLT-384). */}
      <p className="text-ink-muted mt-4 text-xs">
        {showingLastPublished ? (
          <>
            All values calculated at the last published rate of 1 {from} ={' '}
            {formatRate(rate)} {to}. The live rate is unavailable right now, so
            these are not current.
          </>
        ) : (
          <>
            All values calculated at the current mid-market rate of 1 {from} ={' '}
            {formatRate(rate)} {to}
            {observedAt != null && <>, observed {formatRelative(observedAt)}</>}
            . Rates update on each forex-source refresh tick.
          </>
        )}
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
