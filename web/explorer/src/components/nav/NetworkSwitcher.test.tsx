import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { LiveLedger, StreamFrame } from '@/lib/live/hooks';

import { NetworkSwitcher } from './NetworkSwitcher';

// Default build env → CURRENT_NETWORK is mainnet, so the siblings are
// testnet (live) + futurenet (Phase 2, not live).
const useLedgerStream = vi.hoisted(() => vi.fn<() => StreamFrame<LiveLedger> | null>());
vi.mock('@/lib/live/hooks', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/lib/live/hooks')>()),
  useLedgerStream,
  useLiveClock: () => Date.now(),
}));

afterEach(() => {
  useLedgerStream.mockReset();
  vi.unstubAllGlobals();
});

function freshFrame(seq: number): StreamFrame<LiveLedger> {
  return {
    data: { latest_ledger: seq, ingested_at: '2026-08-26T00:00:00Z', lag_seconds: 1 },
    receivedAt: Date.now(),
  };
}

describe('NetworkSwitcher', () => {
  it('shows the current network tag and stays closed until clicked', () => {
    useLedgerStream.mockReturnValue(freshFrame(4_350_000));
    render(<NetworkSwitcher />);
    expect(screen.getByText('mainnet')).toBeInTheDocument();
    expect(screen.queryByText('Testnet')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: /network: mainnet/i })).toHaveAttribute(
      'aria-expanded',
      'false',
    );
  });

  it('opens to list every network; siblings link out, futurenet is disabled', async () => {
    useLedgerStream.mockReturnValue(freshFrame(4_350_000));
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: true,
        json: async () => ({ data: { latest_ledger: 987_654 } }),
      }),
    );

    render(<NetworkSwitcher />);
    fireEvent.click(screen.getByRole('button', { name: /network: mainnet/i }));

    // All three networks listed.
    expect(screen.getByText('Mainnet')).toBeInTheDocument();
    const testnetLink = screen.getByRole('link', { name: /testnet/i });
    expect(testnetLink).toHaveAttribute('href', 'https://testnet.stellarindex.io');

    // Futurenet is Phase 2 → shown but disabled (no link, "Soon").
    expect(screen.getByText('Soon')).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /futurenet/i })).not.toBeInTheDocument();

    // The sibling tip probe resolves into the row.
    await waitFor(() => expect(screen.getByText('987,654')).toBeInTheDocument());
    expect(fetch).toHaveBeenCalledWith(
      'https://api.testnet.stellarindex.io/v1/ledger/tip',
      expect.objectContaining({ headers: { Accept: 'application/json' } }),
    );
  });

  it('degrades a sibling to a dash when its origin is unreachable', async () => {
    useLedgerStream.mockReturnValue(freshFrame(4_350_000));
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('CORS/offline')));

    render(<NetworkSwitcher />);
    fireEvent.click(screen.getByRole('button', { name: /network: mainnet/i }));

    // Still a working hop link even though the live number is unavailable.
    expect(screen.getByRole('link', { name: /testnet/i })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText('—')).toBeInTheDocument());
  });
});
