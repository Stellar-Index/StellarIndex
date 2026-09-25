import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

// Resolve via node:path, not `new URL(rel, base)` — under the jsdom
// environment the global URL is jsdom's and fileURLToPath rejects it
// ("The URL must be of scheme file").
const HERE = dirname(fileURLToPath(import.meta.url));
const SRC = join(HERE, '..');

function walk(dir: string, out: string[] = []): string[] {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) walk(p, out);
    else if (/\.(ts|tsx)$/.test(name) && !/\.test\.(ts|tsx)$/.test(name))
      out.push(p);
  }
  return out;
}

const sources = walk(SRC).map((p) => ({
  rel: relative(SRC, p),
  text: readFileSync(p, 'utf8'),
}));

// GH-774: the guard used to pin the literal pre-2026-08-06 ladder text
// (`n.toExponential(3)`), which no live fork has contained since
// formatSubunitPrice replaced scientific notation — so it matched
// nothing, and three later forks (HomeTopMarkets.formatLastPrice,
// embed/LivePrice's inline ladder, LivePairPrice.formatQuotePrice) went
// uncaught. This matches by STRUCTURE — the `>=1000/>=1/>=0.0001` and
// `>=1/>=0.001` threshold chains that fall back to formatSubunitPrice,
// the shape of lib/format.ts's formatPairPrice and formatPriceSmall —
// so a differently-worded fork still trips it, per
// fec-consolidation-guards.test.ts's enumerate-and-match-by-shape
// pattern.
const PAIR_LADDER_SHAPE =
  />=\s*1000\b[\s\S]{0,300}?>=\s*1\b[\s\S]{0,300}?>=\s*0\.0001\b[\s\S]{0,200}?formatSubunitPrice\(/;
const SMALL_LADDER_SHAPE =
  />=\s*1\b[\s\S]{0,300}?>=\s*0\.001\b[\s\S]{0,200}?formatSubunitPrice\(/;

function hasHandRolledLadder(text: string): boolean {
  return PAIR_LADDER_SHAPE.test(text) || SMALL_LADDER_SHAPE.test(text);
}

describe('no hand-rolled price-ladder forks exist outside lib/format.ts', () => {
  // Shape-matching against the whole tree surfaces older forks GH-774 did
  // not name (build-time asset/embed pages, DepthChart's axis labels) —
  // recorded here rather than silently swept in, so the guard's job from
  // here is to block a NEW (6th) fork, not to have quietly re-narrowed
  // itself back to a fixed list. Migrate one off this list and delete its
  // entry; do not add to it.
  const KNOWN_UNMIGRATED = new Set([
    'app/assets/[slug]/page.tsx',
    'app/embed/asset/[slug]/page.tsx',
    'app/embed/currency/[ticker]/page.tsx',
    'app/embed/pair/[pair]/page.tsx',
    'app/markets/[pair]/page.tsx',
    'components/charts/DepthChart.tsx',
  ]);

  it('formatPairPrice / formatPriceSmall are the only ladder implementations', () => {
    const offenders = sources
      .filter((f) => hasHandRolledLadder(f.text))
      .map((f) => f.rel)
      .filter((r) => r !== 'lib/format.ts' && !KNOWN_UNMIGRATED.has(r));
    expect(offenders).toEqual([]);
  });
});

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
    expect(hasHandRolledLadder(src)).toBe(false);
  });
});

// RLT-388: /convert/[from]/[to]'s headline (ConvertLive.tsx), interactive
// widget (ConvertPair.tsx) and SEO meta description (page.tsx) each
// hand-copied their own quote-per-base rate ladder. The meta copy broke
// at >=100 -> toFixed(2) while the others broke at >=1000, so the same
// rate rendered with a different digit count in the page body than in
// its <meta> description. All three now import formatPairPrice.
const CONVERT_RATE_FILES = [
  '../app/convert/[from]/[to]/page.tsx',
  '../app/convert/[from]/[to]/ConvertLive.tsx',
  '../app/convert/[from]/[to]/ConvertPair.tsx',
];

describe.each(CONVERT_RATE_FILES)('%s', (rel) => {
  const src = readFileSync(resolve(HERE, rel), 'utf8');

  it('imports the shared formatPairPrice instead of a local rate fork', () => {
    expect(src).toMatch(/formatPairPrice.*from ['"]@\/lib\/format['"]/);
    expect(src).not.toMatch(/function formatRate\b/);
    expect(src).not.toMatch(/function formatRateForMeta\b/);
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
    expect(hasHandRolledLadder(src)).toBe(false);
  });
});
