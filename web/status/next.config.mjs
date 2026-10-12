import path from 'node:path';

// The repo root, so the build can read the explorer's incident loader and internal/incidents/data.
const root = path.resolve(import.meta.dirname, '..', '..');

/** @type {import('next').NextConfig} */
const nextConfig = {
  reactStrictMode: true,
  reactCompiler: true,
  output: 'export',
  trailingSlash: true,
  images: { unoptimized: true },
  productionBrowserSourceMaps: true,
  turbopack: { root },
  outputFileTracingRoot: root,
};

export default nextConfig;
