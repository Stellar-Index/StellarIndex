import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';

import EmbedCurrencyPage from './page';

// W8.10: the currency embed baked a BUILD-time FX rate into a static <span>
// and served it as live. The fix wires the shared <LivePrice> client
// component: baked value as initial paint, then a 60s /v1/price poll. Here
// /v1/price is hit ONLY by the poll (build reads /v1/assets/{ticker} +
// /v1/chart), so a live value appearing at all is the corrected behavior.

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

async function renderPage(ticker: string) {
  render(await EmbedCurrencyPage({ params: Promise.resolve({ ticker }) }));
}

describe('EmbedCurrencyPage — live price refresh (W8.10)', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('replaces the BUILD-baked FX rate with the live /v1/price value on mount', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: string | URL) => {
        const url = String(input);
        if (url.includes('/v1/assets/EUR')) {
          return jsonResponse({
            data: { ticker: 'EUR', name: 'Euro', class: 'fiat', price_usd: '1.10' },
          });
        }
        if (url.includes('/v1/chart')) return jsonResponse({ data: { points: [] } });
        if (url.includes('/v1/price')) {
          return jsonResponse({
            data: { price: '1.25', observed_at: '2026-08-16T00:00:00Z' },
          });
        }
        throw new Error(`unexpected fetch: ${url}`);
      }),
    );

    await renderPage('EUR');

    expect(await screen.findByText('$1.2500')).toBeInTheDocument();
    expect(screen.queryByText('$1.1000')).not.toBeInTheDocument();
  });

  it('keeps the baked rate under an honest "as baked at deploy" caption when the live poll fails', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: string | URL) => {
        const url = String(input);
        if (url.includes('/v1/assets/EUR')) {
          return jsonResponse({
            data: { ticker: 'EUR', name: 'Euro', class: 'fiat', price_usd: '1.10' },
          });
        }
        if (url.includes('/v1/chart')) return jsonResponse({ data: { points: [] } });
        if (url.includes('/v1/price')) throw new Error('offline'); // poll fails
        throw new Error(`unexpected fetch: ${url}`);
      }),
    );

    await renderPage('EUR');

    const el = await screen.findByTitle('as baked at deploy');
    expect(el).toHaveTextContent('$1.1000');
  });
});
