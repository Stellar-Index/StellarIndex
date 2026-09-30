// The home panels' "live" is the live cursor namespaces, not "anything
// but the literal `backfill`": each one-shot job writes its own
// namespace (census-backfill, projected-rebuild, …), so a job shard
// ahead of the indexer read as the tip, and a job's fresh row kept the
// indexer light green while the live indexer was stuck.
import { describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';

import { NetworkLivePanel, SystemHealthLivePanel } from './HomeLivePanels';

const { useCoverage, useCursors, useNetworkStats } = vi.hoisted(() => ({
  useCoverage: vi.fn(),
  useCursors: vi.fn(),
  useNetworkStats: vi.fn(),
}));
vi.mock('@/api/hooks', () => ({ useCoverage, useCursors, useNetworkStats }));

function dot(label: string) {
  return screen
    .getByText(label)
    .parentElement?.querySelector('[aria-label]')
    ?.getAttribute('aria-label');
}

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
    useCoverage.mockReturnValue({ data: undefined });
    render(<SystemHealthLivePanel />);

    const indexer = screen.getByText('indexer').parentElement;
    expect(
      indexer?.querySelector('[aria-label]')?.getAttribute('aria-label'),
    ).toBe('down');
    expect(screen.getByText(/1 live cursor,/)).toBeTruthy();
    expect(screen.getByText(/2 job cursors/)).toBeTruthy();
  });

  it.each([
    ['no verdict', undefined, 'unknown'],
    ['no sources', { lake_complete_sources: 0, total_sources: 0 }, 'unknown'],
    ['0/20 lake', { lake_complete_sources: 0, total_sources: 20 }, 'down'],
    [
      '17/20 lake',
      { lake_complete_sources: 17, total_sources: 20 },
      'degraded',
    ],
    ['20/20 lake', { lake_complete_sources: 20, total_sources: 20 }, 'ok'],
  ])('rates archive completeness from /v1/coverage: %s', (_, data, want) => {
    useCursors.mockReturnValue({ data: cursors, isLoading: false });
    useCoverage.mockReturnValue({ data });
    render(<SystemHealthLivePanel />);

    expect(dot('archive completeness')).toBe(want);
  });
});
