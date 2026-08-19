import { defineConfig, devices } from '@playwright/test';

export default defineConfig({
  testDir: './specs',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: process.env.CI ? 2 : undefined,
  reporter: process.env.CI ? [['dot'], ['html', { outputFolder: 'playwright-report', open: 'never' }]] : 'list',
  outputDir: 'test-results',
  use: {
    baseURL: process.env.TIMINGDEX_PLAYWRIGHT_BASE_URL ?? 'https://127.0.0.1:4173',
    ignoreHTTPSErrors: true,
    // Several workflows establish an administrator session. Keep browser
    // artifacts credential-free; assertions and the HTML report retain the
    // regression signal without recording typed tokens or request bodies.
    trace: 'off',
    screenshot: 'off',
    video: 'off',
  },
  projects: [
    { name: 'desktop', use: { ...devices['Desktop Chrome'], viewport: { width: 1280, height: 900 } } },
    { name: 'mobile', use: { ...devices['Pixel 5'], viewport: { width: 375, height: 812 } } },
  ],
});
