import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';

import { MarketsTabPanel } from './MarketsTabPanel';
import type { Market } from '@/api/hooks';

const rows = [
  {
    base: 'crypto:XLM',
    quote: 'fiat:USD',
    trade_count_24h: 30,
    volume_24h_usd: '1000000004999999999',
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

describe('MarketsTabPanel volume', () => {
  it('rounds volume above 2^53 from the exact decimal', () => {
    render(<MarketsTabPanel assetID="native" />);
    expect(screen.getByText('$1,000,000T')).toBeInTheDocument();
  });

  it('divides 24h volume by 24h trades on the exact decimal', () => {
    render(<MarketsTabPanel assetID="native" />);
    expect(screen.getByText('$33,333.33T')).toBeInTheDocument();
  });
});
