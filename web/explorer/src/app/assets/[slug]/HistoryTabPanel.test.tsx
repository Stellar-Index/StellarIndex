import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';

import { HistoryTabPanel } from './HistoryTabPanel';
import type { TradeRow } from '@/api/hooks';

// COR-04/COR-12/AGT-02: quote_amount is always the native-XLM leg
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

vi.mock('@/api/hooks', async () => {
  const actual = await vi.importActual<typeof import('@/api/hooks')>('@/api/hooks');
  return {
    ...actual,
    useHistory: () => ({ isError: false, isLoading: false, data: [row] }),
  };
});

describe('HistoryTabPanel', () => {
  it('scales quote_amount by the quote leg (native, 7 decimals) rather than the base asset decimals prop', () => {
    // decimals=2 mimics a 2-decimal Soroban base asset detail page —
    // the bug scaled quote_amount by this value instead of 10^7.
    render(<HistoryTabPanel assetID="SHEKEL:GABC" decimals={2} />);

    // Correct: 123456789 / 10^7 = 12.3456789 -> abs>=1 branch -> "12.35"
    expect(screen.getByText('12.35')).toBeInTheDocument();
    // Buggy behaviour divides by 10^2 (the base asset's decimals) instead,
    // producing 1234567.89 -> the M-scale branch -> "1.23M".
    expect(screen.queryByText('1.23M')).not.toBeInTheDocument();
  });
});
