// The home panels' "live" is the live cursor namespaces, not "anything
// but the literal `backfill`": each one-shot job writes its own
// namespace (census-backfill, projected-rebuild, …), so a job shard
// ahead of the indexer read as the tip, and a job's fresh row kept the
// indexer light green while the live indexer was stuck.
import { describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';

import { NetworkLivePanel, SystemHealthLivePanel } from './HomeLivePanels';

const { useCursors, useNetworkStats } = vi.hoisted(() => ({
  useCursors: vi.fn(),
  useNetworkStats: vi.fn(),
}));
vi.mock('@/api/hooks', () => ({ useCursors, useNetworkStats }));

const cursors = [
  {
    source: 'ledgerstream',
    sub_source: '',
    last_ledger: 62_000_000,
    lag_seconds: 900,
  },
  {
    source: 'census-backfill',
    sub_source: 'shard-3',
    last_ledger: 69_000_000,
    lag_seconds: 2,
  },
  {
    source: 'projected-rebuild',
    sub_source: 'soroswap',
    last_ledger: 68_000_000,
    lag_seconds: 3,
  },
];

describe('HomeLivePanels', () => {
  it('falls back to the live cursor tip, not a job shard, before stats load', () => {
    useNetworkStats.mockReturnValue({ data: undefined });
    useCursors.mockReturnValue({ data: cursors, isLoading: false });
    render(<NetworkLivePanel />);

    expect(screen.getByText('#62,000,000')).toBeTruthy();
    expect(screen.queryByText('#69,000,000')).toBeNull();
  });

  it('rates the indexer on live cursors only', () => {
    useCursors.mockReturnValue({ data: cursors, isLoading: false });
    render(<SystemHealthLivePanel />);

    const indexer = screen.getByText('indexer').parentElement;
    expect(
      indexer?.querySelector('[aria-label]')?.getAttribute('aria-label'),
    ).toBe('down');
    expect(screen.getByText(/1 live cursor,/)).toBeTruthy();
    expect(screen.getByText(/2 job cursors/)).toBeTruthy();
  });
});
