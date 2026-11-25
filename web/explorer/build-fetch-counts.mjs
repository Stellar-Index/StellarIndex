// Prints the API requests this build's pages sent, per route, summed over the
// per-process tallies src/lib/buildFetch.ts wrote to .next, then the re-asks
// after a degraded answer and every path baked degraded or unavailable.
// Report only; it never fails the build.
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';

const dir = join(process.cwd(), '.next', 'build-fetch-counts');
const total = new Map();
const reasked = new Map();
const degraded = new Set();
let files = [];
try {
  files = readdirSync(dir).filter((f) => f.endsWith('.json'));
} catch {
  // No tally: a CI-stub or offline build sends no requests.
}
const add = (into, counts) => {
  for (const [route, n] of Object.entries(counts ?? {})) {
    into.set(route, (into.get(route) ?? 0) + n);
  }
};
for (const f of files) {
  const tally = JSON.parse(readFileSync(join(dir, f), 'utf8'));
  add(total, tally.requests);
  add(reasked, tally.reasked);
  for (const p of tally.degraded ?? []) degraded.add(p);
}
const sumOf = (m) => [...m.values()].reduce((a, b) => a + b, 0);
const list = (m) =>
  [...m]
    .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
    .map(([route, n]) => `${route} ${n}`)
    .join(', ');
const routes = list(total);
const reaskRoutes = list(reasked);
console.log(
  `build API requests: ${sumOf(total)} over ${files.length} process(es)${routes ? ` — ${routes}` : ''}; ` +
    `re-asked after a degraded answer: ${sumOf(reasked)}${reaskRoutes ? ` — ${reaskRoutes}` : ''}; ` +
    `baked degraded or unavailable: ${degraded.size}`,
);
for (const p of [...degraded].sort()) console.log(`  baked degraded: ${p}`);
