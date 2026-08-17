import { test } from '@playwright/test';
import { say, beat, reframe, portalSignIn } from './demo-kit';

const WHO = ['Subscriber', 'test_user'] as const;

test('PER-006 End Subscriber — the self-service portal', async ({ page }) => {
  await page.goto('/ui/login');
  await reframe(page, ...WHO, 'PER-006 — End Subscriber',
    'The only persona who is not staff. Sees their own account and nothing else.');

  await portalSignIn(page, 'test_user');
  await reframe(page, ...WHO, 'The dashboard',
    'Wallet balance, plan, status, and live usage against the plan allowance');
  await beat(page);

  await say(page, 'The usage bar is fed by RADIUS accounting',
    '2,235 GB of 3,300 GB — the same counters the FUP scanner reads to decide on throttling',
    { hold: 4400 });

  await page.goto('/ui/usage');
  await reframe(page, ...WHO, 'Usage history', 'Past sessions, oldest to newest');
  await beat(page);

  await page.goto('/ui/invoices');
  await reframe(page, ...WHO, 'Invoices',
    'GST broken out — CGST and SGST intrastate, IGST interstate, never both');
  await beat(page);

  await page.goto('/ui/renew');
  await reframe(page, ...WHO, 'Renew',
    'One tap, paid from the wallet balance shown on the dashboard');
  await beat(page);

  await page.goto('/ui/tickets');
  await reframe(page, ...WHO, 'Support',
    'Raise a ticket; it lands in the queue the CSR and technician personas work from');
  await beat(page);

  await page.goto('/ui/notifications');
  await reframe(page, ...WHO, 'Notifications',
    'Delivery history of every WhatsApp and SMS the platform sent them');
  await beat(page);
});
