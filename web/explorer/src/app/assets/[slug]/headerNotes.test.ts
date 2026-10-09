import { describe, it, expect } from 'vitest';
import { supplyAsOfNote, withheldNote } from './headerNotes';

describe('withheldNote', () => {
  it('names each withheld reason', () => {
    expect(withheldNote('substance')).toBe('withheld · thin market');
    expect(withheldNote('scam_issuer')).toBe('withheld · flagged issuer');
    expect(withheldNote('upstream_leg')).toBe('withheld · USD leg withheld');
    expect(withheldNote('unattributed')).toBe('withheld');
  });

  it('is silent when nothing was withheld', () => {
    expect(withheldNote(undefined)).toBeUndefined();
  });
});

describe('supplyAsOfNote', () => {
  it('dates the supply observation in UTC', () => {
    expect(supplyAsOfNote('2026-10-08T23:30:00Z')).toBe('as of 2026-10-08');
  });

  it('is silent when absent or unparseable', () => {
    expect(supplyAsOfNote(undefined)).toBeUndefined();
    expect(supplyAsOfNote('nope')).toBeUndefined();
  });
});
