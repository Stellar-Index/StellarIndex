import type { Metadata } from 'next';
import './globals.css';

// Standalone on its own hosting so it stays up when the API and explorer do not.
export const metadata: Metadata = {
  title: 'Stellar Index status',
  description: 'Incident notices for the Stellar Index API and explorer.',
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en">
      <body className="min-h-screen bg-surface-canvas">{children}</body>
    </html>
  );
}
