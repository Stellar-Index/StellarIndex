import type { Metadata } from 'next';
import { notFound } from 'next/navigation';
import { ExternalLink } from 'lucide-react';

import { Container, PageHeader } from '@/components/ui';
import { Markdown } from '@/lib/markdown';
import { SITE_OG_IMAGES, SITE_TWITTER_IMAGES } from '@/lib/seo';
import { CURRENT_NETWORK } from '@/lib/networks';

// Shared shape of the curated doc types in lib/architecture.ts and
// lib/operations.ts (and any future /research/<category>/<slug> surface).
export type CuratedDoc = {
  slug: string;
  title: string;
  description: string;
  last_verified: string;
  body: string;
  source_path: string;
};

export type DocPageConfig = {
  /** URL segment under /research, e.g. "architecture" or "operations". */
  category: string;
  /** Capitalized noun used in the not-found title and page <title> suffix. */
  label: string;
  /** Text shown in the header pill above the doc title. */
  pillLabel: string;
  loadDoc: (slug: string) => CuratedDoc | null;
};

type SlugParams = { params: Promise<{ slug: string }> };

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

// Builds the generateMetadata + default page export pair shared by every
// curated-doc browser page under /research/<category>/[slug].
export function buildDocPage(config: DocPageConfig) {
  async function generateMetadata({ params }: SlugParams): Promise<Metadata> {
    const { slug } = await params;
    const doc = config.loadDoc(slug);
    if (!doc) return { title: `${config.label} doc not found` };
    const canonical = `${CURRENT_NETWORK.explorerUrl}/research/${config.category}/${slug}`;
    const title = `${doc.title} — Stellar Index ${config.label.toLowerCase()}`;
    return {
      title,
      description: doc.description,
      alternates: { canonical },
      openGraph: {
        title,
        description: doc.description,
        url: canonical,
        type: 'article',
        images: SITE_OG_IMAGES,
      },
      twitter: {
        card: 'summary_large_image',
        title,
        description: doc.description,
        images: SITE_TWITTER_IMAGES,
      },
    };
  }

  async function DocPage({ params }: SlugParams) {
    const { slug } = await params;
    const doc = config.loadDoc(slug);
    if (!doc) notFound();

    return (
      <Container className="space-y-6 py-8 [&>*]:max-w-4xl">
        <PageHeader
          breadcrumbs={[
            { label: 'Home', href: '/' },
            { label: 'Research', href: '/research' },
            { label: config.label, href: `/research/${config.category}` },
            { label: doc.title },
          ]}
          eyebrow={
            doc.last_verified
              ? `${config.pillLabel} · Last verified ${doc.last_verified}`
              : config.pillLabel
          }
          title={doc.title}
          description={doc.description}
          actions={
            <a
              href={`https://github.com/Stellar-Index/StellarIndex/blob/main/${doc.source_path}`}
              target="_blank"
              rel="noreferrer noopener"
              className="text-ink-muted hover:text-brand-600 inline-flex items-center gap-1 text-xs"
            >
              View source on GitHub
              <ExternalLink className="h-3 w-3" />
            </a>
          }
        />

        <article>
          <Markdown
            source={stripDuplicateH1(doc.body)}
            sourcePath={doc.source_path}
          />
        </article>
      </Container>
    );
  }

  return { generateMetadata, DocPage };
}
