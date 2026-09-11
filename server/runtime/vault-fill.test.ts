import assert from 'node:assert/strict';
import test from 'node:test';
import { fillVaultFields, generateTOTP, validVaultFillRequest } from './vault-fill.ts';
import type { VaultFillRequest } from './vault-fill.ts';

declare global {
  interface Window {
    order: string[];
    submitted?: boolean;
  }
}

const seed = 'GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ';

test('RFC 6238 SHA1 vectors, truncated to six digits', () => {
  for (const [seconds, expected] of [
    [59, '287082'], [1111111109, '081804'], [1111111111, '050471'],
    [1234567890, '005924'], [2000000000, '279037'], [20000000000, '353130'],
  ] as const) assert.equal(generateTOTP(seed, seconds * 1000), expected);
  assert.equal(generateTOTP(seed.toLowerCase(), 59000), '287082');
  assert.equal(generateTOTP('MY======', 59000), generateTOTP('MY', 59000));
  assert.notEqual(generateTOTP(seed, 29999), generateTOTP(seed, 30000));
  for (const invalid of ['', 'bad secret!', 'A', 'MZ', 'MY=', 'MY=======', 'MY======A']) {
    assert.throws(() => generateTOTP(invalid), { message: 'invalid_seed' });
  }
});

test('bounded typed request validation', () => {
  const valid: VaultFillRequest = { bindings: [{ selector: '#a', type: 'password', value: '' }] };
  assert.ok(validVaultFillRequest(valid));
  for (const invalid of [null, {}, { bindings: [] }, { ...valid, timeout_ms: 0 },
    { ...valid, timeout_ms: 30001 }, { ...valid, timeout_ms: 1.1 },
    { ...valid, bindings: Array(101).fill(valid.bindings[0]) },
    { bindings: [{ selector: '#a', type: 'totp' }] },
    { bindings: [{ selector: '#a', type: 'button', value: 'secret' }] },
    { bindings: [{ selector: '', type: 'text', value: 'secret' }] },
  ]) assert.equal(validVaultFillRequest(invalid), false);
});

