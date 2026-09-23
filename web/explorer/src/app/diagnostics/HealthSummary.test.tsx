// "Live" means the live cursor namespaces, not "anything but the literal
// `backfill`". Every one-shot job writes its own namespace
// (census-backfill, projected-rebuild, tag-signer, …); a shard cursor
// covering a range ahead of the indexer must not become the live tip,
// and a just-finished job's fresh row must not read as live lag.
import { describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';

import { HealthSummary } from './HealthSummary';

const useCursors = vi.hoisted(() => vi.fn());
vi.mock('@/api/hooks', () => ({ useCursors }));

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
  {
    source: 'backfill',
    sub_source: '0-70000000:sdex',
    last_ledger: 70_000_000,
    lag_seconds: 4,
  },
];

// Cell renders label, value, sub as siblings; return [value, sub].
function cell(label: string): [string, string] {
  const kids = screen.getByText(label).parentElement?.children;
  if (!kids) throw new Error(`no cell for ${label}`);
  return [kids[1]?.textContent ?? '', kids[2]?.textContent ?? ''];
}

describe('HealthSummary', () => {
  it('reads the live tip, live-source count and lag from live namespaces only', () => {
    useCursors.mockReturnValue({ data: cursors, isLoading: false });
    render(<HealthSummary />);

    expect(cell('Live tip')[0]).toBe('#62,000,000');
    expect(cell('Live sources')).toEqual(['1', 'of 4 total']);
    expect(cell('Median lag')[0]).toBe('15.0m');
    expect(cell('Worst lag')[0]).toBe('15.0m');
  });
});
