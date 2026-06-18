import type { MetadataRoute } from 'next';

import { loadIncidents } from '@/lib/incidents';

// Required for `output: 'export'` — sitemap is generated at build
// time and emitted as a static file.
export const dynamic = 'force-static';

const SITE_URL = 'https://status.stellarindex.io';

// next.config sets `trailingSlash: true`, so the static export serves
// every page at its trailing-slash URL and 308-redirects the bare form.
// Sitemap URLs must therefore carry the trailing slash too — otherwise
// every entry is a redirect, which wastes crawl budget and trips SEO
// audits ("sitemap contains redirects"). (Canonical <link> tags are
// normalised by Next automatically; raw sitemap URLs are not.)
export default function sitemap(): MetadataRoute.Sitemap {
  const now = new Date().toISOString();
  const home: MetadataRoute.Sitemap = [
    {
      url: `${SITE_URL}/`,
      lastModified: now,
      changeFrequency: 'always',
      priority: 1,
    },
  ];
  const incidents: MetadataRoute.Sitemap = loadIncidents().map((i) => ({
    url: `${SITE_URL}/incident/${i.slug}/`,
    lastModified: i.resolved_at ?? i.started_at ?? now,
    changeFrequency: 'never',
    priority: 0.4,
  }));
  return [...home, ...incidents];
}
