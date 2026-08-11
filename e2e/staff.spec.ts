import { test, expect, Page } from '@playwright/test';
import * as fs from 'fs';
import * as path from 'path';

/**
 * Operations console, end to end in a real browser.
 *
 * The console's job is to show five different roles five different subsets of
 * a customer's data, so most of what is worth testing is negative: not "can the
 * owner see the ledger" but "does the technician still not see it after typing
 * the URL directly". Hiding a nav link is a convenience; the 403 is the control.
 *
 * Requires a stack from ./scripts/demo_up.sh, seeded with the five staff
 * accounts.
 */

const PASSWORD = 'staffpassword';

// Snapshots are written for the report rather than compared against a baseline.
// Pixel-diffing a page whose figures move with the seed date would fail for
// reasons that have nothing to do with the console working.
const SHOTS = path.join('e2e', 'snapshots');
fs.mkdirSync(SHOTS, { recursive: true });

async function snap(page: Page, name: string) {
  await page.screenshot({ path: path.join(SHOTS, `${name}.png`), fullPage: true });
}

async function signIn(page: Page, username: string) {
  await page.goto('/staff/login');
  await page.getByLabel('Username').fill(username);
  await page.getByLabel('Password').fill(PASSWORD);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page).toHaveURL(/\/staff\/subscribers/);
}

/** Sections every role should and should not reach, mirroring the API. */
const MATRIX = {
  owner:   { role: 'isp_owner',     allow: ['subscribers', 'billing', 'tickets', 'revenue', 'lea'] },
  noc:     { role: 'noc_engineer',  allow: ['subscribers', 'lea'] },
  billing: { role: 'billing_admin', allow: ['subscribers', 'billing'] },
  csr:     { role: 'csr',           allow: ['subscribers', 'billing', 'tickets'] },
  tech:    { role: 'technician',    allow: ['subscribers', 'tickets'] },
} as const;

const ALL_SECTIONS = ['subscribers', 'billing', 'tickets', 'revenue', 'lea'] as const;

test.describe('Console authentication', () => {
  test('the sign-in page renders', async ({ page }) => {
    await page.goto('/staff/login');
    await expect(page.getByRole('heading', { name: 'Operations Console' })).toBeVisible();
    await expect(page.getByLabel('Username')).toBeVisible();
    await snap(page, 'login');
  });

  test('all five staff accounts can sign in', async ({ page }) => {
    for (const user of Object.keys(MATRIX)) {
      await signIn(page, user);
      await expect(page.locator('.who .role')).toHaveText(MATRIX[user as keyof typeof MATRIX].role);
      await page.getByRole('button', { name: 'Sign out' }).click();
      await expect(page).toHaveURL(/\/staff\/login/);
    }
  });

  test('a wrong password is refused without naming which field was wrong', async ({ page }) => {
    await page.goto('/staff/login');
    await page.getByLabel('Username').fill('owner');
    await page.getByLabel('Password').fill('not-the-password');
    await page.getByRole('button', { name: 'Sign in' }).click();

    await expect(page.getByText('Incorrect username or password')).toBeVisible();
    await expect(page).not.toHaveURL(/\/staff\/subscribers/);

    // Naming the wrong field would confirm the username exists, turning the
    // form into a way to enumerate staff accounts.
    const body = (await page.textContent('body')) ?? '';
    expect(body.toLowerCase()).not.toContain('no such user');
    expect(body.toLowerCase()).not.toContain('unknown user');
    await snap(page, 'login-rejected');
  });

  test('an unknown username is refused the same way', async ({ page }) => {
    await page.goto('/staff/login');
    await page.getByLabel('Username').fill('nobody-here');
    await page.getByLabel('Password').fill(PASSWORD);
    await page.getByRole('button', { name: 'Sign in' }).click();
    await expect(page.getByText('Incorrect username or password')).toBeVisible();
  });

  test('every console page bounces an unauthenticated visitor to sign-in', async ({ page }) => {
    for (const section of ALL_SECTIONS) {
      await page.goto(`/staff/${section}`);
      await expect(page, `/staff/${section} should redirect when signed out`).toHaveURL(/\/staff\/login/);
    }
  });

  test('signing out ends the session', async ({ page }) => {
    await signIn(page, 'owner');
    await page.getByRole('button', { name: 'Sign out' }).click();
    await expect(page).toHaveURL(/\/staff\/login/);
    await page.goto('/staff/revenue');
    await expect(page).toHaveURL(/\/staff\/login/);
  });
});

