'use client';

import Link from 'next/link';
import { ArrowRight } from 'lucide-react';
import { useQuery } from '@tanstack/react-query';

import { apiGet } from '@/api/client';
import type { components } from '@/api/types';
import { assetHrefFor } from '@/lib/fiat-slugs';
import { formatRelative } from '@/lib/format';

type PriceBatchEnvelope = components['schemas']['PriceBatchEnvelope'];
type PriceType = components['schemas']['Price']['price_type'];

interface CurrencyRow {
  ticker: string;
  name: string;
  rate_usd: number;
  change_24h_pct?: number;
  /** How the API says this rate was derived — `peg` is a declaration. */
  price_type: PriceType | null;
  /** When the rate was OBSERVED (RFC 3339), off the row itself. */
  observed_at: string | null;
}

interface CurrencyStrip {
  rows: Record<string, CurrencyRow>;
  /**
   * The OLDEST `observed_at` across the rates on screen: a set of rates
   * is only as fresh as its stalest member, and the strip stamps them
   * all with one line.
   */
  observed_at: string | null;
  /** The envelope's `flags.stale` — the OR over the returned rows. */
  stale: boolean;
}

// Names hardcoded — fiat ISO 4217 ticker → English name is stable
// and the 6-tile home strip doesn't justify an extra round-trip
// to /v1/assets/verified for the human-readable label.
const FEATURED: Array<{ ticker: string; name: string }> = [
  { ticker: 'EUR', name: 'Euro' },
  { ticker: 'GBP', name: 'British Pound' },
  { ticker: 'JPY', name: 'Japanese Yen' },
  { ticker: 'CHF', name: 'Swiss Franc' },
  { ticker: 'CAD', name: 'Canadian Dollar' },
  { ticker: 'AUD', name: 'Australian Dollar' },
];

/**
 * HomeCurrencies — strip of major-currency cards on the home page.
 *
 * Migration history (F-1201 audit-2026-05-12): pre-rc.48 this
 * called /v1/currencies (the single bulk endpoint that returned
 * every catalogue currency's ticker + name + rate_usd +
 * change_24h_pct). rc.48 removed that route as part of the
 * /v1/coins + /v1/currencies → /v1/assets consolidation. The
 * home strip now uses /v1/price/batch to get the 6 featured rates
 * in one round-trip against fiat:USD; names are hardcoded above.
 *
 * RLT-384 (audit-2026-09-18): this read used to type the response as
 * `{data: Array<{asset_id, price}>}`, discarding `price_type`,
 * `observed_at` and `flags` — so the strip called itself "Live" over
 * rates the API had flagged stale (measured 2026-09-19: the FX rows
 * carried `observed_at` ~36h old with `flags.stale: true`) and would
 * have shown a declared peg as an observed rate. The envelope is now
 * carried through: the strip stamps itself with the oldest `observed_at`
 * it is showing, repeats the API's stale flag, and names a non-market
 * basis on the tile that has one. `change_24h_pct` rides the same rows
 * and now feeds the tile's change chip, which previously had no
 * producer at all; the batch only emits it for a fiat:USD quote and not
 * for every asset, so a tile without one simply shows no chip.
 */
