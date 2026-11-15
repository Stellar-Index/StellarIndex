import { describe, expect, it } from 'vitest';

import { memoDisplay, memoLabel } from './memo';

describe('memoDisplay', () => {
  it('keeps a valid text memo', () => {
    expect(memoDisplay('12345', 'MTIzNDU=')).toEqual({
      value: '12345',
      base64: false,
    });
  });

  it('shows the base64 copy when the text lost bytes', () => {
    expect(memoDisplay('\uFFFDab\uFFFD', '/2Fi/w==')).toEqual({
      value: '/2Fi/w==',
      base64: true,
    });
  });

  it('shows the base64 copy when the text carries a direction override', () => {
    // Renders as "123456" while the bytes say 654321 after the override.
    expect(memoDisplay('\u202E654321', '4oCuNjU0MzIx')).toEqual({
      value: '4oCuNjU0MzIx',
      base64: true,
    });
    expect(memoDisplay('pay\u200Bme', 'cGF54oCLbWU=').base64).toBe(true);
  });

  it('writes hidden characters out when there is no base64 copy', () => {
    expect(memoDisplay('\u202E654321', null)).toEqual({
      value: '\\u202E654321',
      base64: false,
    });
  });

  it('has nothing to show for a memo-less transaction', () => {
    expect(memoDisplay(null, null).value).toBeUndefined();
  });
});

describe('memoLabel', () => {
  it('tags a base64 value and truncates a long one', () => {
    expect(memoLabel('\uFFFDab', '/2Fi', undefined, ': ')).toBe(
      ': /2Fi (base64)',
    );
    expect(memoLabel('abcdefghij', null, 4)).toBe('abcd…');
    expect(memoLabel(null, null)).toBe('');
  });
});
