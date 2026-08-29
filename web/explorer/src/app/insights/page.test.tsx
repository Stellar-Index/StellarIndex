// #328: the /insights hub and the three feeds it fronts (/anomalies,
// /divergences, /mev) are aggregator outputs — every one of them returns
// an empty list on a network that runs no aggregator. The rail dropped
// the hub on the lean test nets, but the PAGE had no gate at all: a
// direct URL, an old bookmark, the sitemap or the /network hub's card
// grid all still landed the reader on three cards promising signals that
// cannot exist there, each opening an empty feed with no explanation.
//
// CURRENT_NETWORK is resolved at module load from NEXT_PUBLIC_NETWORK, so
// each case re-imports the page with a fresh module registry.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';

async function renderFor(network: string) {
  vi.resetModules();
  vi.stubEnv('NEXT_PUBLIC_NETWORK', network);
  const { default: InsightsPage } = await import('./page');
  return render(<InsightsPage />);
}

afterEach(() => {
  vi.unstubAllEnvs();
  vi.resetModules();
});

describe('InsightsPage network gating', () => {
  it('offers all three signal surfaces on mainnet', async () => {
    await renderFor('mainnet');
    expect(
      screen.getByRole('heading', { level: 1, name: 'Insights' }),
    ).toBeInTheDocument();
    for (const label of ['Anomalies', 'Divergence', 'MEV']) {
      expect(
        screen.getByRole('heading', { level: 2, name: label }),
      ).toBeInTheDocument();
    }
    expect(screen.queryByText(/Not available on/)).toBeNull();
  });

  it.each(['testnet', 'futurenet'])(
    'refuses to offer the signal surfaces on %s',
    async (network) => {
      await renderFor(network);
      // The corrected surface: one honest "not available here" state,
      // naming this network, INSTEAD of three cards to empty feeds.
      const label = network === 'testnet' ? 'Testnet' : 'Futurenet';
      expect(
        screen.getByText(`Not available on ${label}`),
      ).toBeInTheDocument();
      for (const name of ['Anomalies', 'Divergence', 'MEV']) {
        expect(screen.queryByRole('heading', { level: 2, name })).toBeNull();
      }
    },
  );
});
