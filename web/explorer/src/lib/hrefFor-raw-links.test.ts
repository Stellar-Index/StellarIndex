// Guard: 19 sites built /sources/, /exchanges/, /protocols/ and
// /status/incident/ hrefs by interpolating an API-supplied identifier
// (source name, protocol slug, incident filename) raw into a template
// literal, while 13 sibling sites for the SAME fields remembered
// encodeURIComponent. Nothing in the OpenAPI schema constrains these
// fields to a URL-safe charset, so an operator-controlled name containing
// `/`, `?`, `#` or space breaks the link. Fix direction was "route every
// site through hrefFor()"; this is the chokepoint check that a future
// callsite can't quietly skip it and go back to raw interpolation.
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';

import { describe, expect, it } from 'vitest';

const SRC = join(__dirname, '..');

const isTest = (rel: string) => /\.test\.[jt]sx?$/.test(rel);

function walk(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    if (entry === 'node_modules' || entry === '.next') continue;
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) walk(full, out);
    else if (/\.(ts|tsx)$/.test(entry)) out.push(full);
  }
  return out;
}

// A raw href template for one of the four prefixes, NOT wrapped in
// encodeURIComponent and NOT routed through hrefFor.*(...).
const RAW_HREF =
  /href=\{`\/(sources|exchanges|protocols|status\/incident)\/\$\{(?!\s*encodeURIComponent\()[^}]+\}/;
// The same raw path as an absolute URL, canonical or sitemap entry (an
// optional `${origin}` prefix allowed), which crawlers follow like a link.
const RAW_URL =
  /(url:\s*|canonical:\s*|siteURL\()`(\$\{[^}]+\})?\/(sources|exchanges|protocols|status\/incident)\/\$\{(?!\s*encodeURIComponent\()[^}]+\}/;

function offenders(): string[] {
  const bad: string[] = [];
  for (const file of walk(SRC)) {
    const rel = relative(SRC, file).split('\\').join('/');
    if (rel === 'lib/hrefFor.ts' || isTest(rel)) continue;
    const text = readFileSync(file, 'utf8');
    if (RAW_HREF.test(text) || RAW_URL.test(text)) bad.push(rel);
  }
  return bad.sort();
}

describe('no raw (unencoded, non-hrefFor) source/exchange/protocol/incident links', () => {
  it('every /sources|exchanges|protocols|status/incident href is encoded or routed through hrefFor', () => {
    expect(offenders()).toEqual([]);
  });

  it('the url/canonical/sitemap pattern catches a raw interpolation and passes an encoded one', () => {
    expect(RAW_URL.test('url: `${origin}/protocols/${p.name}`')).toBe(true);
    expect(RAW_URL.test('canonical: `/status/incident/${slug}`')).toBe(true);
    expect(RAW_URL.test('siteURL(`/sources/${s.name}`)')).toBe(true);
    expect(
      RAW_URL.test('canonical: `/status/incident/${encodeURIComponent(slug)}`'),
    ).toBe(false);
    expect(RAW_URL.test('asExample(`/v1/protocols/${name}`)')).toBe(false);
  });
});