export function HomeCurrencies() {
  const q = useQuery<CurrencyStrip>({
    queryKey: ['/v1/price/batch', 'home-currencies'],
    queryFn: async () => {
      const assetIds = FEATURED.map((f) => `fiat:${f.ticker}`).join(',');
      const env = await apiGet<PriceBatchEnvelope>(
        `/v1/price/batch?asset_ids=${encodeURIComponent(assetIds)}&quote=fiat:USD`,
        {},
      );
      const rows: Record<string, CurrencyRow> = {};
      // Compared as instants, never as strings: RFC 3339 stamps come back
      // with variable fractional precision ("…00Z" vs "…00.5Z"), and
      // lexicographic order gets that pair backwards.
      let oldest: string | null = null;
      let oldestMs = Number.POSITIVE_INFINITY;
      // /v1/price/batch returns "price of asset in quote", i.e.
      // 1 EUR = X USD. That's exactly the rate_usd field shape
      // the home strip already displays — no inversion needed.
      for (const row of env.data ?? []) {
        const ticker = row.asset_id.replace(/^fiat:/, '');
        const featured = FEATURED.find((f) => f.ticker === ticker);
        if (!featured || !row.price) continue;
        const rate = Number(row.price);
        if (!(rate > 0)) continue;
        const change =
          row.change_24h_pct != null ? Number(row.change_24h_pct) : NaN;
        rows[ticker] = {
          ticker,
          name: featured.name,
          rate_usd: rate,
          ...(Number.isFinite(change) ? { change_24h_pct: change } : {}),
          price_type: row.price_type ?? null,
          observed_at: row.observed_at ?? null,
        };
        const ms = row.observed_at != null ? Date.parse(row.observed_at) : NaN;
        if (Number.isFinite(ms) && ms < oldestMs) {
          oldestMs = ms;
          oldest = row.observed_at;
        }
      }
      return { rows, observed_at: oldest, stale: Boolean(env.flags?.stale) };
    },
    refetchInterval: 5 * 60_000,
  });
  const strip = q.data;

  return (
    <section className="space-y-3">
      <div className="flex items-baseline justify-between">
        <div className="space-y-1">
          <h2 className="text-2xl font-semibold tracking-tight">
            World currencies
          </h2>
          <p className="text-ink-body text-sm">
            USD-base rates for the major fiat currencies — the full reference
            set (19 fiat plus 15 reference coins) at{' '}
            <Link
              href="/external/assets"
              className="text-brand-600 hover:underline"
            >
              /external/assets
            </Link>
            .
          </p>
          {/* One honest freshness line for the whole strip, off the
              oldest row's own observed_at — never a render or fetch
              clock (RLT-384). */}
          {strip?.observed_at != null && (
            <p className="text-ink-muted text-xs">
              Rates observed {formatRelative(strip.observed_at)}
              {strip.stale && ' · flagged stale by the pricing API'}
            </p>
          )}
        </div>
        <Link
          href="/assets"
          className="text-brand-600 inline-flex items-center gap-1 text-xs hover:underline"
        >
          All assets <ArrowRight className="h-3 w-3" />
        </Link>
      </div>
      {q.isError && (
        <div className="border-warn-300 bg-warn-50 text-warn-700 rounded-md border px-4 py-3 text-sm">
          Couldn&apos;t load live currency rates. The full directory at{' '}
          <Link href="/assets" className="underline hover:no-underline">
            /assets
          </Link>{' '}
          may have more luck — or check{' '}
          <a
            href="/status"
            target="_blank"
            rel="noopener noreferrer"
            className="underline hover:no-underline"
          >
            the status page
          </a>{' '}
          for ongoing incidents.
        </div>
      )}
      <div className="grid grid-cols-2 gap-2 sm:grid-cols-3 lg:grid-cols-6">
        {FEATURED.map(({ ticker: t }) => {
          const row = strip?.rows[t];
          return (
            <Link
              key={t}
              href={assetHrefFor(t)}
              className="border-line bg-surface hover:border-brand-500 rounded-xl border p-3 transition-colors"
            >
              <div className="flex items-center justify-between">
                <span className="font-mono text-sm font-medium">{t}</span>
                <span className="text-ink-muted text-[10px] tracking-wider uppercase">
                  vs USD
                </span>
              </div>
              <div className="text-ink mt-2 font-mono text-lg tabular-nums">
                {row && row.rate_usd > 0 ? formatRate(row.rate_usd) : '—'}
              </div>
              {/* A declared peg is the operator's 1:1 statement, not a
                  rate anyone observed — say so on the tile rather than
                  letting it read as an FX quote (RLT-384). */}
              {row?.price_type === 'peg' && (
                <div
                  className="text-ink-muted text-[10px] tracking-wider uppercase"
                  title="Declared 1:1 peg — not an observed market rate"
                >
                  declared peg
                </div>
              )}
              {row && (
                <div className="flex items-baseline justify-between gap-2">
                  <span
                    className="text-ink-muted line-clamp-1 text-[11px]"
                    title={row.name}
                  >
                    {row.name}
                  </span>
                  {row.change_24h_pct != null &&
                    Number.isFinite(row.change_24h_pct) && (
                      <span
                        className={`font-mono text-[11px] tabular-nums ${
                          row.change_24h_pct > 0
                            ? 'text-up'
                            : row.change_24h_pct < 0
                              ? 'text-down'
                              : 'text-ink-muted'
                        }`}
                        title="Trailing-24h % change in USD value, as served beside the rate"
                      >
                        {row.change_24h_pct > 0 ? '+' : ''}
                        {row.change_24h_pct.toFixed(2)}%
                      </span>
                    )}
                </div>
              )}
            </Link>
          );
        })}
      </div>
    </section>
  );
}

function formatRate(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '—';
  if (n >= 1000) return n.toLocaleString('en-US', { maximumFractionDigits: 2 });
  if (n >= 1) return n.toFixed(4);
  return n.toFixed(6);
}
