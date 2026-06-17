import type { MetadataRoute } from 'next';

// Generated at build time and emitted as a static /manifest.webmanifest
// (output: 'export' requires force-static, same as sitemap.ts / robots.ts).
// Gives the site PWA installability + a proper home-screen icon + theme
// colour on Android/Chrome.
export const dynamic = 'force-static';

export default function manifest(): MetadataRoute.Manifest {
  return {
    name: 'Stellar Index',
    short_name: 'Stellar Index',
    description:
      'Protocol explorer + pricing API for the Stellar network — complete, verified, per-protocol on-chain data.',
    start_url: '/',
    display: 'standalone',
    background_color: '#ffffff',
    theme_color: '#1f4ae0',
    icons: [
      { src: '/icon.svg', type: 'image/svg+xml', sizes: 'any' },
      { src: '/icon-192.png', type: 'image/png', sizes: '192x192' },
      { src: '/icon-512.png', type: 'image/png', sizes: '512x512' },
    ],
  };
}
