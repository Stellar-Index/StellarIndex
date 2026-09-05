import { describe, it, expect } from 'vitest';

import { metadata } from './page';

// The page publishes only the assets whose real-world backing an
// independent party has vouched for, per (code, issuer). Metadata that
// claimed to cover "every RWA on Stellar" would overclaim in exactly the
// direction that matters here: a reader would take the ABSENCE of an
// impersonator as proof it does not exist, when the honest statement is
// that this page shows what cleared the bar.
describe('rwa/page metadata', () => {
  it('does not claim to cover every real-world asset', () => {
    expect(String(metadata.title)).not.toMatch(/\ball\b|\bevery\b|complete/i);
    expect(String(metadata.description)).not.toMatch(/^every /i);
    expect(String(metadata.description)).not.toMatch(/\bevery (rwa|real-world|tokeni)/i);
  });

  it('names the evidence the set is built on', () => {
    const copy = `${metadata.title} ${metadata.description}`;
    expect(copy).toMatch(/SEP-1/);
    expect(copy).toMatch(/issuer/i);
    expect(copy).toMatch(/\(code, issuer\)/);
  });

  it('states that a withheld valuation is not shown as a number', () => {
    expect(String(metadata.description)).toMatch(/unavailable/i);
  });
});