// Opt in locally with VAULT_FILL_BROWSER_TESTS=1 and playwright-core installed.
// Chromium is the same real engine used by the daemon, not a DOM mock.
test('pinned credential fill against local Chromium', { skip: !process.env.VAULT_FILL_BROWSER_TESTS, timeout: 60000 }, async t => {
  const { chromium } = await import('playwright-core');
  const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH || '/usr/bin/chromium', headless: true, args: ['--no-sandbox'] });
  t.after(() => browser.close());
  const context = await browser.newContext();
  const page = await context.newPage();
  const binding = (selector: string, value = 'secret-value', type: VaultFillRequest['bindings'][number]['type'] = 'password') => ({ selector, value, type });
  const run = (bindings: VaultFillRequest['bindings'], extra: Partial<VaultFillRequest> = {}) =>
    fillVaultFields(browser, { bindings, timeout_ms: 1000, ...extra }, AbortSignal.timeout(extra.timeout_ms ?? 1000));
  const statuses = (result: Awaited<ReturnType<typeof run>>) => result.fields.map(field => field.status);

  await t.test('fills in order across frames and wrappers; never submits; generates TOTP at write time', async () => {
    await page.setContent(`<form onsubmit="window.submitted=true;return false"><input id="email" type="email"><div id="wrapper"><input type="password"></div><iframe srcdoc='<input id="otp">'></iframe><button>submit</button></form>`);
    await page.frames()[1].waitForSelector('#otp');
    await page.evaluate(() => {
      window.order = [];
      document.addEventListener('input', event => window.order.push((event.target as HTMLInputElement).type));
    });
    const before = generateTOTP(seed);
    const result = await run([binding('#email', 'a@example.com', 'email'), binding('#wrapper'), binding('#otp', seed, 'totp')]);
    assert.equal(result.status, 'filled');
    assert.deepEqual(statuses(result), ['filled', 'filled', 'filled']);
    assert.equal(await page.locator('#wrapper input').inputValue(), 'secret-value');
    const otp = await page.frames()[1].locator('#otp').inputValue();
    assert.ok([before, generateTOTP(seed)].includes(otp));
    assert.notEqual(otp, seed);
    assert.deepEqual(await page.evaluate(() => window.order), ['email', 'password']);
    assert.equal(await page.evaluate(() => window.submitted), undefined);
    assert.ok(!JSON.stringify(result).includes('secret'));
  });

  await t.test('preflights missing, invalid, ambiguous, duplicate, readonly, hidden and invalid-seed targets without any writes', async () => {
    for (const second of [binding('#missing'), binding('['), binding('.ambiguous'), binding('#wrapper'),
      binding('#readonly'), binding('#hidden'), binding('#multiple'), binding('#otp', 'not base32', 'totp'), binding('#a')]) {
      await page.setContent(`<input id="a"><div id="wrapper"><input id="b" class="ambiguous"></div><input class="ambiguous"><input id="readonly" readonly><input id="hidden" hidden><div id="multiple"><input><input></div><input id="otp">`);
      const first = second.selector === '#wrapper' ? binding('#b') : binding('#a');
      const result = await run([first, second]);
      assert.equal(result.status, 'failed', second.selector);
      assert.deepEqual(statuses(result), ['not_attempted', 'failed']);
      assert.equal(await page.locator(first.selector).inputValue(), '');
    }
  });

  await t.test('selector ambiguity across frames is rejected', async () => {
    await page.setContent(`<input id="same"><iframe srcdoc='<input id="same">'></iframe>`);
    await page.frames()[1].waitForSelector('#same');
    assert.equal((await run([binding('#same')])).status, 'failed');
    assert.equal(await page.locator('#same').inputValue(), '');
  });

  await t.test('rejects multiple pages; exact URL resolves across contexts; duplicate URLs still fail', async () => {
    await page.setContent('<input id="a">');
    const otherContext = await browser.newContext();
    const other = await otherContext.newPage();
    assert.equal((await run([binding('#a')])).status, 'failed');
    assert.equal((await run([binding('#a')], { page_url: 'about:blank' })).status, 'failed');
    await other.goto('data:text/html,<input id="a">');
    assert.equal((await run([binding('#a')], { page_url: 'data:text/html,' })).status, 'failed');
    assert.equal((await run([binding('#a')], { page_url: other.url() })).status, 'filled');
    assert.equal(await page.locator('#a').inputValue(), '');
    assert.equal(await other.locator('#a').inputValue(), 'secret-value');
    await otherContext.close();
  });

  await t.test('stops after detachment/replacement; never retargets the selector', async () => {
    await page.setContent(`<input id="a" oninput="document.querySelector('#b').outerHTML='<input id=b>'"><input id="b"><input id="c">`);
    const result = await run([binding('#a'), binding('#b'), binding('#c')]);
    assert.equal(result.status, 'partial');
    assert.deepEqual(statuses(result), ['filled', 'failed', 'not_attempted']);
    assert.equal(await page.locator('#b').inputValue(), '');
    assert.equal(await page.locator('#c').inputValue(), '');
  });

  await t.test('stops on navigation without writing later fields', async () => {
    await page.setContent(`<input id="a" oninput="location.hash='changed'"><input id="b">`);
    const result = await run([binding('#a'), binding('#b')]);
    assert.ok(['unknown', 'partial'].includes(result.status));
    assert.equal(await page.locator('#b').inputValue(), '');
    await page.goto('about:blank');
  });

  await t.test('fill failure is unknown; later fields remain not attempted; raw errors are removed', async () => {
    await page.setContent('<input id="number" type="number"><input id="b">');
    const result = await run([binding('#number', 'secret-not-a-number'), binding('#b')]);
    assert.equal(result.status, 'unknown');
    assert.deepEqual(statuses(result), ['unknown', 'not_attempted']);
    assert.ok(!JSON.stringify(result).includes('secret-not-a-number'));
  });

  await t.test('deadline before execution prevents writes', async () => {
    await page.setContent('<input id="a">');
    const controller = new AbortController();
    controller.abort();
    const result = await fillVaultFields(browser, { bindings: [binding('#a')] }, controller.signal);
    assert.equal(result.status, 'failed');
    assert.equal(await page.locator('#a').inputValue(), '');
  });
});
