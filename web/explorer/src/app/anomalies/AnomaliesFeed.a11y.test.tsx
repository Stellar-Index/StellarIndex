import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AnomaliesFeed } from './AnomaliesFeed';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

const RESPONSE = {
  events: [
    {
      asset_id: 'BBB-GB',
      quote_id: 'USD',
      reason: 'outlier_storm',
      frozen_at: '2026-09-02T00:00:00Z',
      recovered_at: '2026-09-02T00:05:00Z',
      frozen_value: '1.23',
      firing: false,
      detail: {},
    },
  ],
  firing_count: 0,
  reason_tally: [],
};

function mountFeed() {
  vi.mocked(apiGet).mockImplementation(async (path: string) => {
    if (path === '/v1/anomalies') return { data: RESPONSE };
    return { data: { points: [] } };
  });
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false, refetchInterval: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <AnomaliesFeed />
    </QueryClientProvider>,
  );
}

// REGRESSION (RLT-215): the freeze-timeline table's column headers were bare
// <th> with no scope, so a screen reader announcing a data cell never names
// which column it belongs to.
describe('AnomaliesFeed table header cells declare their scope', () => {
  it('every column header is scope="col"', async () => {
    mountFeed();
    const headers = await screen.findAllByRole('columnheader');
    expect(headers.length).toBeGreaterThan(0);
    for (const h of headers) {
      expect(h).toHaveAttribute('scope', 'col');
    }
  });
});
