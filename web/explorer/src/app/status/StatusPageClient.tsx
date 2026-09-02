'use client';

import { useEffect, useMemo, useState } from 'react';
import {
  AlertTriangle,
  CheckCircle2,
  ExternalLink,
  Info,
  XCircle,
} from 'lucide-react';

import { Badge, Card, Container, type BadgeTone } from '@/components/ui';
import { isSafeHref } from '@/lib/markdown';
import type { components, paths } from '@/api/types';
import { API_BASE_URL } from '@/api/client';
import { CURRENT_NETWORK } from '@/lib/networks';
import { useStatus } from '@/api/hooks';
import BackupsPanel from './BackupsPanel';
import {
  formatCompact,
  formatDurationShort,
  formatRelative,
} from '@/lib/format';
import {
  isFrameStale,
  LEDGER_LIVE_STALE_MS,
  useLedgerStream,
  useLiveClock,
  type LiveLedger,
} from '@/lib/live/hooks';

// Polled every 30 s — same cadence as Healthchecks.io's hosted
// status pages and well inside the 60-s indexer/aggregator
// heartbeat budget so a real degradation lands within one poll.
// The /v1/status doc itself now arrives via the SHARED useStatus
// query (STATUS_POLL_MS, same 30 s — FEC A6-6/D2: the banner and
// this page used to poll the same endpoint on separate clocks and
// could disagree in one viewport); this constant drives the
// page-local ingestion + endpoint-probe loops, which stay bespoke
// (per-region independence, latency measurement — A6-6 rationale).
const POLL_INTERVAL_MS = 30_000;

// Hot-tier endpoint probes also run at POLL_INTERVAL_MS. Warm-tier
// probes (catalogue listings, history queries) run at WARM_PROBE_MS
// — 2 minutes — because hammering them every 30 s measurably drives
// the API's SLO burn rate without adding incident-detection signal.
const WARM_PROBE_MS = 120_000;

// /v1/ledger/stream (SSE) reconnect interval after a hard failure.
// A 404 from an API binary that predates the endpoint fails the
// Client-side status union: the wire's StatusResponse.overall is
// "ok" | "degraded" | "down"; "unknown" is this page's local state
// before the first successful poll.
type ServiceStatus = 'ok' | 'degraded' | 'down' | 'unknown';

// Wire shapes from the generated OpenAPI contract (src/api/types.ts,
// `make web-generate-api`).
type ServiceEntry = components['schemas']['StatusService'];

type IncidentEntry = components['schemas']['ActiveIncident'];

type StatusResponse = components['schemas']['StatusResponse'];

// Operator-authored status banner (GET /v1/status/notices). Distinct
// from the alert-derived ActiveIncident above: a StatusNotice is a
// hand-posted announcement (maintenance window, ongoing incident) that
// an operator resolves to clear.
type StatusNotice = components['schemas']['StatusNotice'];

// REGIONS is the deployment fleet the status page queries. One
// entry today (r1); r2/r3 join as their deploys land — just append
// a row and the page renders an extra panel. Each region must
// expose `/v1/diagnostics/ingestion` (any version of the binary
// post-rc.51).
interface RegionDef {
  name: string;
  label: string;
  apiBaseUrl: string;
}

// Mainnet's fleet is r1 (Hetzner Frankfurt), with r2/r3 appended as they land.
// The lean test nets are a single VM on the Hetzner Helsinki box — labelling
// their status page "r1 · Hetzner · Frankfurt" was just wrong, so name the
// region for the actual network there. `apiBaseUrl` is the current network's
// API in every case, so the panel's DATA was always correct — only the label
// was mislabelled.
const REGIONS: RegionDef[] = CURRENT_NETWORK.pricing
  ? [{ name: 'r1', label: 'Hetzner · Frankfurt', apiBaseUrl: API_BASE_URL }]
  : [
      {
        name: CURRENT_NETWORK.label,
        label: 'Hetzner · Helsinki',
        apiBaseUrl: API_BASE_URL,
      },
    ];

// IngestionSnapshot mirrors the wire shape returned by
// `/v1/diagnostics/ingestion`. Field-for-field with the Go
// IngestionDiagnostics struct — see
// `internal/api/v1/diagnostics_ingestion.go`.
// IngestionSnapshot — `/v1/diagnostics/ingestion` body from the
// generated contract, plus the evolving diagnostics fields below that the Go
// handler serves (internal/api/v1/diagnostics_ingestion.go) but the
// spec's inline schema doesn't declare yet.
type IngestionSnapshotWire =
  paths['/diagnostics/ingestion']['get']['responses'][200]['content']['application/json']['data'];

type IngestionSnapshot = Omit<
  IngestionSnapshotWire,
  'backfill_coverage' | 'sources'
> & {
  backfill_coverage: Array<
    IngestionSnapshotWire['backfill_coverage'][number] & {
      // Evolving (spec documents the diagnostics surface as its stable core
      // + a described evolving remainder — board #33): density/gap-free fields
      // (diagnostics_ingestion.go BackfillCoverageView).
      density_pct?: number;
      gap_free_pct?: number;
      covered_ledgers?: number;
      expected_ledgers?: number;
      // Evolving: ADR-0033 Phase 6 watermark-based completeness
      // (substrate + projection verified, no sparsity threshold).
      // Preferred headline when present; falls back to gap_free
      // coverage otherwise.
      completeness_pct?: number;
      completeness_watermark?: number;
      // The ADR-0033/ADR-0034 TWO-AXIS verdict. `completeness_complete`
      // is the SERVED axis (substrate ∧ recognition ∧ the
      // retention-scoped projection reconcile);
      // `completeness_lake_complete` is the ARCHIVE axis (substrate ∧
      // recognition, genesis-to-tip). A source is routinely
      // lake_complete=true with complete=false. Rendering only the
      // served axis (C6-046) made that state look like a data
      // shortfall — see the "reconciling" branch below.
      completeness_complete?: boolean;
      completeness_lake_complete?: boolean;
      // Evolving: the REAL age of the row's data (web-status-4). The gap
      // detector (30 min cadence) stamps `coverage_snapshot_at`; the daily
      // compute-completeness timer stamps `completeness_computed_at`.
      // `backfill_coverage_as_of` is only the API's assembly time.
      coverage_snapshot_at?: string;
      completeness_computed_at?: string;
    }
  >;
  sources: Array<
    IngestionSnapshotWire['sources'][number] & {
      // Evolving: entries_24h — universal trailing-24h per-source event
      // count (stellarindex_source_events_total). Non-zero for every
      // active source, unlike trade_count_24h (trades-table only).
      // Optional + guarded: an explorer deploy ahead of the API can
      // receive a response that omits it — an unguarded render would
      // throw and the segment error boundary would blank the page.
      entries_24h?: number;
      // Evolving: enabled — whether the source is actually switched on
      // (stellarindex_source_enabled). The table lists every registry
      // entry, so without this a source that was implemented but never
      // wired renders 0 beside sources doing millions and reads as
      // broken rather than off. Optional + guarded like entries_24h: an
      // explorer deploy ahead of the API must not throw.
      enabled?: boolean;
    }
  >;
};

// RegionIngestion — one region's /v1/diagnostics/ingestion snapshot PLUS
// the envelope's honesty metadata. The server sets `flags.stale` when a
// critical reader failed during the build (zero-valued fields) or when the
// background refresher preserved a last-known-good snapshot after a
// degraded rebuild; it relies on the client honouring the flag for the
// response to be honest (diagnostics_ingestion.go ingestionFlags). Dropping
// it rendered "Lag from tip 0s" in green for a failed cursor read
// (web-status-1).
interface RegionIngestion {
  snapshot: IngestionSnapshot;
  stale: boolean;
  /** Envelope `as_of`; '' when the server omitted it. */
  asOf: string;
}

// Consecutive failed /v1/status polls before this page stops trusting the
// retained last-known snapshot for its headline verdict. Same value as
// DegradedBanner's FAILURE_THRESHOLD (components/nav/DegradedBanner.tsx) so
// the nav strip, the sidebar pill and this page flip together — one
// transient blip is ridden out, a real outage lands within two polls.
const STATUS_FEED_UNREACHABLE_AFTER = 2;

// Public-facing endpoints we surface on the status page.
// Not auto-derived from the OpenAPI spec because not every
// endpoint deserves a status row — operator surfaces (`/metrics`,
// `/v1/diagnostics/*`) clutter without adding signal.
//
// `probe` shapes how we hit the endpoint to render a real green/
// amber/red badge:
//   { kind: 'get', path: '…' }   — fetch the path verbatim
//   { kind: 'requires-auth' }    — show "auth req'd", no probe
//   { kind: 'streaming' }        — show "stream", no probe (SSE
//                                  open is heavy + blocks the
//                                  probe pool)
//
// Probe paths use minimal safe parameters where required (e.g.
// `?asset=native`, `?limit=1`) so each fetch returns a small
// payload and 200 means "the codepath is alive end-to-end".
type EndpointProbe =
  | { kind: 'get'; path: string }
  | { kind: 'requires-auth' }
  | { kind: 'streaming' };

// tier controls poll cadence:
//   - 'hot'  → 30 s. Health checks, network stats, anything cheap
//             enough that 30 s polling doesn't push tail latency.
//   - 'warm' → 2 min. Catalogue listings, history queries, oracle
//             lookups — endpoints whose backing queries are
//             expensive enough that a 30 s probe loop measurably
//             drives the SLO burn rate. Falls back to 2 min default
//             when omitted.
type ProbeTier = 'hot' | 'warm';

interface PublicEndpoint {
  path: string;
  group: string;
  description: string;
  probe: EndpointProbe;
  tier?: ProbeTier;
}

