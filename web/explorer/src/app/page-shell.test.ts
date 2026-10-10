// Guard: every page renders the shared PageHeader, directly or through the
// local component it renders (two import levels), so a new page cannot ship
// without the common breadcrumbs + title shell.
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';

import { describe, expect, it, vi } from 'vitest';

import { PANEL_LABEL_CLASS, SECTION_HEADING_CLASS } from '@/components/ui/Page';

vi.setConfig({ testTimeout: 30_000 });

const SRC = join(__dirname, '..');
const APP = __dirname;

/** Pages that intentionally have no PageHeader. One-line reason each. */
const EXEMPT: Record<string, string> = {
  'page.tsx': 'home page has its own hero',
  'auth/callback/page.tsx':
    'transient spinner while the OAuth callback completes',
};

function pages(dir: string): string[] {
  const out: string[] = [];
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) out.push(...pages(p));
    else if (name === 'page.tsx') out.push(p);
  }
  return out;
}

function resolveImport(from: string, spec: string): string | null {
  let base: string;
  if (spec.startsWith('@/')) base = join(SRC, spec.slice(2));
  else if (spec.startsWith('.')) base = resolve(dirname(from), spec);
  else return null;
  for (const c of [`${base}.tsx`, join(base, 'index.tsx'), `${base}.ts`]) {
    if (existsSync(c) && statSync(c).isFile()) return c;
  }
  return null;
}

function rendersShell(file: string, depth = 2): boolean {
  const src = readFileSync(file, 'utf8');
  if (/\bPageHeader\b/.test(src)) return true;
  if (depth === 0) return false;
  const specs = [...src.matchAll(/from\s+['"]([^'"]+)['"]/g)].map((m) => m[1]);
  return specs.some((s) => {
    const f = resolveImport(file, s);
    return f !== null && rendersShell(f, depth - 1);
  });
}

function sources(dir: string): string[] {
  const out: string[] = [];
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) out.push(...sources(p));
    else if (name.endsWith('.tsx') && !name.endsWith('.test.tsx')) out.push(p);
  }
  return out;
}

function onlyRedirects(src: string): boolean {
  return (
    /\b(permanentRedirect|redirect)\(/.test(src) &&
    !/<[A-Z][A-Za-z]*[\s/>]/.test(src)
  );
}

describe('page shell', () => {
  it('every page renders PageHeader', () => {
    const skip = new Set(Object.keys(EXEMPT));
    const bad = pages(APP)
      .map((f) => relative(APP, f))
      .filter((rel) => !rel.startsWith('embed/') && !skip.has(rel))
      .filter((rel) => {
        const file = join(APP, rel);
        return (
          !onlyRedirects(readFileSync(file, 'utf8')) && !rendersShell(file)
        );
      });
    expect(
      bad,
      'pages without PageHeader (add it, or EXEMPT with a reason)',
    ).toEqual([]);
  });

  it('allowlist entries still exist', () => {
    for (const rel of Object.keys(EXEMPT)) {
      expect(existsSync(join(APP, rel)), rel).toBe(true);
    }
  });

  it('every section <h2> uses the shared heading style', () => {
    const bad: string[] = [];
    for (const f of sources(APP)) {
      for (const m of readFileSync(f, 'utf8').matchAll(
        /<h2 className="([^"]*)"/g,
      )) {
        const cls = m[1].split(/\s+/);
        const ok =
          SECTION_HEADING_CLASS.split(' ').every((c) => cls.includes(c)) &&
          !cls.some((c) =>
            /^(uppercase|tracking-|text-(xs|sm|base|lg|\d?xl)$)/.test(c),
          );
        if (!ok) bad.push(`${relative(APP, f)}: ${m[1]}`);
      }
    }
    expect(bad).toEqual([]);
  });

  it('every uppercase panel <h3> uses the shared label style', () => {
    // Colour may carry meaning (text-up); size, weight and tracking may not vary.
    const want = PANEL_LABEL_CLASS.split(' ').filter(
      (c) => c !== 'text-ink-muted',
    );
    const bad: string[] = [];
    for (const f of sources(APP)) {
      for (const m of readFileSync(f, 'utf8').matchAll(
        /<h3 className="([^"]*)"/g,
      )) {
        const cls = m[1].split(/\s+/);
        if (!cls.includes('uppercase')) continue;
        const ok =
          want.every((c) => cls.includes(c)) &&
          !cls.some((c) =>
            /^(text-(xs|sm|base|\[10px\])|font-medium)$/.test(c),
          );
        if (!ok) bad.push(`${relative(APP, f)}: ${m[1]}`);
      }
    }
    expect(bad).toEqual([]);
  });
});
