// Guard: every primary-nav page ships its frame in the STATIC document.
//
// This is the check that produced the finding — /transactions,
// /operations and /accounts each exported a body-less page (zero <h1>,
// an empty <main>) because the whole view sat inside
// `<Suspense fallback={null}>` and useSearchParams bailed the subtree
// out to client rendering, so the null is what the exporter baked.
// Turned into something that runs on every build instead of on the day
// someone thinks to view-source.
//
// HOW IT RUNS. Same wiring as heading-order.built.test.ts: `out/` only
// exists after `next build`, so package.json runs this as `postbuild`
// with NAV_SHELL_REQUIRE_BUILD=1 set. Without a build the scan is
// skipped (the parsing it uses is covered by nav-shell.test.ts); with
// the flag set and no `out/` it HARD FAILS, because a gate that reports
// clean by never running is worse than no gate.
//
// Scope is the site's primary navigation, read from the two components
// that render it — the console rail and the Network hub's grid into the
// chain-entity directories (/operations is reached only through the
// hub). These are the pages a first-time visitor lands on, the ones
// search engines crawl most, and the set that grows without anyone
// re-checking the export.
//
// The route set is the UNFILTERED literals. The build filters them by
// network (lib/network-routes) but still emits every page, and a route
// gated off the network being built renders its <h1> above the
// NetworkUnavailable state — so the rule holds on every network and
// this gate needs no network awareness. The deploy workflow builds one
// export per network; a mainnet build never renders the lean-net
// branch of a gated page, so a change there is proven by a testnet or
// futurenet build.
import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';

import { describe, expect, it } from 'vitest';

import { exportedHtmlPath, routesFromLiteral, staticFrame } from './nav-shell';

const OUT_DIR = join(__dirname, '..', '..', 'out');
const NAV_SOURCES: { file: string; literal: string }[] = [
  {
    file: join(__dirname, '..', 'components', 'nav', 'Sidebar.tsx'),
    literal: 'NAV',
  },
  {
    file: join(__dirname, '..', 'app', 'network', 'NetworkView.tsx'),
    literal: 'DEEPER',
  },
];

/**
 * The shortest real frame in the set is /operations (eyebrow + heading
 * + a two-line description, ~146 chars); a gated route's lean-net branch
 * (title + the NetworkUnavailable reason) is longer. The floor sits well
 * below both — a copy edit must not red the build — and far above the 0
 * an unresolved boundary leaves or the ~20 of a heading on its own, so
 * it fails on the defect without policing copy length.
 */
const MIN_FRAME_CHARS = 80;

/**
 * Nav routes whose export carries no `<h1>` for a reason OTHER than a
 * missing shell, named. This is not a place to park a page that ships
 * nothing: "the heading renders after the fetch" is the defect, and a
 * route stays on the frame-length check either way. An exemption covers
 * a MISSING `<h1>` only — two on an exempt route still fail.
 */
const EXEMPT: ReadonlyMap<string, string> = new Map([
  [
    '/sdex',
    'Its frame IS exported. The heading comes from the shared ' +
      'ProtocolView loading branch, which titles the page with a Panel ' +
      '(h3) — a heading-level defect across the /protocols/[name] ' +
      'family, not an empty static document.',
  ],
]);

const built = existsSync(OUT_DIR);
const required = process.env.NAV_SHELL_REQUIRE_BUILD === '1';

describe('built static export: primary-nav page shells', () => {
  it('has a static export to scan when run as postbuild', () => {
    if (!required) {
      // Informational: `pnpm test` on its own never builds.
      expect(required).toBe(false);
      return;
    }
    expect(
      built,
      `NAV_SHELL_REQUIRE_BUILD=1 but ${OUT_DIR} does not exist — the ` +
        'postbuild guard did not scan anything. Run it after `next build`.',
    ).toBe(true);
  });

  it.runIf(built)('every nav page exports an <h1> and its frame', () => {
    const routes = [
      ...new Set(
        NAV_SOURCES.flatMap((s) =>
          routesFromLiteral(readFileSync(s.file, 'utf8'), s.literal),
        ),
      ),
    ];
    const failures: string[] = [];
    const failed = new Set<string>();
    const fail = (route: string, why: string) => {
      failed.add(route);
      failures.push(`${route}: ${why}`);
    };
    let checked = 0;

    for (const route of routes) {
      const file = join(OUT_DIR, exportedHtmlPath(route));
      if (!existsSync(file)) {
        fail(route, `no ${exportedHtmlPath(route)} in the export`);
        continue;
      }
      checked += 1;
      const frame = staticFrame(readFileSync(file, 'utf8'));
      if (frame === null) {
        fail(route, 'exported page has no <main>');
        continue;
      }
      const exemptFromH1 = frame.h1Count === 0 && EXEMPT.has(route);
      if (frame.h1Count !== 1 && !exemptFromH1) {
        fail(route, `${frame.h1Count} <h1> in <main>, want exactly 1`);
      }
      if (frame.text.length < MIN_FRAME_CHARS) {
        fail(
          route,
          `<main> holds ${frame.text.length} chars of text ` +
            `(want >= ${MIN_FRAME_CHARS}) — ${JSON.stringify(frame.text)}`,
        );
      }
    }

    // Self-accounting: a scan that resolved no routes is a scan that did
    // not run. The floor is under the current 20 internal nav entries and
    // above nothing, so a renamed literal or a moved output path fails
    // loudly instead of passing empty. --reporter=verbose in `postbuild`
    // is what makes this line VISIBLE on a pass.
    console.log(
      `[nav-shell] scanned ${checked} of ${routes.length} nav routes, ` +
        `${failed.size} without a static frame, ${EXEMPT.size} exempted`,
    );
    expect(
      routes.length,
      `too few site-relative routes parsed out of ${NAV_SOURCES.map((s) => s.literal).join(' + ')} — the scan did not run`,
    ).toBeGreaterThan(15);

    expect(
      failures,
      'A primary-nav page exported without its own frame. Under ' +
        "output:'export' a `<Suspense fallback={null}>` around a view " +
        'that calls useSearchParams bakes the NULL: the page ships site ' +
        'chrome and an empty <main>. Keep the heading and description in ' +
        'the server component, put only the search-param-dependent ' +
        'subtree in Suspense, and give it a visible skeleton fallback.\n' +
        failures.join('\n'),
    ).toEqual([]);
  });
});
