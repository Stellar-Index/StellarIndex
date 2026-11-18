// Guard: a USD volume total built from rows that may each be a lower bound
// must carry that flag, so a file summing `volume_24h_usd` goes through
// sumUsdVolumes (which returns the flag) instead of bare sumDecimalStrings.
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';

import { describe, expect, it } from 'vitest';

import { sumUsdVolumes } from '@/lib/format';

const SRC = join(__dirname, '..', '..');

/** Files that sum volume rows directly. One-line reason each. */
const ALLOWED: Record<string, string> = {
  'app/HomeNetworkStrip.tsx':
    'derives volumeLowerBound from the same rows and renders "≥ … lower bound"',
  'app/assets/[slug]/LiquidityVenueBars.tsx':
    'per-venue sums carry lower = truncated || volume_lower_bound || unpriced',
  'app/markets/[pair]/SourceBreakdown.tsx':
    'reads volume_lower_bound on the rows it sums',
  'app/markets/[pair]/page.tsx':
    'untriaged debt: sum is a share denominator for per-row pcts, not a rendered total',
  'app/issuers/[g_strkey]/IssuerDetailView.tsx':
    'untriaged debt: issuer volume total, not yet checked for a lower-bound flag',
};

function sources(dir: string): string[] {
  const out: string[] = [];
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) out.push(...sources(p));
    else if (/\.tsx?$/.test(name) && !/\.test\.tsx?$/.test(name)) out.push(p);
  }
  return out;
}

describe('volume lower-bound consumers', () => {
  it('no file sums volume_24h_usd without sumUsdVolumes', () => {
    const offenders = sources(SRC)
      .filter((f) => {
        const text = readFileSync(f, 'utf8');
        return (
          text.includes('volume_24h_usd') && text.includes('sumDecimalStrings(')
        );
      })
      .map((f) => relative(SRC, f))
      .filter((f) => !(f in ALLOWED) && f !== 'lib/format.ts');
    expect(offenders).toEqual([]);
  });

  it('every allow-list entry still exists and still sums', () => {
    for (const f of Object.keys(ALLOWED)) {
      expect(readFileSync(join(SRC, f), 'utf8')).toContain(
        'sumDecimalStrings(',
      );
    }
  });

  it('sumUsdVolumes flags the total when any row is a lower bound', () => {
    expect(
      sumUsdVolumes([
        { volume_24h_usd: '10.5' },
        { volume_24h_usd: '2', volume_lower_bound: true },
        { volume_24h_usd: null, volume_lower_bound: true },
      ]),
    ).toEqual({ total: '12.5', lowerBound: true });
    expect(
      sumUsdVolumes([{ volume_24h_usd: '1' }, { volume_24h_usd: '2' }]),
    ).toEqual({ total: '3', lowerBound: false });
  });
});
