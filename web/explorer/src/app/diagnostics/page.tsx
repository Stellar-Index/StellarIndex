import type { Metadata } from 'next';

import { DataTrustTabs } from '@/components/nav/DataTrustTabs';
import { Container, PageHeader, SectionHeader } from '@/components/ui';
import { ArchivePanel } from './ArchivePanel';
import { BackfillSummary } from './BackfillSummary';
import { CoveragePanel } from './CoveragePanel';
import { CursorsTable } from './CursorsTable';
import { HealthSummary } from './HealthSummary';
import { IngestThroughputChart } from './IngestThroughputChart';

export const metadata: Metadata = {
  title: 'Diagnostics — public system-health view',
  description:
    'Live ingest cursors, per-source completeness verdicts (decoder coverage), and archive completeness. Watch each indexer source tick in real time.',
  alternates: { canonical: '/diagnostics' },
};

/**
 * /diagnostics — public system-health view.
 *
 * Ships today (board #33): live ingest cursors + throughput
 * (/v1/diagnostics/cursors), backfill workers, per-source ADR-0033
 * completeness verdicts (/v1/coverage — decoder coverage IS the
 * completeness verdict), and the ADR-0017 archive-completeness
 * report (/v1/diagnostics/archive).
 *
 * Honestly pending — do NOT fake these:
 *   - SLO burn rates: the sla-probe binary emits its latency/
 *     freshness verdicts ONLY as node_exporter textfile metrics
 *     (cmd/stellarindex-sla-probe/textfile.go) — no table or API
 *     endpoint serves probe-run history, so there is nothing to
 *     compute a 7d pass rate from. A burn-rate panel lands when an
 *     sla-probe read endpoint (or a Prometheus-range proxy) ships.
 *   - Cross-region consistency: single-region (R1) deployment
 *     today; R2/R3 are deferred (see ADR-0016). The panel lands
 *     with multi-region.
 */
export default function DiagnosticsPage() {
  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[{ label: 'Home', href: '/' }, { label: 'Diagnostics' }]}
        title="Diagnostics"
      />
      <DataTrustTabs active="/diagnostics" />

      <section className="space-y-4">
        <SectionHeader title="Live ingest" />
        <HealthSummary />
      </section>

      <section className="space-y-4">
        <SectionHeader
          title="Decoder coverage"
          description="Per-source verdicts; served tier and lake can diverge."
        />
        <CoveragePanel />
      </section>

      <section className="space-y-4">
        <SectionHeader
          title="Archive completeness"
          description="Daily history-archive scan."
        />
        <ArchivePanel />
      </section>

      <section className="space-y-4">
        <SectionHeader title="Throughput" />
        <IngestThroughputChart />
      </section>

      <section className="space-y-4">
        <SectionHeader title="Backfill workers" />
        <BackfillSummary />
      </section>

      <CursorsTable />
    </Container>
  );
}
