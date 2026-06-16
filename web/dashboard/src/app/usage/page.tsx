'use client';

import { useCallback, useEffect, useState } from 'react';
import Link from 'next/link';
import { BarChart3, KeyRound, Gauge } from 'lucide-react';
import { AuthedRoute } from '@/components/AuthedRoute';
import { useAuth } from '@/lib/auth';
import { ApiError, listKeys, type APIKey, type AccountMe } from '@/lib/api';
import {
  Container,
  Section,
  PageHeader,
  Card,
  CardHeader,
  CardBody,
  Stat,
  StatGrid,
  StatCell,
  Badge,
  Button,
  Callout,
  EmptyState,
  Skeleton,
  TableWrap,
  Table,
  THead,
  TBody,
  TR,
  Th,
  Td,
} from '@/components/ui';
import { fmtInt, fmtRelative, fmtDateTime, tierCeiling, tierLabel } from '@/lib/format';

export default function UsagePage() {
  return (
    <AuthedRoute>
      <UsageBody />
    </AuthedRoute>
  );
}

function UsageBody() {
  const { state } = useAuth();
  const [keys, setKeys] = useState<APIKey[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      setError(null);
      setKeys(await listKeys());
    } catch (err) {
      setError(
        err instanceof ApiError ? (err.detail ?? err.message) : 'Failed to load usage',
      );
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  if (state.kind !== 'authed') return null;
  const me = state.me;
  const active = keys?.filter((k) => !k.revoked_at) ?? [];

  return (
    <Container className="py-8">
      <Section className="space-y-6 py-0">
        <PageHeader
          eyebrow="Activity"
          title="Usage"
          description="Per-key activity and your rate-limit headroom. Detailed request-volume charts arrive once the usage pipeline is live."
        />

        {error && (
          <Callout tone="bad" title="Couldn't load usage">
            {error}
          </Callout>
        )}

        {/* ── Headroom summary ─────────────────────────────────────── */}
        <HeadroomStrip me={me} keys={keys} />

        {/* ── Per-key activity ─────────────────────────────────────── */}
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
                  <Link href="/keys/">
                    <Button>Go to API keys</Button>
                  </Link>
                }
              />
            </CardBody>
          ) : (
            <PerKeyTable keys={active} />
          )}
        </Card>

        {/* ── Honest note about the analytics that don't exist yet ── */}
        <Callout tone="info" title="Request-volume analytics are on the way">
          Time-series charts of requests per day, error rates, and per-endpoint
          breakdowns ship once the usage pipeline (Redis stream → Timescale
          worker) is wired through. Until then this page shows the activity the
          API exposes today: the last request seen per key and your tier
          headroom.
        </Callout>
      </Section>
    </Container>
  );
}

function HeadroomStrip({ me, keys }: { me: AccountMe; keys: APIKey[] | null }) {
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

  const active = keys.filter((k) => !k.revoked_at);
  const ceiling = tierCeiling(me.account.tier);
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
          sub={`${tierLabel(me.account.tier)} · req/min`}
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
