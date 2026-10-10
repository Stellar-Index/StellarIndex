import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { dirname, join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

// Consolidation guard pack: only guarded classes stay consolidated. These
// are repo-WALK guards (fixed file lists only catch an exact replay; a new
// price table or a renamed fork walks past them). Extend an allowlist ONLY
// with a reviewed reason.

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
  // No scientific notation anywhere a price could render.
  // Operator decision; no sanctioned exception remains, so the
  // allowlist is empty.
  it('no .toExponential( call sites exist in src', () => {
    const offenders = sources
      .filter((f) => /\.toExponential\(/.test(f.text))
      .map((f) => f.rel);
    expect(offenders).toEqual([]);
  });

  // Importer allowlist: any NEW file importing formatPairPrice (e.g. another
  // venue price table) must use the shared LastPriceCell or be reviewed onto
  // this list. lib/format.ts defines it.
  it('formatPairPrice importers are exactly the reviewed set', () => {
    const allowed = new Set([
      'lib/format.ts',
      'components/LastPriceCell.tsx',
      'app/exchanges/ExchangesView.tsx',
      'app/embed/currency/[ticker]/page.tsx',
      'app/markets/[pair]/page.tsx',
      'app/sources/[name]/page.tsx',
      'app/assets/[slug]/LiquidityTabPanel.tsx',
      'app/convert/[from]/[to]/page.tsx',
      'app/convert/[from]/[to]/ConvertLive.tsx',
      'app/convert/[from]/[to]/ConvertPair.tsx',
      'app/HomeTopMarkets.tsx',
      'app/HomeCurrencies.tsx',
      'app/anomalies/AnomaliesFeed.tsx',
    ]);
    const importers = sources
      .filter((f) => f.text.includes('formatPairPrice'))
      .map((f) => f.rel);
    const unexpected = importers.filter((r) => !allowed.has(r));
    expect(unexpected).toEqual([]);
  });

  // The CI-stub predicate must have ONE home: if the placeholder-URL sentinel
  // changes, a fork would keep the old one and break CI static export.
  it('isCIStub is declared only in lib/buildFetch.ts', () => {
    const offenders = sources
      .filter((f) => /(const|function)\s+isCIStub/.test(f.text))
      .map((f) => f.rel)
      .filter((r) => r !== 'lib/buildFetch.ts');
    expect(offenders).toEqual([]);
  });

  // Loading placeholders use the Skeleton primitive, not literal
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
  // AnomaliesFeed's 1.5x-unit hysteresis (deliberate).
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

  // One SSE layer: a private fork would hold a second unshared connection
  // to a URL already streamed (the per-IP cap is 20).
  it('EventSource is constructed only in the stream multiplexer', () => {
    const offenders = sources
      .filter((f) => f.text.includes('new EventSource('))
      .map((f) => f.rel)
      .filter((r) => r !== 'lib/live/streams.ts');
    expect(offenders).toEqual([]);
  });

  it('useLedgerStream / useLiveClock are declared only in lib/live/hooks.ts', () => {
    const offenders = sources
      .filter((f) =>
        /(const|function)\s+(useLedgerStream|useLiveClock)\b/.test(f.text),
      )
      .map((f) => f.rel)
      .filter((r) => r !== 'lib/live/hooks.ts');
    expect(offenders).toEqual([]);
  });

  // shortAsset display labels have one canonical
  // (lib/asset-label.shortAssetText); the two
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
        /(const|function)\s+short(Asset|Code|Counterparty|Label)\b/.test(
          f.text,
        ),
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

  // A cursor-paged board that follows ledger closes must gate the
  // follow on its first page, or every close re-runs the deeper page's
  // keyset query and reshuffles the rows under the reader.
  it('every paged ledger follow is gated on the pager atTip', () => {
    const offenders = sources
      .filter((f) => f.text.includes('useCursorPager('))
      .flatMap((f) =>
        [...f.text.matchAll(/useLedgerFollow\(([\s\S]*?)\);/g)]
          .filter((m) => !/\batTip\b/.test(m[1]))
          .map(() => f.rel),
      );
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

  // Breadcrumbs one-rule: the visible trail and its schema.org BreadcrumbList
  // render from the SAME Crumb[] (ui/Page's Breadcrumbs derives the LD via
  // lib/seo breadcrumbJsonLd). What can still regress is a hand-rolled fork
  // of either half.
  it('BreadcrumbList JSON-LD is built only by lib/seo breadcrumbJsonLd', () => {
    // Quoted-string form: hand-rolled LD must spell '@type': 'BreadcrumbList'
    // as a string literal; bare prose mentions in comments are fine.
    const offenders = sources
      .filter((f) => /['"`]BreadcrumbList['"`]/.test(f.text))
      .map((f) => f.rel)
      .filter((r) => r !== 'lib/seo.ts');
    expect(offenders).toEqual([]);
  });

  it('visible breadcrumb trails render only through the ui Breadcrumbs primitive', () => {
    // Any nav that self-identifies as a breadcrumb must BE the primitive
    // (a hand-built trail would skip the derived JSON-LD and restart the
    // UI/LD divergence). Excluded by scope: trails that don't declare the
    // landmark can't be grepped — the primitive is the sanctioned way to
    // get one, and A2's census re-runs catch stragglers.
    const offenders = sources
      .filter((f) => f.text.includes('aria-label="Breadcrumb"'))
      .map((f) => f.rel)
      .filter((r) => r !== 'components/ui/Page.tsx');
    expect(offenders).toEqual([]);
  });

  // A3-F6.2 (decision D7): aria-pressed toggle rows have exactly two
  // sanctioned homes — ui/Segmented (the in-card window/metric switch;
  // quiet bg-surface active style + WindowPills' donated a11y) and
  // SortPill (the recorded separate sibling). 7 hand-rolled rows with 4
  // disagreeing active styles were folded.
  it('aria-pressed toggles exist only in ui/Segmented and SortPill', () => {
    const allowed = new Set([
      'components/ui/Tabs.tsx',
      'components/SortPill.tsx',
    ]);
    const offenders = sources
      .filter((f) => f.text.includes('aria-pressed'))
      .map((f) => f.rel)
      .filter((r) => !allowed.has(r));
    expect(offenders).toEqual([]);
  });

  it('no ToggleGroup/WindowPills-style segmented forks are re-declared', () => {
    // The (const|function)\s+Name\b form matches the declaration however it
    // returns. BespokeSection's WindowPills is a thin Segmented wrapper
    // (maps WindowDays↔keys) with no button row of its own, covered by the
    // aria-pressed walk above.
    const offenders = sources
      .filter((f) =>
        /(const|function)\s+(ToggleGroup|WindowPills)\b/.test(f.text),
      )
      .map((f) => f.rel)
      .filter((r) => r !== 'app/protocols/[name]/BespokeSection.tsx');
    expect(offenders).toEqual([]);
  });

  // truncateMiddle's canonical home is server-safe
  // lib/format.ts; ui/Mono re-exports for client back-compat. No third
  // definition may appear.
  it('truncateMiddle is defined only in lib/format.ts', () => {
    const offenders = sources
      .filter((f) => /(const|function)\s+truncateMiddle\b/.test(f.text))
      .map((f) => f.rel)
      .filter((r) => r !== 'lib/format.ts');
    expect(offenders).toEqual([]);
  });

  // A raw `fetch(` in a build-time
  // (non-'use client') app/** file bypasses buildFetch.ts's fail-hard
  // retry contract — a single transient 429/5xx during static export is
  // then swallowed by a local try/catch and silently bakes a fallback or
  // empty page instead of retrying and failing the build. Routed
  // convert/[from]/[to]/page.tsx onto buildFetchData; the allowlist below
  // is the SAME defect on sibling routes this finding also named, not yet
  // migrated — extend it only by migrating a route off the allowlist, not
  // by adding a new one.
  it('build-time app/** pages fetch only through lib/buildFetch.ts', () => {
    const allowed = new Set([
      'app/embed/asset/[slug]/page.tsx',
      'app/embed/currency/[ticker]/page.tsx',
      'app/embed/pair/[pair]/page.tsx',
      'app/external/assets/[slug]/page.tsx',
      'app/assets/[slug]/LiquidityTabPanel.tsx',
    ]);
    const offenders = sources
      .filter((f) => f.rel.startsWith('app/'))
      .filter((f) => !/^'use client';/.test(f.text))
      .filter((f) => /\bfetch\(/.test(f.text))
      .map((f) => f.rel)
      .filter((r) => !allowed.has(r));
    expect(offenders).toEqual([]);
  });
});
