import { render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { LiveLedger, StreamFrame } from '@/lib/live/hooks';

import { LiveLedgerBadge } from './LiveLedgerBadge';

const useLedgerStream = vi.hoisted(() => vi.fn<() => StreamFrame<LiveLedger> | null>());
vi.mock('@/lib/live/hooks', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/lib/live/hooks')>()),
  useLedgerStream,
  // A real clock reading so staleness verdicts run in render (the
  // production interval hasn't ticked yet inside a render test).
  useLiveClock: () => Date.now(),
}));

afterEach(() => {
  useLedgerStream.mockReset();
});

describe('LiveLedgerBadge', () => {
  it('renders nothing while the stream is unavailable', () => {
    useLedgerStream.mockReturnValue(null);
    const { container } = render(<LiveLedgerBadge />);
    expect(container).toBeEmptyDOMElement();
  });

  it('shows the latest ledger as a link when the stream is fresh', () => {
    useLedgerStream.mockReturnValue({
      data: { latest_ledger: 63_861_234, ingested_at: '2026-08-08T00:00:00Z', lag_seconds: 4 },
      receivedAt: Date.now(),
    });
    render(<LiveLedgerBadge />);
    const link = screen.getByRole('link', { name: /latest ledger 63861234/i });
    expect(link).toHaveAttribute('href', '/ledgers/63861234');
    expect(screen.getByText('63,861,234')).toBeInTheDocument();
  });

  it('drops the badge when the last frame is stale (WB-04: quiet is not live)', () => {
    useLedgerStream.mockReturnValue({
      data: { latest_ledger: 63_861_234, ingested_at: '2026-08-08T00:00:00Z', lag_seconds: 4 },
      receivedAt: Date.now() - 60_000,
    });
    const { container } = render(<LiveLedgerBadge />);
    expect(container).toBeEmptyDOMElement();
  });
});
