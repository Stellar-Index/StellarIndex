import { describe, it, expect } from 'vitest';

import {
  scamFlagTags,
  hasDirectoryScamFlag,
  stellarExpertDirectoryUrl,
} from './directory-tags';

const SCAM_AUD_ISSUER = 'GAIF52QZUPYCADXF7I7RNPMED7DT2B5JGPR7DEHCC5TPDPUJTMLGGAUD';

describe('scamFlagTags', () => {
  it('returns the scam-warning subset of the issuer directory tags', () => {
    // The operator-reported scam AUD carries {malicious, unsafe}.
    expect(scamFlagTags(['malicious', 'unsafe'])).toEqual([
      'malicious',
      'unsafe',
    ]);
  });

  it('ignores non-warning tags (exchange/anchor/issuer are not flags)', () => {
    expect(scamFlagTags(['exchange', 'anchor', 'issuer'])).toEqual([]);
    expect(hasDirectoryScamFlag(['exchange'])).toBe(false);
  });

  it('matches case-insensitively but preserves the served casing', () => {
    expect(scamFlagTags(['Malicious', 'SCAM'])).toEqual(['Malicious', 'SCAM']);
  });

  it('covers every listed flag term', () => {
    for (const t of ['malicious', 'unsafe', 'fraud', 'scam', 'hack', 'phishing']) {
      expect(hasDirectoryScamFlag([t])).toBe(true);
    }
  });

  it('is empty/false for null/undefined/empty', () => {
    expect(scamFlagTags(undefined)).toEqual([]);
    expect(scamFlagTags(null)).toEqual([]);
    expect(hasDirectoryScamFlag([])).toBe(false);
  });
});

describe('stellarExpertDirectoryUrl', () => {
  it('builds the directory source link for a valid issuer G-address', () => {
    expect(stellarExpertDirectoryUrl(SCAM_AUD_ISSUER)).toBe(
      `https://stellar.expert/explorer/public/account/${SCAM_AUD_ISSUER}`,
    );
  });

  it('returns null for a missing or malformed issuer', () => {
    expect(stellarExpertDirectoryUrl(undefined)).toBeNull();
    expect(stellarExpertDirectoryUrl('')).toBeNull();
    expect(stellarExpertDirectoryUrl('not-a-strkey')).toBeNull();
    // No path traversal / injection through the issuer segment.
    expect(stellarExpertDirectoryUrl('G../../evil')).toBeNull();
  });
});
