import { describe, it, expect } from 'vitest';

import { maxVolume } from './MarketsTabPanel';

describe('maxVolume', () => {
  it('scales bars to the largest listed volume, skipping absent and garbage', () => {
    expect(
      maxVolume([
        { volume_24h_usd: '120.5' },
        { volume_24h_usd: null },
        { volume_24h_usd: 'n/a' },
        { volume_24h_usd: '9000' },
      ]),
    ).toBe(9000);
  });

  it('is 0 with nothing priced, so no bar is drawn', () => {
    expect(maxVolume([{ volume_24h_usd: null }])).toBe(0);
    expect(maxVolume([])).toBe(0);
  });
});
