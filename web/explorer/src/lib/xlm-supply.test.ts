import { describe, expect, it } from 'vitest';

import {
  TOTAL_COINS_CAPTION_MAINNET,
  TOTAL_COINS_CAPTION_TESTNET,
  totalCoinsCaption,
} from './xlm-supply';

describe('totalCoinsCaption', () => {
  it('names the 2019 burn on mainnet only', () => {
    expect(totalCoinsCaption('mainnet')).toBe(TOTAL_COINS_CAPTION_MAINNET);
    expect(totalCoinsCaption('mainnet')).toContain('2019 burn');
    expect(totalCoinsCaption('testnet')).toBe(TOTAL_COINS_CAPTION_TESTNET);
    expect(totalCoinsCaption('futurenet')).toBe(TOTAL_COINS_CAPTION_TESTNET);
    expect(totalCoinsCaption('testnet')).not.toContain('burn');
  });

  it('defaults to the build network (mainnet when NEXT_PUBLIC_NETWORK is unset)', () => {
    expect(totalCoinsCaption()).toBe(TOTAL_COINS_CAPTION_MAINNET);
  });
});
