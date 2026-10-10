import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { LiveLedger, StreamFrame } from '@/lib/live/hooks';

import { OperationsView } from './OperationsView';

const useLedgerStream = vi.hoisted(() =>
  vi.fn<() => StreamFrame<LiveLedger> | null>(),
);
const apiGet = vi.hoisted(() => vi.fn());
const search = vi.hoisted(() => ({ params: new URLSearchParams() }));
vi.mock('@/lib/live/hooks', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/lib/live/hooks')>()),
  useLedgerStream,
  useLiveClock: () => Date.now(),
}));
vi.mock('@/api/client', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/api/client')>()),
  apiGet,
}));
vi.mock('next/navigation', () => ({
  useSearchParams: () => search.params,
}));
vi.mock('@/components/NetworkInsight', () => ({
  OperationMixPanel: () => null,
  ThroughputPanel: () => null,
}));

afterEach(() => {
  useLedgerStream.mockReset();
  apiGet.mockReset();
  search.params = new URLSearchParams();
});

function op(ledger: number) {
  return {
    ledger,
    tx_hash: `tx${ledger}`,
    op_index: 0,
    type: 'payment',
    close_time: '2026-08-08T00:00:00Z',
    transaction_successful: true,
  };
}

function page(...ledgers: number[]) {
  return { data: { operations: ledgers.map(op), next_cursor: 'next' } };
}

function rowFor(ledger: number) {
  return screen.getByText(ledger.toLocaleString('en-US')).closest('tr');
}

function view(qc: QueryClient) {
  return (
    <QueryClientProvider client={qc}>
      <OperationsView />
    </QueryClientProvider>
  );
}

describe('OperationsView live flash', () => {
  it('flashes only rows newer than the previous page-1 newest', async () => {
    apiGet.mockResolvedValueOnce(page(100)).mockResolvedValue(page(101, 100));
    useLedgerStream.mockReturnValue({
      data: {
        latest_ledger: 101,
        ingested_at: '2026-08-08T00:00:05Z',
        lag_seconds: 4,
      },
      receivedAt: Date.now(),
    });
    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(view(qc));

    expect(await screen.findByText('101')).toBeInTheDocument();
    expect(rowFor(101)).toHaveClass('live-tick');
    expect(rowFor(100)).not.toHaveClass('live-tick');
  });

  it('does not flash page 1 on return from a paged view', async () => {
    useLedgerStream.mockReturnValue(null);
    apiGet.mockImplementation((_path: string, args: { cursor?: string }) =>
      Promise.resolve(args.cursor ? page(1000, 999) : page(2000, 1999)),
    );
    search.params = new URLSearchParams('cursor=c1');
    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const { rerender } = render(view(qc));
    expect(await screen.findByText('1,000')).toBeInTheDocument();

    search.params = new URLSearchParams();
    rerender(view(qc));
    expect(await screen.findByText('2,000')).toBeInTheDocument();
    expect(rowFor(2000)).not.toHaveClass('live-tick');
    expect(rowFor(1999)).not.toHaveClass('live-tick');
  });
});

describe('OperationsView type filter', () => {
  it('sends the chip type to the API and keeps it on the next page', async () => {
    useLedgerStream.mockReturnValue(null);
    search.params = new URLSearchParams('type=payment');
    apiGet.mockResolvedValue(page(100));
    render(view(new QueryClient()));
    await screen.findByText('100');
    expect(apiGet).toHaveBeenCalledWith('/v1/operations', {
      limit: 50,
      type: 'payment',
    });
    expect(
      screen.getByRole('link', { name: 'Older operations →' }),
    ).toHaveAttribute('href', '/operations?type=payment&cursor=next');
    expect(screen.getByRole('link', { name: 'Payments' })).toHaveAttribute(
      'aria-current',
      'page',
    );
  });
});
