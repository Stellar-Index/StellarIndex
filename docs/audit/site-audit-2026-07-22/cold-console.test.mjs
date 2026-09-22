// Regression test for SL38: a CDP-unreachable run must fail with a clean
// one-line stderr message, not an uncaught exception stack trace.
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import assert from 'node:assert/strict';

const here = dirname(fileURLToPath(import.meta.url));
const script = join(here, 'cold-console.mjs');

// No CDP endpoint is listening on 127.0.0.1:9222 in this environment, so
// the script's first fetch fails the same way it would against a dead browser.
const result = spawnSync(process.execPath, [script, 'http://example.com', '800'], {
  encoding: 'utf8',
  timeout: 10000,
});

assert.equal(result.status, 1, 'script should exit non-zero when CDP is unreachable');
assert.match(
  result.stderr,
  /^cold-console: could not reach CDP at 127\.0\.0\.1:9222/,
  `expected a clean one-line error, got:\n${result.stderr}`,
);
assert.doesNotMatch(
  result.stderr,
  /Uncaught|internal\/deps\/undici|processTicksAndRejections/,
  `expected no uncaught-exception stack trace, got:\n${result.stderr}`,
);

console.log('PASS: cold-console.mjs fails cleanly on CDP-unreachable');
