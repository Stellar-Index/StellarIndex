import { type Incident, loadIncidents } from '../lib-incidents';

// Rendered at build time from internal/incidents/data with no API call, so it can
// be published while the API is down (runbooks/sev-status-page-update.md).
export const dynamic = 'force-static';

const LIVE_STATUS = 'https://stellarindex.io/status/';
const HISTORY_LIMIT = 10;

const SEVERITY_CLASS: Record<Incident['severity'], string> = {
  'SEV-1': 'bg-bad-50 text-bad-700',
  'SEV-2': 'bg-warn-50 text-warn-700',
  'SEV-3': 'bg-brand-50 text-brand-700',
};

function IncidentCard({ incident }: { incident: Incident }) {
  return (
    <article className="rounded-lg border border-line p-4">
      <div className="flex flex-wrap items-center gap-2 text-xs">
        <span className={`rounded px-2 py-0.5 font-semibold ${SEVERITY_CLASS[incident.severity]}`}>
          {incident.severity}
        </span>
        <span className="font-medium uppercase text-ink-muted">{incident.status}</span>
        <span className="text-ink-muted">
          started {incident.started_at}
          {incident.resolved_at ? ` · resolved ${incident.resolved_at}` : ''}
        </span>
      </div>
      <h3 className="mt-2 text-base font-semibold text-ink-strong">{incident.title}</h3>
      {incident.affected_components.length > 0 && (
        <p className="mt-1 text-xs text-ink-muted">
          Affected: {incident.affected_components.join(', ')}
        </p>
      )}
      <div className="mt-3 whitespace-pre-wrap text-sm text-ink">{incident.body.trim()}</div>
    </article>
  );
}

export default function StatusPage() {
  const incidents = loadIncidents();
  const active = incidents.filter((i) => i.status !== 'resolved');
  const history = incidents.filter((i) => i.status === 'resolved').slice(0, HISTORY_LIMIT);

  return (
    <main className="mx-auto flex max-w-3xl flex-col gap-8 px-4 py-10">
      <header>
        <h1 className="text-2xl font-semibold text-ink-strong">Stellar Index status</h1>
        <p className="mt-1 text-sm text-ink-muted">
          Incident notices for api.stellarindex.io and stellarindex.io. Live component health:{' '}
          <a href={LIVE_STATUS} className="text-brand-600 underline">
            stellarindex.io/status
          </a>
          .
        </p>
      </header>

      <section aria-labelledby="active">
        <h2 id="active" className="mb-3 text-lg font-semibold text-ink-strong">
          Active incidents
        </h2>
        {active.length === 0 ? (
          <p className="rounded-lg border border-ok-300 bg-ok-50 p-4 text-sm text-ok-700">
            No open incidents. All systems are operating normally.
          </p>
        ) : (
          <div className="flex flex-col gap-4">
            {active.map((i) => (
              <IncidentCard key={i.slug} incident={i} />
            ))}
          </div>
        )}
      </section>

      <section aria-labelledby="history">
        <h2 id="history" className="mb-3 text-lg font-semibold text-ink-strong">
          Past incidents
        </h2>
        {history.length === 0 ? (
          <p className="text-sm text-ink-muted">No past incidents.</p>
        ) : (
          <div className="flex flex-col gap-4">
            {history.map((i) => (
              <IncidentCard key={i.slug} incident={i} />
            ))}
          </div>
        )}
      </section>
    </main>
  );
}
