import { describe, it, expect } from 'vitest';

import { toCsv } from './export';

describe('toCsv', () => {
  it('quotes comma, quote and newline fields and writes null as empty', () => {
    const csv = toCsv(
      ['a', 'b', 'c', 'd'],
      [{ a: 'x,y', b: 'say "hi"', c: 'line1\nline2', d: null }],
    );
    expect(csv).toBe('a,b,c,d\r\n"x,y","say ""hi""","line1\nline2",\r\n');
  });

  it('writes large amounts and small prices byte-for-byte', () => {
    const csv = toCsv(
      ['v', 'p', 'n'],
      [{ v: '123456789012345678901234', p: '0.0000001234567890', n: 7 }],
    );
    expect(csv.split('\r\n')[1]).toBe(
      '123456789012345678901234,0.0000001234567890,7',
    );
  });
});
