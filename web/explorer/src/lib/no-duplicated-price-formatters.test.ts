import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

// Resolve via node:path, not `new URL(rel, base)` — under the jsdom
// environment the global URL is jsdom's and fileURLToPath rejects it
// ("The URL must be of scheme file").
const HERE = dirname(fileURLToPath(import.meta.url));

// COR-14/AGT-05: DexesView.tsx, PoolsTable.tsx, and MarketsTable.tsx each
// hand-copied the exact same quote-per-base price ladder instead of
// importing the shared `formatPairPrice` (@/lib/format) — three
// independent forks of one formatter, free to visibly drift. The first
// remediation made each file import formatPairPrice but left four local
// LastPriceCell components wrapping it — and those forked AGAIN
// (DexesView's copy silently lost the flash-on-change). 2026-08-21: the
// cell itself was extracted to @/components/LastPriceCell, which is now
// the ONLY place formatPairPrice appears in a last-price cell. This
// guard asserts the end state: every price table renders the shared
// cell, defines no local one, and nobody re-copies the raw ladder.
// See SourceStatsPanel.test.tsx for a sibling case (the local
// `formatCompact` fork) where the duplication *had* already drifted.
const DUPLICATED_LADDER =
  'n >= 1000 ? n.toFixed(2) : n >= 1 ? n.toFixed(4) : n >= 0.0001 ? n.toFixed(6) : n.toExponential(3)';

// 2026-08-24 (FEC audit A3-F8): PoolsTable + PairsTable folded into the
// shared VenueMarketsTable — the price-table set is now the two remaining
// route tables + the shared component. (Their thin wrappers are pinned by
// fec-consolidation-guards.test.ts; the repo-wide formatPairPrice importer
// allowlist there is the fixed-list-proof version of this guard.)
const files = [
  '../app/dexes/DexesView.tsx',
  '../app/markets/MarketsTable.tsx',
  '../components/VenueMarketsTable.tsx',
];

describe.each(files)('%s', (rel) => {
  const src = readFileSync(resolve(HERE, rel), 'utf8');

  it('renders the shared LastPriceCell instead of a local fork', () => {
    expect(src).toMatch(/from ['"]@\/components\/LastPriceCell['"]/);
    expect(src).toContain('<LastPriceCell');
    expect(src).not.toMatch(/function LastPriceCell/);
  });

  it('does not hand-copy the price-ladder ternary', () => {
    expect(src).not.toContain(DUPLICATED_LADDER);
  });
});

describe('the shared LastPriceCell', () => {
  const src = readFileSync(
    resolve(HERE, '../components/LastPriceCell.tsx'),
    'utf8',
  );

  it('is the one place the shared formatter + flash meet', () => {
    expect(src).toContain('formatPairPrice');
    expect(src).toMatch(/from ['"]@\/lib\/format['"]/);
    // The drift the second fork introduced: a cell without the tick
    // flash. The shared cell must keep it.
    expect(src).toContain('usePriceFlash');
    expect(src).not.toContain(DUPLICATED_LADDER);
  });
});
