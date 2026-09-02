// Guard: no page in the static export may skip a heading level (#486).
//
// This is the check that produced the finding — 694 of the 1,691 built
// pages carrying a heading jumped h1 → h3 — turned into something that
// runs on every build instead of on the day someone thinks to look.
//
// HOW IT RUNS. `out/` only exists after `next build`, and CI runs
// `pnpm test` BEFORE `pnpm build`, so this file cannot rely on being
// scheduled at the right moment by the normal suite. package.json wires
// it as `postbuild`, which pnpm runs automatically after `pnpm build`
// (locally and in the web-explorer CI job) with
// HEADING_ORDER_REQUIRE_BUILD=1 set:
//
//   * postbuild (out/ present, required): scans every emitted .html.
//   * plain `pnpm test` (out/ absent): the scan is skipped — the pure
//     logic it uses is covered by heading-order.test.ts and the rendered
//     components by app/heading-order.a11y.test.tsx, so nothing here is
//     the only thing standing between a regression and main.
//   * postbuild with out/ missing: HARD FAIL. A gate that reports
//     "clean" because it never ran is worse than no gate.
//
// The scan is deliberately over the emitted HTML rather than a route
// list: it sees whatever the build actually shipped, including the
// per-entity pages (~500 assets, ~100 issuers) where the defect lived.
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';

import { describe, expect, it } from 'vitest';

import {
  extractHeadings,
  formatOutline,
  formatViolation,
  headingOrderViolations,
} from './heading-order';

const OUT_DIR = join(__dirname, '..', '..', 'out');

/**
 * Routes whose outline legitimately skips a level, with the reason.
 * Empty by design: as of #486 every skipping page was a real defect. A
 * new entry needs a reason that survives review — "the page has no
 * h2-level section" is not one (give the section a heading, or make the
 * panel that IS the section an h2), and neither is "hard to fix".
 */
const EXEMPT: ReadonlyMap<string, string> = new Map();

function htmlFiles(dir: string, found: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) htmlFiles(full, found);
    else if (entry.endsWith('.html')) found.push(full);
  }
  return found;
}

const built = existsSync(OUT_DIR);
const required = process.env.HEADING_ORDER_REQUIRE_BUILD === '1';

describe('built static export: heading order (WCAG 1.3.1)', () => {
  it('has a static export to scan when run as postbuild', () => {
    if (!required) {
      // Informational: `pnpm test` on its own never builds.
      expect(required).toBe(false);
      return;
    }
    expect(
      built,
      `HEADING_ORDER_REQUIRE_BUILD=1 but ${OUT_DIR} does not exist — the ` +
        'postbuild guard did not scan anything. Run it after `next build`.',
    ).toBe(true);
  });

  it.runIf(built)('no page skips a heading level', () => {
    const files = htmlFiles(OUT_DIR);
    const failures: string[] = [];
    let withHeadings = 0;
    let exempted = 0;

    for (const file of files) {
      const route = relative(OUT_DIR, file);
      const headings = extractHeadings(readFileSync(file, 'utf8'));
      if (headings.length === 0) continue;
      withHeadings += 1;
      const violations = headingOrderViolations(headings);
      if (violations.length === 0) continue;
      if (EXEMPT.has(route)) {
        exempted += 1;
        continue;
      }
      failures.push(
        `${route}: ${violations.map(formatViolation).join(', ')}\n` +
          `    outline: ${formatOutline(headings)}`,
      );
    }

    // Self-accounting: a scan that found no files is a scan that did not
    // run. The floor is well under the CI stub build's page count (490
    // files / 458 with headings, vs ~2,363 / 1,691 on a live-API build)
    // so it fails on an empty/mis-pathed out/ without being brittle.
    // `postbuild` passes --reporter=verbose precisely so this line is
    // VISIBLE on a pass: vitest 4 hides a passing test's console output,
    // which is how a gate ends up reporting "clean" by printing nothing.
    console.log(
      `[heading-order] scanned ${files.length} html files, ` +
        `${withHeadings} with headings, ${failures.length} violating, ` +
        `${exempted} exempted`,
    );
    expect(
      files.length,
      `no .html found under ${OUT_DIR} — the scan did not run`,
    ).toBeGreaterThan(20);
    expect(withHeadings, 'no built page had a heading at all').toBeGreaterThan(
      20,
    );

    expect(
      failures,
      `${failures.length} of ${withHeadings} built pages with headings skip a ` +
        'level (WCAG 1.3.1 heading-order, #486). Each line is route: skip. ' +
        'A component whose title should be a page-level section takes ' +
        'headingLevel={2} (see ui/Card, reveal/Panel, ui/Feedback).\n' +
        failures.slice(0, 25).join('\n'),
    ).toEqual([]);
  });
});
