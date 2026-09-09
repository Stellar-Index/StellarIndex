import type { Metadata } from 'next';
import Link from 'next/link';
import { Wrench } from 'lucide-react';

import { loadOperationsDocs } from '@/lib/operations';

import { Container } from '@/components/ui';
export const metadata: Metadata = {
  alternates: { canonical: '/research/operations' },
  title: 'Operations runbooks — Stellar Index research',
  description:
    'Operator runbooks: archival-node bring-up, release process, deploy workflow, disaster recovery.',
};

export default function OperationsIndexPage() {
  const docs = loadOperationsDocs();
  return (
    <Container className="space-y-6 py-8">
      <header className="space-y-2">
        <h1 className="text-3xl font-semibold tracking-tight">
          Operations runbooks
        </h1>
        <p className="text-ink-body max-w-3xl text-base">
          Canonical recipes for standing up and operating Stellar Index.{' '}
          <Link href="/research" className="underline decoration-dotted">
            Back to research
          </Link>
          .
        </p>
      </header>
      <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
        {docs.map((d) => (
          <Link
            key={d.slug}
            href={`/research/operations/${d.slug}`}
            className="group border-line bg-surface hover:border-brand-300 flex flex-col gap-2 rounded-xl border p-4 transition hover:shadow-sm"
          >
            <div className="flex items-center gap-2">
              <Wrench className="text-ink-faint group-hover:text-brand-500 h-3.5 w-3.5" />
              <span className="text-sm font-semibold tracking-tight">
                {d.title}
              </span>
            </div>
            <p className="text-ink-body text-xs">{d.description}</p>
            <span className="text-ink-faint text-[10px] tracking-wider uppercase">
              Verified {d.last_verified}
            </span>
          </Link>
        ))}
      </div>
    </Container>
  );
}
