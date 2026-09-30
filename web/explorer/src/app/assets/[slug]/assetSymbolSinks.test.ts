// @vitest-environment node
//
// A Soroban contract asset has no `code`, so rendering `coin.code` directly
// prints "undefined" into the JSON-LD and leaves the <h1> blank. Every
// rendered symbol on this page must go through assetSymbol().
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { describe, expect, it } from 'vitest';

describe('asset page renders the symbol via assetSymbol()', () => {
  it('never interpolates or renders coin.code raw', () => {
    const src = readFileSync(join(__dirname, 'page.tsx'), 'utf8');
    const sinks = [
      /\$\{coin\.code\}/g, // template-literal interpolation (JSON-LD, titles)
      />\s*\{coin\.code\}/g, // JSX child after a tag or text
      /\{coin\.code\}\s*</g, // JSX child before a closing tag
    ];
    const offenders = new Set(
      sinks.flatMap((re) =>
        [...src.matchAll(re)].map(
          (m) => src.slice(0, m.index).split('\n').length,
        ),
      ),
    );
    expect([...offenders]).toEqual([]);
  });
});
