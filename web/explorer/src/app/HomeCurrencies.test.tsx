import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn(() => new Promise(() => {})) };
});

import { HomeCurrencies } from './HomeCurrencies';

// The strip's caption promised "full ~200-ticker coverage at /assets",
// which was wrong on both halves: the reference catalogue served by
// /v1/external/assets holds 34 rows (19 fiat + 15 coins), and /assets is
// the Stellar-only directory — it carries no fiat at all and points the
// reader back out to /external/assets. The caption must send them
// straight to the page that holds the rows.
function renderStrip() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <HomeCurrencies />
    </QueryClientProvider>,
  );
}

describe('HomeCurrencies caption', () => {
  it('does not promise ~200-ticker coverage', () => {
    renderStrip();
    expect(screen.queryByText(/200-ticker/i)).not.toBeInTheDocument();
  });

  it('links the reference set to /external/assets, not /assets', () => {
    renderStrip();
    const link = screen.getByRole('link', { name: '/external/assets' });
    expect(link).toHaveAttribute('href', '/external/assets');
  });

  it('states the real size of the reference set', () => {
    renderStrip();
    expect(
      screen.getByText(/19 fiat plus 15 reference coins/i),
    ).toBeInTheDocument();
  });
});
