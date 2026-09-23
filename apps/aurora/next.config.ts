import type { NextConfig } from "next";
import { config } from "dotenv";
import { resolve } from "path";
import {
  resolveDevRemoteApiUrl,
  resolveRemoteApiUrl,
} from "./config/runtime-urls";

// Load root .env so local next.config.ts rewrites see REMOTE_API_URL, matching
// apps/web. Production requests use proxy.ts runtime rewrites, which read
// process.env when the Next.js server runs instead of baking the URL in at
// build time.
config({ path: resolve(__dirname, "../../.env") });

// `next dev` falls back to the conventional localhost backend; builds use the
// strict resolver so a prebuilt image keeps unset upstreams unproxied.
const isDev = process.env.NODE_ENV === "development";
const remoteApiUrl = isDev
  ? resolveDevRemoteApiUrl(process.env)
  : resolveRemoteApiUrl(process.env);

// Parse hostnames from CORS_ALLOWED_ORIGINS so that Next.js dev server allows
// cross-origin HMR / bundler requests (e.g. from Tailscale IPs).
const allowedDevOrigins = process.env.CORS_ALLOWED_ORIGINS
  ? process.env.CORS_ALLOWED_ORIGINS.split(",")
      .map((origin) => {
        try {
          return new URL(origin.trim()).host;
        } catch {
          return origin.trim();
        }
      })
      .filter(Boolean)
  : undefined;

const nextConfig: NextConfig = {
  ...(process.env.STANDALONE === "true"
    ? { output: "standalone" as const }
    : {}),
  transpilePackages: ["@multica/core", "@multica/ui", "@multica/views"],
  ...(allowedDevOrigins && allowedDevOrigins.length > 0
    ? { allowedDevOrigins }
    : {}),
  async rewrites() {
    return {
      // Nothing to run ahead of the filesystem for: Aurora has no second app
      // mounted under a path of its own, which is what apps/web's `beforeFiles`
      // entry exists for (the docs site sharing /docs).
      beforeFiles: [],
      // Everything the backend serves under this app's origin. There is no
      // docs upstream here — Aurora has no documentation site.
      afterFiles: remoteApiUrl
        ? [
            {
              source: "/v1/:path*",
              destination: `${remoteApiUrl}/v1/:path*`,
            },
            {
              source: "/api/:path*",
              destination: `${remoteApiUrl}/api/:path*`,
            },
            {
              source: "/ws",
              destination: `${remoteApiUrl}/ws`,
            },
            {
              source: "/health",
              destination: `${remoteApiUrl}/health`,
            },
            {
              source: "/auth/:path*",
              destination: `${remoteApiUrl}/auth/:path*`,
            },
            {
              source: "/uploads/:path*",
              destination: `${remoteApiUrl}/uploads/:path*`,
            },
          ]
        : [],
      fallback: [],
    };
  },
};

export default nextConfig;
