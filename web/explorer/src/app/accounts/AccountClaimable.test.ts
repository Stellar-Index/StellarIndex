import { describe, expect, it } from 'vitest';

import { claimCondition, type ClaimableBalance } from './AccountClaimable';

const me = 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
const other = 'GDIEKKIQWMIZ4LD3RP3ABPN7X5KEAEWYMR634BRHB7EULIMEVREWLF3G';

const cb = (
  claimants: ClaimableBalance['entry']['claimants'],
): ClaimableBalance => ({
  balance_id: '00',
  asset: 'native',
  amount: '1',
  last_modified_ledger: 1,
  last_modified_time: '2026-10-06T14:02:29Z',
  entry: { claimants },
});

describe('claimCondition', () => {
  it('reads the account’s own predicate, not another claimant’s', () => {
    const b = cb([
      { destination: other, predicate: { type: 'unconditional' } },
      { destination: me, predicate: { type: 'before_absolute_time' } },
    ]);
    expect(claimCondition(b, me)).toBe('conditional');
    expect(claimCondition(b, other)).toBe('now');
  });
});
