import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { SourceBreakdown } from './SourceBreakdown';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

function renderPanel() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <SourceBreakdown base="native" quote="crypto:USD" />
    </QueryClientProvider>,
  );
}

describe('SourceBreakdown', () => {
  beforeEach(() => {
    vi.mocked(apiGet).mockReset();
  });

  it('shows the error state when the first load fails', async () => {
    vi.mocked(apiGet).mockRejectedValue(new Error('503'));
    renderPanel();
    expect(
      await screen.findByText(/source breakdown is unavailable/i),
    ).toBeInTheDocument();
  });

  it('renders nothing when no source has priced volume', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        window_secs: 86400,
        sources: [
          {
            source: 'sdex',
            volume_24h_usd: null,
            trade_count_24h: 3,
            share_pct: 0,
          },
        ],
      },
    });
    const { container } = renderPanel();
    await waitFor(() => expect(apiGet).toHaveBeenCalled());
    await waitFor(() => expect(container).toBeEmptyDOMElement());
    expect(screen.queryByText(/unavailable/i)).not.toBeInTheDocument();
  });
});
