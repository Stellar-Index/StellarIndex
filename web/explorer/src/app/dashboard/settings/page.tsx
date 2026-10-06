'use client';

import { ArrowUpRight, Download, Loader2, LogOut } from 'lucide-react';
import Link from 'next/link';
import { useRouter } from 'next/navigation';
import { useState, type ReactNode } from 'react';

import { ApiError, deleteAccount, exportAccount, logout } from '@/api/account';
import type { MeResponse } from '@/api/hooks';
import {
  Badge,
  Button,
  ButtonLink,
  Callout,
  Card,
  CardBody,
  CardHeader,
  Container,
  Field,
  Input,
  Mono,
  PageHeader,
  Section,
} from '@/components/ui';
import {
  fmtInt,
  tierCeiling,
  tierLabel,
  titleCase,
} from '@/lib/account-format';
import { cn } from '@/lib/cn';

import { AccountGate } from '../AccountGate';
import { Passkeys } from './Passkeys';

/**
 * /dashboard/settings — read-only profile, the current plan, and a
 * danger zone (sign out). Rename / email change / deletion are
 * honestly deferred to support; webhook configuration is self-service
 * at /dashboard/webhooks. Ported from the standalone dashboard.
 */
export default function SettingsPage() {
  return <AccountGate>{(me) => <SettingsBody me={me} />}</AccountGate>;
}

function SettingsBody({ me }: { me: MeResponse }) {
  return (
    <Container>
      <Section className="max-w-3xl space-y-6">
        <PageHeader
          eyebrow="Account"
          title="Settings"
          description="Your profile, plan, and account controls."
        />

        <ProfileCard me={me} />
        <Passkeys />
        <PlanCard me={me} />
        <DangerZone me={me} />

        <p className="text-ink-faint text-xs">
          Need to change the email on file or rename the account? Contact{' '}
          <a
            className="text-brand-700 font-medium hover:underline"
            href="mailto:support@stellarindex.io"
          >
            support@stellarindex.io
          </a>{' '}
          until self-service controls ship. Webhooks are self-service — manage
          them under{' '}
          <Link
            className="text-brand-700 font-medium hover:underline"
            href="/dashboard/webhooks"
          >
            Webhooks
          </Link>
          .
        </p>
      </Section>
    </Container>
  );
}

