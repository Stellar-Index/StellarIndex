// Build-time loader for the incident corpus. Reads
// `internal/incidents/data/*.md` from the repo root, parses YAML
// frontmatter, and exposes typed records for /incident/[slug].
//
// The runtime API at /v1/incidents serves the same corpus
// (embedded into the Go binary at compile time). We re-load here
// at build time so the static export can pre-render every
// postmortem page without a client-side fetch.
//
// This loader MUST agree with internal/incidents/incidents.go on
// what the frontmatter means: severity/status are validated enums
// (never a blind cast — see [isValidSeverity]/[isValidStatus]), a
// resolved_at that fails to parse is never silently dropped, and a
// file whose frontmatter doesn't validate is skipped with a warning
// rather than published. The Go loader's package doc explains why:
// an unvalidated severity/status defaulted straight through can
// publish a live SEV-1 looking like routine "maintenance", or print
// "Resolved" during an ongoing outage (cold audit 2026-08-03).

import { readFileSync, readdirSync } from 'node:fs';
import path from 'node:path';

export type IncidentSeverity = 'SEV-1' | 'SEV-2' | 'SEV-3';
export type IncidentStatus =
  'investigating' | 'identified' | 'monitoring' | 'resolved';

export type Incident = {
  slug: string;
  title: string;
  severity: IncidentSeverity;
  status: IncidentStatus;
  date: string;
  started_at: string;
  resolved_at: string | null;
  affected_components: string[];
  body: string;
  source_path: string;
};

const VALID_SEVERITIES: readonly IncidentSeverity[] = [
  'SEV-1',
  'SEV-2',
  'SEV-3',
];
const VALID_STATUSES: readonly IncidentStatus[] = [
  'investigating',
  'identified',
  'monitoring',
  'resolved',
];

function isValidSeverity(v: unknown): v is IncidentSeverity {
  return (
    typeof v === 'string' && (VALID_SEVERITIES as readonly string[]).includes(v)
  );
}

function isValidStatus(v: unknown): v is IncidentStatus {
  return (
    typeof v === 'string' && (VALID_STATUSES as readonly string[]).includes(v)
  );
}

const REPO_ROOT = path.resolve(process.cwd(), '..', '..');
const DATA_DIR = path.join(REPO_ROOT, 'internal', 'incidents', 'data');

let cache: Incident[] | null = null;

export function loadIncidents(): Incident[] {
  if (cache) return cache;
  let files: string[] = [];
  try {
    files = readdirSync(DATA_DIR);
  } catch {
    cache = [];
    return cache;
  }
  const out: Incident[] = [];
  for (const f of files) {
    if (!f.endsWith('.md')) continue;
    if (f.startsWith('_')) continue; // _template.md
    const full = path.join(DATA_DIR, f);
    const raw = readFileSync(full, 'utf-8');
    const inc = parseIncidentFile(raw, f);
    if (inc) out.push(inc);
  }
  // Newest first.
  out.sort((a, b) => (a.started_at < b.started_at ? 1 : -1));
  cache = out;
  return out;
}

export function loadIncident(slug: string): Incident | null {
  return loadIncidents().find((i) => i.slug === slug) ?? null;
}

