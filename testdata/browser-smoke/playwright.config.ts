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
    baseURL: process.env.NEXUSGATE_PLAYWRIGHT_BASE_URL ?? 'https://127.0.0.1:4173',
    ignoreHTTPSErrors: true,
    // Use the system Google Chrome (v149, matching Playwright 1.61.1) instead
    // of the bundled Chromium download, which this offline host cannot fetch.
    channel: 'chrome',
    // The default page fixture negotiates zh-CN (the Hub's default locale), so
    // the established Chinese-language assertions in the core surface tests
    // stay valid; the localization tests opt into other locales explicitly via
    // per-context cookies or Accept-Language.
    locale: 'zh-CN',
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
