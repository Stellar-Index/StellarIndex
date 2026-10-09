'use client';

import { useMemo, useState } from 'react';
import Link from 'next/link';
import { ArrowLeftRight } from 'lucide-react';

import { Select } from '@/components/ui';
import { buildConvertParams } from '@/lib/convert-params';
import { ConvertPair } from './[from]/[to]/ConvertPair';
import { ConvertChart } from './[from]/[to]/ConvertChart';
import { ConvertLiveRate, ConvertSnippets } from './[from]/[to]/ConvertLive';

/** Fiat is only ever priced as the quote, so XLM can be "from" but not "to". */
export function ConvertLanding({ tickers }: { tickers: string[] }) {
  const [from, setFrom] = useState('XLM');
  const [to, setTo] = useState(tickers.includes('USD') ? 'USD' : tickers[0]);
  // The link's href comes from the built page list, never from the select value.
  const pairPages = useMemo(
    () =>
      new Map(
        buildConvertParams(tickers).map((p) => [
          `${p.from}/${p.to}`,
          `/convert/${p.from}/${p.to}`,
        ]),
      ),
    [tickers],
  );
  const pairHref = pairPages.get(`${from}/${to}`);

  const pickFrom = (next: string) => {
    setFrom(next);
    if (next === to) setTo(tickers.find((t) => t !== next) ?? to);
  };
  const pickTo = (next: string) => {
    setTo(next);
    if (next === from) setFrom(tickers.find((t) => t !== next) ?? 'XLM');
  };
  const swap = () => {
    setFrom(to);
    setTo(from);
  };

  return (
    <>
      <header className="border-line space-y-4 border-b pb-5">
        <h1 className="text-3xl font-semibold tracking-tight">Convert</h1>
        <div className="flex flex-wrap items-end gap-3">
          <label className="text-ink-muted space-y-1 text-xs tracking-wider uppercase">
            <span className="block">From</span>
            <Select
              aria-label="From"
              value={from}
              onChange={(e) => pickFrom(e.target.value)}
            >
              {['XLM', ...tickers].map((t) => (
                <option key={t} value={t}>
                  {t}
                </option>
              ))}
            </Select>
          </label>
          <button
            type="button"
            onClick={swap}
            disabled={from === 'XLM'}
            aria-label="Swap currencies"
            title={from === 'XLM' ? 'Fiat to XLM is not priced' : 'Swap'}
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
              {tickers.map((t) => (
                <option key={t} value={t}>
                  {t}
                </option>
              ))}
            </Select>
          </label>
          {pairHref && (
            <Link
              href={pairHref}
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
    </>
  );
}
