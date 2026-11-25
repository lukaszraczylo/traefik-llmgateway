import { defineConfig } from '@playwright/test'

const PORT = 5199

export default defineConfig({
  testDir: './e2e',
  testMatch: '*.spec.ts',
  outputDir: '.screenshots/.pw-output',
  fullyParallel: true,
  workers: 4,
  timeout: 120_000,
  reporter: [['list']],
  use: { baseURL: `http://localhost:${PORT}`, browserName: 'chromium' },
  webServer: {
    command: `npm run dev:mock -- --port ${PORT} --strictPort`,
    url: `http://localhost:${PORT}/admin/`,
    reuseExistingServer: !process.env.CI,
    timeout: 60_000,
  },
})
