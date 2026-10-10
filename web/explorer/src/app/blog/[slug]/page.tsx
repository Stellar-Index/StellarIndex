import type { Metadata } from 'next';
import { notFound } from 'next/navigation';
import { GithubIcon } from '@/components/GithubIcon';

import { loadBlogPost, loadBlogPosts } from '@/lib/blog';
import { Markdown } from '@/lib/markdown';
import { SITE_OG_IMAGES, SITE_TWITTER_IMAGES } from '@/lib/seo';
import { CURRENT_NETWORK } from '@/lib/networks';
import { Container, PageHeader } from '@/components/ui';

type Params = Promise<{ slug: string }>;

export function generateStaticParams() {
  return loadBlogPosts().map((p) => ({ slug: p.slug }));
}

export async function generateMetadata({
  params,
}: {
  params: Params;
}): Promise<Metadata> {
  const { slug } = await params;
  const post = loadBlogPost(slug);
  if (!post) return { title: 'Post not found — Blog' };
  const canonical = `${CURRENT_NETWORK.explorerUrl}/blog/${slug}`;
  const title = `${post.title} — Blog`;
  return {
    title,
    description: post.summary,
    alternates: { canonical },
    openGraph: {
      title,
      description: post.summary,
      url: canonical,
      type: 'article',
      publishedTime: post.date,
      images: SITE_OG_IMAGES,
    },
    twitter: {
      card: 'summary_large_image',
      title,
      description: post.summary,
      images: SITE_TWITTER_IMAGES,
    },
  };
}

export default async function BlogPostPage({ params }: { params: Params }) {
  const { slug } = await params;
  const post = loadBlogPost(slug);
  if (!post) notFound();

  return (
    <Container className="space-y-6 py-12 [&>*]:max-w-3xl">
      <PageHeader
        breadcrumbs={[
          { label: 'Home', href: '/' },
          { label: 'Blog', href: '/blog' },
          { label: post.title },
        ]}
        meta={`${post.date} · ${post.author}`}
        title={post.title}
      />

      <article className="prose prose-slate max-w-none">
        <Markdown source={post.body} />
      </article>

      <footer className="border-line border-t pt-4 text-xs">
        <a
          href={`https://github.com/Stellar-Index/StellarIndex/blob/main/${post.source_path}`}
          target="_blank"
          rel="noreferrer noopener"
          className="text-ink-muted hover:text-brand-600 inline-flex items-center gap-1"
        >
          <GithubIcon className="h-3.5 w-3.5" />
          Source: {post.source_path}
        </a>
      </footer>
    </Container>
  );
}
