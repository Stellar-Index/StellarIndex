import type { Metadata } from 'next';
import Link from 'next/link';

import { loadBlogPosts } from '@/lib/blog';

export const metadata: Metadata = {
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
          <p className="font-mono text-xs uppercase tracking-widest text-brand-600">
            Blog
          </p>
          <Link
            href="/blog.atom"
            className="font-mono text-[11px] text-ink-muted hover:text-brand-600"
          >
            atom feed →
          </Link>
        </div>
        <h1 className="text-4xl font-semibold tracking-tight">
          Engineering notes
        </h1>
        <p className="text-base text-ink-body">
          Release notes, architecture decisions, and the why behind
          each surface. Sourced from{' '}
          <code className="font-mono text-sm">docs/blog/*.md</code> in
          the public repo — every post links back to its source on
          GitHub.
        </p>
      </header>

      {posts.length === 0 ? (
        <div className="rounded-xl border border-line bg-surface p-6">
          <p className="text-sm text-ink-body">
            No posts yet. The first post lands once v1 ships. For
            per-release notes meanwhile, see{' '}
            <Link href="/changelog" className="text-brand-600 hover:underline">
              /changelog
            </Link>{' '}
            (also available as an{' '}
            <Link href="/changelog.atom" className="text-brand-600 hover:underline">
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
              className="rounded-xl border border-line bg-surface p-5 transition-colors hover:border-brand-500"
            >
              <Link href={`/blog/${p.slug}`} className="group block space-y-2">
                <div className="flex items-baseline justify-between gap-2">
                  <h2 className="text-xl font-semibold tracking-tight group-hover:text-brand-600">
                    {p.title}
                  </h2>
                  <span className="font-mono text-xs text-ink-muted">{p.date}</span>
                </div>
                {p.summary && (
                  <p className="text-sm text-ink-body">{p.summary}</p>
                )}
                <p className="font-mono text-[11px] text-ink-faint">{p.author}</p>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
