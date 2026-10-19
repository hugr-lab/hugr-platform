import { defineConfig, devices } from '@playwright/test'

// The console end to end (spec 003): test/console/e2e/run.sh starts Keycloak, a node and the console and passes their
// addresses and this run's passwords in the environment. Chromium only; no trace (it would record the passwords).
export default defineConfig({
  testDir: 'e2e',
  workers: 1,
  timeout: 90_000,
  reporter: [['list']],
  use: {
    ...devices['Desktop Chrome'],
    viewport: { width: 1440, height: 1000 },
    ignoreHTTPSErrors: true, // the run's own CA
    trace: 'off',
    screenshot: 'only-on-failure',
  },
})
