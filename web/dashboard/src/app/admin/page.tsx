'use client';

import { ShieldCheck, Users, Sliders, AlertTriangle } from 'lucide-react';
import { useAuth } from '@/lib/auth';
import { AuthedRoute } from '@/components/AuthedRoute';
import {
  Container,
  Section,
  PageHeader,
  Card,
  CardBody,
  Badge,
  Callout,
  EmptyState,
} from '@/components/ui';

// Staff-only landing. Today this gates on `is_staff` and shows the planned
// staff surfaces; Phase 1.5 fills them with the customer look-up +
// impersonation tools from the platform spec §6.
export default function AdminPage() {
  return (
    <AuthedRoute>
      <AdminBody />
    </AuthedRoute>
  );
}

function AdminBody() {
  const { state } = useAuth();
  if (state.kind !== 'authed') return null;

  if (!state.me.user.is_staff) {
    return (
      <Container className="py-8">
        <Section className="max-w-2xl py-0">
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
    <Container className="py-8">
      <Section className="space-y-6 py-0">
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
