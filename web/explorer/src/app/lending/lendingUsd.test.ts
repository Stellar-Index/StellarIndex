import { describe, expect, it } from 'vitest';
import type { components } from '@/api/types';
import { exclusionGroups, reasonText } from './lendingUsd';

type Exclusion = components['schemas']['LendingStatsExclusion'];

const reserve = (asset: string, reason: Exclusion['reason']): Exclusion => ({
  kind: 'reserve',
  pool: 'CPOOL',
  asset,
  reason,
});

describe('exclusionGroups', () => {
  it('reports an archived reserve as a state, not a read failure', () => {
    const { line, groups } = exclusionGroups(
      [reserve('CARCH', 'reserve_archived'), reserve('CBAD', 'decode_failed')],
      (e) => e.asset ?? '',
    );
    expect(line).toBe(
      '1 reserve is archived on-chain, 1 reserve could not be read',
    );
    expect(groups).toEqual([
      { reason: 'archived on-chain', names: ['CARCH'] },
      { reason: 'reserve state could not be decoded', names: ['CBAD'] },
    ]);
  });

  it('counts every archived reserve and none as unread', () => {
    const { line } = exclusionGroups(
      [reserve('CA', 'reserve_archived'), reserve('CB', 'reserve_archived')],
      (e) => e.asset ?? '',
    );
    expect(line).toBe('2 reserves are archived on-chain');
  });

  it('names the archived reason in plain words', () => {
    expect(reasonText('reserve_archived')).toBe('archived on-chain');
  });
});