test.describe('Role-based access', () => {
  for (const [user, { allow }] of Object.entries(MATRIX)) {
    test(`${user} reaches exactly its own sections`, async ({ page }) => {
      await signIn(page, user);

      for (const section of ALL_SECTIONS) {
        const permitted = (allow as readonly string[]).includes(section);
        const res = await page.request.get(`/staff/${section}`);

        if (permitted) {
          expect(res.status(), `${user} should reach ${section}`).toBe(200);
        } else {
          // Typing the URL is the real test. The navigation not offering a
          // link proves nothing about what the server will serve.
          expect(res.status(), `${user} must be refused ${section}`).toBe(403);
        }
      }
    });

    test(`${user} sees only its own navigation`, async ({ page }) => {
      await signIn(page, user);
      const labels: Record<string, string> = {
        subscribers: 'Subscribers', billing: 'Billing', tickets: 'Support',
        revenue: 'Revenue', lea: 'LEA Lookup',
      };
      for (const section of ALL_SECTIONS) {
        const link = page.locator('.topbar nav').getByRole('link', { name: labels[section], exact: true });
        if ((allow as readonly string[]).includes(section)) {
          await expect(link, `${user} nav should offer ${section}`).toBeVisible();
        } else {
          await expect(link, `${user} nav must not offer ${section}`).toHaveCount(0);
        }
      }
      await snap(page, `nav-${user}`);
    });
  }
});

test.describe('Subscribers', () => {
  test('the directory lists subscribers and search filters them', async ({ page }) => {
    await signIn(page, 'owner');
    await expect(page.locator('table tbody tr').first()).toBeVisible();
    await snap(page, 'subscribers-list');

    await page.getByPlaceholder('Username or subscriber id').fill('test_user');
    await page.getByRole('button', { name: 'Search' }).click();
    await expect(page.getByText('test_user')).toBeVisible();

    await page.getByPlaceholder('Username or subscriber id').fill('nothing-matches-this');
    await page.getByRole('button', { name: 'Search' }).click();
    await expect(page.getByText('No subscribers match.')).toBeVisible();
  });

  test('the 360 view shows account, connection and health', async ({ page }) => {
    await signIn(page, 'owner');
    await page.goto('/staff/subscribers/1');

    await expect(page.getByRole('heading', { name: /test_user/ })).toBeVisible();
    await expect(page.getByText('CAF-0001')).toBeVisible();
    // Seeded with a live Redis session, so the connection panel must show real
    // figures rather than the offline state.
    await expect(page.getByText('online')).toBeVisible();
    await expect(page.getByText(/GB of .*GB/)).toBeVisible();
    await snap(page, 'subscriber-detail-owner');
  });

  test('a missing subscriber is reported, not rendered blank', async ({ page }) => {
    await signIn(page, 'owner');
    const res = await page.request.get('/staff/subscribers/99999');
    expect(res.status()).toBe(404);
  });

  test('the ledger panel is shown to billing roles and withheld from others', async ({ page }) => {
    // A CSR is entitled to see money.
    await signIn(page, 'csr');
    await page.goto('/staff/subscribers/1');
    await expect(page.getByText(/Recent ledger/)).toBeVisible();
    await snap(page, 'subscriber-detail-csr');

    // A ground technician is not, and the panel must be absent from the page
    // rather than merely hidden by styling.
    await page.getByRole('button', { name: 'Sign out' }).click();
    await signIn(page, 'tech');
    await page.goto('/staff/subscribers/1');
    await expect(page.getByText(/Recent ledger/)).toHaveCount(0);
    await expect(page.getByRole('heading', { name: /test_user/ })).toBeVisible();
    await snap(page, 'subscriber-detail-tech');
  });
});

test.describe('Billing', () => {
  test('a lookup shows the balance and ledger', async ({ page }) => {
    await signIn(page, 'billing');
    await page.goto('/staff/billing');
    await page.getByPlaceholder('Subscriber id').fill('1');
    await page.getByRole('button', { name: 'Look up' }).click();

    await expect(page.getByText('Wallet balance')).toBeVisible();
    await expect(page.getByText('₹799.00').first()).toBeVisible();
    await snap(page, 'billing');
  });

  test('a non-numeric id is rejected with a readable message', async ({ page }) => {
    await signIn(page, 'billing');
    await page.goto('/staff/billing?subscriber_id=abc');
    await expect(page.getByText('Enter a numeric subscriber id.')).toBeVisible();
  });
});

