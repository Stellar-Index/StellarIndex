import { describe, it, expect } from 'vitest';

import { routeHops, type RouteLeg } from './MevRoute';

const leg = (
  source: string,
  base: string,
  quote: string,
  base_amount: string,
  quote_amount: string,
): RouteLeg => ({ source, base, quote, base_amount, quote_amount });

describe('routeHops', () => {
  it('orients each leg by the asset it shares with the next', () => {
    const hops = routeHops([
      leg('sdex', 'native', 'USDC:G1', '100', '12'),
      leg('soroswap', 'AQUA:G2', 'USDC:G1', '5000', '12'),
      leg('aquarius', 'AQUA:G2', 'native', '5000', '101'),
    ]);
    expect(hops?.map((h) => `${h.from}>${h.to}@${h.source}`)).toEqual([
      'native>USDC:G1@sdex',
      'USDC:G1>AQUA:G2@soroswap',
      'AQUA:G2>native@aquarius',
    ]);
    expect(hops?.[0]).toMatchObject({ fromAmount: '100', toAmount: '12' });
    expect(hops?.[1]).toMatchObject({ fromAmount: '12', toAmount: '5000' });
  });

  it('keeps amounts above 2^53 exact', () => {
    const big = '340282366920938463463374607431768211455';
    const hops = routeHops([
      leg('sdex', 'A:G', 'B:G', big, '1'),
      leg('phoenix', 'C:G', 'B:G', '7', '1'),
      leg('comet', 'C:G', 'A:G', '7', big),
    ]);
    expect(hops?.[0].fromAmount).toBe(big);
    expect(hops?.[2].toAmount).toBe(big);
  });

  it('returns null when legs do not chain or the direction is unprovable', () => {
    expect(
      routeHops([
        leg('sdex', 'A:G', 'B:G', '1', '1'),
        leg('sdex', 'C:G', 'D:G', '1', '1'),
      ]),
    ).toBeNull();
    expect(routeHops([leg('sdex', 'A:G', 'B:G', '1', '1')])).toBeNull();
    expect(
      routeHops([
        leg('sdex', 'A:G', 'B:G', '1', '1'),
        leg('phoenix', 'B:G', 'A:G', '1', '1'),
      ]),
    ).toBeNull();
  });
});
