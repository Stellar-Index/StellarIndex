import { loadIncidents } from '@/lib/incidents';

import StatusPageClient, {
  type IncidentHistoryEntry,
} from './StatusPageClient';

// Server wrapper. The interactive status surface is a client
// component (live polling, SSE, endpoint probes), but the incident
// history is seeded from the build-time corpus so past incidents
// render even when the live API is fully down (WB-02). We load the
// corpus here at build time — `loadIncidents()` reads the repo's
// `internal/incidents/data/*.md`, the same source /incident/[slug]
// and the sitemap use — and project it into the UI-flat shape the
// client overlays the live /v1/incidents feed onto.
export const dynamic = 'force-static';

// seedIncidentHistory mirrors normaliseIncident() in the client: it
// projects the structured corpus record onto the flat shape the
// IncidentHistory panel renders. Kept here (server side) because the
// build-time loader is Node-fs based and can't run in the client
// bundle.
function seedIncidentHistory(): IncidentHistoryEntry[] {
  return loadIncidents().map((inc) => {
    const severity =
      inc.severity === 'SEV-1'
        ? 'major'
        : inc.severity === 'SEV-2'
          ? 'minor'
          : 'maintenance';
    const summary = (inc.body || '')
      .split(/\n## /m)[0]
      ?.replace(/^[#\s]+[^\n]*\n+/, '')
      .replace(/^<!--[\s\S]*?-->\s*/m, '')
      .trim()
      .slice(0, 400);
    const date = inc.started_at
      ? inc.started_at.slice(0, 10)
      : inc.date || '';
    const resolved = inc.resolved_at
      ? `${inc.resolved_at.slice(0, 10)} ${inc.resolved_at.slice(11, 16)} UTC`
      : inc.status;
    return {
      slug: inc.slug,
      date,
      title: inc.title || inc.slug,
      resolved,
      severity,
      summary: summary || inc.title || inc.slug,
    };
  });
}

export default function StatusPage() {
  return <StatusPageClient seedIncidents={seedIncidentHistory()} />;
}
