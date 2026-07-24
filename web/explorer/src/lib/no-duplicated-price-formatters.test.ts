import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

// COR-14/AGT-05: DexesView.tsx, PoolsTable.tsx, and MarketsTable.tsx each
// hand-copied the exact same quote-per-base price ladder instead of
// importing the shared `formatPairPrice` (@/lib/format) — three
// independent forks of one formatter, free to visibly drift. This is a
// structural/duplication defect: the *current* outputs are byte-identical
// to formatPairPrice's (the ladder was copied verbatim), so there is no
// behavioral delta to assert here — the regression this guards against is
// the ladder being re-copied back in instead of the shared helper being
// imported. See SourceStatsPanel.test.tsx for a sibling case (the local
// `formatCompact` fork) where the duplication *had* already drifted and
// produced a different, wrong-looking output — that test asserts the
// corrected value directly.
const DUPLICATED_LADDER =
  "n >= 1000 ? n.toFixed(2) : n >= 1 ? n.toFixed(4) : n >= 0.0001 ? n.toFixed(6) : n.toExponential(3)";

const files = [
  '../app/dexes/DexesView.tsx',
  '../app/dexes/[source]/PoolsTable.tsx',
  '../app/markets/MarketsTable.tsx',
];

describe.each(files)('%s', (rel) => {
  const abs = fileURLToPath(new URL(rel, import.meta.url));
  const src = readFileSync(abs, 'utf8');

  it('imports the shared formatPairPrice instead of re-deriving the ladder locally', () => {
    expect(src).toContain("formatPairPrice");
    expect(src).toMatch(/from ['"]@\/lib\/format['"]/);
  });

  it('does not hand-copy the price-ladder ternary', () => {
    expect(src).not.toContain(DUPLICATED_LADDER);
  });
});
