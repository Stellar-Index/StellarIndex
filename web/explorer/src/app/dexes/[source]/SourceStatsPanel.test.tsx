import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { SourceStatsPanel } from './SourceStatsPanel';

// COR-14/AGT-05: SourceStatsPanel hand-rolled its own formatCompact instead
// of importing the shared one from @/lib/format, and the two disagreed —
// the local version's K-branch never rolled over into M (999999 rendered
// as the wrong-looking "1000.0K" instead of "1M") and it kept an extra
// trailing ".0" the shared Intl-backed formatter drops (1000 -> "1.0K"
// instead of "1K"). Mock the volume at exactly that boundary and assert
// the shared formatter's actual output.
vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

describe('SourceStatsPanel', () => {
  it('formats 24h volume with the shared formatCompact (rolls 999999 up to $1M)', async () => {
    // vitest.config.ts sets `restoreMocks: true` (vi.restoreAllMocks()
    // before every test), which wipes any mockResolvedValue set at
    // vi.mock-factory-eval time — so the implementation has to be
    // (re)configured here, inside the test body.
    vi.mocked(apiGet).mockResolvedValue({
      data: [{ name: 'soroswap', trade_count_24h: 5, volume_24h_usd: '999999' }],
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <SourceStatsPanel source="soroswap" />
      </QueryClientProvider>,
    );
    await waitFor(() => expect(screen.getByText('$1M')).toBeInTheDocument());
    expect(screen.queryByText('$1000.0K')).not.toBeInTheDocument();
  });
});
