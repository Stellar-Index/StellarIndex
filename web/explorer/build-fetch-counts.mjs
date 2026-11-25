// Prints one line: the API requests this build's pages sent, per route,
// summed over the per-process tallies src/lib/buildFetch.ts wrote to .next.
// Report only; it never fails the build.
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';

const dir = join(process.cwd(), '.next', 'build-fetch-counts');
const total = new Map();
let files = [];
try {
  files = readdirSync(dir).filter((f) => f.endsWith('.json'));
} catch {
  // No tally: a CI-stub or offline build sends no requests.
}
for (const f of files) {
  for (const [route, n] of Object.entries(
    JSON.parse(readFileSync(join(dir, f), 'utf8')),
  )) {
    total.set(route, (total.get(route) ?? 0) + n);
  }
}
const sum = [...total.values()].reduce((a, b) => a + b, 0);
const routes = [...total]
  .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
  .map(([route, n]) => `${route} ${n}`)
  .join(', ');
console.log(
  `build API requests: ${sum} over ${files.length} process(es)${routes ? ` — ${routes}` : ''}`,
);
