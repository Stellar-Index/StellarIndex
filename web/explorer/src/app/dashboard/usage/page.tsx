'use client';

import { useQuery } from '@tanstack/react-query';
import { BarChart3, Gauge, KeyRound } from 'lucide-react';

import {
  ApiError,
  fetchUsage,
  listKeys,
  type APIKey,
  type UsageRow,
} from '@/api/account';
import type { MeResponse } from '@/api/hooks';
import {
  Badge,
  ButtonLink,
  Callout,
  Card,
  CardBody,
  CardHeader,
  Container,
  EmptyState,
  PageHeader,
  Section,
  Skeleton,
  Stat,
  StatCell,
  StatGrid,
  Table,
  TableWrap,
  TBody,
  Td,
  Th,
  THead,
  TR,
} from '@/components/ui';
import {
  fmtDate,
  fmtDateTime,
  fmtInt,
  fmtRelative,
  tierCeiling,
  tierLabel,
} from '@/lib/account-format';

import { AccountGate } from '../AccountGate';

/**
 * /dashboard/usage — per-key activity, rate-limit headroom, daily
 * request volume, and the per-endpoint request / error / throttle
 * breakdown served by /v1/account/usage (one row per day × endpoint
 * family from the server-side usage_daily rollups; legacy
 * deployments degrade to endpoint-less per-day rows and the
 * endpoint table hides itself).
 */
export default function UsagePage() {
  return <AccountGate>{(me) => <UsageBody me={me} />}</AccountGate>;
}

function UsageBody({ me }: { me: MeResponse }) {
  const keysQuery = useQuery<APIKey[], Error>({
    queryKey: ['dashboard', 'keys'],
    queryFn: ({ signal }) => listKeys(signal),
  });
  // Daily request totals are best-effort — swallow failures to [] so the
  // page never blocks on the usage backend (matches prior behavior).
  const usageQuery = useQuery<UsageRow[], Error>({
    queryKey: ['dashboard', 'usage'],
    queryFn: async ({ signal }) => {
      try {
        return await fetchUsage(signal);
      } catch {
        return [];
      }
    },
  });

  const keys = keysQuery.data ?? null;
  const usage = usageQuery.data ?? null;
  const error = keysQuery.error
    ? keysQuery.error instanceof ApiError
      ? (keysQuery.error.detail ?? keysQuery.error.message)
      : 'Failed to load usage'
    : null;

  const active = keys?.filter((k) => !k.revoked_at) ?? [];

  return (
    <Container>
      <Section className="space-y-6">
        <PageHeader
          eyebrow="Activity"
          title="Usage"
          description="Your daily request volume, per-key activity, and rate-limit headroom."
        />

        {error && (
          <Callout tone="bad" title="Couldn't load usage">
            {error}
          </Callout>
        )}

        <HeadroomStrip me={me} keys={keys} />

        <DailyRequests usage={usage} />

        <EndpointBreakdown usage={usage} />

        <Card>
          <CardHeader
            title="Per-key activity"
            description="The most recent request seen on each key, with its configured limits."
          />
          {keys === null && !error ? (
            <CardBody className="space-y-3">
              {Array.from({ length: 3 }).map((_, i) => (
                <Skeleton key={i} className="h-12 w-full" />
              ))}
            </CardBody>
          ) : active.length === 0 ? (
            <CardBody>
              <EmptyState
                icon={<KeyRound className="h-5 w-5" />}
                title="No active keys"
                description="Create an API key and start making requests to see activity here."
                action={
                  <ButtonLink href="/dashboard/keys">Go to API keys</ButtonLink>
                }
              />
            </CardBody>
          ) : (
            <PerKeyTable keys={active} />
          )}
        </Card>

      </Section>
    </Container>
  );
}

// ─── Aggregations over the (day × endpoint) rows ───────────────────

interface DayAgg {
  date: string;
  requests: number;
  errors: number;
  throttled: number;
}

/** Collapse per-endpoint rows into per-day totals for the bars. */
function aggregateByDate(rows: UsageRow[]): DayAgg[] {
  const byDate = new Map<string, DayAgg>();
  for (const r of rows) {
    const agg = byDate.get(r.date) ?? {
      date: r.date,
      requests: 0,
      errors: 0,
      throttled: 0,
    };
    agg.requests += r.requests || 0;
    agg.errors += r.errors || 0;
    agg.throttled += r.throttled || 0;
    byDate.set(r.date, agg);
  }
  return [...byDate.values()].sort((a, b) => a.date.localeCompare(b.date));
}

