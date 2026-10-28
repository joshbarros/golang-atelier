import { test, expect, Page } from '@playwright/test';
import { ADMIN, adminToken, signIn, workshopBody } from './support';

const run = () => Date.now().toString(36).toUpperCase() + Math.floor(Math.random() * 1000);

async function openAccount(
  page: Page,
  code: string,
  capacity: number,
  shown: string,
  limit = capacity,
) {
  await signIn(page, ADMIN);
  await page.getByLabel('Workshop code').fill(code);
  await page.getByLabel('Seats on sale').fill(String(capacity));
  await page.getByLabel('Limit per attendee').fill(String(limit));
  await page.getByRole('button', { name: 'Add workshop' }).click();
  await expect(page.getByRole('heading', { name: `Buy seats for ${code}` })).toBeVisible();
  await expect(page.getByTestId('selected-remaining')).toHaveText(shown);
}

async function submit(page: Page, reference: string, quantity: number) {
  await page.getByLabel('Attendee reference').fill(reference);
  await page.getByLabel('Seats', { exact: true }).fill(String(quantity));
  await page.getByRole('button', { name: 'Buy' }).click();
}

const row = (page: Page, reference: string) => page.locator(`tr[data-reference="${reference}"]`);

test('the API refuses bad input and the page says why', async ({ page }) => {
  const id = run();
  await openAccount(page, `UI-ERR-${id}`, 5, '5');

  await submit(page, `ATT-${id}`, 7);
  await expect(page.getByRole('alert')).toContainText('at most 6 seats');

  await submit(page, ' ', 1);
  await expect(page.getByRole('alert')).toContainText(/reference is required/i);

  await submit(page, `ATT-${id}`, 1);
  await expect(row(page, `ATT-${id}`).getByTestId('status')).toHaveText('Sold', {
    timeout: 20_000,
  });
  await submit(page, `ATT-${id}`, 1);
  await expect(page.getByRole('alert')).toContainText('already');
  await expect(page.locator('tbody tr[data-reference]')).toHaveCount(1);
});

test('a poison reference is rejected, gets no document, and the amount comes back', async ({
  page,
}) => {
  const id = run();
  await openAccount(page, `UI-POI-${id}`, 5, '5');

  await submit(page, `ATT-POISON-${id}`, 2);
  const poisoned = row(page, `ATT-POISON-${id}`);
  await expect(poisoned.getByTestId('status')).toHaveText('Rejected', { timeout: 20_000 });
  await expect(poisoned).toContainText('poison');
  await expect(poisoned.getByTestId('voucher')).toHaveText('—');
  await expect(poisoned.getByRole('button', { name: 'Refund' })).toHaveCount(0);
  await expect(page.getByTestId('selected-remaining')).toHaveText('5');
});

test('the last seat goes to one request and the same attendee asking again is held to the limit', async ({
  page,
}) => {
  const id = run();
  await openAccount(page, `UI-LAST-${id}`, 1, '1');

  await submit(page, `ATT-A-${id}`, 1);
  await submit(page, `ATT-B-${id}`, 1);

  const statuses = page.locator('tbody tr[data-reference] [data-testid="status"]');
  await expect(statuses).toHaveCount(2);
  await expect
    .poll(async () => (await statuses.allTextContents()).map((s) => s.trim()).sort(), {
      timeout: 20_000,
    })
    .toEqual(['Sold', 'Rejected'].sort());
  await expect(page.locator('tbody')).toContainText('per attendee');
  await expect(page.getByTestId('selected-remaining')).toHaveText('0');
});

test('the event log shows the Kafka steps and the reversal keeps the document', async ({
  page,
}) => {
  const id = run();
  await openAccount(page, `UI-LOG-${id}`, 4, '4');

  await submit(page, `ATT-${id}`, 2);
  const mine = row(page, `ATT-${id}`);
  await expect(mine.getByTestId('status')).toHaveText('Sold', { timeout: 20_000 });

  await mine.getByRole('button', { name: 'Event log' }).click();
  const log = page.getByRole('region', { name: 'Kafka event log' });
  await expect(log).toContainText('seats.reserve', { timeout: 20_000 });
  await expect(log).toContainText('seats.held');
  await expect(log).toContainText('voucher.issued');

  await mine.getByRole('button', { name: 'Refund' }).click();
  await expect(mine.getByTestId('status')).toHaveText('Refunded');
  await expect(mine.getByRole('button', { name: 'Refund' })).toHaveCount(0);
  await expect(mine.getByTestId('voucher')).toHaveText(/VC-[0-9A-F]{12}/);
  await expect(page.getByTestId('selected-remaining')).toHaveText('4');
  await expect(log).toContainText('seats.released', { timeout: 20_000 });
});

