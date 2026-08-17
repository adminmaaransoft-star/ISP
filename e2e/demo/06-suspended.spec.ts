import { test } from '@playwright/test';
import { say, beat, reframe, portalSignIn } from './demo-kit';

/**
 * One test per spec file throughout this directory, on purpose: Playwright
 * derives the recording's folder name from the spec file plus the test title,
 * and truncates it with a hash when it gets long. Keeping one test per file
 * means the stable file prefix alone identifies the video, so the collection
 * step in scripts/record_demo.sh does not depend on a title surviving intact.
 */
const SUSP = ['Subscriber', 'suspended_user'] as const;

test('what a suspended customer sees', async ({ page }) => {
  await page.goto('/ui/login');
  await reframe(page, ...SUSP, 'The same portal, a cut-off account',
    'This is the screen a CSR gets asked about when collections has suspended someone');

  await portalSignIn(page, 'suspended_user');
  await reframe(page, ...SUSP, 'Hard suspended',
    'Status in red, ₹0.00 balance, and no active session at all');
  await beat(page, 4600);

  await say(page, 'Suspension is a collections event, not churn',
    'The growth report counts the two separately — conflating them makes every dunning run look like an exodus',
    { hold: 4600 });
});
