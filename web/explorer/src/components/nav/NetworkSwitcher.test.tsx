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
    // Trigger shows the capitalized network label, no dot (that lives on the
    // ledger badge below).
    expect(screen.getByText('Mainnet')).toBeInTheDocument();
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

    // All three networks listed; "Mainnet" appears in both the trigger and its
    // own row when open, hence getAllByText.
    expect(screen.getAllByText('Mainnet').length).toBeGreaterThan(0);
    expect(screen.getByRole('link', { name: /testnet/i })).toHaveAttribute(
      'href',
      'https://testnet.stellarindex.io',
    );
    expect(screen.getByRole('link', { name: /futurenet/i })).toHaveAttribute(
      'href',
      'https://futurenet.stellarindex.io',
    );

    // The sibling tip probes resolve into their rows.
    await waitFor(() => expect(screen.getAllByText('987,654').length).toBeGreaterThan(0));
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
    // Both live siblings (testnet + futurenet) degrade to a dash.
    await waitFor(() => expect(screen.getAllByText('—').length).toBeGreaterThanOrEqual(1));
  });
});
