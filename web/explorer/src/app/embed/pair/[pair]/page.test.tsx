import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';

import EmbedPairPage from './page';

// W8.10: the pair embed baked a BUILD-time price into a static <span> and
// served it as live — under a deploy freeze it read days-stale with no
// refresh and no honest "as baked" hint. The fix wires the shared
// <LivePrice> client component (baked value as initial paint, then a 60s
// /v1/price poll that refreshes the SAME base/quote price it baked).

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

async function renderPage(pair: string) {
  render(await EmbedPairPage({ params: Promise.resolve({ pair }) }));
}

describe('EmbedPairPage — live price refresh (W8.10)', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('replaces the BUILD-baked price with the live /v1/price value on mount', async () => {
    // /v1/price is hit twice: once at build (baked 0.25) and once by the
    // LivePrice poll after mount (live 0.75). A static span would show the
    // baked 0.25 forever — the corrected behavior is that 0.75 takes over.
    let priceCalls = 0;
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: string | URL) => {
        const url = String(input);
        if (url.includes('/v1/price')) {
          priceCalls += 1;
          const price = priceCalls === 1 ? '0.25' : '0.75';
          return jsonResponse({
            data: { price, observed_at: '2026-08-16T00:00:00Z' },
          });
        }
        if (url.includes('/v1/chart')) return jsonResponse({ data: { points: [] } });
        throw new Error(`unexpected fetch: ${url}`);
      }),
    );

    await renderPage('native~USDC-GABC');

    // Live value takes over the headline; the baked value is gone.
    expect(await screen.findByText('0.750000')).toBeInTheDocument();
    expect(screen.queryByText('0.250000')).not.toBeInTheDocument();
    // And the poll queried the pair's OWN quote, not a USD conversion.
    const calls = (globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls;
    const pollUrl = calls.map((c) => String(c[0])).find((u, i) => u.includes('/v1/price') && i > 0);
    expect(pollUrl).toContain('quote=USDC-GABC');
  });

  it('keeps the baked price under an honest "as baked at deploy" caption when the live poll fails', async () => {
    let priceCalls = 0;
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: string | URL) => {
        const url = String(input);
        if (url.includes('/v1/price')) {
          priceCalls += 1;
          if (priceCalls === 1) {
            return jsonResponse({ data: { price: '0.25' } }); // build baked
          }
          throw new Error('offline'); // poll fails
        }
        if (url.includes('/v1/chart')) return jsonResponse({ data: { points: [] } });
        throw new Error(`unexpected fetch: ${url}`);
      }),
    );

    await renderPage('native~USDC-GABC');

    const el = await screen.findByTitle('as baked at deploy');
    expect(el).toHaveTextContent('0.250000');
  });
});
