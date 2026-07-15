import type { Metadata } from 'next';
import Link from 'next/link';
import { notFound } from 'next/navigation';
import { ArrowLeft, ExternalLink } from 'lucide-react';

import {
  loadOperationsDoc,
  loadOperationsDocs,
} from '@/lib/operations';
import { Markdown } from '@/lib/markdown';
import { SITE_OG_IMAGES, SITE_TWITTER_IMAGES } from '@/lib/seo';

// Each curated operations doc rendered as a static page. Same
// shape as the ADR / architecture / discovery browsers.

export const dynamic = 'error';
export const dynamicParams = false;

export function generateStaticParams() {
  return loadOperationsDocs().map((d) => ({ slug: d.slug }));
}

export async function generateMetadata({
  params,
}: {
  params: Promise<{ slug: string }>;
}): Promise<Metadata> {
  const { slug } = await params;
  const doc = loadOperationsDoc(slug);
  if (!doc) return { title: 'Operations doc not found' };
  const canonical = `https://stellarindex.io/research/operations/${slug}`;
  const title = `${doc.title} — Stellar Index operations`;
  return {
    title,
    description: doc.description,
    alternates: { canonical },
    openGraph: { title, description: doc.description, url: canonical, type: 'article', images: SITE_OG_IMAGES },
    twitter: { card: 'summary_large_image', title, description: doc.description, images: SITE_TWITTER_IMAGES },
  };
}

export default async function OperationsDocPage({
  params,
}: {
  params: Promise<{ slug: string }>;
}) {
  const { slug } = await params;
  const doc = loadOperationsDoc(slug);
  if (!doc) notFound();

  return (
    <div className="mx-auto max-w-4xl space-y-6 px-6 py-8">
      <Link
        href="/research"
        className="inline-flex items-center gap-1.5 text-sm text-ink-body hover:text-brand-600"
      >
        <ArrowLeft className="h-3.5 w-3.5" />
        Back to research
      </Link>

      <header className="space-y-3 border-b border-line pb-6">
        <div className="flex items-center gap-3 text-xs">
          <span className="font-medium uppercase tracking-wider text-ink-muted">
            Operations runbook
          </span>
          {doc.last_verified && (
            <span className="text-ink-muted">
              Last verified {doc.last_verified}
            </span>
          )}
        </div>
        <h1 className="text-2xl font-semibold tracking-tight">{doc.title}</h1>
        <p className="text-sm text-ink-body">
          {doc.description}
        </p>
        <a
          href={`https://github.com/Stellar-Index/StellarIndex/blob/main/${doc.source_path}`}
          target="_blank"
          rel="noreferrer noopener"
          className="inline-flex items-center gap-1 text-xs text-ink-muted hover:text-brand-600"
        >
          View source on GitHub
          <ExternalLink className="h-3 w-3" />
        </a>
      </header>

      <article>
        <Markdown source={stripDuplicateH1(doc.body)} sourcePath={doc.source_path} />
      </article>
    </div>
  );
}

function stripDuplicateH1(body: string): string {
  const lines = body.split('\n');
  let i = 0;
  while (i < lines.length && lines[i]!.trim() === '') i++;
  if (i < lines.length && lines[i]!.startsWith('# ')) {
    i++;
    while (i < lines.length && lines[i]!.trim() === '') i++;
    return lines.slice(i).join('\n');
  }
  return body;
}
