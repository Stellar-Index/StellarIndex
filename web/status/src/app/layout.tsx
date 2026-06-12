import type { Metadata } from 'next';
import './globals.css';

const SITE_URL = 'https://status.stellarindex.io';
const SITE_DESCRIPTION =
  'Real-time status of the Stellar Index API: per-service health, request latency, ingest freshness, active incidents.';

export const metadata: Metadata = {
  metadataBase: new URL(SITE_URL),
  title: 'Stellar Index — system status',
  description: SITE_DESCRIPTION,
  robots: { index: true, follow: true },
  openGraph: {
    type: 'website',
    siteName: 'Stellar Index — status',
    title: 'Stellar Index — system status',
    description: SITE_DESCRIPTION,
    url: SITE_URL,
    locale: 'en_US',
    images: [
      {
        url: '/og.svg',
        width: 1200,
        height: 630,
        alt: 'Stellar Index — system status',
        type: 'image/svg+xml',
      },
    ],
  },
  twitter: {
    card: 'summary_large_image',
    title: 'Stellar Index — system status',
    description: SITE_DESCRIPTION,
    images: ['/og.svg'],
  },
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en">
      <body className="min-h-screen bg-surface-subtle">{children}</body>
    </html>
  );
}
