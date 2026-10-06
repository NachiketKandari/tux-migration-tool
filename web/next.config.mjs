/** @type {import('next').NextConfig} */
const nextConfig = {
  output: "standalone",
  // Local-only tool: no external images, fonts, or analytics.
  // - No `next/image` remotePatterns (all UI is local SVG/CSS).
  // - No Google Fonts / CDN scripts / Vercel Analytics.
  // - Anonymous Next.js telemetry is disabled via NEXT_TELEMETRY_DISABLED=1
  //   (package.json scripts, .env.example, Dockerfile) plus
  //   `npx next telemetry disable` machine-wide.
  // - Server binds 127.0.0.1 in dev/start scripts; Docker maps the port
  //   explicitly (`-p 127.0.0.1:3000:3000` keeps it loopback-only).
};

export default nextConfig;
