'use client';

import { AlertTriangle, Search, Sliders } from 'lucide-react';
import { useState } from 'react';

import { adminLookup, ApiError, type AdminLookupResult } from '@/api/account';
import type { MeResponse } from '@/api/hooks';
import {
  Badge,
  Button,
  Callout,
  Card,
  CardBody,
  Container,
  Input,
  PageHeader,
  Section,
  Table,
  TBody,
  TableWrap,
  Td,
  Th,
  THead,
  TR,
} from '@/components/ui';

import { AccountGate } from '../AccountGate';

/**
 * /dashboard/admin — staff-only cockpit. Gated on the magic-link session
 * (AccountGate) and then on `me.user.is_staff`. The Customer look-up tool
 * is live (POST /v1/account/admin/lookup); tier overrides + incident tooling
 * are still Phase 1.5 (they need write/impersonation endpoints).
 */
export default function AdminPage() {
  return <AccountGate>{(me) => <AdminBody me={me} />}</AccountGate>;
}

function AdminBody({ me }: { me: MeResponse }) {
  if (!me.user?.is_staff) {
    return (
      <Container>
        <Section className="max-w-2xl">
          <Callout tone="bad" title="Restricted area">
            This area is restricted to staff users.
          </Callout>
        </Section>
      </Container>
    );
  }

  return (
    <Container>
      <Section className="space-y-6">
        <PageHeader
          eyebrow="Internal"
          title="Staff cockpit"
          description="Customer look-up by email or account slug. Tier overrides and incident tooling ship in Phase 1.5."
          actions={
            <Badge tone="brand" dot>
              Staff access
            </Badge>
          }
        />

        <CustomerLookup />

        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <Card flat>
            <CardBody className="space-y-3">
              <span className="bg-surface-subtle text-ink-muted flex h-9 w-9 items-center justify-center rounded-lg">
                <Sliders className="h-[18px] w-[18px]" />
              </span>
              <div>
                <div className="text-ink text-sm font-semibold">
                  Tier overrides
                </div>
                <p className="text-ink-muted mt-1 text-sm">
                  Manually adjust an account tier or rate-limit ceiling.
                </p>
              </div>
              <Badge tone="neutral">Coming in Phase 1.5</Badge>
            </CardBody>
          </Card>
          <Card flat>
            <CardBody className="space-y-3">
              <span className="bg-surface-subtle text-ink-muted flex h-9 w-9 items-center justify-center rounded-lg">
                <AlertTriangle className="h-[18px] w-[18px]" />
              </span>
              <div>
                <div className="text-ink text-sm font-semibold">
                  Incident tools
                </div>
                <p className="text-ink-muted mt-1 text-sm">
                  Bulk key revocation and account suspension for incident
                  response.
                </p>
              </div>
              <Badge tone="neutral">Coming in Phase 1.5</Badge>
            </CardBody>
          </Card>
        </div>
      </Section>
    </Container>
  );
}

type LookupState =
  | { kind: 'idle' }
  | { kind: 'loading' }
  | { kind: 'ok'; result: AdminLookupResult }
  | { kind: 'error'; message: string };

function CustomerLookup() {
  const [q, setQ] = useState('');
  const [state, setState] = useState<LookupState>({ kind: 'idle' });

  async function onSubmit(e: React.FormEvent) {
    e.preventDefault();
    const term = q.trim();
    if (!term) return;
    setState({ kind: 'loading' });
    // A value with an "@" is an email; otherwise treat it as an account slug.
    const query = term.includes('@') ? { email: term } : { slug: term };
    try {
      const result = await adminLookup(query);
      setState({ kind: 'ok', result });
    } catch (err) {
      const message =
        err instanceof ApiError
          ? err.status === 404
            ? 'No matching customer.'
            : (err.detail ?? `${err.status} ${err.message}`)
          : 'Look-up failed.';
      setState({ kind: 'error', message });
    }
  }

  return (
    <Card>
      <CardBody className="space-y-4">
        <div className="flex items-center gap-2">
          <Search className="text-ink-muted h-[18px] w-[18px]" />
          <div className="text-ink text-sm font-semibold">Customer look-up</div>
        </div>
        <form onSubmit={onSubmit} className="flex gap-2">
          <Input
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="email@example.com or account-slug"
            aria-label="Customer email or account slug"
            className="flex-1"
          />
          <Button type="submit" disabled={state.kind === 'loading'}>
            {state.kind === 'loading' ? 'Searching…' : 'Look up'}
          </Button>
        </form>

        {state.kind === 'error' && (
          <Callout tone="bad" title="Not found">
            {state.message}
          </Callout>
        )}

        {state.kind === 'ok' && <LookupResult result={state.result} />}
      </CardBody>
    </Card>
  );
}

function LookupResult({ result }: { result: AdminLookupResult }) {
  const a = result.account;
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-x-6 gap-y-2 text-sm">
        <div>
          <div className="text-ink-muted text-[11px] tracking-wider uppercase">
            Account
          </div>
          <div className="text-ink mt-0.5 font-medium">{a.name || a.slug}</div>
        </div>
        <div>
          <div className="text-ink-muted text-[11px] tracking-wider uppercase">
            Slug
          </div>
          <div className="text-ink-body mt-0.5 font-mono">{a.slug}</div>
        </div>
        <div>
          <div className="text-ink-muted text-[11px] tracking-wider uppercase">
            Tier
          </div>
          <div className="mt-0.5">
            <Badge tone="brand">{a.tier}</Badge>
          </div>
        </div>
        <div>
          <div className="text-ink-muted text-[11px] tracking-wider uppercase">
            Status
          </div>
          <div className="mt-0.5">
            <Badge tone={a.status === 'active' ? 'ok' : 'bad'}>
              {a.status}
            </Badge>
          </div>
        </div>
      </div>
      {a.suspended_reason && (
        <Callout tone="bad" title="Suspended">
          {a.suspended_reason}
        </Callout>
      )}

      <TableWrap>
        <Table>
          <THead>
            <TR>
              <Th>User</Th>
              <Th>Role</Th>
              <Th>Verified</Th>
              <Th>Last login</Th>
            </TR>
          </THead>
          <TBody>
            {result.users.map((u) => (
              <TR key={u.id}>
                <Td>
                  {u.email}
                  {u.is_staff && (
                    <Badge tone="neutral" className="ml-2">
                      staff
                    </Badge>
                  )}
                </Td>
                <Td>{u.role}</Td>
                <Td>{u.email_verified ? 'yes' : 'no'}</Td>
                <Td className="text-ink-muted font-mono text-xs">
                  {u.last_login_at ? u.last_login_at.slice(0, 10) : '—'}
                </Td>
              </TR>
            ))}
          </TBody>
        </Table>
      </TableWrap>
    </div>
  );
}
