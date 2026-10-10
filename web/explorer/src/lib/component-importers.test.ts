// Guard: every module under src/components has at least one importer in
// src/ outside tests, so a component that loses its last caller gets
// deleted instead of lingering as dead code that still type-checks.
import { join, relative } from 'node:path';

import { describe, expect, it } from 'vitest';

import { importsOf, walk } from './route-graph';

const SRC = join(__dirname, '..');
const COMPONENTS = join(SRC, 'components');

const isTest = (f: string) => /\.test\.tsx?$/.test(f);
const isModule = (f: string) => /\.tsx?$/.test(f) && !/\.d\.ts$/.test(f);

describe('component importers', () => {
  it('every component module is imported somewhere in src/', () => {
    // A barrel re-export counts as an importer; tests do not, since a
    // component only its own test renders is still dead.
    const imported = new Set(
      walk(SRC)
        .filter((f) => isModule(f) && !isTest(f))
        .flatMap((f) => importsOf(f)),
    );
    const orphans = walk(COMPONENTS)
      .filter((f) => isModule(f) && !isTest(f) && !imported.has(f))
      .map((f) => relative(SRC, f))
      .sort();
    expect(orphans, 'components with no importer (delete them)').toEqual([]);
  }, 15_000); // walks the whole src tree; 5 s flakes under verify's parallel lanes
});
