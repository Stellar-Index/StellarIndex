import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { LIVE_CURSOR_SOURCES, isLiveCursorSource } from './cursors';

// Lockstep with the server's list: a namespace added there and not here
// would drop out of every explorer "live" figure.
const goSrc = readFileSync(
  resolve(
    dirname(fileURLToPath(import.meta.url)),
    '../../../../internal/storage/timescale/cursors.go',
  ),
  'utf8',
);

describe('LIVE_CURSOR_SOURCES', () => {
  it('matches liveCursorSources in internal/storage/timescale/cursors.go', () => {
    const m = goSrc.match(/var liveCursorSources = \[\]string\{([^}]*)\}/);
    expect(m).not.toBeNull();
    const goList = [...(m?.[1] ?? '').matchAll(/"([^"]+)"/g)].map((x) => x[1]);
    expect(goList.length).toBeGreaterThan(0);
    expect([...LIVE_CURSOR_SOURCES]).toEqual(goList);
  });

  it('treats every one-shot job namespace as not live', () => {
    for (const s of [
      'backfill',
      'census-backfill',
      'projected-rebuild',
      'tag-signer',
      'tag-routed-via',
      'backfill-router',
    ]) {
      expect(isLiveCursorSource(s)).toBe(false);
    }
    expect(isLiveCursorSource('ledgerstream')).toBe(true);
    expect(isLiveCursorSource('projector')).toBe(true);
  });
});
