import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { describe, expect, it, vi } from 'vitest';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import {
  AccountDefiPositionsPanel,
  PositionAmount,
} from './AccountDefiPositions';

const G = 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';

function renderPanel() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <AccountDefiPositionsPanel id={G} />
    </QueryClientProvider>,
  );
}

function serve(coverageNote?: string) {
  vi.mocked(apiGet).mockResolvedValue({
    data: {
      account: G,
      positions: [],
      include_closed: false,
      note: 'Raw on-chain quantities.',
      ...(coverageNote ? { coverage_note: coverageNote } : {}),
    },
  });
}

describe('AccountDefiPositionsPanel coverage', () => {
  it('shows the coverage note and never claims an empty partial read is complete', async () => {
    const note = 'INCOMPLETE: 2 of 6 protocol reads failed (blend, phoenix).';
    serve(note);
    renderPanel();
    expect(await screen.findByText(note)).toBeInTheDocument();
    expect(screen.getByText('partial')).toBeInTheDocument();
    expect(screen.getByText(/DeFi positions \(≥ 0\)/)).toBeInTheDocument();
    expect(
      screen.queryByText(/No DeFi positions observed/),
    ).not.toBeInTheDocument();
    expect(screen.getByText(/others were not checked/)).toBeInTheDocument();
  });

  it('keeps the plain empty claim on a complete read', async () => {
    serve();
    renderPanel();
    expect(
      await screen.findByText(/No DeFi positions observed/),
    ).toBeInTheDocument();
    expect(screen.queryByText('partial')).not.toBeInTheDocument();
    expect(screen.getByText(/DeFi positions \(0\)/)).toBeInTheDocument();
  });
});

describe('PositionAmount', () => {
  it('scales a known-token amount and keeps the exact value in the title', () => {
    render(<PositionAmount amount="90071992547409930000000" decimals={7} />);
    expect(screen.getByTitle('9007199254740993').textContent).toBe('9007.2T');
  });

  it('serves an unscaled amount raw when no scale is known', () => {
    render(<PositionAmount amount="1234567" />);
    expect(screen.getByText('1234567')).toBeTruthy();
  });

  it('renders an unknown amount as a dash, never zero', () => {
    render(<PositionAmount amount="" decimals={7} />);
    expect(screen.getByText('—')).toBeTruthy();
  });
});