interface EndpointAgg {
  endpoint: string;
  requests: number;
  errors: number;
  throttled: number;
}

/** Collapse rows into 30-day per-endpoint totals, busiest first. */
function aggregateByEndpoint(rows: UsageRow[]): EndpointAgg[] {
  const byEndpoint = new Map<string, EndpointAgg>();
  for (const r of rows) {
    if (!r.endpoint) continue; // legacy fallback rows carry no endpoint
    const agg = byEndpoint.get(r.endpoint) ?? {
      endpoint: r.endpoint,
      requests: 0,
      errors: 0,
      throttled: 0,
    };
    agg.requests += r.requests || 0;
    agg.errors += r.errors || 0;
    agg.throttled += r.throttled || 0;
    byEndpoint.set(r.endpoint, agg);
  }
  return [...byEndpoint.values()].sort((a, b) => b.requests - a.requests);
}

function HeadroomStrip({
  me,
  keys,
}: {
  me: MeResponse;
  keys: APIKey[] | null;
}) {
  if (keys === null) {
    return (
      <StatGrid cols={3}>
        {Array.from({ length: 3 }).map((_, i) => (
          <StatCell key={i}>
            <Skeleton className="h-3 w-24" />
            <Skeleton className="mt-2 h-8 w-16" />
          </StatCell>
        ))}
      </StatGrid>
    );
  }

  const tier = me.account?.tier ?? me.tier;
  const active = keys.filter((k) => !k.revoked_at);
  const ceiling = tierCeiling(tier);
  const totalProvisioned = active.reduce(
    (sum, k) => sum + (k.rate_limit_per_min || 0),
    0,
  );

  return (
    <StatGrid cols={3}>
      <StatCell>
        <Stat
          icon={<Gauge className="h-3.5 w-3.5" />}
          label="Tier ceiling"
          value={ceiling !== null ? `${fmtInt(ceiling)}` : '—'}
          sub={`${tierLabel(tier)} · req/min`}
        />
      </StatCell>
      <StatCell>
        <Stat
          icon={<KeyRound className="h-3.5 w-3.5" />}
          label="Active keys"
          value={fmtInt(active.length)}
          sub="authenticating now"
        />
      </StatCell>
      <StatCell>
        <Stat
          icon={<BarChart3 className="h-3.5 w-3.5" />}
          label="Provisioned"
          value={`${fmtInt(totalProvisioned)}`}
          sub="req/min across keys"
        />
      </StatCell>
    </StatGrid>
  );
}

function DailyRequests({ usage }: { usage: UsageRow[] | null }) {
  const days = usage === null ? null : aggregateByDate(usage);
  const total = (days ?? []).reduce((s, r) => s + r.requests, 0);
  return (
    <Card>
      <CardHeader
        title="Requests (last 30 days)"
        description="Per-account daily request counts recorded by the API."
        actions={
          days && days.length > 0 ? (
            <span className="tnum font-mono text-sm text-ink-muted">
              {fmtInt(total)} total
            </span>
          ) : undefined
        }
      />
      <CardBody>
        {days === null ? (
          <Skeleton className="h-12 w-full" />
        ) : days.length === 0 ? (
          <p className="text-sm text-ink-muted">
            No tracked requests yet for this account in the last 30 days.
            Requests count against the per-account daily window once you start
            calling the API with one of your keys.
          </p>
        ) : (
          <UsageBars rows={days} />
        )}
      </CardBody>
    </Card>
  );
}

function UsageBars({ rows }: { rows: DayAgg[] }) {
  const max = Math.max(...rows.map((r) => r.requests), 1);
  return (
    <div className="flex items-end gap-1">
      {rows.map((r) => {
        const h = Math.max(3, (r.requests / max) * 64);
        const extras = [
          r.errors > 0 ? `${fmtInt(r.errors)} errors` : null,
          r.throttled > 0 ? `${fmtInt(r.throttled)} throttled` : null,
        ]
          .filter(Boolean)
          .join(', ');
        return (
          <div
            key={r.date}
            title={`${fmtDate(r.date)}: ${fmtInt(r.requests)} requests${extras ? ` (${extras})` : ''}`}
            className="flex flex-1 flex-col items-center justify-end"
          >
            <div
              className="w-full rounded-xs bg-brand-500/70"
              style={{ height: `${h}px` }}
            />
          </div>
        );
      })}
    </div>
  );
}

