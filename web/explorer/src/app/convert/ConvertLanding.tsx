'use client';

import { useMemo, useState, useSyncExternalStore } from 'react';
import Link from 'next/link';
import { ArrowLeftRight } from 'lucide-react';

import { Callout, PageHeader, Select } from '@/components/ui';
import {
  buildAssetConvertParams,
  buildConvertParams,
  type ConvertAsset,
} from '@/lib/convert-params';
import { ConvertPair } from './[from]/[to]/ConvertPair';
import { ConvertChart } from './[from]/[to]/ConvertChart';
import {
  ConvertAssetIdsProvider,
  ConvertLiveRate,
  ConvertSnippets,
} from './[from]/[to]/ConvertLive';

type Pair = { from: string; to: string };

// The query string only changes on a full navigation here.
const subscribe = () => () => {};

/**
 * The pair the /convert Function redirected with (?from=&to=): `pair` when
 * the picker offers both sides, else `unavailable` when one was asked for.
 * The Function upper-cases tickers, so the match ignores case.
 */
export function requestedPair(
  search: string,
  options: readonly string[],
): { pair: Pair | null; unavailable: boolean } {
  const q = new URLSearchParams(search);
  if (!q.get('from') && !q.get('to')) return { pair: null, unavailable: false };
  const find = (t: string | null) =>
    t == null
      ? undefined
      : options.find((o) => o.toUpperCase() === t.toUpperCase());
  const from = find(q.get('from'));
  const to = find(q.get('to'));
  if (from && to && from !== to) {
    return { pair: { from, to }, unavailable: false };
  }
  return { pair: null, unavailable: true };
}

export function ConvertLanding({
  tickers,
  assets = [],
}: {
  tickers: string[];
  assets?: ConvertAsset[];
}) {
  const assetTickers = useMemo(() => assets.map((a) => a.ticker), [assets]);
  const options = useMemo(
    () => ['XLM', ...assetTickers, ...tickers],
    [assetTickers, tickers],
  );
  const ids = useMemo(
    () => Object.fromEntries(assets.map((a) => [a.ticker, a.assetId])),
    [assets],
  );
  const search = useSyncExternalStore(
    subscribe,
    () => window.location.search,
    () => '',
  );
  const requested = useMemo(
    () => requestedPair(search, options),
    [search, options],
  );
  const [picked, setPicked] = useState<Pair | null>(null);
  const { from, to } = picked ??
    requested.pair ?? {
      from: 'XLM',
      to: tickers.includes('USD') ? 'USD' : tickers[0],
    };
  const unavailable = picked == null && requested.unavailable;
  // The link's href comes from the built page list, never from the select value.
  const pairPages = useMemo(
    () =>
      new Map(
        [
          ...buildConvertParams(tickers),
          ...buildAssetConvertParams(assets, tickers),
        ].map((p) => [`${p.from}/${p.to}`, p]),
      ),
    [tickers, assets],
  );
  const pagePair = pairPages.get(`${from.toUpperCase()}/${to.toUpperCase()}`);

  const pickFrom = (next: string) =>
    setPicked({
      from: next,
      to: next === to ? (tickers.find((t) => t !== next) ?? to) : to,
    });
  const pickTo = (next: string) =>
    setPicked({
      from: next === from ? (tickers.find((t) => t !== next) ?? 'XLM') : from,
      to: next,
    });
  const swap = () => setPicked({ from: to, to: from });

  const optionList = (
    <>
      <option value="XLM">XLM</option>
      {assetTickers.length > 0 && (
        <optgroup label="Verified Stellar assets">
          {assetTickers.map((t) => (
            <option key={t} value={t}>
              {t}
            </option>
          ))}
        </optgroup>
      )}
      <optgroup label="Fiat currencies">
        {tickers.map((t) => (
          <option key={t} value={t}>
            {t}
          </option>
        ))}
      </optgroup>
    </>
  );

  return (
    <ConvertAssetIdsProvider ids={ids}>
      <header className="border-line space-y-4 border-b pb-5">
        <PageHeader
          title="Convert"
          breadcrumbs={[{ label: 'Home', href: '/' }, { label: 'Convert' }]}
        />
        {unavailable && (
          <Callout tone="info" title="That pair isn't available to convert">
            The converter covers XLM, verified Stellar assets and fiat
            currencies. Pick a pair below.
          </Callout>
        )}
        <div className="flex flex-wrap items-end gap-3">
          <label className="text-ink-muted space-y-1 text-xs tracking-wider uppercase">
            <span className="block">From</span>
            <Select
              aria-label="From"
              value={from}
              onChange={(e) => pickFrom(e.target.value)}
            >
              {optionList}
            </Select>
          </label>
          <button
            type="button"
            onClick={swap}
            aria-label="Swap currencies"
            title="Swap"
            className="border-line text-ink-body hover:border-brand-500 hover:text-brand-600 inline-flex h-9 items-center rounded-md border px-3 disabled:opacity-40"
          >
            <ArrowLeftRight className="h-4 w-4" />
          </button>
          <label className="text-ink-muted space-y-1 text-xs tracking-wider uppercase">
            <span className="block">To</span>
            <Select
              aria-label="To"
              value={to}
              onChange={(e) => pickTo(e.target.value)}
            >
              {optionList}
            </Select>
          </label>
          {pagePair && (
            <Link
              href={`/convert/${pagePair.from}/${pagePair.to}`}
              className="text-brand-600 h-9 text-sm leading-9 hover:underline"
            >
              {from} → {to} page
            </Link>
          )}
        </div>
        {/* key: a new pair must not paint the previous pair's rate. */}
        <ConvertLiveRate
          key={`${from}/${to}`}
          from={from}
          to={to}
          initialRate={null}
          initialInverse={null}
        />
      </header>

      <ConvertPair
        key={`pair-${from}/${to}`}
        from={from}
        to={to}
        initialRate={null}
        initialInverse={null}
      />
      <ConvertChart key={`chart-${from}/${to}`} from={from} to={to} />
      <ConvertSnippets
        key={`snip-${from}/${to}`}
        from={from}
        to={to}
        initialRate={null}
        initialInverse={null}
      />
    </ConvertAssetIdsProvider>
  );
}
