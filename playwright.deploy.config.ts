import { defineConfig, devices } from "@playwright/test";

// Minimal deployment-verification config. It deliberately depends on nothing
// local: no webServer, no globalSetup/globalTeardown, no dotenv/.env loading,
// no database, and no import of ./e2e/env. The ordinary playwright.config.ts
// assumes a running local stack (make up), so it is left untouched and this
// separate config is used for the deployed hosts.
//
// Targeted hosts come from the environment:
//   MCA_APP_URL   (fallback PLAYWRIGHT_BASE_URL)  default https://aod.apexxai.net
//   MCA_API_URL                                   default https://mcapi.apexxai.net
//   OD_APP_URL                                    default https://od.apexxai.net

const appURL =
  process.env.MCA_APP_URL ??
  process.env.PLAYWRIGHT_BASE_URL ??
  "https://aod.apexxai.net";

export default defineConfig({
  testDir: "./e2e",
  // Only the deployment smoke spec is in scope. The rest of e2e/ needs a local
  // backend/frontend and must never be swept in by this config.
  testMatch: /deploy-smoke\.spec\.ts/,
  fullyParallel: false,
  workers: 1,
  retries: 0,
  forbidOnly: !!process.env.CI,
  timeout: 60_000,
  expect: { timeout: 15_000 },
  reporter: [["list"]],
  use: {
    baseURL: appURL,
    headless: true,
    // Production TLS must validate. Never disable certificate verification for
    // these hosts; the suite also asserts the certificate explicitly.
    ignoreHTTPSErrors: false,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
  projects: [
    {
      // Dedicated tagged project: npx playwright test --project=smoke
      name: "smoke",
      grep: /@smoke/,
      use: { ...devices["Desktop Chrome"] },
    },
  ],
});
