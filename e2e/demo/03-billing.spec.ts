import { test } from '@playwright/test';
import { say, beat, reframe, staffSignIn } from './demo-kit';

const WHO = ['Billing Admin', 'billing_admin'] as const;

test('PER-003 Billing Admin — the money, and only the money', async ({ page }) => {
  await page.goto('/staff/login');
  await reframe(page, ...WHO, 'PER-003 — Billing / Finance Admin',
    'Subscribers and Billing. No support queue, no revenue dashboard, no LEA.');

  await staffSignIn(page, 'billing');
  await reframe(page, ...WHO, 'Signed in',
    'Two sections in the navigation — narrower than the CSR beside them');
  await beat(page);

  await page.goto('/staff/billing');
  await reframe(page, ...WHO, 'Billing',
    'Wallet balance and the ledger behind it, looked up per subscriber');
  await beat(page);

  await say(page, 'The ledger is double-entry',
    'Every movement has a matching counter-entry — which is what makes the nightly reconciliation mean anything',
    { hold: 4200 });

  await say(page, 'Money is never a float',
    'decimal.Decimal in Go, NUMERIC in PostgreSQL — a rounding artefact in a GST total is not acceptable',
    { hold: 4200 });
});
