import { describe, expect, it } from 'vitest';

import { homeDomainLabel, lookAlikeMeta } from './identityClaims';

describe('homeDomainLabel', () => {
  it('states the domain as fact only when its stellar.toml lists the asset', () => {
    expect(homeDomainLabel('verified')).toBe('Home domain');
  });

  it('calls it a claim for every other resolver state', () => {
    for (const s of ['no_match', 'unreachable', 'not_fetched', undefined]) {
      expect(homeDomainLabel(s)).toBe('Claimed home domain');
    }
  });
});

describe('lookAlikeMeta', () => {
  const issuer = 'GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA';

  it('names the issuer and quotes no price', () => {
    const m = lookAlikeMeta('USDC', issuer);
    expect(m.title).toBe('USDC (issuer GBNZ…AQUA) — unverified look-alike');
    expect(m.description).toContain(issuer);
    expect(m.title + m.description).not.toContain('$');
  });
});
