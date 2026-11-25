import { afterEach, describe, expect, it, vi } from 'vitest';

import { formatPairPrice } from '@/lib/format';

// 20 hubs (convert-params HUB_TICKERS) + 110 long-tail tickers: a hub quote's
// universe is 19 + 110 + 2 assets = 131 ids, two batch chunks.
const HUBS = [
  'USD', 'EUR', 'GBP', 'JPY', 'CHF', 'CAD', 'AUD', 'CNY', 'INR', 'BRL',
  'MXN', 'ZAR', 'NZD', 'SGD', 'HKD', 'SEK', 'NOK', 'KRW', 'TRY', 'PLN',
]; // prettier-ignore
const LETTERS = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ';
const LONG_TAIL = Array.from(
  { length: 110 },
  (_, i) => `Q${LETTERS[Math.floor(i / 26)]}${LETTERS[i % 26]}`,
);
const ISSUER = `G${'A'.repeat(55)}`;
const ASSETS = ['AQUA', 'YUSDC'].map((ticker) => ({
  ticker,
  asset_id: `${ticker}-${ISSUER}`,
  issuer: ISSUER,
  class: 'crypto',
}));
// The API withholds this one pair; the page must still render "no rate".
const WITHHELD = { id: 'fiat:QAA', quote: 'fiat:USD' };

function priceFor(id: string, quote: string): string {
  let h = 0;
  for (const c of `${id}|${quote}`) h = (h * 31 + c.charCodeAt(0)) % 99_991;
  return `${(h % 9_000) + 1}.25`;
}

function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), { status });
}

function stubApi(batchUrls: string[]) {
  const verified = [
    ...[...HUBS, ...LONG_TAIL].map((ticker) => ({ ticker, class: 'fiat' })),
    ...ASSETS,
  ];
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: string | URL) => {
      const url = new URL(String(input));
      if (url.pathname.endsWith('/v1/assets/verified')) {
        return json({ data: verified });
      }
      const ext = url.pathname.match(/\/v1\/external\/assets\/(\w+)$/);
      if (ext) {
        return json({ data: { ticker: ext[1], name: ext[1], price_usd: '1' } });
      }
      if (url.pathname.endsWith('/v1/price/batch')) {
        batchUrls.push(url.href);
        const quote = url.searchParams.get('quote')!;
        const ids = url.searchParams.get('asset_ids')!.split(',');
        return json({
          data: ids
            .filter((id) => !(id === WITHHELD.id && quote === WITHHELD.quote))
            .map((id) => ({ asset_id: id, quote, price: priceFor(id, quote) })),
          stale: [],
          triangulated: [],
        });
      }
      throw new Error(`unexpected fetch: ${url.href}`);
    }),
  );
}

describe('/convert price batching', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.resetModules();
  });

  it('prices every page off one batch per quote chunk, with the per-id values', async () => {
    const batchUrls: string[] = [];
    stubApi(batchUrls);
    const page = await import('./page');
    const params = await page.generateStaticParams();
    const assetIds = new Map(ASSETS.map((a) => [a.ticker, a.asset_id]));
    for (const { from, to } of params) {
      const meta = await page.generateMetadata({
        params: Promise.resolve({ from, to }),
      });
      const id = assetIds.get(from) ?? `fiat:${from}`;
      const quote = `fiat:${to}`;
      if (id === WITHHELD.id && quote === WITHHELD.quote) {
        expect(meta.description).not.toContain(` 1 ${from} =`);
      } else {
        const rate = formatPairPrice(Number(priceFor(id, quote)));
        expect(meta.description).toContain(` 1 ${from} = ${rate} ${to}.`);
      }
    }
    // 20×129 + 110×20 + 2×20 = 4,820 pages; one request each before batching.
    expect(params).toHaveLength(4_820);
    expect(batchUrls).toHaveLength(HUBS.length * 2 + LONG_TAIL.length);
  });
});
