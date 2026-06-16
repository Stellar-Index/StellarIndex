'use client';

import { useCallback, useEffect, useState } from 'react';
import Link from 'next/link';
import {
  KeyRound,
  BarChart3,
  BookOpen,
  Plus,
  CheckCircle2,
  Circle,
  ArrowUpRight,
  Activity,
  ShieldCheck,
} from 'lucide-react';
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
  ButtonLink,
  Skeleton,
  Callout,
  Mono,
} from '@/components/ui';
import { fmtInt, fmtRelative, tierLabel, tierCeiling } from '@/lib/format';

const DOCS_URL = 'https://docs.stellarindex.io';

export default function OverviewPage() {
  return (
    <AuthedRoute>
      <OverviewBody />
    </AuthedRoute>
  );
}

function OverviewBody() {
  const { state } = useAuth();
  const [keys, setKeys] = useState<APIKey[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      setError(null);
      setKeys(await listKeys());
    } catch (err) {
      setError(
        err instanceof ApiError ? (err.detail ?? err.message) : 'Failed to load keys',
      );
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  if (state.kind !== 'authed') return null;
  const me = state.me;

  return (
    <Container className="py-8">
      <Section className="space-y-8 py-0">
        <PageHeader
          eyebrow="Dashboard"
          title={`Welcome back, ${firstName(me)}`}
          description="Your account at a glance — keys, plan, and where to go next."
          actions={
            <ButtonLink href="/keys/" variant="primary">
              <Plus className="h-4 w-4" />
              New API key
            </ButtonLink>
          }
        />

        {error && (
          <Callout tone="bad" title="Couldn't load your keys">
            {error}
          </Callout>
        )}

        {/* ── Headline metrics ─────────────────────────────────────── */}
        <MetricStrip me={me} keys={keys} />

        <div className="grid grid-cols-1 gap-6 lg:grid-cols-3">
          {/* ── Getting started checklist ──────────────────────────── */}
          <div className="lg:col-span-2">
            <GettingStarted me={me} keys={keys} />
          </div>

          {/* ── Quick links + plan ─────────────────────────────────── */}
          <div className="space-y-6">
            <PlanCard me={me} />
            <QuickLinks />
          </div>
        </div>
      </Section>
    </Container>
  );
}

function MetricStrip({ me, keys }: { me: AccountMe; keys: APIKey[] | null }) {
  if (keys === null) {
    return (
      <StatGrid cols={4}>
        {Array.from({ length: 4 }).map((_, i) => (
          <StatCell key={i}>
            <Skeleton className="h-3 w-24" />
            <Skeleton className="mt-2 h-8 w-16" />
          </StatCell>
        ))}
      </StatGrid>
    );
  }

  const active = keys.filter((k) => !k.revoked_at);
  const lastUsedAt = keys
    .map((k) => k.last_used_at)
    .filter((d): d is string => Boolean(d))
    .sort()
    .at(-1);
  const ceiling = tierCeiling(me.account.tier);

  return (
    <StatGrid cols={4}>
      <StatCell>
        <Stat
          icon={<KeyRound className="h-3.5 w-3.5" />}
          label="Active keys"
          value={fmtInt(active.length)}
          sub={
            keys.length > active.length
              ? `${fmtInt(keys.length - active.length)} revoked`
              : 'all active'
          }
        />
      </StatCell>
      <StatCell>
        <Stat
          icon={<ShieldCheck className="h-3.5 w-3.5" />}
          label="Plan"
          value={tierLabel(me.account.tier)}
          sub={
            me.account.status === 'active' ? 'active' : me.account.status
          }
        />
      </StatCell>
      <StatCell>
        <Stat
          icon={<Activity className="h-3.5 w-3.5" />}
          label="Rate limit"
          value={ceiling !== null ? fmtInt(ceiling) : '—'}
          sub="requests / min"
        />
      </StatCell>
      <StatCell>
        <Stat
          icon={<BarChart3 className="h-3.5 w-3.5" />}
          label="Last request"
          value={lastUsedAt ? fmtRelative(lastUsedAt) : 'No traffic yet'}
          sub={lastUsedAt ? 'across all keys' : 'mint a key to begin'}
        />
      </StatCell>
    </StatGrid>
  );
}

function GettingStarted({ me, keys }: { me: AccountMe; keys: APIKey[] | null }) {
  const hasKey = keys !== null && keys.some((k) => !k.revoked_at);
  const hasTraffic =
    keys !== null && keys.some((k) => Boolean(k.last_used_at));

  const steps: { done: boolean; title: string; body: ReactStep }[] = [
    {
      done: true,
      title: 'Create your account',
      body: (
        <>
          Signed in as <Mono value={me.user.email} copy={false} className="text-[13px]" />
        </>
      ),
    },
    {
      done: hasKey,
      title: 'Mint an API key',
      body: hasKey ? (
        'You have at least one active key.'
      ) : (
        <>
          Keys authenticate your requests to{' '}
          <code className="rounded bg-surface-subtle px-1 py-0.5 font-mono text-[12px]">
            api.stellarindex.io
          </code>
          .{' '}
          <Link href="/keys/" className="font-medium text-brand-700 hover:underline">
            Create one →
          </Link>
        </>
      ),
    },
    {
      done: hasTraffic,
      title: 'Make your first request',
      body: hasTraffic ? (
        'We have seen traffic on your keys.'
      ) : (
        <>
          Try the pricing API:{' '}
          <code className="rounded bg-surface-subtle px-1 py-0.5 font-mono text-[12px]">
            GET /v1/price/XLM-USD
          </code>{' '}
          with your{' '}
          <code className="rounded bg-surface-subtle px-1 py-0.5 font-mono text-[12px]">
            X-API-Key
          </code>{' '}
          header.
        </>
      ),
    },
    {
      done: false,
      title: 'Explore the docs',
      body: (
        <>
          Endpoints, rate limits, and SSE streams are in the{' '}
          <a
            href={DOCS_URL}
            target="_blank"
            rel="noopener noreferrer"
            className="font-medium text-brand-700 hover:underline"
          >
            API reference
          </a>
          .
        </>
      ),
    },
  ];

  return (
    <Card>
      <CardHeader
        title="Getting started"
        description="A few steps to get your integration live."
      />
      <CardBody className="space-y-1">
        {keys === null ? (
          <div className="space-y-4 py-2">
            {Array.from({ length: 3 }).map((_, i) => (
              <div key={i} className="flex gap-3">
                <Skeleton className="h-5 w-5 rounded-full" />
                <div className="flex-1 space-y-1.5">
                  <Skeleton className="h-4 w-40" />
                  <Skeleton className="h-3 w-64" />
                </div>
              </div>
            ))}
          </div>
        ) : (
          <ol className="divide-y divide-line-subtle">
            {steps.map((s, i) => (
              <li key={i} className="flex items-start gap-3 py-3 first:pt-1 last:pb-1">
                {s.done ? (
                  <CheckCircle2 className="mt-0.5 h-5 w-5 shrink-0 text-ok-500" />
                ) : (
                  <Circle className="mt-0.5 h-5 w-5 shrink-0 text-line-strong" />
                )}
                <div className="min-w-0">
                  <div
                    className={
                      s.done
                        ? 'text-sm font-medium text-ink-muted line-through decoration-line-strong'
                        : 'text-sm font-medium text-ink'
                    }
                  >
                    {s.title}
                  </div>
                  <div className="mt-0.5 text-sm text-ink-muted">{s.body}</div>
                </div>
              </li>
            ))}
          </ol>
        )}
      </CardBody>
    </Card>
  );
}

function PlanCard({ me }: { me: AccountMe }) {
  const ceiling = tierCeiling(me.account.tier);
  const isEnterprise = me.account.tier.toLowerCase() === 'enterprise';
  return (
    <Card>
      <CardHeader title="Your plan" />
      <CardBody className="space-y-4">
        <div className="flex items-center justify-between">
          <div>
            <div className="text-lg font-semibold text-ink">
              {tierLabel(me.account.tier)}
            </div>
            <div className="mt-0.5 text-sm text-ink-muted">
              {ceiling !== null
                ? `${fmtInt(ceiling)} req/min`
                : 'Custom limits'}
            </div>
          </div>
          <Badge
            tone={me.account.status === 'active' ? 'ok' : 'warn'}
            dot
          >
            {me.account.status}
          </Badge>
        </div>
        {!isEnterprise && (
          <ButtonLink href="/settings/" variant="secondary" size="sm" className="w-full">
            Manage plan
            <ArrowUpRight className="h-3.5 w-3.5" />
          </ButtonLink>
        )}
      </CardBody>
    </Card>
  );
}

function QuickLinks() {
  const links = [
    { href: '/keys/', label: 'API keys', icon: KeyRound, desc: 'Create & revoke' },
    { href: '/usage/', label: 'Usage', icon: BarChart3, desc: 'Requests & quota' },
  ];
  return (
    <Card>
      <CardHeader title="Quick links" />
      <CardBody className="space-y-1">
        {links.map((l) => {
          const Icon = l.icon;
          return (
            <Link
              key={l.href}
              href={l.href}
              className="group flex items-center gap-3 rounded-lg px-2 py-2 transition-colors hover:bg-surface-muted"
            >
              <span className="flex h-8 w-8 items-center justify-center rounded-lg bg-surface-subtle text-ink-muted group-hover:text-brand-700">
                <Icon className="h-4 w-4" />
              </span>
              <span className="min-w-0 flex-1">
                <span className="block text-sm font-medium text-ink">{l.label}</span>
                <span className="block text-xs text-ink-muted">{l.desc}</span>
              </span>
              <ArrowUpRight className="h-4 w-4 text-ink-faint group-hover:text-brand-600" />
            </Link>
          );
        })}
        <a
          href={DOCS_URL}
          target="_blank"
          rel="noopener noreferrer"
          className="group flex items-center gap-3 rounded-lg px-2 py-2 transition-colors hover:bg-surface-muted"
        >
          <span className="flex h-8 w-8 items-center justify-center rounded-lg bg-surface-subtle text-ink-muted group-hover:text-brand-700">
            <BookOpen className="h-4 w-4" />
          </span>
          <span className="min-w-0 flex-1">
            <span className="block text-sm font-medium text-ink">API docs</span>
            <span className="block text-xs text-ink-muted">
              Reference & guides
            </span>
          </span>
          <ArrowUpRight className="h-4 w-4 text-ink-faint group-hover:text-brand-600" />
        </a>
      </CardBody>
    </Card>
  );
}

type ReactStep = React.ReactNode;

function firstName(me: AccountMe): string {
  const name = me.user.display_name?.trim();
  if (name) return name.split(/\s+/)[0];
  return me.user.email.split('@')[0];
}
