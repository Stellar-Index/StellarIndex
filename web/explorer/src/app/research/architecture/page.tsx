import type { Metadata } from 'next';
import Link from 'next/link';
import { BookOpen } from 'lucide-react';

import { loadArchitectureDocs } from '@/lib/architecture';

import { Container, PageHeader } from '@/components/ui';
export const metadata: Metadata = {
  alternates: { canonical: '/research/architecture' },
  title: 'Architecture narratives — Stellar Index research',
  description:
    'Long-form architecture narratives covering the Stellar Index ingest pipeline, aggregation methodology, and operational invariants.',
};

export default function ArchitectureIndexPage() {
  const docs = loadArchitectureDocs();
  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[
          { label: 'Home', href: '/' },
          { label: 'Research', href: '/research' },
          { label: 'Architecture narratives' },
        ]}
        title="Architecture narratives"
      />
      <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
        {docs.map((d) => (
          <Link
            key={d.slug}
            href={`/research/architecture/${d.slug}`}
            className="group border-line bg-surface hover:border-brand-300 flex flex-col gap-2 rounded-xl border p-4 transition hover:shadow-sm"
          >
            <div className="flex items-center gap-2">
              <BookOpen className="text-ink-faint group-hover:text-brand-500 h-3.5 w-3.5" />
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
