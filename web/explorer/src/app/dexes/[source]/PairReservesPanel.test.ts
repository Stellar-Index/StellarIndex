import { describe, it, expect } from 'vitest';

import { UNSCALED_LABEL } from '@/lib/format';
import { displayUnits } from './PairReservesPanel';

describe('PairReservesPanel displayUnits', () => {
  it('labels a reserve whose token scale is absent instead of showing 0', () => {
    const out = displayUnits('1500000000000000000000', undefined);
    expect(out).not.toBe('0');
    expect(out).toContain(UNSCALED_LABEL);
  });

  it('scales a reserve whose token scale is declared', () => {
    expect(displayUnits('12345678', 7)).toBe('1.23457');
  });
});
