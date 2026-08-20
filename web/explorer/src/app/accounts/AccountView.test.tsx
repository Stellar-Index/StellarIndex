import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { AccountView } from './AccountView';

const G = 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';

// The account operations page fans out to many sibling panels (state, movements,
// trades, defi, issuers, …), each of which fetches via apiGet. We resolve ONLY
// the operations endpoint and leave the rest pending, so those panels sit in
// their harmless loading state — this test is about the operations history.
function mockOps(operations: unknown[], coverage_note?: string) {
  vi.mocked(apiGet).mockImplementation((path: string) => {
    if (path.endsWith('/operations')) {
      return Promise.resolve({
        data: { account: G, operations, scope: 'all', ...(coverage_note ? { coverage_note } : {}) },
      });
    }
    // Never resolves — the sibling panels stay in "Loading…", never crash.
    return new Promise(() => {});
  });
}

function renderWithClient(ui: React.ReactElement) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}>{ui}</QueryClientProvider>);
}

describe('AccountView operations history — failed-tx transparency (D-PART-FAILEDTX)', () => {
  it('marks a failed op with its reason slug in red, an unknown op as muted, and renders the coverage_note banner — hiding nothing', async () => {
    mockOps(
      [
        {
          tx_hash: 'a'.repeat(64),
          op_index: 0,
          type: 'payment',
          transaction_successful: false,
          transaction_result: 'tx_failed',
          fields: { amount: '10000000', destination: G },
        },
        {
          // transaction_successful OMITTED → UNKNOWN, never "success".
          tx_hash: 'b'.repeat(64),
          op_index: 0,
          type: 'change_trust',
          fields: {},
        },
        {
          tx_hash: 'c'.repeat(64),
          op_index: 0,
          type: 'manage_sell_offer',
          transaction_successful: true,
          transaction_result: 'tx_success',
          fields: {},
        },
      ],
      'DEGRADED: parent-transaction outcome read failed for some operations.',
    );

    renderWithClient(<AccountView id={G} />);

    // (a) A failed op reads as its reason slug, in the red/down treatment.
    const failed = await screen.findByText('tx_failed');
    expect(failed).toHaveClass('bg-down-subtle');
    expect(failed).toHaveClass('text-down-strong');

    // (b) An op with no transaction_successful renders MUTED "unknown", NOT
    // success — the honest degraded state.
    const unknown = screen.getByText('unknown');
    expect(unknown).toHaveClass('text-ink-muted');
    expect(unknown).toHaveAttribute('title', 'transaction outcome unavailable');
    expect(unknown).not.toHaveClass('bg-up-subtle');

    // The genuine success renders green.
    const ok = screen.getByText('success');
    expect(ok).toHaveClass('bg-up-subtle');

    // (c) The coverage_note honest-degrade banner renders as an alert.
    const banner = screen.getByRole('alert');
    expect(banner).toHaveTextContent('DEGRADED: parent-transaction outcome read failed');

    // Transparency invariant: EVERY op stays listed — the failed and the
    // unknown ones are visible, just marked. None is filtered out.
    expect(screen.getByText('payment')).toBeInTheDocument();
    expect(screen.getByText('change_trust')).toBeInTheDocument();
    expect(screen.getByText('manage_sell_offer')).toBeInTheDocument();
  });

  it('does not render a coverage_note banner when every outcome is known', async () => {
    mockOps([
      {
        tx_hash: 'd'.repeat(64),
        op_index: 0,
        type: 'payment',
        transaction_successful: true,
        transaction_result: 'tx_success',
        fields: {},
      },
    ]);

    renderWithClient(<AccountView id={G} />);

    await screen.findByText('payment');
    // No degraded read → no banner.
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.queryByText('unknown')).not.toBeInTheDocument();
  });
});
