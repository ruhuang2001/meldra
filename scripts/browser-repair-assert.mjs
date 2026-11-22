import assert from 'node:assert/strict';
import { mkdir, writeFile } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';

const [modulePath, chrome, url, directory, expectation] = process.argv.slice(2);
assert.ok(['broken', 'fixed'].includes(expectation));
const { puppeteer = undefined, default: defaultExport } = await import(pathToFileURL(modulePath));
const browser = await (puppeteer ?? defaultExport).launch({ executablePath: chrome, headless: true });
const report = { expectation, passed: false, bookings: [], layouts: [], validation: [], errors: [] };
await mkdir(directory, { recursive: true });
try {
  const page = await browser.newPage();
  page.setDefaultTimeout(10000);
  let posts = 0;
  page.on('request', request => { if (request.method() === 'POST') posts++; });
  page.on('pageerror', error => report.errors.push({ kind: 'uncaught', message: error.message }));
  page.on('console', message => {
    if (message.type() === 'error') report.errors.push({ kind: 'console', message: message.text() });
  });
  const fill = async (selector, value) => {
    await page.locator(selector).fill(value);
    assert.equal(await page.$eval(selector, element => element.value), value);
  };
  const invalid = async label => {
    const before = posts;
    await page.click('#reserve');
    assert.equal(await page.$eval('#booking-form', form => form.checkValidity()), false);
    await new Promise(resolve => setTimeout(resolve, 150));
    assert.equal(posts, before, 'invalid form sent a reservation request');
    report.validation.push(label);
  };
  const submit = async () => {
    const responsePromise = page.waitForResponse(response => response.request().method() === 'POST');
    await page.click('#reserve');
    const response = await responsePromise;
    await page.waitForFunction(() => ['success', 'error'].includes(document.querySelector('#status').dataset.state) && !document.querySelector('#reserve').disabled);
    return { url: response.url(), status: response.status(), body: await response.json(),
      state: await page.$eval('#status', element => element.dataset.state),
      text: await page.$eval('#status', element => element.textContent) };
  };

  for (const test of [
    { label: 'desktop', width: 1280, height: 1000, workshop: 'printmaking', seats: 3, total: 195, reference: 'FW-PRI-3' },
    { label: 'mobile', width: 390, height: 844, workshop: 'ceramics', seats: 2, total: 96, reference: 'FW-CER-2' },
  ]) {
    await page.setViewport({ width: test.width, height: test.height });
    await page.goto(url, { waitUntil: 'networkidle0' });
    assert.equal(await page.$eval('#total', element => element.textContent), '$48');
    await invalid(test.label + ': required fields');
    await page.select('#workshop', test.workshop);
    await fill('#seats', String(test.seats));
    await fill('#name', 'Alex Walker');
    await fill('#email', 'alex@example.test');
    assert.equal(await page.$eval('#total', element => element.textContent), '$' + test.total);
    const booking = await submit();
    report.bookings.push({ ...test, ...booking });
    if (expectation === 'broken') {
      assert.equal(booking.status, 404);
      assert.equal(booking.state, 'error');
      assert.ok(booking.text.includes('Unknown API route'));
    } else {
      assert.equal(booking.url, new URL('/api/reservations', url).href);
      assert.equal(booking.status, 201);
      assert.deepEqual(booking.body, { reference: test.reference, workshop: test.workshop, seats: test.seats, total: test.total });
      assert.equal(booking.state, 'success');
      assert.ok(booking.text.includes(test.reference) && booking.text.includes('$' + test.total));
    }
    await page.evaluate(() => window.scrollTo(0, 0));
    const height = await page.evaluate(() => document.documentElement.scrollHeight);
    await page.screenshot({ path: `${directory}/${test.label}.png`,
      clip: { x: 0, y: 0, width: test.width, height: Math.min(height, 2200) } });
    await fill('#seats', '0');
    await invalid(test.label + ': zero seats');
    await fill('#seats', '5');
    await invalid(test.label + ': above seat limit');
    await fill('#seats', '1');
    await fill('#email', 'invalid-email');
    await invalid(test.label + ': invalid email');
    await page.reload({ waitUntil: 'networkidle0' });
    assert.equal(await page.$eval('#total', element => element.textContent), '$48');
    assert.equal(await page.$eval('#status', element => element.dataset.state ?? ''), '');
  }

  for (const width of [320, 390, 768, 1280]) {
    await page.setViewport({ width, height: 844 });
    await page.evaluate(() => window.scrollTo(0, 0));
    const layout = await page.evaluate(() => ({
      width: innerWidth, scrollWidth: document.documentElement.scrollWidth,
      controls: ['#workshop', '#seats', '#name', '#email', '#reserve'].map(selector => {
        const element = document.querySelector(selector), box = element.getBoundingClientRect();
        return { selector, left: box.left, right: box.right, width: box.width, height: box.height, visible: element.checkVisibility() };
      })
    }));
    report.layouts.push(layout);
    if (expectation === 'fixed') {
      assert.ok(layout.scrollWidth <= width, 'horizontal page overflow');
      for (const box of layout.controls) {
        assert.ok(box.visible && box.left >= 0 && box.right <= width && box.width >= 40 && box.height >= 30, JSON.stringify(box));
      }
    } else if (width < 768) {
      assert.ok(layout.scrollWidth > width, 'baseline did not reproduce mobile overflow');
    }
  }
  if (expectation === 'fixed') {
    assert.deepEqual(report.errors, [], 'unexpected browser errors during successful bookings');
    await page.reload({ waitUntil: 'networkidle0' });
    await fill('#name', 'Jamie Morgan');
    await fill('#email', 'jamie@example.test');
    await page.setRequestInterception(true);
    let failNext = true;
    const intercept = request => {
      if (failNext && request.method() === 'POST') {
        failNext = false;
        void request.respond({ status: 503, contentType: 'application/json', body: JSON.stringify({ error: 'Fixture unavailable' }) });
      } else void request.continue();
    };
    page.on('request', intercept);
    const failure = await submit();
    assert.equal(failure.status, 503);
    assert.equal(failure.state, 'error');
    assert.ok(failure.text.includes('Fixture unavailable'));
    const retry = await submit();
    assert.equal(retry.status, 201);
    assert.equal(retry.state, 'success');
    assert.equal(retry.body.total, 48);
    assert.equal(retry.body.reference, 'FW-CER-1');
    report.recovery = { failure, retry };
  }
  report.passed = true;
} finally {
  await browser.close();
  await writeFile(`${directory}/checks.json`, JSON.stringify(report, null, 2) + '\n');
}
console.log(JSON.stringify({ passed: report.passed, expectation, bookings: report.bookings.length, widths: report.layouts.map(layout => layout.width) }));
