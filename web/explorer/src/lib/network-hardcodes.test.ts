// Guard: no MAINNET-specific literal may be hardcoded in explorer source.
//
// This file exists because the same bug shipped repeatedly and was only ever
// caught by a human looking at a test-net page:
//
//   * /network reported "Pubnet" on testnet.stellarindex.io (value="Pubnet"
//     hardcoded in NetworkView).
//   * 13 outbound links pointed at stellar.expert/explorer/PUBLIC, so a
//     test-net reader clicking a tx/account/contract was sent to MAINNET —
//     where the id either 404s or, worse, resolves to an unrelated entity.
//   * robots.txt, sitemap.xml and ~30 canonical / JSON-LD `url` values named
//     https://stellarindex.io from the test-net builds.
//
// None of these break the build, none fail a type check, and none show up on
// mainnet — which is why they survived. A grep is the only thing that
// actually catches them, so it runs in CI as a test.
//
// Adding a legitimate exception: prefer deriving from CURRENT_NETWORK. If a
// literal genuinely must stay (a doc comment, the networks table itself),
// add it to ALLOWED with the reason.
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join } from 'node:path';

import { describe, expect, it } from 'vitest';

const SRC = join(__dirname, '..');

/** Files permitted to contain the literals, with the reason. */
const ALLOWED = new Map<string, string>([
  // The network table IS the definition — every other module derives from it.
  ['lib/networks.ts', 'defines the per-network origins'],
  // Guard itself.
  ['lib/network-hardcodes.test.ts', 'this guard'],
  // Generated from the OpenAPI spec; its examples are mainnet by nature and
  // it is never hand-edited.
  ['api/types.ts', 'generated from openapi; examples are illustrative'],
  // Verbatim Go/JS source shown to the reader as copy-paste SDK samples.
  // Templating a string INSIDE a code sample would make the sample itself
  // wrong; the samples document the product's canonical API.
  ['app/sdk/page.tsx', 'literal code samples'],
  // The dashboard is mainnet-only (NetworkInfo.accounts is false on the
  // test nets and the whole SaaS surface is hidden there), so a mainnet
  // API example is correct wherever this page can actually render.
  ['app/dashboard/page.tsx', 'mainnet-only surface'],
]);

/**
 * Test files legitimately ASSERT concrete URLs — that is what makes them
 * tests. Exempt them rather than forcing an indirection that would let the
 * assertion drift with the code it checks.
 */
const isTest = (rel: string) => /\.test\.tsx?$/.test(rel);

function walk(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    if (entry === 'node_modules' || entry === '.next') continue;
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) walk(full, out);
    else if (/\.(ts|tsx)$/.test(entry)) out.push(full);
  }
  return out;
}

/**
 * Strip `//` line comments and block comments so a literal that only appears
 * in prose (an iframe usage example, a rationale note) does not trip the
 * guard — the point is to catch literals the CODE emits.
 */
function stripComments(src: string): string {
  return src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '');
}

function offenders(pattern: RegExp): string[] {
  const bad: string[] = [];
  for (const file of walk(SRC)) {
    const rel = file.slice(SRC.length + 1).split('\\').join('/');
    if (ALLOWED.has(rel) || isTest(rel)) continue;
    const code = stripComments(readFileSync(file, 'utf8'));
    if (pattern.test(code)) bad.push(rel);
  }
  return bad.sort();
}

describe('no mainnet hardcodes in explorer source', () => {
  it('never links to stellar.expert with a fixed network segment', () => {
    // Must go through stellarExpertUrl()/StellarExpertLink, which resolve the
    // segment per network and render nothing where stellar.expert has no
    // explorer for this network (futurenet).
    expect(offenders(/stellar\.expert\/explorer\/(public|testnet)\b/)).toEqual([]);
  });

  it('never hardcodes the mainnet explorer origin', () => {
    // Canonicals, JSON-LD `url`, sitemap/robots and outbound self-links must
    // derive from CURRENT_NETWORK.explorerUrl, or a test-net build advertises
    // itself as mainnet.
    expect(offenders(/https:\/\/stellarindex\.io/)).toEqual([]);
  });

  it('never hardcodes the mainnet API origin', () => {
    // The API origin is per-network too (CURRENT_NETWORK.apiBaseUrl /
    // API_BASE_URL); a fixed one makes a test-net page read mainnet data.
    expect(offenders(/https:\/\/api\.stellarindex\.io/)).toEqual([]);
  });
});
