import { defineConfig } from '@playwright/test'

// PW_PORT sets the dev-server port; PW_REUSE=1 attaches to a running server (default: fail fast, so two runs cannot share one).
const PORT = Number(process.env.PW_PORT ?? 5199)

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
    reuseExistingServer: process.env.PW_REUSE === '1',
    timeout: 60_000,
  },
})
