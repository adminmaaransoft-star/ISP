import { test } from '@playwright/test';
import { say, beat, reframe, staffSignIn } from './demo-kit';

const WHO = ['ISP Owner', 'isp_owner'] as const;

test('PER-001 ISP Owner — full reach across the console', async ({ page }) => {
  await page.goto('/staff/login');
  await reframe(page, ...WHO, 'PER-001 — ISP Owner',
    'The only persona with every section of the operations console');

  await staffSignIn(page, 'owner');
  await reframe(page, ...WHO, 'Signed in',
    'Subscribers · Billing · Support · Revenue · LEA Lookup — all five available');
  await beat(page);

  await say(page, 'Subscribers', 'Every account on the platform, searchable by username or id');
  await beat(page);

  await page.goto('/staff/billing');
  await reframe(page, ...WHO, 'Billing',
    'Wallet balance and the double-entry ledger behind it');
  await beat(page);

  await page.goto('/staff/tickets');
  await reframe(page, ...WHO, 'Support',
    'The helpdesk queue, with SLA policies per category and priority');
  await beat(page);

  await page.goto('/staff/revenue');
  await reframe(page, ...WHO, 'Revenue — unique to this persona',
    'Unbilled subscribers, ledger variance, and total wallet balance, read live');
  await beat(page);
  await say(page, 'Ledger variance is the number to watch',
    'Non-zero means money moved without a matching counter-entry — investigate, do not dismiss',
    { hold: 4200 });

  await page.goto('/staff/lea');
  await reframe(page, ...WHO, 'LEA Lookup',
    'Resolve which subscriber held an IP address at a given time');
  await say(page, 'Access needs more than a role',
    'The lea_access claim is required on top of the role — it can never be a side effect of one',
    { hold: 4200 });
});
