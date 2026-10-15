'use client';

import { useMemo, useState } from 'react';
import { ArrowLeftRight } from 'lucide-react';

import {
  buildAssetConvertParams,
  buildConvertParams,
  type ConvertAsset,
} from '@/lib/convert-params';
import { formatPairPrice } from '@/lib/format';
import { CurrencyPicker, type CurrencyGroup } from '../../CurrencyPicker';
import { rateBasis, useConvertRate } from './ConvertLive';

export type ConvertPairChoice = { from: string; to: string };

/** The currencies the converter offers: XLM, the verified assets, then fiat. */
export type ConvertCatalogue = {
  fiat: readonly string[];
  assets: readonly ConvertAsset[];
};

function currencyGroups(c: ConvertCatalogue): CurrencyGroup[] {
  return [
    { label: 'Stellar', items: [{ ticker: 'XLM', name: 'Stellar Lumens' }] },
    {
      label: 'Verified Stellar assets',
      items: c.assets.map((a) => ({ ticker: a.ticker, name: a.name })),
    },
    { label: 'Fiat currencies', items: c.fiat.map((t) => ({ ticker: t })) },
  ].filter((g) => g.items.length > 0);
}

/** `/convert/<from>/<to>` when the export bakes that page, else null. */
export function convertPageHref(
  pair: ConvertPairChoice,
  catalogue: ConvertCatalogue | undefined,
): string | null {
  if (!catalogue) return null;
  const fiat = [...catalogue.fiat];
  const baked = [
    ...buildConvertParams(fiat),
    ...buildAssetConvertParams(catalogue.assets, fiat),
  ];
  const from = pair.from.toUpperCase();
  const to = pair.to.toUpperCase();
  if (!baked.some((p) => p.from === from && p.to === to)) return null;
  return `/convert/${from}/${to}`;
}

function Ticker({ children }: { children: string }) {
  return (
    <span className="bg-surface-subtle text-ink-body rounded-sm px-1.5 py-0.5 font-mono text-xs">
      {children}
    </span>
  );
}

/**
 * ConvertPair — the interactive converter: an amount, the two currencies
 * (searchable pickers when a catalogue is given) and the live result.
 *
 * Picking a currency or swapping reports the new pair through `onPick`;
 * without it the widget navigates to that pair's page, or to the landing
 * with the pair in its query when no page is baked for it.
 *
 * The live rate comes from the shared `useConvertRate` hook so this
 * widget, the header, and the ladder all read ONE deduped
 * `/v1/price/batch` query (refreshed every 60s). SSR-rendered initial
 * value comes from `initialRate` so the first paint is correct without
 * a client roundtrip.
 */
export function ConvertPair({
  from,
  to,
  initialRate,
  initialInverse,
  catalogue,
  onPick,
}: {
  from: string;
  to: string;
  initialRate: number | null;
  initialInverse: number | null;
  catalogue?: ConvertCatalogue;
  onPick?: (pair: ConvertPairChoice) => void;
}) {
  const [amount, setAmount] = useState('1');

  // The freshness stamp below is the ROW's `observed_at`, never the
  // instant the fetch resolved: a query that resolves to "this pair is
  // not priced" is a SUCCESS, so `dataUpdatedAt` would stamp a
  // build-baked rate "Updated 3s ago".
  const read = useConvertRate({ from, to, initialRate, initialInverse });
  const { rate } = read;
  const basis = rateBasis(read);
  const groups = useMemo(
    () => (catalogue ? currencyGroups(catalogue) : null),
    [catalogue],
  );

  const numeric = Number(amount);
  const result =
    Number.isFinite(numeric) && rate != null ? numeric * rate : null;

  const go = (pair: ConvertPairChoice) => {
    if (pair.from === pair.to) return;
    if (onPick) {
      onPick(pair);
      return;
    }
    window.location.assign(
      convertPageHref(pair, catalogue) ??
        `/convert/?${new URLSearchParams(pair)}`,
    );
  };

  return (
    <section className="rounded-card border-line bg-surface border p-4">
      <h2 className="text-h3 text-ink mb-4 font-semibold">
        Convert {from} to {to}
      </h2>
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-[1fr_auto_1fr]">
        <div className="space-y-1">
          <span className="text-ink-muted block text-xs">From</span>
          <div className="border-line bg-surface focus-within:border-brand-500 flex items-center gap-2 rounded-md border p-2">
            <input
              type="number"
              value={amount}
              onChange={(e) => setAmount(e.target.value)}
              min="0"
              step="any"
              inputMode="decimal"
              className="w-full min-w-0 bg-transparent font-mono text-2xl tabular-nums focus:outline-hidden"
              aria-label={`Amount in ${from}`}
            />
            {groups ? (
              <CurrencyPicker
                label="From"
                value={from}
                groups={groups}
                onPick={(t) => go({ from: t, to: t === to ? from : to })}
              />
            ) : (
              <Ticker>{from}</Ticker>
            )}
          </div>
        </div>

        <button
          type="button"
          onClick={() => go({ from: to, to: from })}
          className="border-line bg-surface text-ink-muted hover:border-line-strong hover:text-ink self-end rounded-md border p-2"
          aria-label="Swap currencies"
          title="Swap currencies"
        >
          <ArrowLeftRight className="h-4 w-4" />
        </button>

        <div className="space-y-1">
          <span className="text-ink-muted block text-xs">To</span>
          <div className="border-line bg-surface flex items-center gap-2 rounded-md border p-2">
            <span className="text-ink w-full font-mono text-2xl tabular-nums">
              {result != null ? formatPairPrice(result) : '—'}
            </span>
            {groups ? (
              <CurrencyPicker
                label="To"
                value={to}
                groups={groups}
                onPick={(t) => go({ from: t === from ? to : from, to: t })}
              />
            ) : (
              <Ticker>{to}</Ticker>
            )}
          </div>
        </div>
      </div>
      <p className="text-ink-muted mt-3 text-xs">
        {rate != null ? (
          <>
            1 {from} ={' '}
            <span className="font-mono tabular-nums">
              {formatPairPrice(rate)}
            </span>{' '}
            {to}
            {basis != null && (
              <>
                <span className="mx-1.5">·</span>
                {basis}
              </>
            )}
          </>
        ) : (
          'Rate currently unavailable.'
        )}
      </p>
    </section>
  );
}
