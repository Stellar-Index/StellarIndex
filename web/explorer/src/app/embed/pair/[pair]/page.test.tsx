import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import EmbedPairPage from './page';

// The pair embed baked a BUILD-time price into a static <span> and
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

// The page now also renders <LiveChangeChip> (K061), a TanStack Query
// consumer — every render needs a QueryClient in scope.
async function renderPage(pair: string) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  render(
    <QueryClientProvider client={client}>
      {await EmbedPairPage({ params: Promise.resolve({ pair }) })}
    </QueryClientProvider>,
  );
}

describe('EmbedPairPage — live price refresh (W8.10)', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('replaces the BUILD-baked price with the live /v1/price value on mount', async () => {
    // The build bakes 0.25 off /v1/price/batch; the LivePrice poll after
    // mount reads /v1/price (live 0.75). A static span would show the
    // baked 0.25 forever — the corrected behavior is that 0.75 takes over.
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: string | URL) => {
        const url = String(input);
        // Build bakes from /v1/price/batch; /v1/price is only the live poll.
        if (url.includes('/v1/markets')) return jsonResponse({ data: [] });
        if (url.includes('/v1/price/batch')) {
          return jsonResponse({
            data: [{ asset_id: 'native', price: '0.25', observed_at: '2026-08-16T00:00:00Z' }],
          });
        }
        if (url.includes('/v1/price')) {
          return jsonResponse({
            data: { price: '0.75', observed_at: '2026-08-16T00:00:00Z' },
          });
        }
        if (url.includes('/v1/chart'))
          return jsonResponse({ data: { points: [] } });
        if (url.includes('/v1/changes'))
          return jsonResponse({ type: 'not-found' }, 404);
        throw new Error(`unexpected fetch: ${url}`);
      }),
    );

    await renderPage('native~USDC-GABC');

    // Live value takes over the headline; the baked value is gone.
    expect(await screen.findByText('0.750000')).toBeInTheDocument();
    expect(screen.queryByText('0.250000')).not.toBeInTheDocument();
    // And the poll queried the pair's OWN quote, not a USD conversion.
    const calls = (globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls;
    const pollUrl = calls
      .map((c) => String(c[0]))
      .find((u) => u.includes('/v1/price?'));
    expect(pollUrl).toContain('quote=USDC-GABC');
  });

  it('keeps the baked price under an honest "as baked at deploy" caption when the live poll fails', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: string | URL) => {
        const url = String(input);
        // Build bakes from /v1/price/batch; /v1/price is only the live poll.
        if (url.includes('/v1/markets')) return jsonResponse({ data: [] });
        if (url.includes('/v1/price/batch')) {
          return jsonResponse({
            data: [{ asset_id: 'native', price: '0.25', observed_at: '2026-08-16T00:00:00Z' }],
          });
        }
        if (url.includes('/v1/price')) throw new Error('offline'); // poll fails
        if (url.includes('/v1/chart'))
          return jsonResponse({ data: { points: [] } });
        if (url.includes('/v1/changes'))
          return jsonResponse({ type: 'not-found' }, 404);
        throw new Error(`unexpected fetch: ${url}`);
      }),
    );

    await renderPage('native~USDC-GABC');

    const el = await screen.findByTitle('as baked at deploy');
    expect(el).toHaveTextContent('0.250000');
  });
});
