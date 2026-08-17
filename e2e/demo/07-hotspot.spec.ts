import { test } from '@playwright/test';
import { say, beat, reframe } from './demo-kit';

const WHO = ['Walk-up user', 'no account'] as const;

/**
 * Not a persona in the CRD, but a real person: someone who has just associated
 * with a hotspot and has no account at all. The one unauthenticated surface in
 * the system.
 */
test('Captive portal — the walk-up Wi-Fi user', async ({ page }) => {
  await page.goto('/hotspot/portal?mac=aa-bb-cc-dd-ee-ff&link-orig=https://example.com/');
  await reframe(page, ...WHO, 'The captive portal',
    'Where a device lands after associating with the hotspot, before it has any network access');
  await beat(page);

  await say(page, 'The NAS sent the device MAC in the redirect',
    'Typed as aa-bb-cc-dd-ee-ff; shown as AA:BB:CC:DD:EE:FF — normalised to the form RADIUS will authorise',
    { hold: 4800 });

  await say(page, 'Two ways onto the network',
    'A prepaid voucher code, or an existing subscriber account — both end in a RADIUS authentication',
    { hold: 4400 });

  await page.getByLabel('Voucher code').fill('HS-WXYZ-2468-MNPQ');
  await say(page, 'Redeeming a voucher that does not exist',
    'Watch what the refusal says — and what it deliberately does not say');
  await page.getByRole('button', { name: 'Get online' }).click();
  await page.waitForLoadState('networkidle');

  await reframe(page, ...WHO, 'Refused, without saying why',
    'Wrong code, expired code and already-redeemed code all produce this same response');
  await beat(page, 4800);

  await say(page, 'That uniformity is the control',
    'A distinct message per case would turn this public form into an oracle for which codes exist',
    { hold: 4800 });

  await say(page, 'And the attempt was counted',
    'Ten per MAC per fifteen minutes. If Redis is unavailable the endpoint refuses rather than admits.',
    { hold: 4600 });

  await say(page, 'Even a valid code does not grant access here',
    'Redemption writes a grant; the NAS still authenticates the device over RADIUS afterwards',
    { hold: 4800 });
});
