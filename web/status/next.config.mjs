/** @type {import('next').NextConfig} */
const nextConfig = {
  reactStrictMode: true,

  // React Compiler (babel-plugin-react-compiler 1.0, stable) — auto-memoizes
  // components at build time. Enabled now that the react-hooks/* rules are
  // enforced clean; the compiler bails out on anything it can't prove safe.
  reactCompiler: true,
  output: 'export',
  trailingSlash: true,
  images: { unoptimized: true },
  productionBrowserSourceMaps: true,
  env: {
    NEXT_PUBLIC_API_BASE_URL:
      process.env.NEXT_PUBLIC_API_BASE_URL ?? 'https://api.stellarindex.io',
  },
};

export default nextConfig;
