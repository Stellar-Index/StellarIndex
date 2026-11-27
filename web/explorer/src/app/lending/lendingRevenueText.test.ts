import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { describe, expect, it } from 'vitest';

import { routeTemplate } from '@/lib/buildFetch';
import {
  BAD_DEBT_VALUATION_REASONS,
  REVENUE_REASON_TEXT,
  revenueReasonText,
} from './lendingRevenueText';

// The generated contract (CI fails when it drifts from openapi/stellar-index.v1.yaml).
const types = readFileSync(
  join(__dirname, '..', '..', 'api', 'types.ts'),
  'utf8',
);

/** The text of each named block, from its opening line to its closing brace at the same indent. */
function blocks(re: RegExp): string[] {
  const lines = types.split('\n');
  const out: string[] = [];
  lines.forEach((line, i) => {
    const m = re.exec(line);
    if (!m) return;
    const close = `${m[1]}};`;
    const end = lines.findIndex((l, j) => j > i && l === close);
    out.push(lines.slice(i, end).join('\n'));
  });
  return out;
}

const scope = [
  ...blocks(/^( {8})Lending(?:Revenue|BadDebt)\w*: \{$/),
  ...blocks(/^( {4})getLendingPool(?:Revenue|BadDebt): \{$/),
];

function reasonEnums(): Set<string> {
  const out = new Set<string>();
  for (const b of scope)
    for (const m of b.matchAll(/\b\w*reason\??: ((?:"[a-z_]+"(?: \| )?)+)/g))
      for (const v of m[1].matchAll(/"([a-z_]+)"/g)) out.add(v[1]);
  return out;
}

describe('revenue and bad-debt reason text', () => {
  const enums = reasonEnums();

  it('reads the generated enums (the scan is not vacuous)', () => {
    expect(scope.length).toBeGreaterThanOrEqual(8);
    for (const r of [
      'derive_stale',
      'withheld',
      'as_of_mismatch',
      'not_derivable',
      'backstop_role_unverified',
    ])
      expect(enums).toContain(r);
  });

  it('maps every reason enum the two endpoints serve to plain words', () => {
    const missing = [...enums].filter((r) => !(r in REVENUE_REASON_TEXT));
    expect(missing).toEqual([]);
  });

  it('maps every valuation reason a bad-debt part names in prose', () => {
    const part = scope.find((b) => /^ {8}LendingBadDebtPart: \{/.test(b)) ?? '';
    const named = new Set<string>();
    for (const line of part.split('\n'))
      if (/@description Why (usd|underlying) is null/.test(line))
        for (const m of line.matchAll(/\b[a-z]+(?:_[a-z]+)+\b/g))
          if (!m[0].endsWith('_reason')) named.add(m[0]);
    expect(named.size).toBeGreaterThan(3);
    for (const r of named) expect(BAD_DEBT_VALUATION_REASONS).toContain(r);
  });

  it('answers each code with its own plain words, never empty or a raw code', () => {
    for (const [code, text] of Object.entries(REVENUE_REASON_TEXT)) {
      expect(text.trim()).not.toBe('');
      expect(text).not.toContain('_');
      expect(revenueReasonText(code)).toBe(text);
    }
  });

  it('counts both endpoints under their own route template', () => {
    expect(routeTemplate('/v1/lending/pools/CPOOL/revenue?window=30d')).toBe(
      '/v1/lending/pools/{id}/revenue',
    );
    expect(routeTemplate('/v1/lending/pools/CPOOL/bad-debt')).toBe(
      '/v1/lending/pools/{id}/bad-debt',
    );
  });
});
