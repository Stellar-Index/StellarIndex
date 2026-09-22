import { render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

// T321 — on a testnet build the API link's href correctly tracks
// CURRENT_NETWORK, but the visible label used to be the literal string
// 'api.stellarindex.io' regardless of network, so a testnet reader saw a
// mainnet hostname next to a testnet href.
vi.mock('next/navigation', () => ({
  usePathname: () => '/',
}));

vi.mock('@/lib/networks', async () => {
  const actual =
    await vi.importActual<typeof import('@/lib/networks')>('@/lib/networks');
  const testnet = actual.NETWORKS.find((n) => n.id === 'testnet');
  if (!testnet) throw new Error('testnet fixture missing from NETWORKS');
  return { ...actual, CURRENT_NETWORK: testnet };
});

describe('Footer', () => {
  it('labels the API link with the current network host, not a hardcoded mainnet one', async () => {
    const { Footer } = await import('./Footer');
    render(<Footer />);

    const link = screen.getByRole('link', {
      name: 'api.testnet.stellarindex.io',
    });
    expect(link).toHaveAttribute('href', 'https://api.testnet.stellarindex.io');
    expect(screen.queryByText('api.stellarindex.io')).not.toBeInTheDocument();
  });
});
