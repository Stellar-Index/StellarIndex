import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { dirname, join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

// One query key must always cache one data shape. Two queryFns that fetch the
// same path but transform it differently (one keeps the envelope, one unwraps
// `.data`) crash whichever page mounts second after client-side navigation.
// Sites with different queryFns must end their key with distinct string tags.
const SRC = join(dirname(fileURLToPath(import.meta.url)), '..');

function walk(dir: string, out: string[] = []): string[] {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) walk(p, out);
    else if (/\.tsx?$/.test(p) && !p.includes('.test.')) out.push(p);
  }
  return out;
}

const OPEN = '([{';
const CLOSE = ')]}';

// The balanced expression starting at i, ending before a top-level `,` or closer.
function exprAt(s: string, i: number): string {
  let depth = 0;
  let j = i;
  for (; j < s.length; j++) {
    const c = s[j];
    if (OPEN.includes(c)) depth++;
    else if (CLOSE.includes(c)) {
      if (depth === 0) break;
      depth--;
    } else if (c === ',' && depth === 0) break;
  }
  return s.slice(i, j);
}

// The object literal that encloses position i.
function enclosingObject(s: string, i: number): string {
  let depth = 0;
  let start = i;
  for (; start >= 0; start--) {
    const c = s[start];
    if (CLOSE.includes(c)) depth++;
    else if (OPEN.includes(c)) {
      if (depth === 0 && c === '{') break;
      if (depth > 0) depth--;
    }
  }
  let end = start;
  for (depth = 0; end < s.length; end++) {
    if (OPEN.includes(s[end])) depth++;
    else if (CLOSE.includes(s[end]) && --depth === 0) break;
  }
  return s.slice(start, end + 1);
}

const normalise = (code: string) =>
  code
    .replace(/\/\*[\s\S]*?\*\//g, '')
    .replace(/\/\/[^\n]*/g, '')
    .replace(/\s+/g, '')
    .replace(/,([)\]}])/g, '$1');

// Splits a key array literal's text into its top-level elements.
function elements(arr: string): string[] {
  const inner = arr.trim().slice(1, -1);
  const out: string[] = [];
  let i = 0;
  while (i < inner.length) {
    const e = exprAt(inner, i);
    if (e.trim()) out.push(e.trim());
    i += e.length + 1;
  }
  return out;
}

type Site = { file: string; path: string; tag: string; fn: string };

// The path literals a key head can hold: the literal itself, or every string
// literal assigned to that name in the file (a const, ternary or default param).
function headPaths(s: string, head: string): string[] {
  if (/^'[^']*'$/.test(head)) return [head.slice(1, -1)];
  if (!/^\w+$/.test(head)) return [];
  const paths: string[] = [];
  for (const m of s.matchAll(new RegExp(`\\b${head}\\s*=\\s*([^;\\n]+)`, 'g')))
    for (const lit of m[1].matchAll(/'(\/[^']*)'/g)) paths.push(lit[1]);
  return paths;
}

const unresolved: string[] = [];

function sites(): Site[] {
  const out: Site[] = [];
  unresolved.length = 0;
  for (const file of walk(SRC)) {
    const s = readFileSync(file, 'utf8');
    for (const m of s.matchAll(/queryKey:\s*\[/g)) {
      const at = m.index + m[0].length - 1;
      const els = elements(exprAt(s, at) + ']');
      if (els.length === 0) continue;
      const last = els[els.length - 1];
      const tag = els.length > 1 && /^'[^']*'$/.test(last) ? last : '';
      const obj = enclosingObject(s, m.index);
      const fi = obj.indexOf('queryFn:');
      if (fi < 0) continue; // invalidate/cancel filters match keys, they cache nothing
      const fn = normalise(exprAt(obj, fi + 'queryFn:'.length));
      const paths = headPaths(s, els[0]);
      if (paths.length === 0)
        unresolved.push(`${relative(SRC, file)}: ${els[0]}`);
      for (const path of paths)
        out.push({ file: relative(SRC, file), path, tag, fn });
    }
  }
  return out;
}

describe('query key shapes', () => {
  it('finds the query sites', () => {
    expect(sites().length).toBeGreaterThan(100);
  });

  it('starts every query key with a path the check can read', () => {
    sites();
    expect(unresolved).toEqual([]);
  });

  it('never caches two shapes under one key', () => {
    const byKey = new Map<string, Map<string, string[]>>();
    for (const x of sites()) {
      const k = `${x.path} ${x.tag}`;
      const fns = byKey.get(k) ?? new Map<string, string[]>();
      fns.set(x.fn, [...(fns.get(x.fn) ?? []), x.file]);
      byKey.set(k, fns);
    }
    const clashes = [...byKey]
      .filter(([, fns]) => fns.size > 1)
      .map(([k, fns]) => `${k}: ${[...fns.values()].flat().join(', ')}`);
    expect(clashes).toEqual([]);
  });
});
