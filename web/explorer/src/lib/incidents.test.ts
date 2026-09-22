import { describe, expect, it, vi } from 'vitest';

import { loadIncidentsFrom, parseIncidentFile } from './incidents';

// TEMPLATE_SEED reproduces the exact shape internal/incidents/_template.md
// ships: `resolved_at:` and `affected_components:` carry dangling
// `# comment` text with no value, and the bullet list for
// affected_components is indented under the key on the following lines.
// A real incident authored by copying the template and filling in only
// the required fields (title/date/severity/status/started_at) yields
// this exact frontmatter for resolved_at and affected_components.
const TEMPLATE_SEEDED_OPEN_SEV1 = `---
title: "[SEV-1] partial pricing outage — 2026-09-19"
date: 2026-09-19
severity: SEV-1
status: investigating
started_at: 2026-09-19T10:00:00Z
resolved_at:                                 # leave empty until resolved
affected_components:                         # one or more — must match status-page component names
  - api
  - indexer
postmortem:                                  # leave empty until the postmortem is written
---

# [SEV-1] partial pricing outage

Some \`/v1/price\` queries are returning 503.
`;

describe('parseIncidentFile', () => {
  it('does not publish a template-seeded open incident as resolved', () => {
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    const inc = parseIncidentFile(
      TEMPLATE_SEEDED_OPEN_SEV1,
      '2026-09-19-partial-pricing-outage.md',
    );

    expect(inc).not.toBeNull();
    // The template's dangling `# leave empty until resolved` comment
    // must never become the resolved_at VALUE — an open incident's
    // resolved_at is null, not truthy comment text.
    expect(inc!.resolved_at).toBeNull();
    expect(inc!.status).toBe('investigating');
    // The bullet list under affected_components must still be picked
    // up even though its key line carries a trailing comment.
    expect(inc!.affected_components).toEqual(['api', 'indexer']);
  });

  it('strips a trailing comment from a plain scalar value', () => {
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    const raw = `---
title: "[SEV-2] degraded ingest — 2026-09-19"
date: 2026-09-19
severity: SEV-2
status: monitoring
started_at: 2026-09-19T09:00:00Z
resolved_at: 2026-09-19T09:45:00Z  # closed by on-call
---

Body.
`;
    const inc = parseIncidentFile(raw, '2026-09-19-degraded-ingest.md');
    expect(inc).not.toBeNull();
    expect(inc!.resolved_at).toBe('2026-09-19T09:45:00Z');
  });

  it('preserves a `#` character inside a quoted value', () => {
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    const raw = `---
title: "API returning #500 for some requests"
date: 2026-09-19
severity: SEV-3
status: resolved
started_at: 2026-09-19T08:00:00Z
resolved_at: 2026-09-19T08:30:00Z
---

Body.
`;
    const inc = parseIncidentFile(raw, '2026-09-19-hash-in-title.md');
    expect(inc).not.toBeNull();
    expect(inc!.title).toBe('API returning #500 for some requests');
  });

  it('rejects a post with an invalid severity instead of publishing it', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const raw = `---
title: "typo'd severity"
date: 2026-09-19
severity: sev-1
status: investigating
started_at: 2026-09-19T10:00:00Z
resolved_at:
---

Body.
`;
    const inc = parseIncidentFile(raw, '2026-09-19-bad-severity.md');
    expect(inc).toBeNull();
    expect(warn).toHaveBeenCalled();
  });

  it('rejects a post with a missing/invalid status instead of defaulting to resolved', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const raw = `---
title: "missing status"
date: 2026-09-19
severity: SEV-1
started_at: 2026-09-19T10:00:00Z
resolved_at:
---

Body.
`;
    const inc = parseIncidentFile(raw, '2026-09-19-missing-status.md');
    expect(inc).toBeNull();
    expect(warn).toHaveBeenCalled();
  });
});

describe('loadIncidentsFrom', () => {
  it('logs a warning instead of publishing a silent empty corpus when the data dir is unreadable', () => {
    // RLT-467: the readdirSync catch used to set the cache to `[]` with zero
    // logging on any error — indistinguishable, to every caller, from "no
    // incidents have ever happened" (a false all-clear on /status). Use a
    // real nonexistent path so this exercises the actual ENOENT branch
    // rather than a mocked one.
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const incidents = loadIncidentsFrom(
      '/nonexistent/rlt-467-incidents-dir-does-not-exist',
    );
    expect(incidents).toEqual([]);
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining('incidents: failed to read'),
    );
  });
});
