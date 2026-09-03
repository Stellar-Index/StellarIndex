import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn(() => new Promise(() => {})) };
});

import HomePage from './page';

// The landing page's API pitch sold two things the deployment
// contradicts:
//
//   1. "an API key raises your rate limit from 60 to 1,000+ requests a
//      minute". The hosted API's anonymous header is
//      `x-ratelimit-limit: 6000`; a free key is stamped at 1,000. A key
//      is a per-key budget, not a raise — /pricing has said so all
//      along, so the front door contradicted the plans page.
//   2. "Candles — daily history back to 2015". The first daily bar
//      /v1/ohlc serves for XLM/USD is 2018-07-01, and 2021-01-31 →
//      2026-03-12 has none at all.
//
// Assertions are scoped to the two elements that carry the claims
// rather than the whole document: the page also renders the changelog
// strip, so a released entry QUOTING an old claim would otherwise read
// as the page still making it.
function renderHome() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  render(
    <QueryClientProvider client={qc}>
      <HomePage />
    </QueryClientProvider>,
  );
  const pitch = screen.getByText(/Anonymous reads are free forever/i);
  // The endpoint list is a <dt>/<dd> pair per row; the description
  // sits in the row wrapper alongside the path.
  const ohlcRow = screen.getByText('GET /v1/ohlc').parentElement;
  if (!ohlcRow) throw new Error('no wrapper around the /v1/ohlc row');
  return { pitch, ohlcRow };
}

describe('HomePage API pitch', () => {
  it('does not claim a key raises the limit from 60 to 1,000+', () => {
    const { pitch } = renderHome();
    expect(pitch).not.toHaveTextContent(/from 60 to 1,000/i);
    expect(pitch).not.toHaveTextContent(/raises your rate limit/i);
  });

  it('states the anonymous and per-key limits /pricing publishes', () => {
    const { pitch } = renderHome();
    expect(pitch).toHaveTextContent(/6,000\s+requests a minute per IP/i);
    expect(pitch).toHaveTextContent(/per-key budget of\s+1,000 a minute/i);
  });

  it('does not claim daily OHLC history back to 2015', () => {
    const { ohlcRow } = renderHome();
    expect(ohlcRow).not.toHaveTextContent(/back to 2015/i);
    expect(ohlcRow).toHaveTextContent(/daily bars from 2018/i);
  });
});
