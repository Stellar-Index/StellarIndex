import { describe, expect, it } from 'vitest';

import { muxedBaseAccount } from './strkey';

// SEP-23 test vectors.
const G = 'GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ';
const M =
  'MA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUAAAAAAAAAAAACJUQ';
const M_ID_1234 =
  'MA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUAAAAAAAAAAE2JUG6';

describe('muxedBaseAccount', () => {
  it('resolves an M-address to its base G-account', () => {
    expect(muxedBaseAccount(M)).toBe(G);
    expect(muxedBaseAccount(M_ID_1234)).toBe(G);
  });

  it('rejects a bad checksum, a G-address and junk', () => {
    expect(muxedBaseAccount(M.slice(0, -1) + 'A')).toBeNull();
    expect(muxedBaseAccount(G)).toBeNull();
    expect(muxedBaseAccount('M123')).toBeNull();
  });
});
