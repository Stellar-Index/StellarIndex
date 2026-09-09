import type { Metadata } from 'next';
import Link from 'next/link';

import { loadBlogPosts } from '@/lib/blog';

export const metadata: Metadata = {
  alternates: { canonical: '/blog' },
  title: 'Blog — engineering notes + product updates',
  description:
    'Stellar Index engineering blog — release notes, architecture decisions, post-mortems, product updates.',
};

export default function BlogIndexPage() {
  const posts = loadBlogPosts();

  return (
    <div className="mx-auto max-w-3xl space-y-8 px-6 py-12">
      <header className="space-y-2">
        <div className="flex items-center justify-between">
          <p className="text-brand-600 font-mono text-xs tracking-widest uppercase">
            Blog
          </p>
          <Link
            href="/blog.atom"
            className="text-ink-muted hover:text-brand-600 font-mono text-[11px]"
          >
            atom feed →
          </Link>
        </div>
        <h1 className="text-4xl font-semibold tracking-tight">
          Engineering notes
        </h1>
        <p className="text-ink-body text-base">
          Release notes, architecture decisions, and the why behind each
          surface. Sourced from{' '}
          <code className="font-mono text-sm">docs/blog/*.md</code> in the
          public repo — every post links back to its source on GitHub.
        </p>
      </header>

      {posts.length === 0 ? (
        <div className="border-line bg-surface rounded-xl border p-6">
          <p className="text-ink-body text-sm">
            No posts match this view. For per-release notes meanwhile, see{' '}
            <Link href="/changelog" className="text-brand-600 hover:underline">
              /changelog
            </Link>{' '}
            (also available as an{' '}
            <Link
              href="/changelog.atom"
              className="text-brand-600 hover:underline"
            >
              Atom feed
            </Link>
            ); for design rationale, see{' '}
            <Link href="/research" className="text-brand-600 hover:underline">
              /research
            </Link>
            .
          </p>
        </div>
      ) : (
        <ul className="space-y-4">
          {posts.map((p) => (
            <li
              key={p.slug}
              className="border-line bg-surface hover:border-brand-500 rounded-xl border p-5 transition-colors"
            >
              <Link href={`/blog/${p.slug}`} className="group block space-y-2">
                <div className="flex items-baseline justify-between gap-2">
                  <h2 className="group-hover:text-brand-600 text-xl font-semibold tracking-tight">
                    {p.title}
                  </h2>
                  <span className="text-ink-muted font-mono text-xs">
                    {p.date}
                  </span>
                </div>
                {p.summary && (
                  <p className="text-ink-body text-sm">{p.summary}</p>
                )}
                <p className="text-ink-faint font-mono text-[11px]">
                  {p.author}
                </p>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
