import { existsSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

// Resolve via node:path, not `new URL(rel, base)` — under the jsdom
// environment the global URL is jsdom's and fileURLToPath rejects it
// ("The URL must be of scheme file").
const APP_DIR = dirname(fileURLToPath(import.meta.url));

// T295: a segment without its own error.tsx has no boundary — a render
// throw anywhere under it propagates up to the nearest ancestor
// error.tsx (or, absent one, to global-error.tsx, which replaces the
// ENTIRE root layout and white-screens the whole site for a failure
// local to one route). 20 other data-heavy segments already carry the
// shared RouteError wrapper; these were the gap.
const SEGMENTS_REQUIRING_ERROR_BOUNDARY = [
  'embed',
  'dashboard',
  'diagnostics',
  'oracles',
  'network',
  'convert',
  'operation',
];

describe('error boundary coverage', () => {
  it('has a root error.tsx as the final backstop before global-error', () => {
    expect(existsSync(resolve(APP_DIR, 'error.tsx'))).toBe(true);
  });

  it.each(SEGMENTS_REQUIRING_ERROR_BOUNDARY)(
    'has an error.tsx for /%s',
    (segment) => {
      expect(existsSync(resolve(APP_DIR, segment, 'error.tsx'))).toBe(true);
    },
  );
});