test('one attendee cannot go past the limit per attendee, and a refund gives the allowance back', async ({
  page,
}) => {
  const id = run();
  await openAccount(page, `UI-CAP-${id}`, 10, '10', 2);
  await expect(page.getByTestId(`remaining-UI-CAP-${id}`)).toContainText('max 2 each');

  await submit(page, `ATT-ONE-${id}`, 2);
  await expect(row(page, `ATT-ONE-${id}`).getByTestId('status')).toHaveText('Sold', {
    timeout: 20_000,
  });

  await submit(page, `ATT-TWO-${id}`, 1);
  const capped = row(page, `ATT-TWO-${id}`);
  await expect(capped.getByTestId('status')).toHaveText('Rejected', { timeout: 20_000 });
  await expect(capped).toContainText('at most 2 seats per attendee');
  await expect(page.getByTestId('selected-remaining')).toHaveText('8');

  await row(page, `ATT-ONE-${id}`).getByRole('button', { name: 'Refund' }).click();
  await expect(row(page, `ATT-ONE-${id}`).getByTestId('status')).toHaveText('Refunded', {
    timeout: 20_000,
  });
  await expect(page.getByTestId('selected-remaining')).toHaveText('10');

  await submit(page, `ATT-THREE-${id}`, 2);
  await expect(row(page, `ATT-THREE-${id}`).getByTestId('status')).toHaveText('Sold', {
    timeout: 20_000,
  });
});

test('a bundle takes a seat and a laptop kit together: when the kits run out nothing is booked', async ({
  page,
  request,
}) => {
  const id = run();
  const code = `UI-KIT-${id}`;
  const admin = { Authorization: `Bearer ${await adminToken(request)}` };
  const created = await request.post('/api/workshops', {
    headers: admin,
    data: { ...workshopBody(code, 10), kitsTotal: 3, kitPriceCents: 1500 },
  });
  expect(created.status()).toBe(201);

  await signIn(page, ADMIN);
  await page.getByRole('button', { name: new RegExp(`^${code}`) }).click();
  await expect(page.getByTestId(`remaining-${code}`)).toContainText('3 of 3 kits left');
  await page.getByLabel(/Also reserve a laptop kit/).check();

  await submit(page, `ATT-BUNDLE-${id}`, 2); // 2 x (5000 + 1500) = 13000, plus 12% = 14560
  await expect(row(page, `ATT-BUNDLE-${id}`).getByTestId('status')).toHaveText('Sold', {
    timeout: 20_000,
  });
  await expect(row(page, `ATT-BUNDLE-${id}`)).toContainText('$145.60');
  await expect(row(page, `ATT-BUNDLE-${id}`).getByTestId('kit')).toBeVisible();
  await expect(page.getByTestId('selected-remaining')).toHaveText('8');
  await expect(page.getByTestId(`remaining-${code}`)).toContainText('1 of 3 kits left');

  // Two kits wanted, one left: 8 seats are free, but the bundle is refused whole and no seat is taken.
  await submit(page, `ATT-NOKIT-${id}`, 2);
  await expect(row(page, `ATT-NOKIT-${id}`).getByTestId('status')).toHaveText('Rejected', {
    timeout: 20_000,
  });
  await expect(row(page, `ATT-NOKIT-${id}`)).toContainText('No seat was taken');
  await expect(page.getByTestId('selected-remaining')).toHaveText('8');

  // Without a kit the same attendee can still take seats.
  await page.getByLabel(/Also reserve a laptop kit/).uncheck();
  await submit(page, `ATT-SEATONLY-${id}`, 2);
  await expect(row(page, `ATT-SEATONLY-${id}`).getByTestId('status')).toHaveText('Sold', {
    timeout: 20_000,
  });
  await expect(page.getByTestId('selected-remaining')).toHaveText('6');
});
