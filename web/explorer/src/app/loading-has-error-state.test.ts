// Guard: a component that shows a loading state for `isLoading || !data`
// must also read the query's error state. Otherwise a failed fetch leaves
// the loading text on screen indefinitely, indistinguishable from a slow page.
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';

import { describe, expect, it } from 'vitest';

const SRC = join(__dirname, '..');

function sources(dir: string): string[] {
  const out: string[] = [];
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) out.push(...sources(p));
    else if (/\.tsx$/.test(name) && !/\.test\.tsx$/.test(name)) out.push(p);
  }
  return out;
}

describe('data panels that render a loading state', () => {
  it('also handle the query error state', () => {
    const offenders = sources(SRC)
      .filter((f) => {
        const text = readFileSync(f, 'utf8');
        return (
          /isLoading\s*\|\|\s*!\w+/.test(text) &&
          !/\bisError\b|\bisPending\b|\berror\b/.test(text)
        );
      })
      .map((f) => relative(SRC, f));
    expect(offenders).toEqual([]);
  });
});
