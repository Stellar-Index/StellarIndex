import type { Metadata } from 'next';
import Link from 'next/link';
import { ShieldCheck } from 'lucide-react';

import { loadDiscoveryDocs } from '@/lib/discovery';

export const metadata: Metadata = {
  title: 'Discovery audits — Stellar Index research',
  description:
    'Per-DEX and per-oracle integration audits documenting how each on-chain venue\'s event schema was verified against the upstream Rust source.',
};

export default function DiscoveryIndexPage() {
  const docs = loadDiscoveryDocs();
  return (
    <div className="mx-auto max-w-7xl space-y-6 px-6 py-8">
      <header className="space-y-2">
        <h1 className="text-3xl font-semibold tracking-tight">
          Discovery audits
        </h1>
        <p className="max-w-3xl text-base text-ink-body">
          Per-DEX / per-oracle integration audits.{' '}
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
            href={`/research/discovery/${d.slug}`}
            className="group flex flex-col gap-2 rounded-xl border border-line bg-surface p-4 transition hover:border-brand-300 hover:shadow-sm"
          >
            <div className="flex items-center gap-2">
              <ShieldCheck className="h-3.5 w-3.5 text-ink-faint group-hover:text-brand-500" />
              <span className="text-sm font-semibold tracking-tight">
                {d.title}
              </span>
              <span className="ml-auto rounded bg-surface-subtle px-1.5 py-0.5 text-[10px] uppercase tracking-wider text-ink-body">
                {d.category}
              </span>
            </div>
            <p className="text-xs text-ink-body">
              {d.description}
            </p>
          </Link>
        ))}
      </div>
    </div>
  );
}
