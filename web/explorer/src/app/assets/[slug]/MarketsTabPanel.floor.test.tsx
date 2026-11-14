import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';

import { MarketsTabPanel } from './MarketsTabPanel';
import type { Market } from '@/api/hooks';

const rows = [
  {
    base: 'crypto:XLM',
    quote: 'fiat:USD',
    trade_count_24h: 30,
    volume_24h_usd: '3000',
    volume_lower_bound: true,
  },
] as Market[];

vi.mock('@/api/hooks', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/hooks')>('@/api/hooks');
  return {
    ...actual,
    useMarkets: () => ({
      isError: false,
      isLoading: false,
      data: { markets: rows },
    }),
  };
});
vi.mock('@/lib/live/hooks', async () => {
  const actual =
    await vi.importActual<typeof import('@/lib/live/hooks')>(
      '@/lib/live/hooks',
    );
  return { ...actual, useLedgerFollow: () => undefined };
});

describe('MarketsTabPanel volume floor', () => {
  it('marks a lower-bound volume and derives no average from it', () => {
    render(<MarketsTabPanel assetID="native" />);
    expect(screen.getByText('≥ $3K')).toBeInTheDocument();
    expect(screen.queryByText('$100')).toBeNull();
  });
});
