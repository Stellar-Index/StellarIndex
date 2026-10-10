import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { PoolView } from './PoolView';

const HEX = 'ab'.repeat(32);

function renderView(id: string) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <PoolView id={id} />
    </QueryClientProvider>,
  );
}

describe('PoolView', () => {
  beforeEach(() => vi.mocked(apiGet).mockReset());

  it('rejects an id that is not a pool id without calling the API', () => {
    renderView('not-a-pool');
    expect(screen.getByText(/Not a native liquidity-pool id/)).toBeTruthy();
    expect(apiGet).not.toHaveBeenCalled();
  });

  it('says so when the lake has no such pool', async () => {
    vi.mocked(apiGet).mockResolvedValue({ data: [] });
    renderView(HEX);
    expect(
      await screen.findByText(/No native liquidity pool with that id/),
    ).toBeTruthy();
    expect(apiGet).toHaveBeenCalledWith(`/v1/liquidity-pools?pool=${HEX}`);
  });
});