function EndpointBreakdown({ usage }: { usage: UsageRow[] | null }) {
  const endpoints = usage === null ? null : aggregateByEndpoint(usage);
  return (
    <Card>
      <CardHeader
        title="Per-endpoint breakdown (last 30 days)"
        description="Requests, errors (4xx + 5xx), and rate-limit rejections per endpoint family. Throttled calls never count against your quota."
      />
      {endpoints === null ? (
        <CardBody>
          <Skeleton className="h-12 w-full" />
        </CardBody>
      ) : endpoints.length === 0 ? (
        <CardBody>
          <p className="text-sm text-ink-muted">
            No per-endpoint data yet. Rows appear within a few minutes of your
            first API request — the usage pipeline rolls counters up every five
            minutes.
          </p>
        </CardBody>
      ) : (
        <EndpointTable rows={endpoints} />
      )}
    </Card>
  );
}

function EndpointTable({ rows }: { rows: EndpointAgg[] }) {
  return (
    <TableWrap className="rounded-t-none border-0 border-t">
      <Table>
        <THead>
          <tr>
            <Th>Endpoint</Th>
            <Th align="right">Requests</Th>
            <Th align="right">Errors</Th>
            <Th align="right">Error rate</Th>
            <Th align="right">Throttled</Th>
          </tr>
        </THead>
        <TBody>
          {rows.map((r) => {
            const rate = r.requests > 0 ? (r.errors / r.requests) * 100 : 0;
            return (
              <TR key={r.endpoint}>
                <Td>
                  <code className="font-mono text-xs text-ink">
                    {r.endpoint}
                  </code>
                </Td>
                <Td align="right">{fmtInt(r.requests)}</Td>
                <Td align="right">
                  {r.errors > 0 ? (
                    fmtInt(r.errors)
                  ) : (
                    <span className="text-ink-faint">0</span>
                  )}
                </Td>
                <Td align="right">
                  {r.errors > 0 ? (
                    <span className={rate >= 5 ? 'text-down' : undefined}>
                      {rate.toFixed(rate < 10 ? 1 : 0)}%
                    </span>
                  ) : (
                    <span className="text-ink-faint">—</span>
                  )}
                </Td>
                <Td align="right">
                  {r.throttled > 0 ? (
                    <Badge tone="warn">{fmtInt(r.throttled)}</Badge>
                  ) : (
                    <span className="text-ink-faint">0</span>
                  )}
                </Td>
              </TR>
            );
          })}
        </TBody>
      </Table>
    </TableWrap>
  );
}

function PerKeyTable({ keys }: { keys: APIKey[] }) {
  return (
    <TableWrap className="rounded-t-none border-0 border-t">
      <Table>
        <THead>
          <tr>
            <Th>Key</Th>
            <Th align="right">Rate limit</Th>
            <Th align="right">Monthly quota</Th>
            <Th>Last request</Th>
          </tr>
        </THead>
        <TBody>
          {keys.map((k) => (
            <TR key={k.id}>
              <Td>
                <div className="font-medium text-ink">{k.name}</div>
                <code className="mt-0.5 block font-mono text-xs text-ink-muted">
                  {k.key_prefix}…
                </code>
              </Td>
              <Td align="right">{fmtInt(k.rate_limit_per_min)}/min</Td>
              <Td align="right">
                {k.monthly_quota ? (
                  fmtInt(k.monthly_quota)
                ) : (
                  <Badge tone="neutral">Unlimited</Badge>
                )}
              </Td>
              <Td>
                {k.last_used_at ? (
                  <span title={fmtDateTime(k.last_used_at)}>
                    {fmtRelative(k.last_used_at)}
                  </span>
                ) : (
                  <span className="text-ink-faint">No traffic yet</span>
                )}
              </Td>
            </TR>
          ))}
        </TBody>
      </Table>
    </TableWrap>
  );
}
