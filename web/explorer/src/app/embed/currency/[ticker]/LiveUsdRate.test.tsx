import { render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { LivePrice } from '../../LivePrice';
import { LiveUsdRate } from './LiveUsdRate';

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('LiveUsdRate', () => {
  it('starts from the build-time rate', () => {
    render(<LiveUsdRate ticker="ZZA" initialInverseUsd="0.5" />);
    expect(screen.getByText(/1 USD = 2\.0000 ZZA/)).toBeInTheDocument();
  });

  it('follows the price the headline fetched', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => ({
          data: { price: '0.25', observed_at: '2026-09-22T00:00:00Z' },
        }),
      }),
    );

    render(
      <>
        <LivePrice assetId="fiat:ZZB" initial="$0.5000" />
        <LiveUsdRate ticker="ZZB" initialInverseUsd="0.5" />
      </>,
    );

    expect(await screen.findByText(/1 USD = 4\.0000 ZZB/)).toBeInTheDocument();
  });
});
