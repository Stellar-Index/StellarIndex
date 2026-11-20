import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, describe, expect, it, vi } from 'vitest';

vi.mock('@/lib/live/hooks', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/lib/live/hooks')>()),
  useTipStream: () => null,
}));

import { PriceSparklines } from './PriceSparklines';

const series = [
  { t: '2026-11-19T00:00:00Z', p: '0.20' },
  { t: '2026-11-19T12:00:00Z', p: '0.21' },
];

function renderSparklines() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <PriceSparklines
        assetId="native"
        priceUSD="0.21"
        priceProvenance="vwap1m"
        points24h={series}
        points7d={series}
      />
    </QueryClientProvider>,
  );
}

afterEach(() => vi.unstubAllGlobals());

// The build-time series must not keep drawing a price the server withdrew.
describe('PriceSparklines', () => {
  it('draws the baked series while the price stands', () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('offline')));
    renderSparklines();
    expect(screen.getByText('Price')).toBeInTheDocument();
  });

  it('hides once the live read says the price is withheld', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        status: 404,
        ok: false,
        json: async () => ({
          type: 'https://stellarindex.io/problems/price-withheld',
        }),
      }),
    );
    renderSparklines();
    await vi.waitFor(() =>
      expect(screen.queryByText('Price')).not.toBeInTheDocument(),
    );
  });
});
