import Link from 'next/link';

import { PROTOCOLS } from '@/app/protocols/registry';
import { Container, PageHeader } from '@/components/ui';
import { serializeJsonLd } from '@/lib/seo';

/**
 * A category landing page (SEO plan D5): "Stellar {category} protocols". Lists
 * the protocols in a category from the static registry as cards linking to
 * their /protocols/{name} detail. Used by /amm (the one category without a
 * bespoke page); /lending, /dexes, /oracles, /bridges and /yield keep their
 * existing bespoke pages.
 */
export function CategoryHub({
  category,
  title,
  description,
  footnote,
}: {
  category: string;
  title: string;
  description: string;
  footnote?: React.ReactNode;
}) {
  const items = PROTOCOLS.filter((p) => p.category === category);

  const itemListLD = {
    '@context': 'https://schema.org',
    '@type': 'ItemList',
    name: title,
    itemListElement: items.map((p, i) => ({
      '@type': 'ListItem',
      position: i + 1,
      name: p.label,
      url: `https://stellarindex.io/protocols/${p.name}`,
    })),
  };

  return (
    <Container className="space-y-8 py-8 sm:py-10">
      <script
        type="application/ld+json"
        dangerouslySetInnerHTML={{ __html: serializeJsonLd(itemListLD) }}
      />
      <PageHeader
        breadcrumbs={[
          { label: 'Home', href: '/' },
          { label: 'Protocols', href: '/protocols' },
        ]}
        title={title}
        description={description}
      />

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        {items.map((p) => (
          <Link
            key={p.name}
            href={`/protocols/${p.name}`}
            className="group border-line bg-surface hover:border-brand-300 hover:bg-surface-subtle rounded-xl border p-5 transition-colors"
          >
            <h2 className="text-ink group-hover:text-brand-600 text-lg font-semibold">
              {p.label}
            </h2>
            <p className="text-ink-body mt-1.5 text-sm leading-relaxed">
              {p.description}
            </p>
          </Link>
        ))}
      </div>

      {footnote && (
        <p className="border-line text-ink-muted border-t pt-5 text-sm">
          {footnote}
        </p>
      )}
    </Container>
  );
}
