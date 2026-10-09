import type { ReactNode } from 'react';

import { Badge, type BadgeTone } from '@/components/ui';

/** A visible badge (default "partial") whose click expands the qualifying note. */
export function NoteBadge({
  label = 'partial',
  tone = 'warn',
  children,
}: {
  label?: string;
  tone?: BadgeTone;
  children: ReactNode;
}) {
  return (
    <details className="text-ink-muted text-xs">
      <summary className="cursor-pointer">
        <Badge tone={tone}>{label}</Badge>
      </summary>
      <div className="mt-1">{children}</div>
    </details>
  );
}
