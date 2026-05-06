import type { Metadata } from 'next';
import './globals.css';

export const metadata: Metadata = {
  title: 'Rates Engine — system status',
  description:
    'Real-time status of the Rates Engine API: per-service health, request latency, ingest freshness, active incidents.',
  robots: { index: true, follow: true },
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
