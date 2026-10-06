import { describe, it, expect, vi } from 'vitest';
import { render, screen, within } from '@testing-library/react';

import { MarketsTabPanel } from './MarketsTabPanel';
import type { Market } from '@/api/hooks';

const USDC = 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';

const rows = [
  { base: 'crypto:XLM', quote: 'fiat:USD', trade_count_24h: 30 },
  { base: 'crypto:BTC', quote: 'crypto:XLM', trade_count_24h: 20 },
  { base: 'native', quote: USDC, trade_count_24h: 10 },
] as Market[];

let nextCursor: string | undefined;

vi.mock('@/api/hooks', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/hooks')>('@/api/hooks');
  return {
    ...actual,
    useMarkets: () => ({
      isError: false,
      isLoading: false,
      data: { markets: rows, nextCursor },
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

function sideOf(counterparty: string | RegExp): string {
  const row = screen.getByText(counterparty).closest('tr');
  if (!row) throw new Error(`no row for ${String(counterparty)}`);
  return within(row).getAllByRole('cell')[0].textContent ?? '';
}

describe('MarketsTabPanel side label', () => {
  // The catalogue slug expands to crypto:/fiat: ids for CEX/FX rows; the
  // side matcher has to see the ticker through that namespace.
  it.each(['xlm', 'native', 'crypto:XLM'])(
    'labels the asset side of every row for assetID %s',
    (assetID) => {
      render(<MarketsTabPanel assetID={assetID} />);
      expect(sideOf('USD')).toBe('base');
      expect(sideOf('BTC')).toBe('quote');
      expect(sideOf(/^USDC \(/)).toBe('base');
    },
  );
});

describe('MarketsTabPanel title', () => {
  it('states an exact count when the server has no further page', () => {
    nextCursor = undefined;
    render(<MarketsTabPanel assetID="xlm" />);
    expect(screen.getByText('3 active markets')).toBeTruthy();
  });

  it('never presents a truncated page as the total', () => {
    nextCursor = 'abc';
    render(<MarketsTabPanel assetID="xlm" />);
    expect(screen.getByText('Top 3 markets by 24h volume')).toBeTruthy();
    expect(screen.queryByText('3 active markets')).toBeNull();
    nextCursor = undefined;
  });
});
