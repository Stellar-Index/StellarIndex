import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { dirname, join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

// Explorer pages share one top offset so breadcrumbs don't jump between
// routes. Prose pages ([&>*]:max-w-*) and the landing pages keep their own.
const HERE = dirname(fileURLToPath(import.meta.url));
const APP = join(HERE, '..', 'app');

const ALLOWED = new Set([
  'space-y-6 py-8',
  'max-w-4xl space-y-6 py-8',
  'space-y-8 pb-10', // /sdex: continues ProtocolView's container below it
  'text-ink-muted py-16 text-sm', // inline fallback, not a page wrapper
]);
const EXEMPT_FILES = new Set([
  'page.tsx',
  'pricing/page.tsx',
  'dev/primitives/page.tsx',
  'sdk/page.tsx',
]);

function walk(dir: string, out: string[] = []): string[] {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) walk(p, out);
    else if (p.endsWith('.tsx') && !p.includes('.test.')) out.push(p);
  }
  return out;
}

describe('page rhythm', () => {
  it('explorer page wrappers use the shared vertical offset', () => {
    const drift: string[] = [];
    for (const file of walk(APP)) {
      const rel = relative(APP, file);
      if (EXEMPT_FILES.has(rel)) continue;
      for (const m of readFileSync(file, 'utf8').matchAll(
        /<Container className="([^"]*)"/g,
      )) {
        const cls = m[1];
        if (cls.includes('[&>*]:max-w')) continue;
        if (!ALLOWED.has(cls)) drift.push(`${rel}: ${cls}`);
      }
    }
    expect(drift).toEqual([]);
  });
});
