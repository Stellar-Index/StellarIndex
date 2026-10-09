import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { DeploymentStats, contractTypeBadge } from './ContractsView';

afterEach(() => {
  vi.unstubAllGlobals();
});

function stubStats(data: unknown) {
  vi.stubGlobal(
    'fetch',
    vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      text: async () => JSON.stringify({ data }),
      json: async () => ({ data }),
    }),
  );
}

function renderStats() {
  const qc = new QueryClient();
  return render(
    <QueryClientProvider client={qc}>
      <DeploymentStats />
    </QueryClientProvider>,
  );
}

describe('DeploymentStats', () => {
  it('marks totals as a lower bound and shows a dash for a null active count', async () => {
    stubStats({
      deployments: [{ month: '2024-02', sac: 120, wasm: 340 }],
      total_deployed: 460,
      total_sac: 120,
      total_wasm: 340,
      active_90d: null,
      history_complete: false,
      lower_bound: true,
    });
    renderStats();
    expect(await screen.findByText('≥ 460')).toBeInTheDocument();
    expect(screen.getByText('—')).toBeInTheDocument();
    expect(
      screen.getByRole('img', { name: 'Contracts deployed per month' }),
    ).toBeInTheDocument();
  });

  it('shows exact totals and the active count when complete', async () => {
    stubStats({
      deployments: [{ month: '2024-02', sac: 1, wasm: 2 }],
      total_deployed: 3,
      total_sac: 1,
      total_wasm: 2,
      active_90d: 2,
      history_complete: true,
      lower_bound: false,
    });
    renderStats();
    expect(await screen.findByText('Active (90d)')).toBeInTheDocument();
    expect(screen.queryByText(/≥/)).toBeNull();
  });
});

describe('contractTypeBadge', () => {
  it.each([
    [{ protocol: 'blend', type: 'wasm' }, /blend/],
    [{ type: 'sac' }, /^SAC$/],
    [{ type: 'wasm' }, /^Contract$/],
    [{}, /^—$/],
  ])('labels %j', (row, text) => {
    render(<div>{contractTypeBadge(row)}</div>);
    expect(screen.getByText(text)).toBeInTheDocument();
  });
});
