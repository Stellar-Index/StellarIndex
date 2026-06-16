import type { Metadata } from 'next';
import { Inter, JetBrains_Mono } from 'next/font/google';
import './globals.css';
import { AuthProvider } from '@/lib/auth';
import { Toaster } from 'sonner';

// Inter (UI) + JetBrains Mono (numeric / addresses / code). next/font
// self-hosts both at build time — no runtime Google dependency, no layout
// shift — and exposes them as the --font-sans / --font-mono CSS variables
// the Tailwind theme reads. Same setup as web/explorer/src/app/layout.tsx so
// the dashboard renders in the shared design system.
const inter = Inter({
  subsets: ['latin'],
  display: 'swap',
  variable: '--font-sans',
});
const jetbrainsMono = JetBrains_Mono({
  subsets: ['latin'],
  display: 'swap',
  variable: '--font-mono',
});

const SITE_NAME = 'Stellar Index — Dashboard';
const SITE_DESCRIPTION =
  'Manage your Stellar Index API keys, monitor usage, and configure billing.';

export const metadata: Metadata = {
  title: {
    default: SITE_NAME,
    template: `%s · ${SITE_NAME}`,
  },
  description: SITE_DESCRIPTION,
  applicationName: SITE_NAME,
  // Dashboard is private — keep it out of the index.
  robots: {
    index: false,
    follow: false,
  },
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en" className={`${inter.variable} ${jetbrainsMono.variable}`}>
      <body className="min-h-screen bg-surface-canvas">
        <AuthProvider>{children}</AuthProvider>
        <Toaster position="top-right" />
      </body>
    </html>
  );
}
