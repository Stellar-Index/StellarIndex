import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { MevFeed, mevCsvRow } from './MevFeed';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});
vi.mock('./MevKindCharts', () => ({ MevKindCharts: () => null }));

import { apiGet } from '@/api/client';

describe('MevFeed', () => {
  it('shows notional_usd above 2^53 digit for digit', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: [
        {
          event_id: 'e1',
          detected_at: '2026-09-10T00:00:00Z',
          detected_at_ledger: 100,
          kind: 'arbitrage',
          tx_hashes: [],
          accounts: [],
          detail: { notional_usd: '9007199254740993' },
          profit_usd: null,
        },
      ],
    });
    render(
      <QueryClientProvider client={new QueryClient()}>
        <MevFeed />
      </QueryClientProvider>,
    );
    expect(
      await screen.findByText('$9,007,199,254,740,993'),
    ).toBeInTheDocument();
  });
});

describe('mevCsvRow', () => {
  it('keeps money as strings and joins the pair and lists', () => {
    expect(
      mevCsvRow({
        event_id: 'e2',
        detected_at: '2026-09-10T00:00:00Z',
        detected_at_ledger: 7,
        kind: 'sandwich',
        tx_hashes: ['t1', 't2'],
        accounts: ['GA'],
        detail: { pair: 'native|USDC-GB', notional_usd: '9007199254740993' },
        profit_usd: null,
      }),
    ).toEqual({
      event_id: 'e2',
      detected_at: '2026-09-10T00:00:00Z',
      detected_at_ledger: 7,
      kind: 'sandwich',
      assets: 'native USDC-GB',
      sources: '',
      notional_usd: '9007199254740993',
      profit_usd: '',
      accounts: 'GA',
      tx_hashes: 't1 t2',
    });
  });
});
