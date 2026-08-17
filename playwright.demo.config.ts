import { defineConfig, devices } from '@playwright/test';

/**
 * Screen-recording config for demo videos — separate from playwright.config.ts
 * on purpose.
 *
 * Video capture and `slowMo` make a run roughly an order of magnitude slower
 * and write a .webm per test, which is exactly wrong for the suite that gates
 * commits and exactly right for a demo. Keeping them apart means the normal
 * suite stays fast and nobody has to remember a flag.
 *
 * Run against a stack already up:
 *   ./scripts/demo_up.sh
 *   npx playwright test --config playwright.demo.config.ts
 *
 * Pacing is deliberately slow: DEMO_SLOWMO milliseconds between every browser
 * action, plus explicit pauses at each captioned step. A recording that moves
 * at machine speed shows that the software works but not what it does.
 */
const SLOWMO = Number(process.env.DEMO_SLOWMO ?? 450);

export default defineConfig({
  testDir: './e2e/demo',
  outputDir: './e2e/demo-recordings',
  fullyParallel: false,
  retries: 0,
  workers: 1,
  // A captioned walkthrough with pauses runs far longer than an assertion test.
  timeout: 180_000,
  reporter: [['list']],

  use: {
    baseURL: process.env.PORTAL_URL ?? 'https://localhost',
    ignoreHTTPSErrors: true,

    // 720p: legible when played back at full size, and small enough that a
    // six-persona set stays shareable.
    viewport: { width: 1280, height: 720 },
    video: { mode: 'on', size: { width: 1280, height: 720 } },

    launchOptions: { slowMo: SLOWMO },
  },

  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
  ],
});