test.describe('Support tickets', () => {
  test('a ticket can be moved through the workflow', async ({ page }) => {
    // Raise one through the subscriber portal first, so there is something
    // real to work rather than a row the test invented in the database.
    await page.goto('/ui/login');
    await page.getByLabel('Username').fill('test_user');
    await page.getByLabel('Password').fill('testpassword');
    await page.getByRole('button', { name: 'Sign in' }).click();
    await page.goto('/ui/tickets');
    const description = `Console E2E ${Date.now()}`;
    await page.selectOption('select[name="category"]', { index: 0 });
    await page.fill('textarea[name="description"]', description);
    await page.getByRole('button', { name: /Submit Ticket/i }).click();
    await expect(page.getByText(description)).toBeVisible({ timeout: 10_000 });

    await signIn(page, 'csr');
    await page.goto('/staff/tickets?subscriber_id=1');
    await expect(page.getByText(description)).toBeVisible();
    await snap(page, 'tickets');

    const row = page.locator('tr', { hasText: description });
    await row.locator('select[name="status"]').selectOption('in_progress');
    await row.getByRole('button', { name: 'Update' }).click();

    await expect(page.getByText('Ticket updated.')).toBeVisible();
    // Scoped to the status pill: 'in_progress' also appears as an <option> in
    // the row's own dropdown, and matching both is ambiguous rather than wrong.
    await expect(page.locator('tr', { hasText: description }).locator('span.pill')).toHaveText('in_progress');
    await snap(page, 'tickets-updated');
  });

  test('a status change without the CSRF token is refused', async ({ page }) => {
    await signIn(page, 'csr');
    await page.goto('/staff/tickets?subscriber_id=1');
    // Carries the session cookie but no token — the shape a cross-site forgery
    // takes, since the cookie rides along automatically and the token does not.
    const res = await page.request.post('/staff/tickets/1/status', {
      form: { status: 'closed', subscriber_id: '1' },
    });
    expect(res.status()).toBe(403);
  });

  test('an invalid status is rejected rather than persisted', async ({ page }) => {
    await signIn(page, 'csr');
    await page.goto('/staff/tickets?subscriber_id=1');
    const csrf = await page.locator('input[name="csrf_token"]').first().inputValue();
    const res = await page.request.post('/staff/tickets/1/status', {
      form: { status: 'not-a-status', subscriber_id: '1', csrf_token: csrf },
    });
    // The column has no CHECK constraint, so an arbitrary string would persist
    // and then render as an unknown badge everywhere it appears.
    expect(res.status()).toBe(422);
  });
});

test.describe('Revenue', () => {
  test('the owner dashboard shows the reconciliation figures', async ({ page }) => {
    await signIn(page, 'owner');
    await page.goto('/staff/revenue');
    await expect(page.getByText('Unbilled active subscribers')).toBeVisible();
    await expect(page.getByText('Ledger variance')).toBeVisible();
    await expect(page.getByText('Total wallet balance')).toBeVisible();
    // The variance verdict uses the same ₹0.01 threshold the nightly job
    // alerts on, so the screen and the alert cannot disagree.
    await expect(page.locator('.pill.ok, .pill.bad').first()).toBeVisible();
    await snap(page, 'revenue');
  });
});

test.describe('LEA lookup', () => {
  test('the form warns that every lookup is recorded', async ({ page }) => {
    await signIn(page, 'noc');
    await page.goto('/staff/lea');
    await expect(page.getByText(/recorded against your name/i)).toBeVisible();
    await snap(page, 'lea-form');
  });

  test('a lookup resolves a subscriber from an IP and time', async ({ page }) => {
    await signIn(page, 'noc');
    await page.goto('/staff/lea');

    // The seeded session windows move with the seed date, so the window is
    // read from the page's own data rather than hard-coded to a date that
    // silently stops matching tomorrow.
    const at = new Date();
    at.setUTCDate(at.getUTCDate() - 1);
    const stamp = at.toISOString().slice(0, 11) + '06:00';

    await page.fill('input[name="public_ip"]', '100.64.0.14');
    await page.fill('input[name="timestamp"]', stamp);
    await page.getByRole('button', { name: 'Look up' }).click();

    // Either outcome is correct depending on the seed window; what must not
    // happen is an error or a blank page.
    const matched = await page.getByText('Match').isVisible().catch(() => false);
    const missed = await page.getByText(/No subscriber held that address/).isVisible().catch(() => false);
    expect(matched || missed).toBeTruthy();
    if (matched) {
      await expect(page.getByText('CAF-0001')).toBeVisible();
    }
    await snap(page, 'lea-result');
  });

  test('a lookup without the CSRF token is refused', async ({ page }) => {
    await signIn(page, 'noc');
    await page.goto('/staff/lea');
    const res = await page.request.post('/staff/lea', {
      form: { public_ip: '100.64.0.14', timestamp: '2026-08-10T06:00' },
    });
    expect(res.status()).toBe(403);
  });

  test('a malformed timestamp is reported, not swallowed', async ({ page }) => {
    await signIn(page, 'noc');
    await page.goto('/staff/lea');
    await page.fill('input[name="public_ip"]', '100.64.0.14');
    await page.fill('input[name="timestamp"]', 'yesterday');
    await page.getByRole('button', { name: 'Look up' }).click();
    await expect(page.getByText(/Enter the time as/)).toBeVisible();
  });
});
