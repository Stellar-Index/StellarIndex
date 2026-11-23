import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';

import {
  HistoryTabPanel,
  historyQuote,
  tradeVenueRows,
} from './HistoryTabPanel';
import { USDC_ASSET_ID } from '@/lib/networks';
import type { TradeRow } from '@/api/hooks';

// quote_amount is always the native-XLM leg
// (DEFAULT_QUOTE = 'native', fixed 7 decimals) regardless of the base
// asset's own decimals. A row for a 2-decimal Soroban base asset must
// still scale its 7-decimal-native quote_amount by 10^7, not by the
// base asset's 10^2.
const row: TradeRow = {
  source: 'soroswap',
  ledger: 123456,
  tx_hash: 'a'.repeat(64),
  op_index: 0,
  ts: new Date().toISOString(),
  base_asset: 'SHEKEL:GABC',
  quote_asset: 'native',
  // 2-decimal base asset: 500 whole units == 50000 smallest units.
  base_amount: '50000',
  base_decimals: 2,
  // native XLM quote leg: 12.3456789 XLM == 123456789 stroops (7 decimals).
  quote_amount: '123456789',
  quote_decimals: 7,
  price: '0.02469136',
};
let tape: TradeRow[] = [row];
// The scatter needs two plotted trades.
const twoTrades = () => [row, { ...row, op_index: 1, price: '0.025' }];

vi.mock('@/api/hooks', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/hooks')>('@/api/hooks');
  return {
    ...actual,
    useHistory: () => ({ isError: false, isLoading: false, data: tape }),
    // No live asset row yet: the page's build-time scale verdict stands.
    useAsset: () => ({ data: undefined }),
  };
});

// The panel now subscribes to the pair's observations stream for live
// refresh; this is a pure-render test (quote scaling), so stub the live
// hook — its behaviour is covered by src/lib/live's own tests.
vi.mock('@/lib/live/hooks', async () => {
  const actual =
    await vi.importActual<typeof import('@/lib/live/hooks')>(
      '@/lib/live/hooks',
    );
  return { ...actual, useObservationsFollow: () => undefined };
});

describe('HistoryTabPanel', () => {
  it('scales quote_amount by the quote leg (native, 7 decimals) rather than the base asset decimals prop', () => {
    // decimals=2 mimics a 2-decimal Soroban base asset detail page —
    // the bug scaled quote_amount by this value instead of 10^7.
    render(
      <HistoryTabPanel
        assetID="SHEKEL:GABC"
        decimals={2}
        scaleMismatch={false}
      />,
    );

    // 123456789 / 10^7, truncated to 4 places; 10^2 would give 1,234,567.89
    expect(screen.getByText('12.35')).toBeInTheDocument();
    expect(screen.queryByText('1,234,567.89')).not.toBeInTheDocument();
  });

  it('renders amounts above 2^53 digit for digit', () => {
    row.base_amount = '900719925474099312';
    render(
      <HistoryTabPanel
        assetID="SHEKEL:GABC"
        decimals={2}
        scaleMismatch={false}
      />,
    );
    expect(screen.getByText('9007.2T')).toBeInTheDocument();
  });

  it('shows the per-unit price and its scatter when the scale stands', () => {
    tape = twoTrades();
    render(
      <HistoryTabPanel
        assetID="SHEKEL:GABC"
        decimals={2}
        scaleMismatch={false}
      />,
    );
    expect(screen.getByText('0.02469136')).toBeInTheDocument();
    expect(
      screen.getByRole('img', { name: /Recent trades plotted/ }),
    ).toBeTruthy();
  });

  // `price` is normalised by the projection the page's verdict contradicts;
  // the amounts are on the token's own read scale and stay.
  it('withholds the price column and scatter under the page scale refusal', () => {
    tape = twoTrades();
    render(
      <HistoryTabPanel
        assetID="SHEKEL:GABC"
        decimals={2}
        scaleMismatch={true}
      />,
    );
    expect(screen.queryByText('0.02469136')).not.toBeInTheDocument();
    expect(
      screen.queryByRole('img', { name: /Recent trades plotted/ }),
    ).toBeNull();
    expect(screen.queryByText('0.025')).not.toBeInTheDocument();
    expect(screen.getAllByText('12.35')).toHaveLength(2);
    expect(
      screen.getByText(/Price withheld · decimals disagree/),
    ).toBeInTheDocument();
  });
});

describe('historyQuote', () => {
  it('quotes XLM in USDC, since XLM/XLM is no market the API serves', () => {
    expect(historyQuote('native')).toBe(USDC_ASSET_ID);
  });
  it('quotes every other asset in XLM', () => {
    expect(historyQuote(USDC_ASSET_ID)).toBe('native');
  });
});

describe('tradeVenueRows', () => {
  it('ranks venues by trade count with their share of the rows', () => {
    const rows = [
      { source: 'sdex' },
      { source: 'soroswap' },
      { source: 'soroswap' },
      { source: 'soroswap' },
    ];
    expect(tradeVenueRows(rows)).toEqual([
      { label: 'soroswap', value: 3, display: '3 trades', annotation: '75%' },
      { label: 'sdex', value: 1, display: '1 trade', annotation: '25%' },
    ]);
  });
});
