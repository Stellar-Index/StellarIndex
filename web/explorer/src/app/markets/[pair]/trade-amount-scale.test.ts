import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { formatBaseUnits } from '@/lib/format';

// RLT-214: the recent-trades table rendered base_amount/quote_amount as
// `Number(raw) / 10 ** decimals`, the same silent-rounding-above-2^53
// pattern F096 removed from the OHLC volume figure elsewhere on this
// page. A Soroban i128 trade amount can exceed 2^53 stroops; Number()
// rounds it before the divide ever runs. format.ts's formatBaseUnits
// divides via BigInt first, so every digit survives.
//
// Source-text guard, like ohlc-volume-scale.test.ts alongside it: the
// regression is a literal reimplementation at this call site, whatever
// the surrounding JSX looks like.
const abs = resolve(dirname(fileURLToPath(import.meta.url)), 'page.tsx');
const src = readFileSync(abs, 'utf8');

describe('markets/[pair] recent-trades amount scale', () => {
  it('does not divide a trade amount with Number()-then-divide', () => {
    expect(src).not.toMatch(/Number\(t\.base_amount\)/);
    expect(src).not.toMatch(/Number\(t\.quote_amount\)/);
  });

  it('renders base_amount/quote_amount through the shared BigInt-safe helper', () => {
    expect(src).toMatch(
      /formatBaseUnits\(t\.base_amount, t\.base_decimals \?\? 7\)/,
    );
    expect(src).toMatch(
      /formatBaseUnits\(t\.quote_amount, t\.quote_decimals \?\? 7\)/,
    );
  });

  it('the shared helper keeps a >2^53 stroop amount exact where Number()-then-divide rounds it', () => {
    const raw = '9007199254740993'; // 2^53 + 1
    const decimals = 0;
    const broken = (Number(raw) / 10 ** decimals).toLocaleString('en-US', {
      maximumFractionDigits: 4,
    });
    const fixed = formatBaseUnits(raw, decimals);
    expect(broken).toBe('9,007,199,254,740,992'); // Number() rounds to the nearest even double
    expect(fixed).toBe('9,007,199,254,740,993'); // BigInt-divide-first keeps every digit
    expect(fixed).not.toBe(broken);
  });
});
