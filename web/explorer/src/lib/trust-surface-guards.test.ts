import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join } from 'node:path';

import { describe, expect, it } from 'vitest';

/**
 * TRUST-SURFACE GUARD PACK
 *
 * A sibling of `network-hardcodes.test.ts` and
 * `fec-consolidation-guards.test.ts`, deliberately NOT folded into the
 * latter: that pack's header scopes it to the 2026-08-24 FEC
 * consolidation classes, and widening it silently would make its stated
 * scope false.
 *
 * What these guards share is a failure mode, not a subject. Each covers a
 * rendering obligation that is trust-critical, enforced by convention at
 * several call sites, and silently absent at one — because there is no
 * chokepoint and nothing derives the call-site set from the source. That
 * is how all of the following shipped:
 *
 *   - the scam callout existed on 1 of 2 asset-detail render paths, and
 *     the path missing it was the LONG-TAIL one (wave-D EXR-01);
 *   - the SEC-10 image-host gate existed at 2 of 3 `<img>` sites.
 *
 * Enumerating from `src/` at test time is the point: a fourth site fails
 * on the day it is written, rather than at the next audit.
 */

const SRC = join(__dirname, '..');

/**
 * Remove block and line comments, so a guard matching on code idioms
 * cannot be tripped (or silenced) by prose. Deliberately crude — it is
 * good enough to keep a doc comment from reading as code, and a guard
 * that needs a real parser is a guard aimed at the wrong thing.
 */
function stripComments(body: string): string {
  return body.replace(/\/\*[\s\S]*?\*\//g, '').replace(/\/\/[^\n]*/g, '');
}

/** Every non-test source file under src/, as [repo-relative path, contents]. */
function sourceFiles(): Array<[string, string]> {
  const out: Array<[string, string]> = [];
  const walk = (dir: string) => {
    for (const entry of readdirSync(dir)) {
      const full = join(dir, entry);
      if (statSync(full).isDirectory()) {
        if (entry === 'node_modules' || entry === '.next') continue;
        walk(full);
        continue;
      }
      if (!/\.tsx?$/.test(entry)) continue;
      if (/\.(test|spec)\.tsx?$/.test(entry)) continue;
      out.push([full.slice(SRC.length + 1), readFileSync(full, 'utf8')]);
    }
  };
  walk(SRC);
  return out;
}

describe('trust-surface guards', () => {
  it('every file rendering an <img> validates the URL host first (SEC-10)', () => {
    // An issuer-controlled image URL is attacker-authorable. The repo's
    // answer is isSafePublicImageUrl (lib/safe-domain.ts), applied at the
    // render site. Deriving the site list from source is what makes this
    // a guard rather than a spot check: the predicate was applied at two
    // of three <img> sites, and nothing said which three.
    const offenders = sourceFiles()
      .filter(([, body]) => /<img[\s>]/.test(body))
      .filter(([, body]) => !body.includes('isSafePublicImageUrl'))
      .map(([path]) => path)
      .sort();
    expect(offenders).toEqual([]);
  });

  it('every asset detail view renders the scam callout', () => {
    // /assets/[slug] has TWO render paths — the build-time pre-render for
    // the top 500, and the client shell for everything else. Both read
    // the same /v1/assets/{id} payload, carrying the same directory
    // flags. Only one showed the warning, and it was not the one serving
    // the long tail.
    //
    // Subject set: the views under app/assets/[slug]/ that render a whole
    // asset page. Identified by their own consumption of the detail
    // payload's directory fields OR by being one of the two known page
    // entry points — kept explicit rather than heuristic, because a
    // wrong subject set is worse than none.
    const views = ['app/assets/[slug]/page.tsx', 'app/assets/[slug]/AssetPathView.tsx'];
    const files = new Map(sourceFiles());
    const offenders = views.filter((v) => {
      const body = files.get(v);
      // A missing file means the view was renamed; fail loudly rather
      // than silently passing on an empty subject set.
      if (body === undefined) return true;
      return !body.includes('AssetScamCallout');
    });
    expect(offenders).toEqual([]);
  });

  it('no /assets/ href is built from a code-truncated canonical id', () => {
    // A classic asset_id is CODE-GISSUER…, and the bare code is
    // AMBIGUOUS: every USDC-alike shares /assets/USDC. A link built by
    // slicing at the dash can therefore land the user on a DIFFERENT
    // issuer's asset than the row they clicked — including resolving a
    // scam issuer's token to the legitimate one's page, or the reverse
    // (wave-D EXR-02). markets/[pair] already took this decision
    // (AM-09); AssetLink was the straggler.
    //
    // Matches the truncation idiom rather than the href, because the
    // slug is usually computed a few lines above the <Link>. Display
    // helpers are unaffected: shortAssetText is a deliberately short
    // LABEL and is not routed through here.
    // SCOPE, stated honestly: this covers the slug-RESOLUTION helpers —
    // the files that decide what an /assets/ link points AT — and not
    // every file in the tree.
    //
    // An earlier version of this comment claimed the broader property.
    // It does not hold, and cannot with a regex: the defect shape is
    // "a truncated value is returned from a resolver, then used as an
    // href several call-frames away", so proving it repo-wide needs
    // dataflow analysis, not text matching. Widening the file filter
    // instead just flags co-occurrence — it reports
    // markets/[pair]/page.tsx, which is CORRECT code (it truncates for
    // the label and hrefs the full canonical id, the AM-09 decision).
    //
    // A guard that flags correct code gets disabled, so this stays
    // narrow and truthful. The seven other truncation sites were
    // checked by hand and are label-only.
    const truncation = /\.slice\(\s*0\s*,\s*(dashIx|i|idx|dash)\s*\)/;
    const offenders = sourceFiles()
      .filter(([path]) => /AssetLink|assetSlug/.test(path))
      // Strip comments first. A guard that a COMMENT can trip is a guard
      // people learn to work around by rewording prose, which is how a
      // guard stops meaning anything — and this one tripped on the very
      // comment explaining the fix.
      .filter(([, body]) => truncation.test(stripComments(body)))
      .map(([path]) => path)
      .sort();
    expect(offenders).toEqual([]);
  });
});
