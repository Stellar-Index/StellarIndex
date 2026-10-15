import { describe, expect, it } from 'vitest';

import { muxedBaseAccount, sacAddress } from './strkey';

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

const PUBNET = 'Public Global Stellar Network ; September 2015';

describe('sacAddress', () => {
  it('derives known mainnet SAC addresses', async () => {
    expect(await sacAddress('native', null, PUBNET)).toBe(
      'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA',
    );
    expect(
      await sacAddress(
        'USDC',
        'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
        PUBNET,
      ),
    ).toBe('CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75');
    expect(
      await sacAddress(
        'sUSD',
        'GCHW7CWI7GMIYQYFXMFJNJX5645XGWIINIAEQK3SABQO6CAYL5T7JYIH',
        PUBNET,
      ),
    ).toBe('CDFZUVS5YNLXU7VENKOUDEOHCJGKQNVUBWD7KMN6E7ZROKPYPFLRUJFG');
  });

  it('derives the per-network native SAC', async () => {
    expect(
      await sacAddress('native', null, 'Test SDF Network ; September 2015'),
    ).toBe('CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC');
  });

  it('rejects a bad issuer checksum, a bad code and a missing issuer', async () => {
    expect(await sacAddress('USDC', G.slice(0, -1) + 'A', PUBNET)).toBeNull();
    expect(await sacAddress('US-DC', G, PUBNET)).toBeNull();
    expect(await sacAddress('USDC', null, PUBNET)).toBeNull();
  });
});
