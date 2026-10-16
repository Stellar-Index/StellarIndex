import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { describe, expect, it, vi } from 'vitest';

const apiGet = vi.hoisted(() => vi.fn());
vi.mock('@/api/client', async (orig) => ({
  ...(await orig<typeof import('@/api/client')>()),
  apiGet,
}));

import { ProtocolView } from './ProtocolView';

function renderView(name: string) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <ProtocolView name={name} label={name} />
    </QueryClientProvider>,
  );
}

describe('ProtocolView stat strip', () => {
  it('does not call a protocol without contracts an analytics outage', async () => {
    apiGet.mockResolvedValue({
      data: {
        name: 'sdex',
        category: 'dex',
        contract_count: 0,
        factories: [],
        contracts: [],
        event_kinds: ['sdex.trade'],
        completeness: { complete: false, watermark_ledger: 50_746_444 },
        events_24h: 703_710,
        genesis_ledger: 2,
        analytics: { status: 'ok', as_of: '2026-01-01T00:00:00Z' },
      },
    });
    renderView('sdex');
    expect(await screen.findByText('Events · 24h')).toBeInTheDocument();
    expect(
      screen.queryByText('Lake analytics unavailable'),
    ).not.toBeInTheDocument();
  });
});
