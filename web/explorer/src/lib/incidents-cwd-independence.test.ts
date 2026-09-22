import { describe, expect, it } from 'vitest';
import os from 'node:os';

// RLT-467: `DATA_DIR` used to be derived from `path.resolve(process.cwd(),
// '..', '..')` — correct only when the process happens to be launched from
// web/explorer. Kept in its own file so this is genuinely the first-ever
// import of incidents.ts in this module graph: chdir before that first
// import, then confirm the real internal/incidents/data corpus still loads.
describe('incidents module resolution', () => {
  it('finds the real corpus even when process.cwd() is not web/explorer', async () => {
    const originalCwd = process.cwd();
    process.chdir(os.tmpdir());
    try {
      const { loadIncidents } = await import('./incidents');
      const incidents = loadIncidents();
      expect(incidents.length).toBeGreaterThan(0);
      expect(
        incidents.every((i) =>
          i.source_path.startsWith('internal/incidents/data/'),
        ),
      ).toBe(true);
    } finally {
      process.chdir(originalCwd);
    }
  });
});
