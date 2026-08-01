import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { SourceHealthPanel } from './SourceHealthPanel';

// W6-web-2: on an explorer-ahead-of-API deploy the /v1/sources/{name}/health
// response can omit `entries_24h` (the field predates the older API). The
// unguarded `data.entries_24h.toLocaleString()` throws on the missing value,
// which trips the segment error boundary and blanks the panel. The guarded
// render must degrade to the em-dash absent-value idiom instead of throwing.
vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

describe('SourceHealthPanel — deploy-skew resilience', () => {
  it('renders "—" for a health row that omits entries_24h (no render throw)', async () => {
    // restoreMocks: true wipes any factory-time impl, so configure here.
    vi.mocked(apiGet).mockResolvedValue({
      // entries_24h intentionally absent — simulates an API older than the
      // explorer, from before the field existed.
      data: {
        name: 'binance',
        class: 'exchange',
        trade_count_24h: 5,
        markets_count_24h: 3,
        volume_24h_usd: '1000',
      },
    });
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={client}>
        <SourceHealthPanel source="binance" />
      </QueryClientProvider>,
    );
    // The events-seen stat degrades to the em-dash + "no decoded events"
    // sub-label rather than throwing. Reaching this assertion at all proves
    // the panel rendered (an unguarded render throws before it can).
    await waitFor(() =>
      expect(screen.getByText('no decoded events in 24h')).toBeInTheDocument(),
    );
    expect(screen.getByText('—')).toBeInTheDocument();
  });
});
