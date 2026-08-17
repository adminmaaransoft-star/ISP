import { test } from '@playwright/test';
import { say, beat, reframe, staffSignIn } from './demo-kit';

/**
 * The two personas are recorded together on purpose. Their difference is the
 * clearest demonstration in the product that role restriction is real rather
 * than cosmetic, and it only lands if you see the same record twice.
 */
test('PER-004 / PER-005 — the same subscriber, two roles', async ({ page }) => {
  const CSR = ['CSR', 'csr'] as const;
  const TECH = ['Ground Technician', 'technician'] as const;

  await page.goto('/staff/login');
  await reframe(page, ...CSR, 'PER-004 — Customer Service Representative',
    'The desk that answers the phone: Subscribers, Billing, Support');

  await staffSignIn(page, 'csr');
  await reframe(page, ...CSR, 'Signed in as CSR',
    'Three sections — everything needed to answer a call without switching systems');
  await beat(page);

  await page.getByRole('link', { name: 'Open' }).first().click();
  await page.waitForLoadState('networkidle');
  await reframe(page, ...CSR, 'Opening a subscriber record',
    'Account, connection, and health in one view');
  await beat(page);

  await say(page, 'Note what is on this screen',
    'Wallet balance and the ledger are both present — a CSR can answer a billing question directly',
    { hold: 4600 });

  // Same record, different person.
  await page.goto('/staff/login');
  await reframe(page, ...TECH, 'PER-005 — Ground Technician',
    'Now the same record, opened by the technician who will visit the site');

  await staffSignIn(page, 'tech');
  await reframe(page, ...TECH, 'Signed in as technician',
    'Two sections. Billing is gone from the navigation entirely.');
  await beat(page);

  await page.getByRole('link', { name: 'Open' }).first().click();
  await page.waitForLoadState('networkidle');
  await reframe(page, ...TECH, 'The same subscriber, seconds later',
    'Account and connection are here — the wallet and ledger panels are simply absent');
  await beat(page, 4600);

  await say(page, 'This is not the navigation being tidy',
    'The API refuses the data independently. Typing the URL directly returns 403, not the record.',
    { hold: 4800 });

  await say(page, 'A technician needs to know whether the line is up',
    'Not what the customer owes. The restriction follows the job.',
    { hold: 4000 });
});
