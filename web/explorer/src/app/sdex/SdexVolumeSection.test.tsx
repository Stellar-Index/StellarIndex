import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { CURRENT_NETWORK } from '@/lib/networks';
import { SdexVolumeSection } from './SdexVolumeSection';

function renderWithKpis(
  kpis: { label: string; value: string; unit?: string }[],
  series: unknown[] = [],
) {
  vi.mocked(apiGet).mockResolvedValue({
    data: { name: 'sdex', bespoke: { kpis, series } },
  });
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  render(
    <QueryClientProvider client={qc}>
      <SdexVolumeSection />
    </QueryClientProvider>,
  );
}

// Number("999.994999999999998") is 999.995, which rounds to "$1K"; the
// exact value rounds to "$999.99".
describe('SdexVolumeSection KPIs', () => {
  it.runIf(CURRENT_NETWORK.pricing)(
    'rounds a USD KPI from its exact decimal',
    async () => {
      renderWithKpis([
        { label: '24h volume', value: '999.994999999999998', unit: 'USD' },
      ]);
      const dt = await screen.findByText('24h volume');
      expect(dt.nextElementSibling?.textContent).toBe('$999.99');
    },
  );

  it('shows counts compact and a non-decimal value verbatim', async () => {
    renderWithKpis([
      { label: 'Trades', value: '1234567' },
      { label: 'Pairs', value: 'n/a' },
    ]);
    const trades = await screen.findByText('Trades');
    expect(trades.nextElementSibling?.textContent).toBe('1.23M');
    expect(screen.getByText('Pairs').nextElementSibling?.textContent).toBe(
      'n/a',
    );
  });

  it.runIf(CURRENT_NETWORK.pricing)(
    'rounds the one-day volume line from its exact decimal',
    async () => {
      renderWithKpis(
        [],
        [
          {
            name: 'USD volume',
            unit: 'USD',
            points: [{ date: '2020-01-01', value: '999.994999999999998' }],
          },
        ],
      );
      expect(await screen.findByText(/2020-01-01/)).toHaveTextContent(
        '2020-01-01: $999.99',
      );
    },
  );
});
