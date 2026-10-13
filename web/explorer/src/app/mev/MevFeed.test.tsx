import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { MevFeed, MEV_CSV_COLUMNS, mevCsvRow } from './MevFeed';
import { toCsv } from '@/lib/export';

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

  it('marks a lower-bound notional with \u2265 and an anchor-valued one as est.', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: [
        {
          event_id: 'e3',
          detected_at: '2026-09-10T00:00:00Z',
          detected_at_ledger: 101,
          kind: 'arbitrage',
          tx_hashes: [],
          accounts: [],
          detail: {
            notional_usd: '1234.60',
            notional_usd_lower_bound: true,
            notional_usd_estimated: true,
          },
          profit_usd: null,
        },
      ],
    });
    render(
      <QueryClientProvider client={new QueryClient()}>
        <MevFeed />
      </QueryClientProvider>,
    );
    const notional = await screen.findByText(/\u2265 \$1,234/);
    expect(notional).toHaveTextContent('est.');
    expect(notional.title).toMatch(/lower bound/);
    expect(notional.title).toMatch(/estimated/);
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
      notional_usd_lower_bound: '',
      notional_usd_estimated: '',
      profit_usd: '',
      accounts: 'GA',
      tx_hashes: 't1 t2',
    });
  });

  it('writes the notional flags into the downloaded CSV', () => {
    const csv = toCsv(MEV_CSV_COLUMNS, [
      mevCsvRow({
        event_id: 'e4',
        detected_at: '2026-09-10T00:00:00Z',
        detected_at_ledger: 9,
        kind: 'arbitrage',
        tx_hashes: ['t9'],
        accounts: ['GB'],
        detail: {
          notional_usd: '1234',
          notional_usd_lower_bound: true,
          notional_usd_estimated: true,
        },
        profit_usd: null,
      }),
    ]);
    const [header, row] = csv.split('\r\n');
    expect(header).toBe(
      'event_id,detected_at,detected_at_ledger,kind,assets,sources,notional_usd,notional_usd_lower_bound,notional_usd_estimated,profit_usd,accounts,tx_hashes',
    );
    expect(row).toBe('e4,2026-09-10T00:00:00Z,9,arbitrage,,,1234,true,true,,GB,t9');
  });
});
