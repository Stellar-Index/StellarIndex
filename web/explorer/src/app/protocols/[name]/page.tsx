import type { Metadata } from 'next';
import { permanentRedirect } from 'next/navigation';
import { Suspense } from 'react';

import { ogImageFor } from '@/lib/seo';
import { ProtocolView } from './ProtocolView';
import { PROTOCOLS, protocolMeta } from '../registry';
import { Container } from '@/components/ui';
import { CURRENT_NETWORK } from '@/lib/networks';

// Static-export dynamic route. The protocol name set is the bounded
// registry (../registry.ts, mirrored from internal/api/v1/
// protocols_registry.go), so we pre-render exactly those slugs and 404
// anything else — there is no unbounded long-tail here (unlike the
// contract/tx/ledger explorer pages, whose entities are unbounded and
// therefore use query-param pages instead).
export function generateStaticParams() {
  return PROTOCOLS.map((p) => ({ name: p.name }));
}

// Bounded registry → reject unknown slugs at build, don't client-render
// a fetch that would 404.
export const dynamicParams = false;

type Params = Promise<{ name: string }>;

export async function generateMetadata({
  params,
}: {
  params: Params;
}): Promise<Metadata> {
  const { name } = await params;
  const meta = protocolMeta(name);
  // /protocols/sdex redirects to /sdex (the one canonical SDEX surface,
  // nav revision follow-up 2026-08-24) — point its metadata there too.
  const canonical =
    name === 'sdex'
      ? `${CURRENT_NETWORK.explorerUrl}/sdex`
      : `${CURRENT_NETWORK.explorerUrl}/protocols/${encodeURIComponent(name)}`;
  const title = meta
    ? `${meta.label} — protocol analytics`
    : `${name} — protocol analytics`;
  const description = meta
    ? `${meta.description} Every contract, event type and on-chain activity for ${meta.label}, verified against the certified ledger lake.`
    : `On-chain analytics, contract roster and event-type breakdown for ${name}.`;
  return {
    title,
    description,
    alternates: { canonical },
    openGraph: {
      title,
      description,
      url: canonical,
      type: 'website',
      images: [ogImageFor('protocols', name)],
    },
    twitter: {
      card: 'summary_large_image',
      title,
      description,
      images: [ogImageFor('protocols', name)],
    },
  };
}

export default async function ProtocolDetailPage({
  params,
}: {
  params: Params;
}) {
  const { name } = await params;
  if (name === 'sdex') {
    // The one canonical SDEX surface is /sdex (it renders this same
    // ProtocolView plus the SDEX-only order-book/volume sections).
    permanentRedirect('/sdex');
  }
  const meta = protocolMeta(name);

  // FEC A1-6: the BreadcrumbList JSON-LD now derives from the visible
  // trail ProtocolView renders (ui Breadcrumbs emits both from one
  // Crumb[]) — this page used to hand-roll LD with no visible crumbs.
  return (
    <>
      <Suspense
        fallback={
          <Container className="py-16 text-sm text-ink-muted">
            Loading {meta?.label ?? name}…
          </Container>
        }
      >
        <ProtocolView name={name} label={meta?.label ?? name} />
      </Suspense>
    </>
  );
}
