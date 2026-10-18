const { test, expect } = require('@playwright/test');
const AxeBuilder = require('@axe-core/playwright').default;
const { execFileSync } = require('node:child_process');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { pathToFileURL } = require('node:url');

const repoRoot = path.resolve(__dirname, '..');
const indexPath = path.join(repoRoot, 'index.html');

function statusOf(value) {
  if (value && typeof value === 'object') return statusOf(value.result ?? value.status ?? value.overall);
  return String(value ?? '').toLowerCase();
}

test('standalone verifier checks exported proofs offline', async ({ page }, testInfo) => {
  test.setTimeout(120000);
  expect(fs.existsSync(indexPath), 'generate index.html with cmd/verifygen before running this suite').toBeTruthy();

  const fixtureDir = fs.mkdtempSync(path.join(os.tmpdir(), 'mytracker-verifier-'));
  const externalRequests = [];
  const pageErrors = [];
  page.context().on('request', request => {
    if (/^https?:\/\//i.test(request.url())) externalRequests.push(request.url());
  });
  page.on('pageerror', error => pageErrors.push(error.message));

  try {
    execFileSync('go', ['run', './e2e/verifier-fixture.go', fixtureDir], {
      cwd: repoRoot,
      env: { ...process.env, GOCACHE: process.env.GOCACHE || '/tmp/mytracker-verifier-gocache' },
      stdio: 'pipe',
      timeout: 90000
    });

    await page.goto(pathToFileURL(indexPath).href);
    await expect(page.getByRole('heading', { name: 'Location proof verification', exact: true })).toBeVisible();
    await expect(page.getByText('It cannot prove that the reported location, event time, or identity is true.', { exact: false })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'One record, two time anchors' })).toBeVisible();
    await expect(page.locator('.trust-boundary')).toContainText('not proof of authorship');
    await expect(page.locator('.proof-guide')).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath('verifier-intro.png'), fullPage: true });
    const accessibility = await new AxeBuilder({ page }).analyze();
    expect(accessibility.violations.filter(item => ['critical', 'serious'].includes(item.impact))).toEqual([]);
    await page.getByText('Archive structure and the exact record binding', { exact: true }).click();
    await expect(page.locator('.reference-body').first()).toContainText('Other payload fields are not covered');
    await page.getByText('Archive structure and the exact record binding', { exact: true }).click();
    await page.getByText('Verification scope, trust policy, and result meanings', { exact: true }).click();
    await expect(page.locator('.reference-body').nth(1)).toContainText('does not validate parent links');
    await page.getByText('Verification scope, trust policy, and result meanings', { exact: true }).click();
    const initialTheme = await page.locator('html').getAttribute('data-theme');
    await page.locator('#theme-toggle').click();
    await expect(page.locator('html')).not.toHaveAttribute('data-theme', initialTheme);
    await page.locator('#theme-toggle').click();
    await expect(page.locator('html')).toHaveAttribute('data-theme', initialTheme);
    await page.locator('#roots-file').setInputFiles(path.join(fixtureDir, 'roots.pem'));
    await expect(page.locator('#roots-state')).toContainText('Selected PEM replaces embedded trust roots: roots.pem');
    await page.locator('#roots-file').setInputFiles({
      name: 'oversized.pem',
      mimeType: 'application/x-pem-file',
      buffer: Buffer.alloc((1 << 20) + 1, 'x')
    });
    await expect(page.locator('#roots-state')).toContainText('No trust roots configured');
    await expect(page.locator('#roots-state')).not.toContainText('roots.pem');
    await page.locator('#roots-file').setInputFiles(path.join(fixtureDir, 'roots.pem'));
    await page.locator('#policy-oids').fill('1.2.3.4');
    await page.locator('#archive-file').setInputFiles(path.join(fixtureDir, 'fixture.zip'));
    await expect(page.locator('#verify-button')).toBeEnabled();
    await page.locator('#verify-button').click();

    await expect(page.locator('#locations-body tr')).toHaveCount(1, { timeout: 60000 });
    const row = page.locator('#locations-body tr').first();
    await expect(row.locator('td').nth(3).locator('.status')).toHaveText('verified');
    await expect(row.locator('td').nth(4).locator('.status')).toHaveText('pending');

    const downloadEvent = page.waitForEvent('download');
    await page.locator('#download-report').click();
    const download = await downloadEvent;
    const report = JSON.parse(fs.readFileSync(await download.path(), 'utf8'));
    expect(statusOf(report.integrity)).toBe('valid');
    expect(statusOf(report.locations[0].tsa)).toBe('verified');
    expect(statusOf(report.locations[0].ots)).toBe('pending');

    const dimensions = await page.evaluate(() => ({
      client: document.documentElement.clientWidth,
      scroll: document.documentElement.scrollWidth
    }));
    expect(dimensions.scroll).toBeLessThanOrEqual(dimensions.client);
    await page.screenshot({ path: testInfo.outputPath('verifier.png'), fullPage: true });

    await page.locator('#clear-report').click();
    await expect(page.locator('#results')).toBeHidden();
    await expect(page.locator('#empty-report')).toBeVisible();

    await page.locator('#archive-file').setInputFiles({
      name: 'malformed.zip',
      mimeType: 'application/zip',
      buffer: Buffer.from('not a zip archive')
    });
    await page.locator('#verify-button').click();
    await expect(page.locator('#run-state')).toHaveAttribute('data-kind', 'error', { timeout: 60000 });
    await expect(page.locator('#results')).toBeHidden();
    expect(externalRequests).toEqual([]);
    let providerCalls = 0;
    await page.context().route('https://*.invalid/**', async route => {
      providerCalls++;
      await route.fulfill({ status: 503, headers: { 'Access-Control-Allow-Origin': '*' }, body: 'unavailable' });
    });
    await page.locator('#providers').fill('https://esplora-a.invalid/api\nhttps://esplora-b.invalid/api');
    await page.locator('#online-check').check();
    await page.locator('#archive-file').setInputFiles(path.join(fixtureDir, 'anchored.zip'));
    await page.locator('#verify-button').click();
    await expect(page.locator('#locations-body tr').first().locator('td').nth(4).locator('.status')).toHaveText('error', { timeout: 60000 });
    expect(providerCalls).toBe(2);
    const onlineDownload = page.waitForEvent('download');
    await page.locator('#download-report').click();
    const onlineText = fs.readFileSync(await (await onlineDownload).path(), 'utf8');
    expect(JSON.parse(onlineText).overall).toBe('error');
    expect(onlineText).not.toContain('.invalid');
    await page.locator('#verify-button').click();
    await page.locator('#cancel-button').click();
    await expect(page.locator('#verify-button')).toBeEnabled();
    await expect(page.locator('#results')).toBeHidden();
    expect(pageErrors).toEqual([]);
  } finally {
    fs.rmSync(fixtureDir, { recursive: true, force: true });
  }
});
