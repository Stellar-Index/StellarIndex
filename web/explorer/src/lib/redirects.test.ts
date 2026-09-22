import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { describe, expect, it } from 'vitest';

/**
 * T247: `public/_redirects` only ever lists the trailing-slash form of the
 * `/currencies/*` aliases, on the premise ("next.js 308 handles bare form",
 * comments at the top of the crypto/fiat currency blocks) that a live
 * Next.js server 308-redirects a bare-form request to the trailing-slash
 * one before Cloudflare Pages' `_redirects` ever sees it.
 *
 * That premise is false for this deployment: `next.config.mjs` sets
 * `output: 'export'` by default (no OPEN_NEXT), i.e. a static export served
 * directly by Cloudflare Pages with no live Next.js process to issue that
 * 308. `/currencies/[ticker]` also isn't even a page in this tree (no
 * `src/app/currencies` route exists), so nothing here is "the explorer's
 * own route" the way the `/sponsor`/`/creator` comment distinguishes.
 *
 * Net effect: a bare-form request like `/currencies/xlm` (no trailing
 * slash — exactly what a typed URL or an external share produces) matches
 * no rule in `_redirects` and 404s on Cloudflare Pages.
 *
 * This test parses the real `_redirects` file with a minimal CF
 * Pages-shaped matcher (first-match-wins, `*` splat, `:name` capture) and
 * follows the redirect chain for a handful of real slugs, bare-form.
 */

type Rule = { pattern: string; dest: string; status: string };

function parseRedirects(text: string): Rule[] {
  return text
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line.length > 0 && !line.startsWith('#'))
    .map((line) => {
      const [pattern, dest, status] = line.split(/\s+/);
      return { pattern, dest, status };
    });
}

function compile(pattern: string): { regex: RegExp; names: string[] } {
  const names: string[] = [];
  const regexStr = pattern
    .split('/')
    .map((seg) => {
      if (seg === '*') {
        names.push('splat');
        return '(.*)';
      }
      if (seg.startsWith(':')) {
        names.push(seg.slice(1));
        return '([^/]+)';
      }
      return seg.replace(/[.+?^${}()|[\]\\]/g, '\\$&');
    })
    .join('/');
  return { regex: new RegExp(`^${regexStr}$`), names };
}

function applyDest(dest: string, names: string[], match: RegExpMatchArray): string {
  let result = dest;
  names.forEach((name, i) => {
    result = result.replace(new RegExp(`:${name}\\b`), match[i + 1] ?? '');
  });
  return result;
}

function matchOne(path: string, rules: Rule[]): Rule | null {
  for (const rule of rules) {
    const { regex, names } = compile(rule.pattern);
    const m = path.match(regex);
    if (m) {
      return { pattern: path, dest: applyDest(rule.dest, names, m), status: rule.status };
    }
  }
  return null;
}

/**
 * Follows the CF Pages redirect chain (first-match-wins per hop). `resolved`
 * is false only when the ORIGINAL path matched no rule at all — the 404
 * case for a path (like `/currencies/*`) that names no real static page.
 * Landing on a path that itself matches no further rule, after at least one
 * hop matched, is the normal terminal case (the destination page).
 */
function follow(
  path: string,
  rules: Rule[],
  maxHops = 5,
): { resolved: boolean; finalPath: string } {
  let current = path;
  let matchedAtLeastOnce = false;
  for (let hop = 0; hop < maxHops; hop++) {
    const rule = matchOne(current, rules);
    if (!rule) return { resolved: matchedAtLeastOnce, finalPath: current };
    matchedAtLeastOnce = true;
    if (rule.dest.startsWith('http')) return { resolved: true, finalPath: rule.dest };
    current = rule.dest;
  }
  return { resolved: true, finalPath: current };
}

const REDIRECTS_PATH = join(__dirname, '../../public/_redirects');
const rules = parseRedirects(readFileSync(REDIRECTS_PATH, 'utf8'));

describe('_redirects: /currencies bare-form aliases (T247)', () => {
  it('still resolves the trailing-slash form (no regression)', () => {
    expect(follow('/currencies/xlm/', rules)).toEqual({
      resolved: true,
      finalPath: '/assets/XLM/',
    });
  });

  it('resolves a bare-form crypto alias the same as its trailing-slash form', () => {
    expect(follow('/currencies/xlm', rules)).toEqual({
      resolved: true,
      finalPath: '/assets/XLM/',
    });
  });

  it('resolves a bare-form fiat alias the same as its trailing-slash form', () => {
    expect(follow('/currencies/usd', rules)).toEqual({
      resolved: true,
      finalPath: '/external/assets/us-dollar/',
    });
  });

  it('404s cleanly (no redirect loop) for a slug the table does not know', () => {
    // A splat-based fallback (`/currencies/*`) would also re-match its own
    // trailing-slash output on the next hop, looping and growing an extra
    // `/` each time instead of terminating. `:slug` matches one segment
    // with no trailing slash, so the second hop (already slashed) matches
    // no further rule and settles.
    const result = follow('/currencies/doge', rules);
    expect(result.finalPath.endsWith('//')).toBe(false);
    expect(result.finalPath).toBe('/currencies/doge/');
  });
});
