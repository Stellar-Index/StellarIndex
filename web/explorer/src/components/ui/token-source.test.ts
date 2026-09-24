import { existsSync, readdirSync, readFileSync } from 'node:fs';
import { dirname, extname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

// node:path, not `new URL(rel, base)`: jsdom's URL breaks fileURLToPath.
const HERE = fileURLToPath(import.meta.url);
const EXPLORER = resolve(dirname(HERE), '../../..');
const APPS = ['explorer', 'status'];
const SCANNED = new Set(['.ts', '.tsx', '.js', '.mjs', '.css']);
const CONFIG_NAMES = ['ts', 'js', 'mjs', 'cjs'].map(
  (e) => `tailwind.config.${e}`,
);

function sourceFiles(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((d) => {
    const p = join(dir, d.name);
    if (d.isDirectory()) return sourceFiles(p);
    return SCANNED.has(extname(d.name)) && p !== HERE ? [p] : [];
  });
}

// Tailwind v4: tokens are the `@theme` block in src/app/globals.css. A comment
// pointing at a tailwind config file sends a restyle to a file that does not exist.
describe('design-token source references', () => {
  it.each(APPS)(
    'web/%s cites no tailwind config file it does not have',
    (name) => {
      const app = resolve(EXPLORER, '..', name);
      if (CONFIG_NAMES.some((n) => existsSync(join(app, n)))) return;
      const stale = sourceFiles(join(app, 'src'))
        .filter((f) => readFileSync(f, 'utf8').includes('tailwind.config'))
        .map((f) => relative(app, f));
      expect(stale).toEqual([]);
    },
  );

  it('ui barrel names the @theme block in globals.css as the token source', () => {
    const header = readFileSync(resolve(dirname(HERE), 'index.ts'), 'utf8')
      .split('\n')
      .slice(0, 5)
      .join('\n');
    expect(header).toContain('@theme');
    expect(header).toContain('src/app/globals.css');
  });
});
