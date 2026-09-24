// docs/operations/api-explorer-coverage.md classifies every OpenAPI path by
// whether the explorer can reach it. A path added to the spec without a row
// silently leaves the document stale while its Result table still looks
// authoritative, so this pins the table and its totals to the spec.
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { describe, expect, it } from 'vitest';

const REPO = join(__dirname, '..', '..', '..', '..');
const SPEC = join(REPO, 'openapi', 'stellar-index.v1.yaml');
const DOC = join(REPO, 'docs', 'operations', 'api-explorer-coverage.md');

function specPaths(): string[] {
  return readFileSync(SPEC, 'utf8')
    .split('\n')
    .map((line) => /^ {2}(\/\S+):\s*$/.exec(line)?.[1])
    .filter((p): p is string => p !== undefined);
}

function section(doc: string, heading: string): string {
  const start = doc.indexOf(`\n## ${heading}\n`);
  expect(start, `missing "## ${heading}" section`).toBeGreaterThanOrEqual(0);
  const end = doc.indexOf('\n## ', start + 1);
  return doc.slice(start, end === -1 ? undefined : end);
}

function tableRows(doc: string): { path: string; level: string }[] {
  return section(doc, 'The table')
    .split('\n')
    .map((line) => /^\| `(\/[^`]+)` \| [A-Z, ]+ \| (1|2|3|ex) \|/.exec(line))
    .filter((m): m is RegExpExecArray => m !== null)
    .map((m) => ({ path: m[1], level: m[2] }));
}

function resultCount(doc: string, label: string): number {
  const m = new RegExp(`^\\| ${label} \\| \\*\\*(\\d+)\\*\\* \\|$`, 'm').exec(
    section(doc, 'Result'),
  );
  expect(m, `missing Result row "${label}"`).not.toBeNull();
  return Number(m?.[1]);
}

describe('api-explorer-coverage.md', () => {
  const doc = readFileSync(DOC, 'utf8');
  const rows = tableRows(doc);

  it('has exactly one table row per OpenAPI path', () => {
    const inDoc = rows.map((r) => r.path);
    expect(new Set(inDoc).size, 'duplicate rows').toBe(inDoc.length);
    expect([...inDoc].sort()).toEqual([...specPaths()].sort());
  });

  it('states Result totals that match the table', () => {
    const tally = (level: string) =>
      rows.filter((r) => r.level === level).length;
    expect(resultCount(doc, 'Paths in the OpenAPI contract')).toBe(
      specPaths().length,
    );
    expect(resultCount(doc, 'Level 3 — reachable')).toBe(tally('3'));
    expect(resultCount(doc, 'Level 2 — consumed but unreachable')).toBe(
      tally('2'),
    );
    expect(resultCount(doc, 'Level 1 — not consumed')).toBe(tally('1'));
    expect(resultCount(doc, 'Deliberately excluded \\(operational\\)')).toBe(
      tally('ex'),
    );
  });
});