// parseIncidentFile turns one raw markdown file into an Incident, or
// null if the file doesn't parse or its frontmatter doesn't validate.
// Malformed posts are skipped with a console.warn rather than
// defaulting severity/status to something publishable — a bad post
// must never look like a routine, resolved one (see the package
// comment above and internal/incidents/incidents.go's parseSource).
// Exported for unit testing against fixtures without touching the
// real corpus directory.
export function parseIncidentFile(
  raw: string,
  filename: string,
): Incident | null {
  const parsed = parseFrontmatter(raw);
  if (!parsed) {
    console.warn(
      `incidents: skip malformed post ${filename}: no closing frontmatter delimiter`,
    );
    return null;
  }

  const severityRaw = parsed.fm['severity'];
  if (!isValidSeverity(severityRaw)) {
    console.warn(
      `incidents: skip malformed post ${filename}: severity ${JSON.stringify(severityRaw)} is not one of ${VALID_SEVERITIES.join('/')}`,
    );
    return null;
  }
  const statusRaw = parsed.fm['status'];
  if (!isValidStatus(statusRaw)) {
    console.warn(
      `incidents: skip malformed post ${filename}: status ${JSON.stringify(statusRaw)} is not one of ${VALID_STATUSES.join('/')}`,
    );
    return null;
  }

  const resolvedRaw = parsed.fm['resolved_at'];
  let resolvedAt: string | null = null;
  if (
    typeof resolvedRaw === 'string' &&
    resolvedRaw !== '' &&
    resolvedRaw !== 'null'
  ) {
    if (Number.isNaN(Date.parse(resolvedRaw))) {
      // Do NOT silently drop it — an unparseable resolved_at used to
      // leave the field null, which publishes a resolved-looking
      // status page entry with no timestamp (or vice versa). Reject
      // the whole post instead, matching the Go loader.
      console.warn(
        `incidents: skip malformed post ${filename}: resolved_at ${JSON.stringify(resolvedRaw)} does not parse as a timestamp`,
      );
      return null;
    }
    resolvedAt = resolvedRaw;
  }

  const slug = filename.replace(/\.md$/, '');
  return {
    slug,
    title: String(parsed.fm['title'] ?? slug),
    severity: severityRaw,
    status: statusRaw,
    date: String(parsed.fm['date'] ?? ''),
    started_at: String(parsed.fm['started_at'] ?? ''),
    resolved_at: resolvedAt,
    affected_components: Array.isArray(parsed.fm['affected_components'])
      ? (parsed.fm['affected_components'] as string[])
      : [],
    body: parsed.body.trim(),
    source_path: `internal/incidents/data/${filename}`,
  };
}

// stripComment removes a trailing YAML comment from an already
// key-stripped value. A `#` only starts a comment when it begins the
// value or is preceded by whitespace (the YAML rule) — never inside a
// quoted string, so a quoted value is returned whole up to its closing
// quote. Without this, the incident template's own
// `resolved_at:  # leave empty until resolved` seeds a REAL incident
// file with the literal comment text as a truthy resolved_at, and the
// status page renders it as the resolved timestamp of a live outage.
function stripComment(v: string): string {
  const quote = v[0];
  if (quote === '"' || quote === "'") {
    const close = v.indexOf(quote, 1);
    if (close !== -1) return v.slice(0, close + 1);
    return v;
  }
  const idx = v.search(/(?:^|\s)#/);
  return (idx === -1 ? v : v.slice(0, idx)).trimEnd();
}

// parseFrontmatter — handles the small set of shapes our incident
// template uses: scalar `key: value`, quoted strings, `key: null`,
// trailing `# comment`s, and bullet lists indented under a key:
//
//   affected_components:                 # one or more
//     - indexer
//     - storage
//
// No nested objects. If we ever need them, swap for a real YAML
// lib.
function parseFrontmatter(
  raw: string,
): { fm: Record<string, unknown>; body: string } | null {
  if (!raw.startsWith('---')) return { fm: {}, body: raw };
  const end = raw.indexOf('\n---', 3);
  if (end === -1) return null;
  const head = raw.slice(3, end).trim();
  const body = raw.slice(end + 4).replace(/^\n/, '');

  const fm: Record<string, unknown> = {};
  const lines = head.split('\n');
  let i = 0;
  while (i < lines.length) {
    const line = lines[i]!;
    const m = line.match(/^([A-Za-z_][A-Za-z0-9_]*):\s*(.*)$/);
    if (!m) {
      i++;
      continue;
    }
    const k = m[1]!;
    const v = stripComment(m[2]!.trim());
    if (v === '' || v === 'null') {
      // Could be a bullet-list block.
      const items: string[] = [];
      let j = i + 1;
      while (j < lines.length && /^\s+-\s+/.test(lines[j]!)) {
        items.push(
          stripComment(lines[j]!.replace(/^\s+-\s+/, '')).replace(
            /^['"]|['"]$/g,
            '',
          ),
        );
        j++;
      }
      if (items.length > 0) {
        fm[k] = items;
        i = j;
        continue;
      }
      fm[k] = v === 'null' ? null : '';
    } else {
      fm[k] = v.replace(/^['"]|['"]$/g, '');
    }
    i++;
  }
  return { fm, body };
}
