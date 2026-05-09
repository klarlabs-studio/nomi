import { defineConfig, devices } from '@playwright/test';

/**
 * Playwright configuration for Nomi e2e tests.
 *
 * These tests run against the built web frontend (Vite preview server) while
 * requiring the Go backend (nomid) to be running on :8080.
 *
 * Start the backend before running tests:
 *   cd .. && go run ./cmd/nomid
 *
 * Then run tests:
 *   npx playwright test
 *
 * Two long-lived processes are started by Playwright itself:
 *   - vite preview on :4173 (the UI under test)
 *   - fake-llm on :21434 (the deterministic LLM stand-in)
 *
 * globalSetup registers the fake LLM as a provider profile against the
 * already-running nomid daemon, so every spec that previously skipped
 * "default LLM not configured" can now run real plan / step / approval
 * flows end-to-end.
 */
export default defineConfig({
  testDir: './e2e',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  workers: process.env.CI ? 1 : undefined,
  reporter: 'list',
  globalSetup: './e2e/global-setup.mts',
  use: {
    baseURL: 'http://localhost:4173',
    trace: 'on-first-retry',
    screenshot: 'only-on-failure',
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
  webServer: [
    {
      command: 'npx vite preview --port 4173',
      url: 'http://localhost:4173',
      reuseExistingServer: !process.env.CI,
      timeout: 120 * 1000,
    },
    {
      command: 'node ./e2e/fixtures/fake-llm-server.mjs',
      // The runner emits "fake-llm ready at ..." on stdout; Playwright
      // greps the URL we give it. We poll the port directly via TCP.
      url: 'http://127.0.0.1:21434',
      // The fake-llm only accepts POST; a GET to / returns 404 fast,
      // which Playwright treats as ready (server is up).
      ignoreHTTPSErrors: true,
      reuseExistingServer: !process.env.CI,
      timeout: 30 * 1000,
    },
  ],
});
