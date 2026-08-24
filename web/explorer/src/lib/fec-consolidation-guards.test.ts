import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { dirname, join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

// FEC audit 2026-08-24 guard pack. The audit's meta-finding (A2-07/A5-10):
// only guarded consolidation classes hold — every unguarded class kept
// forking. These are repo-WALK guards (A5-11's lesson: fixed file lists
// only bite on an exact replay of the last regression; a 5th price table
// or a renamed fork walks straight past them). Each block names its
// finding; extend the allowlist ONLY with a reviewed reason.

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

describe('FEC guards (repo-walk)', () => {
  // A5-12 / UXP-26: no scientific notation anywhere a price could render.
  // 2026-08-06 operator decision; F-A4-03 removed the last sanctioned
  // exception (DepthChart), so the allowlist is empty.
  it('no .toExponential( call sites exist in src', () => {
    const offenders = sources
      .filter((f) => /\.toExponential\(/.test(f.text))
      .map((f) => f.rel);
    expect(offenders).toEqual([]);
  });

  // A5-11: importer-allowlist inversion of the LastPriceCell guard. Any
  // NEW file importing formatPairPrice (e.g. a 5th venue price table)
  // must either use the shared LastPriceCell or be reviewed onto this
  // list. lib/format.ts defines it; the four non-cell importers use it
  // for stat lines, reviewed 2026-08-24.
  it('formatPairPrice importers are exactly the reviewed set', () => {
    const allowed = new Set([
      'lib/format.ts',
      'components/LastPriceCell.tsx',
      'app/exchanges/ExchangesView.tsx',
      'app/markets/[pair]/page.tsx',
      'app/sources/[name]/page.tsx',
      'app/assets/[slug]/LiquidityTabPanel.tsx',
    ]);
    const importers = sources
      .filter((f) => f.text.includes('formatPairPrice'))
      .map((f) => f.rel);
    const unexpected = importers.filter((r) => !allowed.has(r));
    expect(unexpected).toEqual([]);
  });

  // A5-04: the CI-stub predicate must have ONE home. Seven verbatim forks
  // existed; if the placeholder-URL sentinel ever changes, forks silently
  // keep the old sentinel and CI static export breaks.
  it('isCIStub is declared only in lib/buildFetch.ts', () => {
    const offenders = sources
      .filter((f) => /(const|function)\s+isCIStub/.test(f.text))
      .map((f) => f.rel)
      .filter((r) => r !== 'lib/buildFetch.ts');
    expect(offenders).toEqual([]);
  });

  // A2-04: loading placeholders use the Skeleton primitive, not literal
  // animate-pulse divs. Allowlist: the primitive itself and the /status
  // LIVE-indicator dot (a status signal, not a loading skeleton).
  it('no hand-rolled animate-pulse skeletons outside the primitive', () => {
    const allowed = new Set([
      'components/ui/Feedback.tsx',
      'app/status/StatusPageClient.tsx',
    ]);
    const offenders = sources
      .filter((f) => /animate-pulse[^-]/.test(f.text))
      .map((f) => f.rel)
      .filter((r) => !allowed.has(r));
    expect(offenders).toEqual([]);
  });

  // A3-F1: relative-time / duration bucket math has ONE home. 15 forks
  // existed; three had real rendering bugs ("-1s ago", raw-ISO leak,
  // "NaNd ago"). Recorded variants: ConvertPair's absolute-time switch and
  // AnomaliesFeed's 1.5x-unit hysteresis (deliberate, reviewed 2026-08-24).
  it('relative-time bucket math lives only in lib/format.ts (+ recorded variants)', () => {
    const allowed = new Set([
      'lib/format.ts',
      'app/convert/[from]/[to]/ConvertPair.tsx',
      'app/anomalies/AnomaliesFeed.tsx',
    ]);
    const offenders = sources
      .filter(
        (f) =>
          /Date\.now\(\) - /.test(f.text) && /\/ 3600|\/ 86_?400/.test(f.text),
      )
      .map((f) => f.rel)
      .filter((r) => !allowed.has(r));
    expect(offenders).toEqual([]);
  });

  // A6-1: one SSE layer. A private useLedgerStream fork in the status page
  // held a second unshared connection to the same URL the sidebar badge
  // already streams (2 of the 20-per-IP cap) with a doubled stale window.
  it('EventSource is constructed only in the stream multiplexer', () => {
    const offenders = sources
      .filter((f) => f.text.includes('new EventSource('))
      .map((f) => f.rel)
      .filter((r) => r !== 'lib/live/streams.ts');
    expect(offenders).toEqual([]);
  });

  it('useLedgerStream / useLiveClock are declared only in lib/live/hooks.ts', () => {
    const offenders = sources
      .filter((f) => /(const|function)\s+(useLedgerStream|useLiveClock)\b/.test(f.text))
      .map((f) => f.rel)
      .filter((r) => r !== 'lib/live/hooks.ts');
    expect(offenders).toEqual([]);
  });

  // A3-F2b: shortAsset display labels have one canonical
  // (lib/asset-label.shortAssetText). 13 forks folded 2026-08-24; the two
  // allowlisted files are the recorded issuer-parenthetical VARIANTS
  // (per-asset pages where the bare code is ambiguous).
  it('shortAsset-style helpers are defined only in the canonical + recorded variants', () => {
    const allowed = new Set([
      'lib/asset-label.ts',
      'app/assets/[slug]/MarketsTabPanel.tsx',
      'app/assets/[slug]/page.tsx',
    ]);
    const offenders = sources
      .filter((f) =>
        /(const|function)\s+short(Asset|Code|Counterparty|Label)\b/.test(f.text),
      )
      .map((f) => f.rel)
      .filter((r) => !allowed.has(r));
    expect(offenders).toEqual([]);
  });

  // A3-F3/F6.1: pager state machine + sort pill have one home each.
  it('cursor-stack pagination lives only in lib/useCursorPager.ts', () => {
    const offenders = sources
      .filter(
        (f) =>
          f.text.includes('cursorStack') || f.text.includes('setCursorStack'),
      )
      .map((f) => f.rel)
      .filter((r) => r !== 'lib/useCursorPager.ts');
    expect(offenders).toEqual([]);
  });

  it('SortPill is defined only in components/SortPill.tsx', () => {
    const offenders = sources
      .filter((f) => /(const|function)\s+SortPill\b/.test(f.text))
      .map((f) => f.rel)
      .filter((r) => r !== 'components/SortPill.tsx');
    expect(offenders).toEqual([]);
  });

  // A3-F8: the venue markets table is ONE component. PoolsTable and
  // PairsTable were a 300-line whole-component fork that had already
  // drifted once (LastPriceCell); they must stay thin wrappers.
  it('PoolsTable and PairsTable stay thin wrappers over VenueMarketsTable', () => {
    for (const rel of [
      'app/dexes/[source]/PoolsTable.tsx',
      'app/exchanges/[name]/PairsTable.tsx',
    ]) {
      const f = sources.find((x) => x.rel === rel)!;
      expect(f.text).toContain('VenueMarketsTable');
      expect(f.text).not.toMatch(/(const|function)\s+(Th|Td|SortPill)\b/);
      expect(f.text).not.toContain('useQuery');
    }
  });

  // A3-F7: clipboard behavior has one home (the ui hook). 5 forks existed;
  // each was missing at least one of: unmount-safe reset, propagation
  // guards (copy inside a row <Link> must not navigate), try/catch.
  it('navigator.clipboard is touched only inside components/ui', () => {
    const offenders = sources
      .filter((f) => f.text.includes('navigator.clipboard'))
      .map((f) => f.rel)
      .filter((r) => !r.startsWith('components/ui/'));
    expect(offenders).toEqual([]);
  });

  // A3-F9: the markdown inline tokenizer (the actual top jscpd clone,
  // triplicated + drifted) lives only in lib/markdown.tsx.
  it('the inline-markdown tokenizer pattern exists only in lib/markdown.tsx', () => {
    const offenders = sources
      .filter((f) => f.text.includes('/^\\*\\*([^*]+)\\*\\*/'))
      .map((f) => f.rel)
      .filter((r) => r !== 'lib/markdown.tsx');
    expect(offenders).toEqual([]);
  });

  // A2-06/A1-1 adjunct: truncateMiddle's canonical home is server-safe
  // lib/format.ts; ui/Mono re-exports for client back-compat. No third
  // definition may appear.
  it('truncateMiddle is defined only in lib/format.ts', () => {
    const offenders = sources
      .filter((f) => /(const|function)\s+truncateMiddle\b/.test(f.text))
      .map((f) => f.rel)
      .filter((r) => r !== 'lib/format.ts');
    expect(offenders).toEqual([]);
  });
});
