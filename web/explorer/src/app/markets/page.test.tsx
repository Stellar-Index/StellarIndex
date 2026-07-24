import { describe, it, expect } from 'vitest';

import { metadata } from './page';

// UXP-10/UXP-16: thousands of pairs trade on Stellar in a 14-day window,
// but /markets only ever renders the top 100 by 24h volume (see the
// Panel's own "top N by volume" label in MarketsTable.tsx). The page
// metadata previously claimed "every active trading pair" — overclaiming
// coverage the page doesn't provide.
describe('markets/page metadata', () => {
  it('does not claim to cover every active pair', () => {
    expect(String(metadata.title)).not.toMatch(/every active/i);
    expect(String(metadata.description)).not.toMatch(/^every /i);
  });

  it('qualifies coverage as the top 100 by volume', () => {
    expect(String(metadata.title)).toMatch(/top/i);
    expect(String(metadata.description)).toMatch(/top 100/i);
  });
});