const PUBLIC_ENDPOINTS: PublicEndpoint[] = [
  {
    path: '/v1/healthz',
    group: 'Health',
    description: 'Liveness probe',
    probe: { kind: 'get', path: '/v1/healthz' },
    tier: 'hot',
  },
  {
    path: '/v1/readyz',
    group: 'Health',
    description: 'Readiness probe',
    probe: { kind: 'get', path: '/v1/readyz' },
    tier: 'hot',
  },
  {
    path: '/v1/price',
    group: 'Pricing',
    description: 'Current VWAP price for one asset',
    probe: { kind: 'get', path: '/v1/price?asset=native&quote=fiat:USD' },
    tier: 'hot',
  },
  {
    path: '/v1/price/batch',
    group: 'Pricing',
    description: 'Batch lookup, up to 1000 assets',
    probe: {
      kind: 'get',
      path: '/v1/price/batch?asset_ids=native&quote=fiat:USD',
    },
    tier: 'hot',
  },
  {
    path: '/v1/price/tip',
    group: 'Pricing',
    description: 'Rolling-window tip price',
    probe: { kind: 'get', path: '/v1/price/tip?asset=native&quote=fiat:USD' },
    tier: 'hot',
  },
  {
    path: '/v1/price/stream',
    group: 'Pricing',
    description: 'Closed-bucket SSE stream',
    probe: { kind: 'streaming' },
  },
  {
    path: '/v1/vwap',
    group: 'Pricing',
    description: 'VWAP over a window',
    // Probes that scan the trades hypertable directly need a pair
    // with real on-chain trades. (native, fiat:USD) doesn't exist
    // — XLM's USD price is a stablecoin-proxied USDC quote per the
    // aggregator policy. Use USDC SAC's underlying classic for
    // probe success.
    probe: {
      kind: 'get',
      path: '/v1/vwap?base=native&quote=USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
    },
  },
  {
    path: '/v1/twap',
    group: 'Pricing',
    description: 'TWAP over a window',
    probe: {
      kind: 'get',
      path: '/v1/twap?base=native&quote=USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
    },
  },
  {
    path: '/v1/ohlc',
    group: 'Pricing',
    description: 'OHLC bar',
    probe: {
      kind: 'get',
      path: '/v1/ohlc?base=native&quote=USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
    },
  },
  {
    path: '/v1/chart',
    group: 'Pricing',
    description: 'Multi-bar chart series',
    probe: {
      kind: 'get',
      path: '/v1/chart?asset=native&quote=fiat:USD&timeframe=24h&granularity=1h',
    },
  },
  {
    path: '/v1/history',
    group: 'Historical',
    description: 'Trade history within a window',
    probe: {
      kind: 'get',
      path: '/v1/history?base=native&quote=fiat:USD&limit=1',
    },
  },
  {
    path: '/v1/observations',
    group: 'Historical',
    description: 'Per-source latest trade',
    probe: {
      kind: 'get',
      path: '/v1/observations?asset=native&quote=fiat:USD',
    },
  },
  {
    path: '/v1/network/stats',
    group: 'Catalogue',
    description: 'Consolidated network aggregate (volume, markets, assets)',
    probe: { kind: 'get', path: '/v1/network/stats' },
    tier: 'hot',
  },
  {
    path: '/v1/assets',
    group: 'Catalogue',
    description:
      'Asset directory (440K+ classic assets, with coin-overlay fields)',
    probe: { kind: 'get', path: '/v1/assets?limit=1' },
  },
  {
    path: '/v1/assets/{id}',
    group: 'Catalogue',
    description: 'Asset detail + supply + market cap',
    probe: { kind: 'get', path: '/v1/assets/native' },
  },
  {
    path: '/v1/markets',
    group: 'Catalogue',
    description: 'Trading pairs',
    probe: { kind: 'get', path: '/v1/markets?limit=1' },
  },
  {
    path: '/v1/issuers',
    group: 'Catalogue',
    description: 'Issuer directory',
    probe: { kind: 'get', path: '/v1/issuers?limit=1' },
  },
  {
    path: '/v1/sources',
    group: 'Catalogue',
    description: 'Per-venue source metadata',
    probe: { kind: 'get', path: '/v1/sources' },
    tier: 'hot',
  },
  {
    path: '/v1/oracle/latest',
    group: 'Oracle',
    description: 'Latest oracle readings',
    // SEP-40 endpoints quote in fiat:USD; pick crypto:XLM as the
    // probe asset because Reflector consistently publishes XLM →
    // USD oracle observations. (USDC/USDT lastprice 404s — those
    // are stablecoins quoted in themselves.)
    probe: { kind: 'get', path: '/v1/oracle/latest?asset=crypto:XLM' },
  },
  {
    path: '/v1/oracle/lastprice',
    group: 'Oracle',
    description: 'SEP-40 lastprice',
    probe: { kind: 'get', path: '/v1/oracle/lastprice?asset=crypto:XLM' },
  },
  {
    path: '/v1/auth/login',
    group: 'Dashboard auth',
    description: 'Magic-link request',
    probe: { kind: 'requires-auth' },
  },
  {
    path: '/v1/auth/callback',
    group: 'Dashboard auth',
    description: 'Magic-link consume',
    probe: { kind: 'requires-auth' },
  },
  {
    path: '/v1/auth/sep10/challenge',
    group: 'API auth',
    description: 'SEP-10 challenge',
    probe: { kind: 'requires-auth' },
  },
];

// On the lean test nets there is no aggregator and no oracle contracts, so the
// Pricing / Oracle / Historical(price-observation) probe groups ALWAYS fail —
// they would render entire red sections that read as a major outage and spam
// 404s into the console every poll. Drop those groups there; the chain-native
// probe groups (Health, Ledgers, Assets, Markets, Accounts, Contracts, …) stay.
const VISIBLE_ENDPOINTS: PublicEndpoint[] = CURRENT_NETWORK.pricing
  ? PUBLIC_ENDPOINTS
  : PUBLIC_ENDPOINTS.filter(
      (e) =>
        e.group !== 'Pricing' &&
        e.group !== 'Oracle' &&
        e.group !== 'Historical',
    );

// 5s probe budget — well under the 30s polling interval. Every
// public endpoint should serve a 200 response within this budget;
// crossing it gets the "slow" tone even on 200.
const PROBE_TIMEOUT_MS = 5_000;
const PROBE_SLOW_MS = 800;

// IncidentHistoryEntry is the shape the IncidentHistory section
// consumes. It's a deliberately UI-flat shape — the API returns
// a richer shape (severity codes, structured timestamps, etc.)
// which IncidentHistory normalises before render. The build-time
// corpus is projected into the same shape by the server wrapper.
export interface IncidentHistoryEntry {
  slug: string;
  date: string;
  title: string;
  resolved: string;
  summary: string;
  severity: 'major' | 'minor' | 'maintenance';
}

interface IncidentsAPIShape {
  data: {
    incidents: Array<{
      slug: string;
      title: string;
      severity: 'SEV-1' | 'SEV-2' | 'SEV-3';
      status: 'investigating' | 'identified' | 'monitoring' | 'resolved';
      started_at: string;
      resolved_at?: string | null;
      affected_components?: string[];
      body_markdown: string;
    }>;
    count: number;
  };
}

// Live-feed fetch outcome — distinguishes "the request failed"
// (we're showing only the build-time corpus) from "succeeded but
// empty" (genuinely no incidents) so the empty-state copy can be
// honest. (WB-02c)
type IncidentFeedState = 'loading' | 'ok' | 'error';

