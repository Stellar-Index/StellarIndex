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
