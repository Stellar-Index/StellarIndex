import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { LiveLedger, StreamFrame } from '@/lib/live/hooks';

import { LedgersTable } from './LedgersTable';

const useLedgerStream = vi.hoisted(() => vi.fn<() => StreamFrame<LiveLedger> | null>());
const apiGet = vi.hoisted(() => vi.fn());
vi.mock('@/lib/live/hooks', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/lib/live/hooks')>()),
  useLedgerStream,
  useLiveClock: () => Date.now(),
}));
vi.mock('@/api/client', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/api/client')>()),
  apiGet,
}));

afterEach(() => {
  useLedgerStream.mockReset();
  apiGet.mockReset();
});

function ledgerRow(sequence: number) {
  return {
    sequence,
    close_time: '2026-08-08T00:00:00Z',
    tx_count: 42,
    op_count: 100,
    soroban_event_count: 5,
  };
}

function renderTable() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <LedgersTable />
    </QueryClientProvider>,
  );
}

describe('LedgersTable live follow', () => {
  it('renders the fetched page without a stream', async () => {
    useLedgerStream.mockReturnValue(null);
    apiGet.mockResolvedValue({ data: { ledgers: [ledgerRow(100)], next_before: 99 } });
    renderTable();
    expect(await screen.findByText('#100')).toBeInTheDocument();
    expect(apiGet).toHaveBeenCalledTimes(1);
    expect(screen.queryByText(/live/)).not.toBeInTheDocument();
  });

  it('refetches the tip page when the stream reports a newer ledger', async () => {
    apiGet
      .mockResolvedValueOnce({ data: { ledgers: [ledgerRow(100)], next_before: 99 } })
      .mockResolvedValue({
        data: { ledgers: [ledgerRow(101), ledgerRow(100)], next_before: 99 },
      });
    useLedgerStream.mockReturnValue({
      data: { latest_ledger: 101, ingested_at: '2026-08-08T00:00:05Z', lag_seconds: 4 },
      receivedAt: Date.now(),
    });
    renderTable();

    expect(await screen.findByText('#101')).toBeInTheDocument();
    expect(screen.getByText('#100')).toBeInTheDocument();
    expect(apiGet).toHaveBeenCalledTimes(2);
    expect(screen.getByText(/live/)).toBeInTheDocument();
  });

  it('does not refetch when the stream ledger is not newer than the page', async () => {
    apiGet.mockResolvedValue({ data: { ledgers: [ledgerRow(100)], next_before: 99 } });
    useLedgerStream.mockReturnValue({
      data: { latest_ledger: 100, ingested_at: '2026-08-08T00:00:00Z', lag_seconds: 4 },
      receivedAt: Date.now(),
    });
    renderTable();
    expect(await screen.findByText('#100')).toBeInTheDocument();
    // Give any wrongly-scheduled refetch a chance to fire before asserting.
    await waitFor(() => expect(apiGet).toHaveBeenCalledTimes(1));
  });
});
