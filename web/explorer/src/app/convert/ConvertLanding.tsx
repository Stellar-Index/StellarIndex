'use client';

import { useMemo, useState, useSyncExternalStore } from 'react';

import { ButtonLink, Callout, PageHeader } from '@/components/ui';
import { type ConvertAsset } from '@/lib/convert-params';
import { ConvertPair, convertPageHref } from './[from]/[to]/ConvertPair';
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
  const options = useMemo(
    () => ['XLM', ...assets.map((a) => a.ticker), ...tickers],
    [assets, tickers],
  );
  const ids = useMemo(
    () => Object.fromEntries(assets.map((a) => [a.ticker, a.assetId])),
    [assets],
  );
  const catalogue = useMemo(
    () => ({ fiat: tickers, assets }),
    [tickers, assets],
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
  // The link's href comes from the built page list, never from the picker value.
  const pageHref = convertPageHref({ from, to }, catalogue);

  return (
    <ConvertAssetIdsProvider ids={ids}>
      <header className="border-line space-y-4 border-b pb-5">
        <PageHeader
          title="Convert"
          breadcrumbs={[{ label: 'Home', href: '/' }, { label: 'Convert' }]}
          actions={
            pageHref && (
              <ButtonLink href={pageHref} variant="secondary" size="sm">
                {from} → {to} page
              </ButtonLink>
            )
          }
        />
        {unavailable && (
          <Callout tone="info" title="That pair isn't available to convert">
            The converter covers XLM, verified Stellar assets and fiat
            currencies. Pick a pair below.
          </Callout>
        )}
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
        catalogue={catalogue}
        onPick={setPicked}
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