export default function StatusPageClient({
  seedIncidents,
}: {
  // Build-time incident corpus, pre-projected to the UI-flat shape
  // by the server wrapper. Rendered immediately so past incidents
  // are visible even when the live API is fully down (WB-02b).
  seedIncidents: IncidentHistoryEntry[];
}) {
  // The /v1/status doc — from the SHARED useStatus query (one poll loop
  // per viewport, shared with DegradedBanner + the sidebar pill). The
  // feed keeps the last-known snapshot through failures, so the "showing
  // the last known snapshot" degraded rendering below still works.
  const statusQ = useStatus();
  const status = statusQ.data?.status ?? null;
  const asOf = statusQ.data?.asOf ?? '';
  const error = statusQ.data?.error ?? null;
  const consecutiveFailures = statusQ.data?.consecutiveFailures ?? 0;
  // The feed keeps the last-known snapshot through failures so the tiles
  // below can still show it — but the HEADLINE verdict must not be derived
  // from a snapshot we can no longer refresh (WB-04: a fetch we could not
  // complete is absence-of-signal, not an all-clear). Pre-fix an open tab
  // read "All systems operational" with a green pulse for the whole
  // outage (web-status-2).
  const feedUnreachable =
    error !== null && consecutiveFailures >= STATUS_FEED_UNREACHABLE_AFTER;
  const loading = statusQ.isPending;
  // Seed the static (auth / streaming) endpoints once via the lazy
  // initializer — they never get a fetch fired against them, so their
  // labels are derivable up front rather than painted by a setState in
  // the probe effect. GET endpoints fill in as their probes resolve.
  const [endpointHealth, setEndpointHealth] = useState<
    Record<string, EndpointProbeResult>
  >(() => {
    const init: Record<string, EndpointProbeResult> = {};
    for (const ep of VISIBLE_ENDPOINTS) {
      if (ep.probe.kind !== 'get') {
        init[ep.path] = { kind: 'static', label: ep.probe.kind };
      }
    }
    return init;
  });
  // Seed from the build-time corpus; the live feed overlays it on
  // a successful fetch.
  const [incidentHistory, setIncidentHistory] =
    useState<IncidentHistoryEntry[]>(seedIncidents);
  const [incidentFeed, setIncidentFeed] =
    useState<IncidentFeedState>('loading');

  // ingestionByRegion is keyed by REGIONS[].name. Each region
  // polls its own /v1/diagnostics/ingestion at hot cadence and
  // independently — a r2 outage shouldn't block r1's panel from
  // refreshing.
  const [ingestionByRegion, setIngestionByRegion] = useState<
    Record<string, RegionIngestion | null>
  >({});

  // Per-region ingestion snapshot. One fetch per region per
  // POLL_INTERVAL_MS (the backend response has Cache-Control
  // public, max-age=15 so the underlying load is minimal even
  // across many viewers). Each region polls independently —
  // r2 timing out doesn't stall r1's refresh.
  useEffect(() => {
    let cancelled = false;
    async function pollRegion(region: RegionDef) {
      try {
        const res = await fetch(
          `${region.apiBaseUrl}/v1/diagnostics/ingestion`,
          { cache: 'no-store' },
        );
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const env = (await res.json()) as {
          data: IngestionSnapshot;
          as_of?: string;
          flags?: { stale?: boolean };
        };
        if (cancelled) return;
        setIngestionByRegion((prev) => ({
          ...prev,
          [region.name]: {
            snapshot: env.data,
            // Fail closed: `flags` is a required EnvelopeMeta member, so an
            // absent flag is a contract breach we read as "cannot vouch for
            // freshness", never as an all-clear.
            stale: env.flags?.stale !== false,
            asOf: env.as_of ?? '',
          },
        }));
      } catch {
        if (cancelled) return;
        // Soft-fail: render the previous snapshot if any, else
        // an empty-state card. We don't surface this in the
        // top-level error banner because the /v1/status feed is
        // the canonical "is the region up" signal — ingestion
        // diagnostics being temporarily slow shouldn't paint the
        // whole page red.
        setIngestionByRegion((prev) =>
          region.name in prev ? prev : { ...prev, [region.name]: null },
        );
      }
    }
    for (const r of REGIONS) {
      pollRegion(r);
    }
    const id = setInterval(() => {
      for (const r of REGIONS) {
        pollRegion(r);
      }
    }, POLL_INTERVAL_MS);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, []);

  // Per-endpoint live probe. Endpoints split by tier:
  //   - hot  → POLL_INTERVAL_MS (30 s)
  //   - warm → WARM_PROBE_MS    (2 min)
  // Endpoints marked `requires-auth` or `streaming` keep their
  // static label and never get a fetch fired against them — they
  // populate once at mount.
  useEffect(() => {
    let cancelled = false;

    function runTier(tier: ProbeTier) {
      const tierEps = VISIBLE_ENDPOINTS.filter(
        (e) => e.probe.kind === 'get' && (e.tier ?? 'warm') === tier,
      );
      const probes = tierEps.map((e) => probeEndpoint(e));
      Promise.allSettled(probes.map((p) => p())).then((results) => {
        if (cancelled) return;
        setEndpointHealth((prev) => {
          const next = { ...prev };
          tierEps.forEach((ep, i) => {
            const r = results[i];
            next[ep.path] =
              r && r.status === 'fulfilled'
                ? r.value
                : { kind: 'error', latencyMs: -1 };
          });
          return next;
        });
      });
    }

    // Static labels (auth / streaming) are seeded in the useState
    // initializer above — nothing to paint here.
    runTier('hot');
    runTier('warm');
    const hotId = setInterval(() => runTier('hot'), POLL_INTERVAL_MS);
    const warmId = setInterval(() => runTier('warm'), WARM_PROBE_MS);

    return () => {
      cancelled = true;
      clearInterval(hotId);
      clearInterval(warmId);
    };
  }, []);

  // Incident history is fetched once at mount from /v1/incidents and
  // overlaid on the build-time seed. The corpus is small (a handful
  // of posts per year of operation) and changes only on redeploy, so
  // polling would be wasted work. On a fetch failure we keep the seed
  // — the panel never collapses to "no incidents" during an outage.
  useEffect(() => {
    // Lean test-nets run no Alertmanager and hide the incident-history
    // panel entirely, so skip the doomed /v1/incidents fetch (which would
    // only log a console error and set the feed to "error").
    if (!CURRENT_NETWORK.pricing) return;
    let cancelled = false;
    fetch(`${API_BASE_URL}/v1/incidents`, { cache: 'no-store' })
      .then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
      .then((env: IncidentsAPIShape) => {
        if (cancelled) return;
        const live = (env.data?.incidents ?? []).map(normaliseIncident);
        setIncidentHistory(mergeIncidents(seedIncidents, live));
        setIncidentFeed('ok');
      })
      .catch(() => {
        if (cancelled) return;
        // Keep the build-time seed; flag the feed as errored so the
        // empty-state copy distinguishes "fetch failed" from
        // "genuinely no incidents".
        setIncidentFeed('error');
      });
    return () => {
      cancelled = true;
    };
  }, [seedIncidents]);

  // On the lean nets, re-derive the service list and overall verdict from
  // what's genuinely observable (see leanServices) instead of trusting the
  // API's global roll-up, which counts the absent aggregator + un-heartbeated
  // indexer as "degraded". Mainnet uses the API's verdict verbatim.
  const displayServices = useMemo(
    () =>
      status
        ? CURRENT_NETWORK.pricing
          ? status.services
          : leanServices(status.services, ingestionByRegion)
        : [],
    [status, ingestionByRegion],
  );
  const effectiveOverall: ServiceStatus = feedUnreachable
    ? 'unknown'
    : CURRENT_NETWORK.pricing
      ? (status?.overall ?? 'unknown')
      : status
        ? worstStatus(displayServices)
        : 'unknown';
  const overallTone = useMemo(
    () => toneFor(effectiveOverall),
    [effectiveOverall],
  );

  return (
    <Container className="max-w-5xl space-y-8 py-10">
      <PageHead error={error} asOf={asOf} />
      <StatusNotices />
      {/* The unreachable notice sits ABOVE the headline so the verdict is
          never read without its caveat. */}
      {error && (
        <Card className="border-bad-300 bg-bad-50 text-bad-700 px-4 py-3 text-sm">
          Status feed unreachable: {error}.{' '}
          {status
            ? `Showing the last known snapshot below${asOf ? ` (from ${formatRelative(asOf)})` : ''}.`
            : 'No snapshot has been received yet — independent endpoint probes below still show live results, and past incidents are loaded from the build-time corpus.'}
        </Card>
      )}
      <OverallBanner status={effectiveOverall} tone={overallTone} />
      {loading && !status && !error && (
        <Card className="text-ink-faint px-4 py-8 text-center text-sm">
          Loading status…
        </Card>
      )}
      {status && (
        <>
          <ServiceGrid services={displayServices} />
          <LatencyStrip latency={status.latency} />
          {/* "Ingest freshness" is aggregator-centric (last aggregator tick +
              exchange-source count) — both absent by design on the lean nets,
              where the real freshness signal is the per-region ledger lag in
              IngestionRegions below. Show it only where the aggregator runs. */}
          {CURRENT_NETWORK.pricing && (
            <FreshnessRow freshness={status.freshness} />
          )}
          <IngestionRegions regions={REGIONS} snapshots={ingestionByRegion} />
          <ActiveIncidents incidents={status.incidents?.active ?? []} />
        </>
      )}
      {/* Backups panel is mainnet-only: the lean test-nets run with
              pgbackrest_backup_enabled=false (no backups, no drill), so
              there is nothing honest to show there. It fetches its own
              feed (/v1/diagnostics/backups) independently of /v1/status. */}
      {CURRENT_NETWORK.pricing && <BackupsPanel />}
      {/* EndpointMatrix renders UNCONDITIONALLY — it doesn't depend on
              the /v1/status feed; the matrix runs its own independent
              probes (so red badges show during an outage). WB-02 */}
      <EndpointMatrix endpoints={VISIBLE_ENDPOINTS} health={endpointHealth} />
      {/* Incident history is mainnet-only: the lean test-nets run no
              Alertmanager, so there is no incident feed to show and no
              postmortem corpus is bundled for them. */}
      {CURRENT_NETWORK.pricing && (
        <IncidentHistory entries={incidentHistory} feed={incidentFeed} />
      )}
      {status && <RegionMeta asOf={asOf} region={status.region} />}
    </Container>
  );
}

function PageHead({ error, asOf }: { error: string | null; asOf: string }) {
  return (
    <div className="flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between">
      <div>
        <div className="text-brand-600 mb-1.5 text-xs font-medium tracking-wider uppercase">
          System status
        </div>
        <h1 className="text-h1 text-ink font-semibold">Stellar Index status</h1>
        <p className="text-ink-muted mt-2 max-w-prose text-[15px] leading-relaxed">
          Live service health, request latency, ingest freshness, and the full
          public-endpoint matrix — probed independently from your browser.
        </p>
      </div>
      {/* The pulse is a liveness claim: it only pulses green while the
          latest poll succeeded. During an outage it goes grey and says how
          old the snapshot below actually is. */}
      {error === null ? (
        <div className="text-ink-muted flex items-center gap-2 text-sm whitespace-nowrap">
          <span className="animate-pulse-dot bg-ok-500 inline-block h-2 w-2 rounded-full" />
          Live · refreshed every 30 s
        </div>
      ) : (
        <div className="text-bad-700 flex items-center gap-2 text-sm whitespace-nowrap">
          <span className="bg-ink-faint inline-block h-2 w-2 rounded-full" />
          {asOf
            ? `Status feed unreachable · last successful poll ${formatRelative(asOf)}`
            : 'Status feed unreachable · no successful poll yet'}
        </div>
      )}
    </div>
  );
}

function OverallBanner({
  status,
  tone,
}: {
  status: ServiceStatus;
  tone: ReturnType<typeof toneFor>;
}) {
  const headlines: Record<ServiceStatus, string> = {
    ok: 'All systems operational',
    degraded: 'Degraded performance',
    down: 'Major outage',
    unknown: 'Status unknown',
  };
  const subtitles: Record<ServiceStatus, string> = {
    ok: 'Every service is reporting healthy.',
    degraded:
      'One or more services are reporting degraded performance. The API is still serving but customers may notice slower responses or stale data.',
    down: 'A major component is down. Pricing endpoints are likely returning errors. We are investigating.',
    unknown:
      'We can’t reach the status feed. The endpoint probes and incident history below are independent and remain live.',
  };
  const badgeLabels: Record<ServiceStatus, string> = {
    ok: 'Operational',
    degraded: 'Degraded',
    down: 'Outage',
    unknown: 'Unknown',
  };
  const Icon = tone.icon;
  return (
    <Card className={`overflow-hidden border ${tone.cardBorder}`}>
      <div className={`flex items-start gap-4 p-6 ${tone.cardBg}`}>
        <div
          className={`rounded-card bg-surface flex h-12 w-12 shrink-0 items-center justify-center ${tone.fg} ring-1 ${tone.ring}`}
        >
          <Icon className="h-6 w-6" />
        </div>
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-3">
            <h2 className="text-h3 text-ink font-semibold">
              {headlines[status]}
            </h2>
            <Badge tone={tone.badge} dot>
              {badgeLabels[status]}
            </Badge>
          </div>
          <p className="text-ink-muted mt-1.5 text-sm leading-relaxed">
            {subtitles[status]}
          </p>
        </div>
      </div>
    </Card>
  );
}

// StatusNotices renders the operator-authored banners from
// GET /v1/status/notices at the top of the page — a maintenance window
// or ongoing-incident announcement is the first thing a visitor should
// see. It polls independently on the standard cadence and collapses to
// nothing on an empty list. A fetch/parse FAILURE keeps the last-known
// notices and marks them (WB-04 — mirrors useStatus): the endpoint fails
// during exactly the outage an operator's notice announces, and clearing
// on error blanked the banner from every open tab for the whole window
// (web-status-6). Only a successful (possibly empty) response clears it.
function StatusNotices() {
  const [notices, setNotices] = useState<StatusNotice[]>([]);
  // Latest poll failure; null while the latest poll succeeded.
  const [fetchError, setFetchError] = useState<string | null>(null);
  // Wall-clock of the last successful poll, for the stale marker.
  const [fetchedAt, setFetchedAt] = useState<string>('');

  useEffect(() => {
    let cancelled = false;
    async function poll() {
      try {
        const res = await fetch(`${API_BASE_URL}/v1/status/notices`, {
          cache: 'no-store',
        });
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const env = (await res.json()) as {
          data?: { notices?: StatusNotice[] };
        };
        if (cancelled) return;
        // The endpoint already returns only active notices, but filter
        // defensively so a `resolved` row can never leak into the banner.
        setNotices(
          (env.data?.notices ?? []).filter((n) => n.status === 'active'),
        );
        setFetchError(null);
        setFetchedAt(new Date().toISOString());
      } catch (err) {
        if (cancelled) return;
        // Keep the last-known notices; surface the failure alongside them.
        setFetchError(err instanceof Error ? err.message : 'Network error');
      }
    }
    poll();
    const id = setInterval(poll, POLL_INTERVAL_MS);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, []);

  if (notices.length === 0) return null;

  return (
    <section className="space-y-3" aria-label="Operator notices">
      {notices.map((n) => (
        <NoticeBanner key={n.id} notice={n} />
      ))}
      {fetchError !== null && (
        <p className="text-warn-700 text-xs">
          Notices feed unreachable ({fetchError}) — showing the last notices
          received{fetchedAt ? ` ${formatRelative(fetchedAt)}` : ''}; they may
          have been updated or resolved since.
        </p>
      )}
    </section>
  );
}

function NoticeBanner({ notice }: { notice: StatusNotice }) {
  const tone = noticeTone(notice.severity);
  const Icon = tone.icon;
  const posted = notice.updated_at ?? notice.created_at;
  return (
    <Card className={`overflow-hidden border ${tone.cardBorder}`}>
      <div className={`flex items-start gap-4 p-5 ${tone.cardBg}`}>
        <div
          className={`rounded-card bg-surface flex h-10 w-10 shrink-0 items-center justify-center ${tone.fg} ring-1 ${tone.ring}`}
        >
          <Icon className="h-5 w-5" />
        </div>
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-3">
            <h2 className="text-ink text-base font-semibold">{notice.title}</h2>
            <Badge tone={tone.badge}>{tone.label}</Badge>
          </div>
          {/* Plain-text body rendered as a React child — escaped by
              React, never dangerouslySetInnerHTML. `whitespace-pre-line`
              preserves operator line breaks without allowing markup. */}
          <p className="text-ink-muted mt-1.5 text-sm leading-relaxed whitespace-pre-line">
            {notice.body}
          </p>
          {posted && (
            <p className="text-ink-faint mt-2 text-xs">
              Updated {formatRelative(posted)}
            </p>
          )}
        </div>
      </div>
    </Card>
  );
}

// The lean test nets run no aggregator, and their indexer posts no status
// heartbeat (so it comes back `unknown` with an epoch-0 last_seen even while
// it ingests perfectly). The API's global `overall` roll-up counts both as
// degrading, so /v1/status reports "degraded" on a completely healthy net.
// Rebuild the service list from what's actually true there: drop the
// by-design-absent aggregator, and derive the indexer's health from live
// ingestion freshness — a ledger advancing at 1–3 s lag is proof the indexer
// is healthy, heartbeat or not.
const LEAN_INGEST_STALE_SECS = 180;

function leanServices(
  services: ServiceEntry[],
  ingestion: Record<string, RegionIngestion | null>,
): ServiceEntry[] {
  // Only MEASURED lags vote. A stale (server-degraded / preserved) snapshot
  // or an unmeasured tip (latest_ledger 0 — the cursors reader failed or
  // no ledgerstream cursor exists) would otherwise contribute a 0 s lag
  // and flip the indexer to "ok" during the very stall this detects.
  const lags = Object.values(ingestion)
    .filter((r): r is RegionIngestion => r != null && !r.stale)
    .map((r) => r.snapshot.ledger)
    .filter((l) => l != null && l.latest_ledger > 0)
    .map((l) => l.lag_seconds)
    .filter((l): l is number => typeof l === 'number');
  const freshestLag = lags.length ? Math.min(...lags) : null;
  return services
    .filter((svc) => svc.name !== 'aggregator')
    .map((svc) => {
      if (svc.name !== 'indexer' || freshestLag == null) return svc;
      // Heartbeat isn't wired on the lean nets — trust live ingestion.
      // (ServiceEntry.status is the wire union ok|down|unknown; a genuinely
      // stalled ingest maps to `down`.)
      return {
        ...svc,
        status: freshestLag <= LEAN_INGEST_STALE_SECS ? 'ok' : 'down',
      } as ServiceEntry;
    });
}

const STATUS_RANK: Record<ServiceStatus, number> = {
  down: 3,
  degraded: 2,
  unknown: 1,
  ok: 0,
};

function worstStatus(services: ServiceEntry[]): ServiceStatus {
  let worst: ServiceStatus = 'ok';
  for (const s of services) {
    const st = (s.status ?? 'unknown') as ServiceStatus;
    if ((STATUS_RANK[st] ?? 0) > (STATUS_RANK[worst] ?? 0)) worst = st;
  }
  return worst;
}

function ServiceGrid({ services }: { services: ServiceEntry[] }) {
  return (
    <section>
      <SectionHead>Services</SectionHead>
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
        {services.map((svc) => (
          <ServiceCard key={svc.name} service={svc} />
        ))}
      </div>
    </section>
  );
}

function ServiceCard({ service }: { service: ServiceEntry }) {
  const tone = toneFor(service.status);
  const Icon = tone.icon;
  // A service that has never reported (the aggregator isn't run on the lean
  // test nets; an indexer heartbeat not yet recorded) comes back with an
  // epoch-0 last_seen, which formatRelative renders as an absurd "739854d ago".
  // Treat a pre-2000 timestamp as "never reported" rather than a real age.
  const seenAt = service.last_seen ? new Date(service.last_seen) : null;
  const validSeen =
    seenAt != null &&
    !Number.isNaN(seenAt.getTime()) &&
    seenAt.getFullYear() > 2000;
  // On the lean nets the indexer posts no heartbeat, so last_seen is epoch-0
  // even though leanServices() has already derived its true health from live
  // ingestion. Don't render the contradictory "Not reporting" under a green
  // icon — describe it by the derived status instead.
  const leanIndexerNoHeartbeat =
    !CURRENT_NETWORK.pricing && service.name === 'indexer' && !validSeen;
  const subtitle = validSeen
    ? `Last seen ${formatRelative(service.last_seen)}`
    : leanIndexerNoHeartbeat
      ? service.status === 'ok'
        ? 'Ingesting live'
        : service.status === 'unknown'
          ? 'Awaiting ingest signal'
          : 'Ingest stalled'
      : 'Not reporting';
  return (
    <Card className="flex items-start justify-between p-4">
      <div className="min-w-0">
        <div className="text-ink font-medium capitalize">{service.name}</div>
        <div className="text-ink-faint mt-1 text-xs">{subtitle}</div>
      </div>
      <Icon className={`h-5 w-5 shrink-0 ${tone.fg}`} />
    </Card>
  );
}

function LatencyStrip({ latency }: { latency: StatusResponse['latency'] }) {
  // The lean test nets wire no request-latency metrics, so /v1/status.latency
  // comes back all-zero over a 0-second window. `?? null` only catches
  // null/undefined, so a literal 0 rendered "0.0 ms" against "target 0" (red
  // breach bars) under a "0-min window". A zero/absent window means NOT
  // MEASURED, not a real 0ms measurement — pass null so the cells show '—'.
  const measured = (latency?.window_secs ?? 0) > 0;
  const cell = (v: number | null | undefined) =>
    measured ? (v ?? null) : null;
  return (
    <section>
      <SectionHead
        aside={
          measured
            ? `${Math.round((latency?.window_secs ?? 0) / 60)}-min window`
            : 'not measured'
        }
      >
        Request latency
      </SectionHead>
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
        {/* Targets come from the API so the bars are drawn against the
            same thresholds the `overall` roll-up judges against. `|| N`
            (not `?? N`) so a test-net 0 target falls back to the literal. */}
        <LatencyCell label="p50" value={cell(latency?.p50_ms)} target={50} />
        <LatencyCell
          label="p95"
          value={cell(latency?.p95_ms)}
          target={latency?.p95_target_ms || 200}
        />
        <LatencyCell
          label="p99"
          value={cell(latency?.p99_ms)}
          target={latency?.p99_target_ms || 500}
        />
      </div>
    </section>
  );
}

function LatencyCell({
  label,
  value,
  target,
}: {
  label: string;
  value: number | null;
  target: number;
}) {
  if (value == null) {
    return (
      <Card className="p-4">
        <div className="text-ink-faint text-[11px] font-medium tracking-wider uppercase">
          {label}
        </div>
        <div className="mt-1 flex items-baseline gap-2">
          <span className="tnum text-ink-faint text-2xl font-semibold">—</span>
          <span className="text-ink-muted text-xs">not measured</span>
          <span className="text-ink-faint ml-auto text-xs">
            target {target}
          </span>
        </div>
        <div className="bg-surface-subtle mt-2 h-1.5 overflow-hidden rounded-full" />
      </Card>
    );
  }
  const pct = Math.min(100, (value / target) * 100);
  const tone = pct < 60 ? 'ok' : pct < 100 ? 'warn' : ('bad' as const);
  const fg = {
    ok: 'text-ok-700',
    warn: 'text-warn-700',
    bad: 'text-bad-700',
  }[tone];
  const bar = {
    ok: 'bg-ok-500',
    warn: 'bg-warn-500',
    bad: 'bg-bad-500',
  }[tone];
  return (
    <Card className="p-4">
      <div className="text-ink-faint text-[11px] font-medium tracking-wider uppercase">
        {label}
      </div>
      <div className="mt-1 flex items-baseline gap-2">
        <span className={`tnum text-2xl font-semibold ${fg}`}>
          {value.toFixed(1)}
        </span>
        <span className="text-ink-muted text-xs">ms</span>
        <span className="text-ink-faint ml-auto text-xs">target {target}</span>
      </div>
      <div className="bg-surface-subtle mt-2 h-1.5 overflow-hidden rounded-full">
        <div className={`h-full ${bar}`} style={{ width: `${pct}%` }} />
      </div>
    </Card>
  );
}

function FreshnessRow({
  freshness,
}: {
  freshness: StatusResponse['freshness'];
}) {
  // Absent counts mean the freshness probe didn't answer. `?? 0` read
  // as "0 / 0 active sources" — total ingest death — on the public
  // status page. Absent renders '—' instead.
  const activeSources = freshness?.active_sources ?? null;
  const totalSources = freshness?.total_sources ?? null;
  const measured = activeSources != null && totalSources != null;
  const sourcePct =
    measured && totalSources > 0 ? (activeSources / totalSources) * 100 : 0;
  return (
    <section>
      <SectionHead>Ingest freshness</SectionHead>
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <Card className="p-4">
          <div className="text-ink-faint text-[11px] font-medium tracking-wider uppercase">
            Last aggregator tick
          </div>
          <div className="text-ink mt-1 font-mono text-sm">
            {/* Epoch-0 tick on the lean nets (no aggregator) → "—", not the
                absurd "739854d ago" formatRelative would produce. */}
            {snapshotAgeSeconds(freshness?.last_aggregator_tick) == null
              ? '—'
              : formatRelative(freshness?.last_aggregator_tick)}
          </div>
        </Card>
        <Card className="p-4">
          <div className="text-ink-faint text-[11px] font-medium tracking-wider uppercase">
            Active sources
          </div>
          <div className="mt-1 flex items-baseline gap-2">
            <span
              className={`tnum text-2xl font-semibold ${measured ? 'text-ink' : 'text-ink-faint'}`}
            >
              {measured ? activeSources : '—'}
            </span>
            <span className="text-ink-muted text-sm">
              {measured ? `/ ${totalSources}` : 'not measured'}
            </span>
          </div>
          <div className="bg-surface-subtle mt-2 h-1.5 overflow-hidden rounded-full">
            <div
              className="bg-brand-500 h-full"
              style={{ width: `${sourcePct}%` }}
            />
          </div>
        </Card>
      </div>
    </section>
  );
}

function ActiveIncidents({ incidents }: { incidents: IncidentEntry[] }) {
  return (
    <section>
      <SectionHead>Active incidents</SectionHead>
      {incidents.length === 0 ? (
        <Card className="text-ink-faint px-4 py-6 text-center text-sm">
          No active incidents.
        </Card>
      ) : (
        <ul className="space-y-2">
          {incidents.map((inc) => {
            const tone: BadgeTone =
              inc.severity === 'page'
                ? 'bad'
                : inc.severity === 'ticket'
                  ? 'warn'
                  : 'ok';
            return (
              <li key={inc.name}>
                <Card className="flex items-start justify-between p-4">
                  <div className="min-w-0">
                    <div className="text-ink font-mono text-sm font-medium">
                      {inc.name}
                    </div>
                    <div className="mt-1.5">
                      <Badge tone={tone} dot>
                        {inc.severity}
                      </Badge>
                    </div>
                  </div>
                  {/* runbook_url is live API JSON — only render it as a
                      live link when it passes the scheme allowlist
                      (http/https/mailto), else drop the anchor so a
                      javascript:/data: URL can't execute on click. */}
                  {inc.runbook_url && isSafeHref(inc.runbook_url) && (
                    <a
                      href={inc.runbook_url}
                      target="_blank"
                      rel="noopener noreferrer"
                      className="text-ink-muted hover:text-brand-600 ml-4 flex shrink-0 items-center gap-1 text-xs"
                    >
                      Runbook
                      <ExternalLink className="h-3 w-3" />
                    </a>
                  )}
                </Card>
              </li>
            );
          })}
        </ul>
      )}
    </section>
  );
}

function EndpointMatrix({
  endpoints,
  health,
}: {
  endpoints: typeof PUBLIC_ENDPOINTS;
  health: Record<string, EndpointProbeResult>;
}) {
  const grouped = useMemo(() => {
    const out: Record<string, typeof endpoints> = {};
    for (const ep of endpoints) {
      out[ep.group] = out[ep.group] ?? [];
      out[ep.group]!.push(ep);
    }
    return Object.entries(out);
  }, [endpoints]);

  return (
    <section>
      <SectionHead>Endpoints</SectionHead>
      <div className="space-y-5">
        {grouped.map(([group, eps]) => (
          <div key={group}>
            <h3 className="text-ink-faint mb-2 text-[11px] font-semibold tracking-wider uppercase">
              {group}
            </h3>
            <Card flat className="overflow-x-auto">
              <table className="w-full text-sm">
                <tbody className="divide-line divide-y">
                  {eps.map((ep) => {
                    const probe = health[ep.path];
                    return (
                      <tr key={ep.path}>
                        <td className="text-ink-body px-4 py-2.5 font-mono text-xs">
                          {ep.path}
                        </td>
                        <td className="text-ink-muted hidden px-4 py-2.5 text-xs sm:table-cell">
                          {ep.description}
                        </td>
                        <td className="px-4 py-2.5 text-right">
                          <EndpointBadge probe={probe} />
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </Card>
          </div>
        ))}
      </div>
    </section>
  );
}

// EndpointProbeResult is the union of states the matrix renders.
//   - 'fast' / 'slow' / 'down' come from a real fetch
//   - 'error' is a fetch that threw (network, abort, TLS)
//   - 'static' is a non-probed endpoint (auth-gated, streaming);
//     `label` is what to show in the badge ("auth req'd",
//     "stream"). These never animate or change colour because
//     the page can't observe them without escalating to a paid
//     synthetic-monitor.
type EndpointProbeResult =
  | { kind: 'fast'; latencyMs: number }
  | { kind: 'slow'; latencyMs: number }
  | { kind: 'down'; latencyMs: number; status: number }
  | { kind: 'error'; latencyMs: number }
  | { kind: 'static'; label: 'requires-auth' | 'streaming' };

// probeEndpoint returns a closure so the same-shape probe runs
// every poll without re-allocating the URL.
function probeEndpoint(ep: PublicEndpoint): () => Promise<EndpointProbeResult> {
  if (ep.probe.kind === 'requires-auth' || ep.probe.kind === 'streaming') {
    const label = ep.probe.kind;
    return () => Promise.resolve({ kind: 'static', label });
  }
  const url = `${API_BASE_URL}${ep.probe.path}`;
  return async () => {
    // Two-shot probe. The status page polls every 30 s (hot tier)
    // or 2 min (warm tier); between polls Cloudflare lets the
    // edge→origin connection pool go cold, so a single probe's
    // FIRST request pays a full CF↔origin TCP+TLS setup (~2-3 s
    // measured) that has nothing to do with API latency — the API
    // itself serves cached asset detail in <10 ms. The first fetch
    // below is a throwaway warm-up; the second, on the now-warm
    // connection, is the latency a returning user actually
    // experiences and the one we report. cache:'no-store' on both
    // so neither the browser nor the CDN serves a stale body — we
    // always measure a real round trip, just not a cold-pool one.
    try {
      const warm = await fetch(url, {
        signal: AbortSignal.timeout(PROBE_TIMEOUT_MS),
        cache: 'no-store',
      });
      // A non-2xx is a real status — report it without spending a
      // second request (and a second timeout) re-confirming it.
      if (!warm.ok) {
        return { kind: 'down', latencyMs: -1, status: warm.status };
      }
    } catch {
      // Warm-up threw (network / abort / TLS). No point timing a
      // second doomed request — report the error now.
      return { kind: 'error', latencyMs: -1 };
    }
    const start = performance.now();
    try {
      const res = await fetch(url, {
        signal: AbortSignal.timeout(PROBE_TIMEOUT_MS),
        cache: 'no-store',
      });
      const latencyMs = performance.now() - start;
      if (!res.ok) {
        return { kind: 'down', latencyMs, status: res.status };
      }
      return latencyMs < PROBE_SLOW_MS
        ? { kind: 'fast', latencyMs }
        : { kind: 'slow', latencyMs };
    } catch {
      const latencyMs = performance.now() - start;
      return { kind: 'error', latencyMs };
    }
  };
}

function EndpointBadge({ probe }: { probe?: EndpointProbeResult }) {
  if (!probe) {
    return (
      <Badge tone="neutral" className="font-mono text-[10px]">
        —
      </Badge>
    );
  }
  if (probe.kind === 'static') {
    return (
      <Badge tone="neutral" className="text-[10px]">
        {probe.label === 'requires-auth' ? "auth req'd" : 'stream'}
      </Badge>
    );
  }
  if (probe.kind === 'fast') {
    return (
      <Badge tone="ok" className="text-[10px]">
        <CheckCircle2 className="h-3 w-3" />
        <span className="tnum">{Math.round(probe.latencyMs)}ms</span>
      </Badge>
    );
  }
  if (probe.kind === 'slow') {
    return (
      <Badge tone="warn" className="text-[10px]">
        <AlertTriangle className="h-3 w-3" />
        <span className="tnum">{Math.round(probe.latencyMs)}ms</span>
      </Badge>
    );
  }
  if (probe.kind === 'down') {
    return (
      <Badge tone="bad" className="text-[10px]">
        <XCircle className="h-3 w-3" />
        <span className="tnum">{probe.status}</span>
      </Badge>
    );
  }
  return (
    <Badge tone="bad" className="text-[10px]">
      <XCircle className="h-3 w-3" />
      err
    </Badge>
  );
}

// normaliseIncident projects the API's structured shape onto the
// flat one IncidentHistory renders. Severity codes (SEV-1..3) map
// onto the UI's tone names (major / minor / maintenance);
// `summary` is the first paragraph of the markdown body so the
// panel doesn't render the entire post.
function normaliseIncident(
  raw: IncidentsAPIShape['data']['incidents'][number],
): IncidentHistoryEntry {
  const severity =
    raw.severity === 'SEV-1'
      ? 'major'
      : raw.severity === 'SEV-2'
        ? 'minor'
        : 'maintenance';
  const summary = (raw.body_markdown || '')
    .split(/\n## /m)[0]
    ?.replace(/^[#\s]+[^\n]*\n+/, '')
    .replace(/^<!--[\s\S]*?-->\s*/m, '')
    .trim()
    .slice(0, 400);
  const date = raw.started_at ? raw.started_at.slice(0, 10) : '';
  const resolved = raw.resolved_at
    ? `${raw.resolved_at.slice(0, 10)} ${raw.resolved_at.slice(11, 16)} UTC`
    : raw.status;
  return {
    slug: raw.slug,
    date,
    title: raw.title || raw.slug,
    resolved,
    severity,
    summary: summary || raw.title || raw.slug,
  };
}

// mergeIncidents overlays the live feed on the build-time seed,
// preferring the live entry for any slug present in both (it's
// fresher — a build-time seed of an unresolved incident can be
// stale). Slugless seed entries (shouldn't happen, the corpus is
// file-named) are kept as-is. Newest-first by date.
function mergeIncidents(
  seed: IncidentHistoryEntry[],
  live: IncidentHistoryEntry[],
): IncidentHistoryEntry[] {
  const bySlug = new Map<string, IncidentHistoryEntry>();
  for (const e of seed) bySlug.set(e.slug || `${e.date}|${e.title}`, e);
  for (const e of live) bySlug.set(e.slug || `${e.date}|${e.title}`, e);
  return Array.from(bySlug.values()).sort((a, b) =>
    a.date < b.date ? 1 : a.date > b.date ? -1 : 0,
  );
}

function severityBadgeTone(s: IncidentHistoryEntry['severity']): BadgeTone {
  return s === 'major' ? 'bad' : s === 'minor' ? 'warn' : 'ok';
}

function IncidentHistory({
  entries,
  feed,
}: {
  entries: IncidentHistoryEntry[];
  feed: IncidentFeedState;
}) {
  return (
    <section>
      <SectionHead
        action={
          <a
            href={`${API_BASE_URL}/v1/incidents.atom`}
            target="_blank"
            rel="noreferrer noopener"
            className="text-ink-faint hover:text-brand-600 text-xs"
            title="Atom feed — subscribe in Feedly, Slack RSS bot, etc."
          >
            Subscribe (Atom) ↗
          </a>
        }
      >
        Incident history
      </SectionHead>
      {entries.length === 0 ? (
        <Card className="text-ink-faint px-4 py-6 text-center text-sm">
          {feed === 'error'
            ? 'Incident feed unreachable — and no postmortems are bundled in this build. Past incidents will appear here once the feed is reachable again.'
            : 'No past incidents recorded yet. Resolved incidents will appear here once they post-mortem.'}
        </Card>
      ) : (
        <ul className="space-y-3">
          {entries.map((e) => (
            <li key={e.slug || e.date + e.title}>
              <Card interactive className="p-4">
                <div className="flex items-center justify-between gap-3">
                  <div className="flex min-w-0 items-center gap-2">
                    <Badge tone={severityBadgeTone(e.severity)}>
                      {e.severity}
                    </Badge>
                    {e.slug ? (
                      <a
                        href={`/status/incident/${e.slug}/`}
                        className="text-ink hover:text-brand-600 truncate font-medium"
                      >
                        {e.title}
                      </a>
                    ) : (
                      <span className="text-ink truncate font-medium">
                        {e.title}
                      </span>
                    )}
                  </div>
                  <span className="text-ink-faint shrink-0 font-mono text-xs">
                    {e.date}
                  </span>
                </div>
                <p className="text-ink-muted mt-2 text-sm leading-relaxed">
                  {e.summary}
                </p>
                <p className="text-ink-faint mt-1 text-xs">
                  Resolved: {e.resolved}
                </p>
                {e.slug && (
                  <a
                    href={`/status/incident/${e.slug}/`}
                    className="text-brand-600 mt-2 inline-block text-xs font-medium hover:underline"
                  >
                    Read full postmortem →
                  </a>
                )}
              </Card>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function RegionMeta({
  asOf,
  region,
}: {
  asOf: string;
  region: { name: string; deployment: string };
}) {
  return (
    <div className="border-line text-ink-faint border-t pt-4 text-xs">
      Region: <span className="font-mono">{region.name}</span> ·{' '}
      <span className="font-mono">{region.deployment}</span> · Last update:{' '}
      <span className="font-mono">
        {asOf ? new Date(asOf).toISOString() : '—'}
      </span>
    </div>
  );
}

// IngestionRegions renders one RegionPanel per entry in REGIONS.
// Today there's just r1 so the page collapses to a single panel;
// when r2/r3 ship, each gets its own framed block.
function IngestionRegions({
  regions,
  snapshots,
}: {
  regions: RegionDef[];
  snapshots: Record<string, RegionIngestion | null>;
}) {
  return (
    <section className="space-y-4">
      <SectionHead>Ingestion</SectionHead>
      {regions.map((r) => (
        <RegionPanel key={r.name} region={r} ingestion={snapshots[r.name]} />
      ))}
    </section>
  );
}

function RegionPanel({
  region,
  ingestion,
}: {
  region: RegionDef;
  ingestion: RegionIngestion | null | undefined;
}) {
  // Subscribe unconditionally (hooks can't be conditional) — the
  // result is null until the first SSE event lands, and LedgerCard
  // falls back to the snapshot while it is.
  //
  // FEC audit A6-1: this used to be a private useLedgerStream fork with a
  // raw EventSource (a second, unshared connection to the SAME URL the
  // sidebar badge already holds via the multiplexer) and a 60s stale
  // window checked every 30s (~90s worst-case "live" lie on the one page
  // whose job is truthful liveness). Canonical hook + 30s window + 10s
  // clock now — identical to the sidebar badge, connection shared.
  const liveLedger = useLedgerStream(region.apiBaseUrl);
  const clock = useLiveClock();

  // Treat the stream as live only while events are still arriving. (WB-04)
  const liveFresh =
    liveLedger != null &&
    !isFrameStale(clock, liveLedger.receivedAt, LEDGER_LIVE_STALE_MS)
      ? liveLedger.data
      : null;

  if (!ingestion) {
    return (
      <Card className="text-ink-faint p-4 text-sm">
        Waiting for first ingestion snapshot from{' '}
        <span className="font-mono">{region.name}</span>…
      </Card>
    );
  }
  const { snapshot, stale, asOf } = ingestion;
  return (
    <Card className="space-y-3 p-5">
      <RegionHeader
        region={region}
        snapshot={snapshot}
        stale={stale}
        asOf={asOf}
      />
      <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
        <LedgerCard ledger={snapshot.ledger} live={liveFresh} stale={stale} />
        {/* FX-backfill is aggregator-derived (fiat quotes) — 0 on the lean
            nets. Show it only where the aggregator runs. */}
        {CURRENT_NETWORK.pricing && (
          <FXBackfillCard fx={snapshot.fx_backfill} />
        )}
        <SupplyCard supply={snapshot.supply} />
      </div>
      {/* Backfill-coverage + source-health enumerate the full oracle/DEX-adapter
          source roster. The lean nets still carry that mainnet roster in config
          (every adapter at 0 trades, with mainnet genesis ledgers that render as
          negative expected-ledger counts against a 4M test-net tip), so both
          tables are noise there. The dedicated /sources page shows the real
          on-chain sources. Render them only where the aggregator runs. */}
      {CURRENT_NETWORK.pricing && (
        <>
          <BackfillCoverageTable
            rows={snapshot.backfill_coverage}
            asOf={snapshot.backfill_coverage_as_of}
          />
          <SourceHealthTable rows={snapshot.sources} />
        </>
      )}
    </Card>
  );
}

function RegionHeader({
  region,
  snapshot,
  stale,
  asOf,
}: {
  region: RegionDef;
  snapshot: IngestionSnapshot;
  stale: boolean;
  asOf: string;
}) {
  const v = snapshot.version;
  const commitShort = v.commit ? v.commit.slice(0, 7) : '—';
  const dirty = v.dirty === 'true';
  return (
    <div className="border-line flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1 border-b pb-3">
      <div className="flex items-baseline gap-2">
        <span className="text-ink font-mono text-sm font-semibold">
          {region.name}
        </span>
        <span className="text-ink-faint text-xs tracking-wider uppercase">
          {snapshot.region.deployment}
        </span>
        <span className="text-ink-muted text-xs">· {region.label}</span>
        {stale && (
          <span
            className="bg-warn-50 text-warn-700 rounded-sm px-1.5 py-0.5 text-[10px] font-medium tracking-wide uppercase"
            title="The API flagged this snapshot stale (flags.stale): a critical reader failed during the build, or this is a preserved last-known-good snapshot. Zero-valued fields are absent measurements, not readings."
          >
            stale · server degraded
            {asOf ? ` · as of ${formatRelative(asOf)}` : ''}
          </span>
        )}
      </div>
      <div
        className="text-ink-faint font-mono text-xs"
        title={`commit ${v.commit}\nbuilt ${v.build_date}\nGo ${v.go_version}`}
      >
        {v.version}{' '}
        <span className="text-ink-muted">
          @ {commitShort}
          {dirty && (
            <span className="bg-warn-50 text-warn-700 ml-1 rounded-sm px-1 text-[10px]">
              dirty
            </span>
          )}
        </span>
      </div>
    </div>
  );
}

function LedgerCard({
  ledger,
  live,
  stale,
}: {
  ledger: IngestionSnapshot['ledger'];
  live: LiveLedger | null;
  stale: boolean;
}) {
  // The SSE stream (when connected AND fresh) carries a fresher tip
  // than the 30s snapshot — prefer it for the ledger number + lag,
  // but keep the snapshot for volume / markets / assets, which the
  // stream does not carry. Falls back to the snapshot whenever the
  // stream is unavailable, disconnected, OR has gone stale (the
  // caller passes null once the last event ages past the staleness
  // window).
  //
  // Honesty (web-status-1): a fresh SSE frame is an independent, trusted
  // measurement. The SNAPSHOT is only a measurement when the server did
  // not flag it stale AND it carries a tip — `latest_ledger: 0` with
  // `lag_seconds: 0` is what a failed cursors/network-stats reader (or a
  // missing ledgerstream cursor) leaves behind, and pre-fix it painted a
  // best-possible "0s" in green. Unmeasured renders '—', never a tone.
  const snapshotMeasured = !stale && ledger.latest_ledger > 0;
  const latestLedger = live?.latest_ledger ?? ledger.latest_ledger;
  const lagSeconds = live?.lag_seconds ?? ledger.lag_seconds;
  const lagMeasured = live != null || snapshotMeasured;
  const lagTone = !lagMeasured
    ? ('unmeasured' as const)
    : lagSeconds < 15
      ? ('ok' as const)
      : lagSeconds < 60
        ? ('warn' as const)
        : ('bad' as const);
  const lagColor = {
    ok: 'text-ok-700',
    warn: 'text-warn-700',
    bad: 'text-bad-700',
    unmeasured: 'text-ink-muted',
  }[lagTone];
  // Snapshot-only counters: a zero on a stale snapshot is an absent
  // measurement, not a reading. (A SERVED zero on a fresh snapshot stays a
  // zero — that is a real, alarming number.)
  const countValue = (n: number) =>
    stale && n === 0 ? '—' : n.toLocaleString('en-US');
  return (
    <Panel
      title="Live ledger"
      accessory={
        live ? (
          <span className="text-ok-700 flex items-center gap-1 text-[10px] font-medium">
            <span className="bg-ok-700 h-1.5 w-1.5 animate-pulse rounded-full" />
            live
          </span>
        ) : null
      }
    >
      <Row
        label="Latest ledger"
        value={
          live != null || latestLedger > 0
            ? latestLedger.toLocaleString('en-US')
            : '—'
        }
        valueClass={live == null && stale ? 'text-ink-muted' : undefined}
        mono
      />
      <Row
        label="Lag from tip"
        value={lagMeasured ? `${lagSeconds}s` : '—'}
        valueClass={lagColor}
        mono
      />
      {/* USD volume needs the aggregator's pricing — unmeasurable on the lean
          nets, where it comes back "0"; show "—" rather than a false "$0". */}
      <Row
        label="24h volume"
        value={
          CURRENT_NETWORK.pricing &&
          ledger.volume_24h_usd &&
          Number(ledger.volume_24h_usd) > 0
            ? formatUSD(ledger.volume_24h_usd)
            : '—'
        }
      />
      <Row
        label="Markets (24h)"
        value={countValue(ledger.markets_count_24h)}
        valueClass={stale ? 'text-ink-muted' : undefined}
      />
      <Row
        label="Assets indexed"
        value={countValue(ledger.assets_indexed)}
        valueClass={stale ? 'text-ink-muted' : undefined}
      />
    </Panel>
  );
}

function FXBackfillCard({ fx }: { fx: IngestionSnapshot['fx_backfill'] }) {
  return (
    <Panel title="FX backfill (fx_quotes)">
      <Row
        label="Coverage"
        value={
          fx.earliest_quote && fx.latest_quote
            ? `${fx.earliest_quote} → ${fx.latest_quote}`
            : '—'
        }
        mono
      />
      <Row
        label="Currencies"
        value={fx.currencies_count.toLocaleString('en-US')}
      />
      <Row
        label="Total quotes"
        value={fx.total_quotes.toLocaleString('en-US')}
      />
    </Panel>
  );
}

function SupplyCard({ supply }: { supply: IngestionSnapshot['supply'] }) {
  // Age is computed via a module-scope helper (like `timeSince`) so the
  // impure `Date.now()` read stays out of the component's render body.
  const ageS = snapshotAgeSeconds(supply.last_snapshot_at);
  return (
    <Panel title="Supply observers">
      <Row
        label="Classic assets"
        value={supply.classic_assets_with_supply.toLocaleString('en-US')}
      />
      <Row
        label="SEP-41 assets"
        value={supply.sep41_assets_with_supply.toLocaleString('en-US')}
      />
      <Row
        label="Latest snapshot"
        value={ageS == null ? '—' : `${formatDurationShort(ageS)} ago`}
        mono
      />
    </Panel>
  );
}

// Per-axis staleness for the coverage table (web-status-4). The header's
// `backfill_coverage_as_of` is the API's per-request ASSEMBLY time (it
// reads "4s ago" forever); the figures come from rows with their own
// cadence. Two missed cycles = stale:
//   - gap detector → `coverage_snapshot_at`, every 30 min
//     (internal/storage/timescale/gap_detector.go GapDetectorInterval);
//   - compute-completeness → `completeness_computed_at`, the daily 05:30 UTC
//     systemd timer (configs/ansible … compute-completeness.timer.j2).
const COVERAGE_SNAPSHOT_STALE_MS = 2 * 30 * 60_000;
const COMPLETENESS_STALE_MS = 2 * 24 * 3_600_000;

// coverageDataAge — which timestamp the row's DISPLAYED figure is dated by,
// and whether it is older than its axis allows. A verified (`ran`) row shows
// the completeness verdict, so it is dated by the verifier's run; an
// unverified row shows the gap-detector figure.
function coverageDataAge(
  r: IngestionSnapshot['backfill_coverage'][number],
  ran: boolean,
): { at: string | undefined; stale: boolean } {
  const at = ran ? r.completeness_computed_at : r.coverage_snapshot_at;
  const ageS = snapshotAgeSeconds(at);
  if (ageS == null) return { at, stale: false };
  const limitMs = ran ? COMPLETENESS_STALE_MS : COVERAGE_SNAPSHOT_STALE_MS;
  return { at, stale: ageS * 1000 > limitMs };
}

function BackfillCoverageTable({
  rows,
  asOf,
}: {
  rows: IngestionSnapshot['backfill_coverage'];
  asOf?: string;
}) {
  if (!rows || rows.length === 0) {
    return (
      <div className="border-warn-300 bg-warn-50 text-warn-700 rounded-lg border p-3 text-xs">
        Coverage snapshot pending. This table shows two figures on
        different cadences: the gap-detector snapshot refreshes every 30
        min, and the ADR-0033 completeness verdict is a daily job (05:30
        UTC), so a verified row is normally hours old and that is
        expected — not a stalled pipeline.
      </div>
    );
  }
  const onChain = rows.filter((r) => r.applies);
  const offChain = rows.filter((r) => !r.applies);
  // The OLDEST on-chain row's data age — the honest headline freshness of
  // the table, shown alongside (not instead of) the assembly time.
  const oldestDataAt = onChain
    .map((r) => coverageDataAge(r, r.completeness_pct != null).at)
    .filter((t): t is string => snapshotAgeSeconds(t) != null)
    .sort((a, b) => new Date(a).getTime() - new Date(b).getTime())[0];
  return (
    <div>
      <div className="mb-2 flex items-baseline justify-between">
        <h3 className="text-ink-faint text-[11px] font-semibold tracking-wider uppercase">
          Ingest coverage — genesis → tip
        </h3>
        <span
          className="text-ink-faint text-[10px]"
          /* "oldest data" is dominated by the ADR-0033 completeness verdict,
             which is a DAILY job (05:30 UTC) — so a reading of a few hours is
             the normal state, not a stall. Without saying so the figure reads
             as a broken pipeline to anyone who assumes it tracks ingest. */
          title="Oldest figure in the table. The completeness verdict is recomputed daily (05:30 UTC) and the gap-detector snapshot every 30 min, so a few hours here is expected."
        >
          {oldestDataAt && (
            <>
              oldest data {formatRelative(oldestDataAt)}
              {asOf ? ' · ' : ''}
            </>
          )}
          {asOf && <>assembled {formatRelative(asOf)}</>}
        </span>
      </div>
      <p className="text-ink-faint mb-2 text-[11px]">
        <strong>Coverage</strong> = verified completeness (ADR-0033). A green %
        is <strong>fully verified</strong>: the lake is hash-chained to the tip
        (substrate), every event shape is recognized, AND the served tier
        reconciles to the lake (Δ=0). <em>reconciling</em> (amber) = data is
        captured in the lake but the served tier hasn&apos;t reconciled yet —{' '}
        <em>captured, not yet verified</em>; the % shown is capture, not the
        verdict. <em>unverified</em> = only a gap-free liveness signal exists
        (the verifier hasn&apos;t run), which can read ~100% for sparse or
        partially-indexed sources.
      </p>
      <div className="border-line overflow-x-auto rounded-lg border">
        <table className="w-full text-xs">
          <thead className="bg-surface-muted text-ink-faint">
            <tr>
              <th className="px-3 py-2 text-left font-medium">Source</th>
              <th className="px-3 py-2 text-right font-medium">Genesis</th>
              <th className="px-3 py-2 text-right font-medium">Earliest</th>
              <th className="px-3 py-2 text-right font-medium">Latest</th>
              <th className="px-3 py-2 text-right font-medium">Coverage</th>
              <th className="px-3 py-2 text-right font-medium">Data age</th>
              <th className="px-3 py-2 text-right font-medium">Entries</th>
            </tr>
          </thead>
          <tbody className="divide-line divide-y">
            {onChain.map((r) => {
              // ADR-0033 truthfulness: a source's coverage is only TRUSTWORTHY
              // once its completeness watermark (completeness_pct) is computed —
              // that's the substrate+projection-verified signal. Until then the
              // only other number is gap_free_pct, a LIVENESS proxy ("no large
              // gap detected"), which is NOT completeness: it reads ~100% for
              // sources that are merely sparse OR only recently/partially
              // indexed (e.g. 18 of 11.3M ledgers). Crucially we cannot tell
              // "sparse-but-complete" from "incomplete" without the watermark,
              // so we never dress an unverified figure up as a trustworthy
              // coverage bar — it's shown muted + tagged "unverified".
              // `ran` = the ADR-0033 verifier has computed a watermark for this
              // source. `reconciled` = the FULL verdict (substrate ∧ recognition
              // ∧ the served-tier projection reconciled to Δ=0) — the `complete`
              // flag. completeness_pct alone is DATA CAPTURE (the lake is
              // hash-chained to the tip); it can read 100% while the served tier
              // is still short, so the green "verified" bar is gated on
              // `reconciled`, NOT on the percentage.
              const ran = r.completeness_pct != null;
              const reconciled = r.completeness_complete === true;
              // The archive axis. When it is true and `reconciled` is
              // false, the data PROVABLY EXISTS genesis-to-tip and only
              // the served projection is behind — a materially different
              // statement from "we may be missing history", and the one
              // the status page previously could not make (C6-046).
              const lakeComplete = r.completeness_lake_complete === true;
              // The age of the figure this row DISPLAYS. An aged verdict
              // (web-status-4) must not keep the green verified tone: a
              // days-old complete=true says nothing about a source that
              // stalled since.
              const age = coverageDataAge(r, ran);
              const pct =
                (ran
                  ? (r.completeness_pct as number)
                  : (r.coverage_pct ?? r.gap_free_pct ?? r.density_pct ?? 0)) *
                100;
              const tone = !ran
                ? ('pending' as const)
                : age.stale
                  ? ('pending' as const)
                  : !reconciled
                    ? ('warn' as const)
                    : pct >= 99
                      ? 'ok'
                      : pct >= 50
                        ? 'warn'
                        : ('bad' as const);
              const colors = {
                ok: 'bg-ok-500 text-ok-700',
                warn: 'bg-warn-500 text-warn-700',
                bad: 'bg-bad-500 text-bad-700',
                pending: 'bg-line text-ink-muted',
              };
              return (
                <tr key={r.source}>
                  <td className="text-ink-body px-3 py-2 font-mono">
                    {r.source}
                  </td>
                  <td className="tnum text-ink-muted px-3 py-2 text-right font-mono">
                    {r.genesis_ledger?.toLocaleString('en-US') ?? '—'}
                  </td>
                  <td className="tnum text-ink-body px-3 py-2 text-right font-mono">
                    {r.earliest_ledger?.toLocaleString('en-US') ?? '—'}
                  </td>
                  <td className="tnum text-ink-body px-3 py-2 text-right font-mono">
                    {r.latest_ledger?.toLocaleString('en-US') ?? '—'}
                  </td>
                  <td
                    className="px-3 py-2 text-right"
                    title={
                      r.covered_ledgers !== undefined &&
                      r.expected_ledgers !== undefined
                        ? `${r.covered_ledgers.toLocaleString('en-US')} / ${r.expected_ledgers.toLocaleString('en-US')} ledgers covered by completed backfill ranges`
                        : undefined
                    }
                  >
                    {ran && reconciled ? (
                      <div
                        className="inline-flex items-center justify-end gap-2"
                        title={
                          age.stale
                            ? 'Verified verdict is older than two compute-completeness cycles — the source may have stalled since. Treat as unverified until the verifier re-runs.'
                            : undefined
                        }
                      >
                        {age.stale && (
                          <span className="bg-line text-warn-700 rounded-sm px-1 py-0.5 text-[10px] tracking-wide uppercase">
                            stale
                          </span>
                        )}
                        <div className="bg-surface-subtle h-1.5 w-16 overflow-hidden rounded-full">
                          <div
                            className={`h-full ${colors[tone].split(' ')[0]}`}
                            style={{ width: `${Math.max(2, pct)}%` }}
                          />
                        </div>
                        <span className={`tnum ${colors[tone].split(' ')[1]}`}>
                          {pct.toFixed(1)}%
                        </span>
                      </div>
                    ) : ran ? (
                      <span
                        className="text-warn-700 inline-flex items-center justify-end gap-1.5"
                        title={
                          lakeComplete
                            ? 'Archive PROVEN genesis-complete (ADR-0034 lake_complete=true: substrate continuity + hash chain + recognition, genesis to tip). The served tier has not reconciled to it yet (ADR-0033 complete=false), so the served numbers can still be short. No data is missing; the hourly completeness verify + lake re-derive close the gap.'
                            : 'Captured, not yet verified. The completeness watermark has been computed but NEITHER axis is complete: the archive is not yet proven genesis-complete (lake_complete=false) and the served tier has not reconciled. Genuine history may still be missing.'
                        }
                      >
                        <span className="bg-line text-warn-700 rounded-sm px-1 py-0.5 text-[10px] tracking-wide uppercase">
                          {lakeComplete ? 'archive complete' : 'reconciling'}
                        </span>
                        <span className="tnum">
                          {pct.toFixed(1)}%{' '}
                          {lakeComplete ? 'served' : 'captured'}
                        </span>
                      </span>
                    ) : (
                      <span
                        className="text-ink-muted inline-flex items-center justify-end gap-1.5"
                        title="Completeness not yet verified (ADR-0033). The figure is a gap-free liveness signal — no large gap detected — which can read ~100% for sparse or only-partially-indexed sources. Verified completeness is pending the data-recovery backfills."
                      >
                        <span className="bg-line rounded-sm px-1 py-0.5 text-[10px] tracking-wide uppercase">
                          unverified
                        </span>
                        <span className="tnum">{pct.toFixed(1)}% gap-free</span>
                      </span>
                    )}
                  </td>
                  <td
                    className={`tnum px-3 py-2 text-right ${age.stale ? 'text-warn-700' : 'text-ink-muted'}`}
                    title={
                      ran
                        ? 'When compute-completeness last verified this source (daily timer).'
                        : 'When the gap detector last measured this source (30 min cadence).'
                    }
                  >
                    {age.at ? formatRelative(age.at) : '—'}
                  </td>
                  <td className="tnum text-ink-muted px-3 py-2 text-right">
                    {r.entries.toLocaleString('en-US')}
                  </td>
                </tr>
              );
            })}
            {offChain.map((r) => (
              <tr key={r.source} className="text-ink-muted">
                <td className="px-3 py-2 font-mono">{r.source}</td>
                <td
                  className="px-3 py-2 text-right text-[10px] italic"
                  colSpan={5}
                >
                  off-chain — no Stellar ledger context
                </td>
                <td className="tnum px-3 py-2 text-right">
                  {r.entries.toLocaleString('en-US')}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function SourceHealthTable({ rows }: { rows: IngestionSnapshot['sources'] }) {
  // Defensive shape-handling — Go marshals nil slices as `null`,
  // not `[]`, so a typed-as-array field can still arrive as null.
  const safeRows = rows ?? [];
  if (safeRows.length === 0) return null;
  return (
    <div>
      <h3 className="text-ink-faint mb-2 text-[11px] font-semibold tracking-wider uppercase">
        Sources — {safeRows.length} registered
      </h3>
      <div className="border-line overflow-x-auto rounded-lg border">
        <table className="w-full text-xs">
          <thead className="bg-surface-muted text-ink-faint">
            <tr>
              <th className="px-3 py-2 text-left font-medium">Source</th>
              <th className="px-3 py-2 text-left font-medium">Class</th>
              <th className="px-3 py-2 text-right font-medium">Entries 24h</th>
              <th className="px-3 py-2 text-right font-medium">Volume 24h</th>
              <th className="px-3 py-2 text-right font-medium">Markets</th>
              <th className="px-3 py-2 text-center font-medium">VWAP</th>
            </tr>
          </thead>
          <tbody className="divide-line divide-y">
            {safeRows.map((r) => {
              const classLabel = r.subclass
                ? `${r.class}/${r.subclass}`
                : r.class;
              // `enabled === false` means never switched on. Treat an
              // ABSENT field as enabled: an explorer running ahead of an
              // API that does not send it yet must not label every source
              // "not enabled".
              const notEnabled = r.enabled === false;
              const silent =
                !notEnabled && r.include_in_vwap && r.entries_24h === 0;
              return (
                <tr key={r.name}>
                  <td className="text-ink-body px-3 py-2 font-mono">
                    {r.name}
                  </td>
                  <td className="text-ink-muted px-3 py-2">{classLabel}</td>
                  <td
                    className={`tnum px-3 py-2 text-right ${
                      silent ? 'text-bad-700' : 'text-ink-body'
                    }`}
                  >
                    {notEnabled ? (
                      <span
                        className="text-ink-faint"
                        title="This source is implemented but has never been switched on for this deployment — it is not failing, it is off."
                      >
                        not enabled
                      </span>
                    ) : (
                      (r.entries_24h?.toLocaleString('en-US') ?? '—')
                    )}
                  </td>
                  <td className="tnum text-ink-muted px-3 py-2 text-right">
                    {r.volume_24h_usd ? formatUSD(r.volume_24h_usd) : '—'}
                  </td>
                  <td className="tnum text-ink-muted px-3 py-2 text-right">
                    {r.markets_count_24h.toLocaleString('en-US')}
                  </td>
                  <td className="px-3 py-2 text-center">
                    {r.include_in_vwap ? (
                      <span className="text-ok-700">✓</span>
                    ) : (
                      <span className="text-ink-faint">—</span>
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </div>
  );
}

// Panel is a recessed sub-card used inside a RegionPanel for the
// metric well groups (live ledger, FX, market-cap, supply).
function Panel({
  title,
  accessory,
  children,
}: {
  title: string;
  accessory?: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <div className="border-line bg-surface-muted rounded-lg border p-4">
      <h3 className="text-ink-faint mb-2 flex items-center justify-between gap-2 text-[11px] font-semibold tracking-wider uppercase">
        <span>{title}</span>
        {accessory}
      </h3>
      <dl className="space-y-1.5 text-sm">{children}</dl>
    </div>
  );
}

function Row({
  label,
  value,
  mono,
  valueClass,
}: {
  label: string;
  value: string;
  mono?: boolean;
  valueClass?: string;
}) {
  return (
    <div className="flex items-baseline justify-between gap-3">
      <dt className="text-ink-muted text-xs">{label}</dt>
      <dd
        className={`tnum text-ink ${mono ? 'font-mono text-xs' : ''} ${
          valueClass ?? ''
        }`}
      >
        {value}
      </dd>
    </div>
  );
}

// formatUSD renders a backend-shaped decimal string (e.g.
// "4382354579.48040914") as a compact human form ("$4.38B").
// The backend keeps full precision via ADR-0003 stringified
// numerics; the UI rounds for display.
function formatUSD(s: string): string {
  // FEC audit F-A4-10: bucket math delegated to lib formatCompact (this
  // was a byte-level re-implementation); the zero→'—' gate stays local.
  const n = Number(s);
  if (!Number.isFinite(n) || n === 0) return '—';
  return `$${formatCompact(n)}`;
}

// formatAge turns seconds into "12s" / "5m" / "3h" / "2d".
// SectionHead is the page's between-block heading — an uppercase
// kicker with an optional right-aligned aside (window label) or
// action (a link). Mirrors the explorer's SectionHeader rhythm.
function SectionHead({
  children,
  aside,
  action,
}: {
  children: React.ReactNode;
  aside?: React.ReactNode;
  action?: React.ReactNode;
}) {
  return (
    <div className="mb-3 flex items-baseline justify-between gap-3">
      <h2 className="text-ink-muted text-sm font-semibold tracking-wider uppercase">
        {children}
        {aside && (
          <span className="text-ink-faint ml-2 text-xs font-normal tracking-normal normal-case">
            · {aside}
          </span>
        )}
      </h2>
      {action}
    </div>
  );
}

function toneFor(status?: ServiceStatus): {
  icon: typeof CheckCircle2;
  fg: string;
  ring: string;
  cardBg: string;
  cardBorder: string;
  badge: BadgeTone;
} {
  switch (status) {
    case 'ok':
      return {
        icon: CheckCircle2,
        fg: 'text-ok-700',
        ring: 'ring-ok-300/60',
        cardBg: 'bg-ok-50',
        cardBorder: 'border-ok-300',
        badge: 'ok',
      };
    case 'degraded':
      return {
        icon: AlertTriangle,
        fg: 'text-warn-700',
        ring: 'ring-warn-300/60',
        cardBg: 'bg-warn-50',
        cardBorder: 'border-warn-300',
        badge: 'warn',
      };
    case 'down':
      return {
        icon: XCircle,
        fg: 'text-bad-700',
        ring: 'ring-bad-300/60',
        cardBg: 'bg-bad-50',
        cardBorder: 'border-bad-300',
        badge: 'bad',
      };
    default:
      return {
        icon: Info,
        fg: 'text-ink-muted',
        ring: 'ring-line',
        cardBg: 'bg-surface-muted',
        cardBorder: 'border-line',
        badge: 'neutral',
      };
  }
}

// noticeTone maps a StatusNotice severity onto the same semantic
// palette the rest of the page uses: critical/major → bad (red),
// minor → warn (amber), maintenance → brand (blue, informational).
function noticeTone(severity: StatusNotice['severity']): {
  icon: typeof CheckCircle2;
  fg: string;
  ring: string;
  cardBg: string;
  cardBorder: string;
  badge: BadgeTone;
  label: string;
} {
  switch (severity) {
    case 'critical':
      return {
        icon: XCircle,
        fg: 'text-bad-700',
        ring: 'ring-bad-300/60',
        cardBg: 'bg-bad-50',
        cardBorder: 'border-bad-300',
        badge: 'bad',
        label: 'Critical',
      };
    case 'major':
      return {
        icon: AlertTriangle,
        fg: 'text-bad-700',
        ring: 'ring-bad-300/60',
        cardBg: 'bg-bad-50',
        cardBorder: 'border-bad-300',
        badge: 'bad',
        label: 'Major',
      };
    case 'minor':
      return {
        icon: AlertTriangle,
        fg: 'text-warn-700',
        ring: 'ring-warn-300/60',
        cardBg: 'bg-warn-50',
        cardBorder: 'border-warn-300',
        badge: 'warn',
        label: 'Minor',
      };
    case 'maintenance':
    default:
      return {
        icon: Info,
        fg: 'text-brand-700',
        ring: 'ring-brand-200',
        cardBg: 'bg-brand-50',
        cardBorder: 'border-brand-200',
        badge: 'brand',
        label: 'Maintenance',
      };
  }
}

// Seconds elapsed since an ISO timestamp, or null when absent. Kept at
// module scope so the `Date.now()` read isn't a purity violation inside a
// component's render (same rationale as `timeSince`).
function snapshotAgeSeconds(iso: string | null | undefined): number | null {
  if (!iso) return null;
  const t = new Date(iso).getTime();
  // Guard epoch-0 / invalid timestamps (a never-recorded snapshot on the lean
  // test nets) — they'd otherwise render an absurd ~2000-year "739854d ago".
  if (Number.isNaN(t) || new Date(iso).getUTCFullYear() < 2000) return null;
  return Math.floor((Date.now() - t) / 1000);
}
