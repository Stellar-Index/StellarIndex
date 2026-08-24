'use client';

import { useState } from 'react';
import Link from 'next/link';

import { OrderBookPanel } from '../markets/[pair]/OrderBookPanel';
import { Segmented } from '@/components/ui';

/**
 * SdexOrderBookSection — the live SDEX book for a handful of headline
 * classic pairs, with a picker. Reuses the /markets/[pair] OrderBookPanel
 * wholesale (cumulative depth chart + spread strip + level tables over
 * GET /v1/sdex/orderbook), so the two surfaces can never drift.
 *
 * The pair list is a curated set of the deepest classic markets — the
 * book only exists for classic (`native` / `CODE-G...`) assets, so this
 * is a showcase, not a directory; every pair links to its full market
 * page.
 */
const HEADLINE_PAIRS: { label: string; base: string; quote: string }[] = [
  {
    label: 'XLM / USDC',
    base: 'native',
    quote: 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
  },
  {
    label: 'XLM / EURC',
    base: 'native',
    quote: 'EURC-GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2',
  },
  {
    label: 'AQUA / XLM',
    base: 'AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA',
    quote: 'native',
  },
  {
    label: 'yXLM / XLM',
    base: 'yXLM-GARDNV3Q7YGT4AKSDF25LT32YSCCW4EV22Y2TV3I2PU2MMXJTEDL5T55',
    quote: 'native',
  },
];

export function SdexOrderBookSection() {
  const [ix, setIx] = useState(0);
  const pair = HEADLINE_PAIRS[ix];
  const pairSlug = `${pair.base}~${pair.quote}`;

  return (
    <section className="space-y-3">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h2 className="text-lg font-semibold text-ink">Live order book</h2>
        <Link
          href={`/markets/${encodeURIComponent(pairSlug)}/`}
          className="text-sm text-brand-600 hover:underline"
        >
          Full {pair.label} market page →
        </Link>
      </div>
      <Segmented
        ariaLabel="Headline pair"
        options={HEADLINE_PAIRS.map((p, i) => ({ label: p.label, value: String(i) }))}
        value={String(ix)}
        onChange={(v) => setIx(Number(v))}
      />
      <OrderBookPanel base={pair.base} quote={pair.quote} />
    </section>
  );
}
