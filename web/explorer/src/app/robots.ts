import type { MetadataRoute } from 'next';

import { CURRENT_NETWORK } from '@/lib/networks';

export const dynamic = 'force-static';

/**
 * robots.txt — emits the manifest at build time. The site is
 * fully public, but a few paths have no SEO value and shouldn't
 * eat crawler budget:
 *   /dev/   — design-iteration scaffolding
 *   /embed/ — iframe widget targets, not standalone content
 *   /auth/  — magic-link callback, expires after one click
 *   /dashboard — authenticated dashboard, irrelevant to crawlers
 *   /signin, /signup — auth gateways, not content
 */
export default function robots(): MetadataRoute.Robots {
  return {
    rules: [
      {
        userAgent: '*',
        allow: '/',
        disallow: ['/dev/', '/embed/', '/auth/', '/dashboard', '/signin', '/signup'],
      },
    ],
    // Per-network origin. These were hardcoded to stellarindex.io, so the
    // test-net builds published a robots.txt naming MAINNET as their host
    // and pointing crawlers at MAINNET's sitemap — inviting the test nets'
    // duplicate-looking content to be consolidated onto, or confused with,
    // the production site. Same hardcode class as the /network page
    // reporting "Pubnet" on testnet.
    sitemap: `${CURRENT_NETWORK.explorerUrl}/sitemap.xml`,
    host: CURRENT_NETWORK.explorerUrl,
  };
}

// llms.txt lives at web/explorer/public/llms.txt — the
// llmstxt.org-spec discovery file for AI agents indexing the
// site. Not referenced from robots.txt because the spec is
// "look at the well-known path", not "follow Sitemap:".
