import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen } from '@testing-library/react';

import { LiquidityTabPanel } from './LiquidityTabPanel';

const ASSET = 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';

describe('LiquidityTabPanel volume', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('orders and formats pool volume above 2^53 from the exact decimal', async () => {
    const pool = (source: string, volume_24h_usd: string) => ({
      source,
      base: ASSET,
      quote: 'native',
      last_price: '0.42',
      volume_24h_usd,
      trade_count_24h: 5,
    });
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(
            JSON.stringify({
              data: [
                pool('aquarius', '9007199254740992'),
                pool('soroswap', '9007199254740993'),
                pool('phoenix', '1000000004999999999'),
              ],
            }),
            { status: 200, headers: { 'content-type': 'application/json' } },
          ),
      ),
    );
    const { container } = render(
      await LiquidityTabPanel({ assetID: ASSET, code: 'USDC' }),
    );
    expect(screen.getByText('$1,000,000T')).toBeInTheDocument();
    const text = container.textContent ?? '';
    expect(text.indexOf('soroswap')).toBeLessThan(text.indexOf('aquarius'));
  });
});
