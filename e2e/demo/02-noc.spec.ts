import { test } from '@playwright/test';
import { say, beat, reframe, staffSignIn } from './demo-kit';

const WHO = ['NOC Engineer', 'noc_engineer'] as const;

test('PER-002 NOC Engineer — network operations, no money', async ({ page }) => {
  await page.goto('/staff/login');
  await reframe(page, ...WHO, 'PER-002 — NOC Engineer',
    'Runs the network. Deliberately has no view of billing or revenue.');

  await staffSignIn(page, 'noc');
  await reframe(page, ...WHO, 'Signed in — look at the navigation',
    'Only Subscribers and LEA Lookup. No Billing, no Support, no Revenue.');
  await beat(page);

  await say(page, 'Subscribers', 'Enough to see whether an account is up and what it is connected to');
  await beat(page);

  await page.goto('/staff/lea');
  await reframe(page, ...WHO, 'LEA Lookup',
    'Answers "which subscriber held this IP address at this time"');
  await beat(page);

  await say(page, 'The form says every lookup is recorded',
    'That warning is part of the control, not decoration — the audit record is tamper-evident',
    { hold: 4000 });

  await say(page, 'A role alone does not open this',
    'The lea_access claim is required on top of the NOC role. Granting NOC never grants LEA.',
    { hold: 4400 });

  await page.goto('/staff/revenue');
  await reframe(page, ...WHO, 'And what happens on a section this role cannot use',
    'Navigating straight to /staff/revenue — the URL exists, the authorisation does not');
  await beat(page, 4200);
});
