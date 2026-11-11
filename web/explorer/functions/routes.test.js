// @vitest-environment node
//
// public/_routes.json replaces the route list CF Pages would derive from this
// directory, so the two must agree: a Function missing from `include` never
// runs, and a `[[path]]` prefix missing its `*.txt` exclude bills a Function
// call for every RSC prefetch of a link to it.
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, it, expect } from 'vitest';

const functionsDir = path.dirname(fileURLToPath(import.meta.url));
const routes = JSON.parse(
  fs.readFileSync(path.join(functionsDir, '..', 'public', '_routes.json'), 'utf8'),
);

function discoverRoutes(dir, segments = []) {
  const found = [];
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    if (entry.name.startsWith('_')) continue;
    if (entry.isDirectory()) {
      found.push(...discoverRoutes(path.join(dir, entry.name), [...segments, entry.name]));
    } else if (entry.name.endsWith('.js') && !entry.name.endsWith('.test.js')) {
      const name = entry.name.slice(0, -'.js'.length);
      found.push(
        name === '[[path]]'
          ? { pattern: `/${segments.join('/')}/*`, catchAll: true }
          : { pattern: `/${[...segments, name].join('/')}`, catchAll: false },
      );
    }
  }
  return found;
}

const discovered = discoverRoutes(functionsDir);

describe('public/_routes.json', () => {
  it('stays within the CF Pages limits', () => {
    expect(routes.version).toBe(1);
    expect(routes.include.length + routes.exclude.length).toBeLessThanOrEqual(100);
    for (const rule of [...routes.include, ...routes.exclude]) {
      expect(rule.startsWith('/') && rule.length <= 100).toBe(true);
    }
  });

  it('includes exactly the Function routes', () => {
    expect([...routes.include].sort()).toEqual(discovered.map((r) => r.pattern).sort());
  });

  it('excludes RSC payloads under every catch-all route', () => {
    const expected = discovered
      .filter((r) => r.catchAll && r.pattern !== '/og/*')
      .map((r) => r.pattern.replace(/\*$/, '*.txt'));
    expect([...routes.exclude].sort()).toEqual(expected.sort());
  });
});