function ProfileCard({ me }: { me: MeResponse }) {
  const user = me.user;
  const account = me.account;
  const rows: { label: string; value: ReactNode; mono?: boolean }[] = [
    { label: 'Email', value: user?.email ?? '—', mono: Boolean(user?.email) },
    { label: 'Display name', value: user?.display_name || '—' },
    { label: 'Role', value: titleCase(user?.role) },
    { label: 'Account', value: account?.name ?? '—' },
    {
      label: 'Account slug',
      value: account?.slug ?? '—',
      mono: Boolean(account?.slug),
    },
    {
      label: 'Account ID',
      value: account?.id ? <Mono value={account.id} truncate copy /> : '—',
    },
  ];
  return (
    <Card>
      <CardHeader title="Profile" description="Read-only account identity." />
      <CardBody className="p-0">
        <dl className="divide-line divide-y">
          {rows.map((r) => (
            <div
              key={r.label}
              className="flex items-center justify-between gap-4 px-5 py-3.5"
            >
              <dt className="text-ink-muted text-sm">{r.label}</dt>
              <dd
                className={cn(
                  'text-ink min-w-0 truncate text-right text-sm',
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

function PlanCard({ me }: { me: MeResponse }) {
  const tier = me.account?.tier ?? me.tier;
  const ceiling = tierCeiling(tier);
  // Enforced on a key minted without an explicit limit; never the tier
  // ceiling, which only caps what a key may be minted with.
  const rateLimit = me.account?.rate_limit_per_min ?? null;
  const status = me.account?.status ?? 'active';
  const isPartner = ['partner', 'enterprise'].includes(
    (tier ?? '').toLowerCase(),
  );
  return (
    <Card>
      <CardHeader
        title="Plan"
        description="Your account tier and its limits."
        actions={
          <Badge tone={status === 'active' ? 'ok' : 'warn'} dot>
            {status}
          </Badge>
        }
      />
      <CardBody className="flex flex-col gap-5 sm:flex-row sm:items-center sm:justify-between">
        <div>
          <div className="text-ink text-2xl font-semibold tracking-tight">
            {tierLabel(tier)}
          </div>
          <div className="tnum text-ink-muted mt-1 text-sm">
            {rateLimit !== null
              ? `${fmtInt(rateLimit)} requests / minute default key limit`
              : 'Custom rate limits'}
          </div>
          {ceiling !== null && (
            <div className="tnum text-ink-faint mt-0.5 text-xs">
              Plan ceiling: {fmtInt(ceiling)} requests / minute
            </div>
          )}
        </div>
        {isPartner ? (
          <ButtonLink href="mailto:sales@stellarindex.io" variant="secondary">
            Contact your account team
          </ButtonLink>
        ) : (
          <ButtonLink href="mailto:sales@stellarindex.io" variant="primary">
            Contact us for higher limits
            <ArrowUpRight className="h-4 w-4" />
          </ButtonLink>
        )}
      </CardBody>
    </Card>
  );
}

function DangerZone({ me }: { me: MeResponse }) {
  const router = useRouter();
  const slug = me.account?.slug ?? '';
  const [exporting, setExporting] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [typed, setTyped] = useState('');
  const [deleting, setDeleting] = useState(false);
  const [accountError, setAccountError] = useState<string | null>(null);
  const [signingOut, setSigningOut] = useState(false);
  const [signOutFailed, setSignOutFailed] = useState(false);

  async function handleSignOut() {
    setSigningOut(true);
    setSignOutFailed(false);
    try {
      await logout();
    } catch {
      setSignOutFailed(true);
      setSigningOut(false);
      return;
    }
    router.replace('/signin');
  }

  async function handleExport() {
    setExporting(true);
    setAccountError(null);
    try {
      const data = await exportAccount();
      const url = URL.createObjectURL(
        new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' }),
      );
      const a = document.createElement('a');
      a.href = url;
      a.download = `stellarindex-account-export-${Math.floor(Date.now() / 1000)}.json`;
      a.click();
      URL.revokeObjectURL(url);
    } catch (e) {
      setAccountError(accountErrorMessage(e, 'Export failed. Try again.'));
    } finally {
      setExporting(false);
    }
  }

  async function handleDelete() {
    setDeleting(true);
    setAccountError(null);
    try {
      await deleteAccount(typed);
    } catch (e) {
      setAccountError(
        accountErrorMessage(
          e,
          'Deletion failed. Nothing was changed; try again.',
        ),
      );
      setDeleting(false);
      return;
    }
    router.replace('/');
  }

  return (
    <Card className="border-bad-300">
      <CardHeader className="border-bad-300/60" title="Danger zone" />
      <CardBody className="space-y-4">
        <div className="flex items-center justify-between gap-4">
          <div className="text-ink-muted text-sm">
            Download everything held about this account as JSON. Owner only;
            sign in again first if your session is over 10 minutes old.
          </div>
          <Button
            variant="secondary"
            onClick={handleExport}
            disabled={exporting}
          >
            {exporting ? (
              <Loader2 className="h-4 w-4 animate-spin" />
            ) : (
              <Download className="h-4 w-4" />
            )}
            Export data
          </Button>
        </div>
        {confirming ? (
          <div className="space-y-3">
            <Callout tone="bad">
              This permanently deletes the account, its members, API keys,
              webhooks and alerts. It cannot be undone.
            </Callout>
            <Field
              label={`Type the account slug (${slug}) to confirm`}
              htmlFor="delete-confirm"
            >
              <Input
                id="delete-confirm"
                value={typed}
                onChange={(e) => setTyped(e.target.value)}
                autoComplete="off"
              />
            </Field>
            <div className="flex gap-2">
              <Button
                variant="secondary"
                onClick={handleDelete}
                disabled={deleting || slug === '' || typed !== slug}
              >
                {deleting && <Loader2 className="h-4 w-4 animate-spin" />}
                Delete account permanently
              </Button>
              <Button
                variant="secondary"
                onClick={() => {
                  setConfirming(false);
                  setTyped('');
                }}
                disabled={deleting}
              >
                Cancel
              </Button>
            </div>
          </div>
        ) : (
          <div className="flex items-center justify-between gap-4">
            <div className="text-ink-muted text-sm">
              Delete this account and revoke all keys.
            </div>
            <Button
              variant="secondary"
              onClick={() => setConfirming(true)}
              disabled={slug === ''}
            >
              Delete account
            </Button>
          </div>
        )}
        {accountError && <Callout tone="bad">{accountError}</Callout>}
        <div className="flex items-center justify-between gap-4">
          <div className="text-ink-muted text-sm">
            Sign out of your account on this device.
          </div>
          <Button
            variant="secondary"
            onClick={handleSignOut}
            disabled={signingOut}
          >
            {signingOut ? (
              <Loader2 className="h-4 w-4 animate-spin" />
            ) : (
              <LogOut className="h-4 w-4" />
            )}
            {signingOut ? 'Signing out…' : 'Sign out'}
          </Button>
        </div>
        {signOutFailed && (
          <Callout tone="bad">Sign out failed. Try again.</Callout>
        )}
      </CardBody>
    </Card>
  );
}

function accountErrorMessage(e: unknown, fallback: string): string {
  if (e instanceof ApiError) {
    if (e.status === 401)
      return 'Your session is too old. Sign in again, then retry.';
    if (e.status === 403) return 'Only the account owner can do this.';
    if (e.status === 409)
      return 'This account has billing state or a staff member and is handled by support@stellarindex.io.';
    if (e.status === 429) return 'Too many attempts. Try again in an hour.';
  }
  return fallback;
}
