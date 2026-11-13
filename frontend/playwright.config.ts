import { defineConfig, devices } from '@playwright/test'

const port = process.env.PORT || '4173'

export default defineConfig({
  testDir: './e2e',
  fullyParallel: false,
  forbidOnly: Boolean(process.env.CI),
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  reporter: process.env.CI ? 'github' : 'list',
  use: {
    baseURL: `http://127.0.0.1:${port}`,
    trace: 'retain-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  webServer: {
    command: 'npm run build && node e2e/server.mjs',
    url: `http://127.0.0.1:${port}/api/v1/health`,
    reuseExistingServer: !process.env.CI,
    timeout: 180000,
  },
})
