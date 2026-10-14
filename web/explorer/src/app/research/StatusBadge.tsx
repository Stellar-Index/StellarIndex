import type { ComponentProps } from 'react';
import { Badge } from '@/components/ui';
import type { ADRStatus } from '@/lib/adr';

const TONES: Record<ADRStatus, ComponentProps<typeof Badge>['tone']> = {
  Accepted: 'ok',
  Proposed: 'warn',
  Superseded: 'neutral',
  Rejected: 'bad',
};

export function StatusBadge({ status }: { status: ADRStatus }) {
  return <Badge tone={TONES[status] ?? 'warn'}>{status}</Badge>;
}
