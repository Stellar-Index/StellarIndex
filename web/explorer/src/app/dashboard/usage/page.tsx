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
import { isKeyLive } from '@/lib/api-key-status';
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
  fmtDateTime,
  fmtDateUTC,
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
  // A failed read renders as an error, never as the empty state: "no
  // tracked requests" is a claim of zero traffic. The keys section still
  // renders, so the page never blocks on the usage backend.
  const usageQuery = useQuery<UsageRow[], Error>({
    queryKey: ['dashboard', 'usage'],
    queryFn: ({ signal }) => fetchUsage(signal),
  });

  const keys = keysQuery.data ?? null;
  const usage = usageQuery.data ?? null;
  const error = keysQuery.error
    ? keysQuery.error instanceof ApiError
      ? (keysQuery.error.detail ?? keysQuery.error.message)
      : 'Failed to load usage'
    : null;
  const usageError = usageQuery.error
    ? usageQuery.error instanceof ApiError
      ? (usageQuery.error.detail ?? usageQuery.error.message)
      : 'Failed to load request history'
    : null;

  const active = keys?.filter((k) => isKeyLive(k)) ?? [];

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

        {usageError ? (
          <Callout tone="bad" title="Couldn't load request history">
            {usageError}
          </Callout>
        ) : (
          <>
            <MonthlyQuota me={me} usage={usage} />

            <DailyRequests usage={usage} />

            <EndpointBreakdown usage={usage} />
          </>
        )}

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

/**
 * The UTC calendar-day axis for the bars: dense, oldest → newest,
 * ending on the newest day actually served (never the client clock —
 * a skewed clock, or a server window not ending today, would mint
 * columns the reader never scanned) and capped at `windowDays` so the
 * chart never grows past the requested window.
 */
export function usageWindowUTC(rows: UsageRow[], windowDays: number): string[] {
  const days = rows
    .map((r) => r.date)
    .filter((d) => /^\d{4}-\d{2}-\d{2}$/.test(d))
    .sort();
  if (days.length === 0) return [];
  const endMs = Date.parse(`${days[days.length - 1]}T00:00:00Z`);
  const startMs = Math.max(
    Date.parse(`${days[0]}T00:00:00Z`),
    endMs - (windowDays - 1) * 86_400_000,
  );
  const out: string[] = [];
  for (let ms = startMs; ms <= endMs; ms += 86_400_000) {
    out.push(new Date(ms).toISOString().slice(0, 10));
  }
  return out;
}

/** Collapse per-endpoint rows into per-day totals for the bars, zero-filling
 * any day in the served window with no rows so a gap in traffic renders as
 * a real gap, not a shortened run of neighbouring bars. */
export function aggregateByDate(rows: UsageRow[]): DayAgg[] {
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
  return usageWindowUTC(rows, 30).map(
    (date) => byDate.get(date) ?? { date, requests: 0, errors: 0, throttled: 0 },
  );
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

/** UTC calendar-month prefix ("YYYY-MM") `date` falls in, defaulting to now. */
function utcMonthPrefix(now: Date = new Date()): string {
  return now.toISOString().slice(0, 7);
}

/**
 * Sum of `billable` for rows in the given UTC calendar month (the current
 * one by default) — the figure a monthly-quota 429 reports as
 * `month_to_date`. Never sums `requests`: that column includes 5xx, which
 * never consumes quota.
 */
export function monthToDateBillable(
  rows: UsageRow[],
  monthPrefix: string = utcMonthPrefix(),
): number {
  return rows
    .filter((r) => r.date.startsWith(monthPrefix))
    .reduce((sum, r) => sum + (r.billable || 0), 0);
}

/** Month-to-date billable usage against the account's monthly quota. */
function MonthlyQuota({
  me,
  usage,
}: {
  me: MeResponse;
  usage: UsageRow[] | null;
}) {
  const quota = me.account?.monthly_request_quota;
  const used = usage === null ? null : monthToDateBillable(usage);

  return (
    <StatGrid cols={2}>
      <StatCell>
        <Stat
          icon={<BarChart3 className="h-3.5 w-3.5" />}
          label="Month-to-date"
          value={used === null ? '—' : fmtInt(used)}
          sub="billable requests, UTC month"
        />
      </StatCell>
      <StatCell>
        <Stat
          icon={<Gauge className="h-3.5 w-3.5" />}
          label="Monthly quota"
          value={quota ? fmtInt(quota) : 'Unlimited'}
          sub="account-wide, per calendar month"
        />
      </StatCell>
    </StatGrid>
  );
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
  const active = keys.filter((k) => isKeyLive(k));
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
          label="Plan ceiling"
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
        title="Requests (last 30 days, UTC)"
        description="Per-account request counts recorded by the API, bucketed by UTC calendar day."
        actions={
          days && days.length > 0 ? (
            <span className="tnum text-ink-muted font-mono text-sm">
              {fmtInt(total)} requests (incl. errors, not the billable total)
            </span>
          ) : undefined
        }
      />
      <CardBody>
        {days === null ? (
          <Skeleton className="h-12 w-full" />
        ) : days.length === 0 ? (
          <p className="text-ink-muted text-sm">
            No tracked requests yet for this account in the last 30 days.
            Requests count against your account&apos;s monthly quota once you
            start calling the API with one of your keys.
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
    <div>
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
              title={`${fmtDateUTC(r.date)} UTC: ${fmtInt(r.requests)} requests${extras ? ` (${extras})` : ''}`}
              className="flex flex-1 flex-col items-center justify-end"
            >
              <div
                className="bg-brand-500/70 w-full rounded-xs"
                style={{ height: `${h}px` }}
              />
            </div>
          );
        })}
      </div>
      <div className="text-ink-muted tnum mt-1 flex justify-between font-mono text-xs">
        <span>{fmtDateUTC(rows[0].date)}</span>
        <span>{fmtDateUTC(rows[rows.length - 1].date)}</span>
      </div>
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
          <p className="text-ink-muted text-sm">
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
                  <code className="text-ink font-mono text-xs">
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
                <div className="text-ink font-medium">{k.name}</div>
                <code className="text-ink-muted mt-0.5 block font-mono text-xs">
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
