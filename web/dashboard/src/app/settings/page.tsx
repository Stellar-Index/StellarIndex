'use client';

import { useState } from 'react';
import { useRouter } from 'next/navigation';
import { LogOut, ArrowUpRight, Loader2 } from 'lucide-react';
import { useAuth } from '@/lib/auth';
import { AuthedRoute } from '@/components/AuthedRoute';
import { logout, type AccountMe } from '@/lib/api';
import {
  Container,
  Section,
  PageHeader,
  Card,
  CardHeader,
  CardBody,
  Badge,
  Button,
  ButtonLink,
  Callout,
  Mono,
} from '@/components/ui';
import { fmtInt, tierLabel, tierCeiling, titleCase } from '@/lib/format';
import { cn } from '@/lib/cn';

export default function SettingsPage() {
  return (
    <AuthedRoute>
      <SettingsBody />
    </AuthedRoute>
  );
}

function SettingsBody() {
  const { state } = useAuth();
  if (state.kind !== 'authed') return null;
  const me = state.me;

  return (
    <Container className="py-8">
      <Section className="max-w-3xl space-y-6 py-0">
        <PageHeader
          eyebrow="Account"
          title="Settings"
          description="Your profile, plan, and account controls."
        />

        <ProfileCard me={me} />
        <PlanCard me={me} />
        <DangerZone />

        <p className="text-xs text-ink-faint">
          Need to change the email on file, rename the account, or configure
          webhooks? Contact{' '}
          <a className="font-medium text-brand-700 hover:underline" href="mailto:support@stellarindex.io">
            support@stellarindex.io
          </a>{' '}
          until self-service controls ship.
        </p>
      </Section>
    </Container>
  );
}

function ProfileCard({ me }: { me: AccountMe }) {
  const { user, account } = me;
  const rows: { label: string; value: React.ReactNode; mono?: boolean }[] = [
    { label: 'Email', value: user.email, mono: true },
    { label: 'Display name', value: user.display_name || '—' },
    { label: 'Role', value: titleCase(user.role) },
    { label: 'Account', value: account.name },
    { label: 'Account slug', value: account.slug, mono: true },
    { label: 'Account ID', value: <Mono value={account.id} truncate copy /> },
  ];
  return (
    <Card>
      <CardHeader title="Profile" description="Read-only account identity." />
      <CardBody className="p-0">
        <dl className="divide-y divide-line-subtle">
          {rows.map((r) => (
            <div
              key={r.label}
              className="flex items-center justify-between gap-4 px-5 py-3.5"
            >
              <dt className="text-sm text-ink-muted">{r.label}</dt>
              <dd
                className={cn(
                  'min-w-0 truncate text-right text-sm text-ink',
                  r.mono && 'font-mono text-[13px]',
                )}
              >
                {r.value}
              </dd>
            </div>
          ))}
        </dl>
      </CardBody>
    </Card>
  );
}

function PlanCard({ me }: { me: AccountMe }) {
  const ceiling = tierCeiling(me.account.tier);
  const isEnterprise = me.account.tier.toLowerCase() === 'enterprise';
  return (
    <Card>
      <CardHeader
        title="Plan"
        description="Your subscription tier and its limits."
        actions={
          <Badge tone={me.account.status === 'active' ? 'ok' : 'warn'} dot>
            {me.account.status}
          </Badge>
        }
      />
      <CardBody className="flex flex-col gap-5 sm:flex-row sm:items-center sm:justify-between">
        <div>
          <div className="text-2xl font-semibold tracking-tight text-ink">
            {tierLabel(me.account.tier)}
          </div>
          <div className="mt-1 text-sm text-ink-muted tnum">
            {ceiling !== null
              ? `${fmtInt(ceiling)} requests / minute`
              : 'Custom rate limits'}
          </div>
        </div>
        {isEnterprise ? (
          <ButtonLink
            href="mailto:sales@stellarindex.io"
            variant="secondary"
          >
            Contact your account team
          </ButtonLink>
        ) : (
          <ButtonLink href="mailto:sales@stellarindex.io" variant="primary">
            Upgrade plan
            <ArrowUpRight className="h-4 w-4" />
          </ButtonLink>
        )}
      </CardBody>
    </Card>
  );
}

function DangerZone() {
  const router = useRouter();
  const [signingOut, setSigningOut] = useState(false);

  async function handleSignOut() {
    setSigningOut(true);
    try {
      await logout();
    } finally {
      router.replace('/signin/');
    }
  }

  return (
    <Card className="border-bad-300">
      <CardHeader className="border-bad-300/60" title="Danger zone" />
      <CardBody className="space-y-4">
        <Callout tone="bad">
          Account deletion is handled by support. Email{' '}
          <a className="font-medium underline" href="mailto:support@stellarindex.io">
            support@stellarindex.io
          </a>{' '}
          to close your account and revoke all keys.
        </Callout>
        <div className="flex items-center justify-between gap-4">
          <div className="text-sm text-ink-muted">
            Sign out of the dashboard on this device.
          </div>
          <Button variant="secondary" onClick={handleSignOut} disabled={signingOut}>
            {signingOut ? (
              <Loader2 className="h-4 w-4 animate-spin" />
            ) : (
              <LogOut className="h-4 w-4" />
            )}
            {signingOut ? 'Signing out…' : 'Sign out'}
          </Button>
        </div>
      </CardBody>
    </Card>
  );
}
