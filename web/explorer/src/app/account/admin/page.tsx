'use client';

import { AlertTriangle, ShieldCheck, Sliders, Users } from 'lucide-react';

import type { MeResponse } from '@/api/hooks';
import {
  Badge,
  Callout,
  Card,
  CardBody,
  Container,
  EmptyState,
  PageHeader,
  Section,
} from '@/components/ui';

import { AccountGate } from '../AccountGate';

/**
 * /account/admin — staff-only cockpit. Gated on the magic-link session
 * (AccountGate) and then on `me.user.is_staff`. Today it advertises the
 * planned staff surfaces; Phase 1.5 fills them with the customer look-up +
 * impersonation tools from the platform spec §6. Ported from the standalone
 * dashboard's /admin when that app was consolidated into the site.
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

  const tools = [
    {
      icon: Users,
      title: 'Customer look-up',
      desc: 'Search accounts by email or slug; inspect tier, status, and keys.',
    },
    {
      icon: Sliders,
      title: 'Tier overrides',
      desc: 'Manually adjust an account tier or rate-limit ceiling.',
    },
    {
      icon: AlertTriangle,
      title: 'Incident tools',
      desc: 'Bulk key revocation and account suspension for incident response.',
    },
  ];

  return (
    <Container>
      <Section className="space-y-6">
        <PageHeader
          eyebrow="Internal"
          title="Staff cockpit"
          description="Customer look-up, manual tier overrides, key revocation, and incident tooling."
          actions={
            <Badge tone="brand" dot>
              Staff access
            </Badge>
          }
        />

        <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
          {tools.map((t) => {
            const Icon = t.icon;
            return (
              <Card key={t.title} flat>
                <CardBody className="space-y-3">
                  <span className="flex h-9 w-9 items-center justify-center rounded-lg bg-surface-subtle text-ink-muted">
                    <Icon className="h-[18px] w-[18px]" />
                  </span>
                  <div>
                    <div className="text-sm font-semibold text-ink">{t.title}</div>
                    <p className="mt-1 text-sm text-ink-muted">{t.desc}</p>
                  </div>
                  <Badge tone="neutral">Coming in Phase 1.5</Badge>
                </CardBody>
              </Card>
            );
          })}
        </div>

        <EmptyState
          icon={<ShieldCheck className="h-5 w-5" />}
          title="Staff tools ship in Phase 1.5"
          description="The platform-store cutover (spec §6) wires the customer look-up and impersonation surfaces into this cockpit."
        />
      </Section>
    </Container>
  );
}
