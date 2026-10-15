import { defineConfig } from '@playwright/test';

if (!process.env.UI_BASE_URL || !process.env.UI_WORKSPACE) {
  throw new Error('Run through TestConsoleBrowser so the isolated Go fixture is available.');
}

const browsers = ['chromium', 'firefox', 'webkit'];
const selected = process.env.UI_BROWSER;
if (selected && !browsers.includes(selected)) throw new Error('UI_BROWSER must be chromium, firefox or webkit.');
if (process.env.UI_BROWSER_EXECUTABLE && selected !== 'chromium') {
  throw new Error('UI_BROWSER_EXECUTABLE requires UI_BROWSER=chromium.');
}

export default defineConfig({
  testDir: '.',
  testMatch: '*.spec.mjs',
  workers: 1,
  retries: 0,
  timeout: 30000,
  expect: { timeout: 5000 },
  outputDir: process.env.UI_TEST_OUTPUT_DIR || 'test-results',
  reporter: [['list']],
  projects: (selected ? [selected] : browsers).map(browserName => ({
    name: browserName,
    use: {
      browserName,
      launchOptions: browserName === 'chromium' && process.env.UI_BROWSER_EXECUTABLE
        ? { executablePath: process.env.UI_BROWSER_EXECUTABLE } : {},
    },
  })),
  use: {
    baseURL: process.env.UI_BASE_URL,
    viewport: { width: 1440, height: 1000 },
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
});
