import Link from 'next/link';
import type { ReactNode } from 'react';

import { Container, EmptyState } from '@/components/ui';

/**
 * Shared frame for entity not-found / CI-stub branches (FEC audit A1-7/A1-8):
 * every "no such {entity}" branch renders in the standard Container frame with
 * the design-system EmptyState. The bare prose-width divs this replaces were
 * the founding container-drift shape, hand-written divergently per route
 * (sources vs issuers vs markets).
 */
export function EntityNotFoundShell({
  title,
  description,
  backHref,
  backLabel,
}: {
  title: ReactNode;
  description?: ReactNode;
  backHref: string;
  backLabel: ReactNode;
}) {
  return (
    <Container className="py-8">
      <EmptyState
        title={title}
        description={description}
        action={
          <Link
            href={backHref}
            className="text-brand-600 inline-flex items-center gap-1 text-sm hover:underline"
          >
            {backLabel} →
          </Link>
        }
      />
    </Container>
  );
}
