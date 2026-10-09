import type { DonutSlice } from './DonutChart';
import { sumDecimalStrings } from '@/lib/format';

/**
 * topSlices — the `max` largest slices plus one summed "Other" slice. The
 * remainder is summed on the decimal strings (ADR-0003); `value` stays a
 * float for arc geometry only. Input slices must all carry `decimal`.
 */
export function topSlices(
  slices: (DonutSlice & { decimal: string })[],
  max: number,
  otherLabel = 'Other',
): DonutSlice[] {
  const sorted = [...slices].sort((a, b) => b.value - a.value);
  if (sorted.length <= max) return sorted;
  const head = sorted.slice(0, max);
  const tail = sorted.slice(max);
  const decimal = sumDecimalStrings(tail.map((s) => s.decimal));
  if (decimal == null) return head;
  return [
    ...head,
    {
      id: '__other__',
      label: `${otherLabel} (${tail.length})`,
      value: tail.reduce((n, s) => n + s.value, 0),
      decimal,
      color: '#5b6472',
    },
  ];
}
