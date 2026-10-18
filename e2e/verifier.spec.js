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

function syntheticReport(count = 53) {
  const statuses = ['invalid', 'degraded', 'verified'];
  return {
    generated_at: '2026-10-19T12:34:56Z', version: 1, archive_version: 6,
    integrity: 'valid', overall: 'invalid', entries: count + 6,
    policy: {
      tsa_roots_configured: true, tsa_policy_oids: ['1.2.3.4'],
      ots_providers: 2, ots_min_confirmations: 6, online: true
    },
    locations: Array.from({ length: count }, (_, index) => {
      const overall = statuses[index % statuses.length];
      const tsa = overall === 'verified' ? 'verified' : overall;
      const ots = overall === 'invalid' ? 'verified' : overall === 'degraded' ? 'pending' : 'verified';
      return {
        id: String(index + 1), digest_sha256: `${String(index + 1).padStart(2, '0')}${'a'.repeat(62)}`,
        overall, tsa: { result: tsa, error_code: tsa === 'invalid' ? 'digest_mismatch' : undefined, evidence: { source: 'synthetic' } },
        ots: { result: ots, evidence: { height: 100 + index, confirmations: 6 } }
      };
    })
  };
}

function reportForOverall(overall, locations = 1) {
  const report = syntheticReport(locations);
  report.overall = overall;
  if (locations) {
    report.locations[0].overall = overall;
    report.locations[0].tsa.result = overall;
    report.locations[0].ots.result = overall;
  }
  return report;
}

async function installSyntheticWorker(page) {
  await page.addInitScript(() => {
    window.__verifyWorkerPlan = { type: 'hold' };
    class SyntheticWorker {
      constructor() {
        this.terminated = false;
        window.__verifyWorkers = window.__verifyWorkers || [];
        window.__verifyWorkers.push(this);
        queueMicrotask(() => this.onmessage?.({ data: { type: 'ready' } }));
      }

      postMessage(message) {
        window.__verifyOptions = message.options;
        const plan = window.__verifyWorkerPlan || { type: 'hold' };
        if (plan.type === 'hold') return;
        setTimeout(() => {
          if (this.terminated) return;
          this.onmessage?.({ data: plan.type === 'error'
            ? { type: 'error', message: plan.message }
            : { type: 'result', report: plan.report } });
        }, plan.delay || 0);
      }

      terminate() { this.terminated = true; }
    }
    window.Worker = SyntheticWorker;
  });
}

async function selectSyntheticArchive(page) {
  await page.locator('#archive-file').setInputFiles({
    name: 'synthetic.zip', mimeType: 'application/zip', buffer: Buffer.from('synthetic')
  });
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
    await page.screenshot({ path: testInfo.outputPath('verifier-intro.png'), fullPage: true });
    const accessibility = await new AxeBuilder({ page }).analyze();
    expect(accessibility.violations.filter(item => ['critical', 'serious'].includes(item.impact))).toEqual([]);
    await expect(page.locator('#settings')).not.toHaveAttribute('open', '');
    await page.getByText('Verification settings', { exact: true }).click();
    await expect(page.locator('#settings')).toHaveAttribute('open', '');
    await page.getByText('Method and limitations', { exact: true }).click();
    await expect(page.locator('#method')).toContainText('does not validate parent links');
    await page.getByText('Method and limitations', { exact: true }).click();
    const initialTheme = await page.locator('html').getAttribute('data-theme');
    await page.locator('#theme-toggle').click();
    await expect(page.locator('html')).not.toHaveAttribute('data-theme', initialTheme);
    await page.locator('#theme-toggle').click();
    await expect(page.locator('html')).toHaveAttribute('data-theme', initialTheme);
    await page.locator('#roots-file').setInputFiles(path.join(fixtureDir, 'roots.pem'));
    await expect(page.locator('#roots-state')).toHaveText('roots.pem');
    await page.locator('#roots-file').setInputFiles({
      name: 'oversized.pem',
      mimeType: 'application/x-pem-file',
      buffer: Buffer.alloc((1 << 20) + 1, 'x')
    });
    await expect(page.locator('#roots-state')).toHaveText('Not configured');
    await expect(page.locator('#roots-state')).not.toContainText('roots.pem');
    await page.locator('#roots-file').setInputFiles(path.join(fixtureDir, 'roots.pem'));
    await page.locator('#policy-oids').fill('1.2.3.4');
    await page.locator('#archive-file').setInputFiles(path.join(fixtureDir, 'fixture.zip'));
    await expect(page.locator('#verify-button')).toBeEnabled();
    await page.locator('#verify-button').click();

    await expect(page.locator('#locations-body tr')).toHaveCount(1, { timeout: 60000 });
    await expect(page.locator('#results')).toBeVisible();
    await expect(page.locator('#verify-form')).toBeHidden();
    await expect(page.locator('#result-title')).toBeFocused();
    await expect(page.locator('#run-message')).toHaveText('');
    const row = page.locator('#locations-body tr').first();
    await expect(row.locator('td').nth(3).locator('.status')).toHaveText('verified');
    await expect(row.locator('td').nth(4).locator('.status')).toHaveText('pending');
    await expect(page.locator('#report-file')).toContainText('fixture.zip');
    await expect(page.locator('#report-time')).not.toHaveText('');
    await expect(page.locator('#report-roots')).toContainText(/configured/i);
    await expect(page.locator('#report-oids')).toContainText('1.2.3.4');
    await expect(page.locator('#report-network')).toContainText(/offline/i);
    await expect(page.locator('#report-confirmations')).toContainText('6');
    await expect(page.locator('#report-scope')).toContainText(/entire archive.*1 location/i);
    await expect(page.locator('#outcome-message')).not.toHaveText('');
    await expect(page.locator('#run-state')).toHaveAttribute('data-kind', 'info');
    await expect(page.locator('#counts-body [data-status="verified"]')).toBeVisible();

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

    await page.locator('#new-verification').click();
    await expect(page.locator('#results')).toBeHidden();
    await expect(page.locator('#verify-form')).toBeVisible();
    await expect(page.locator('#policy-oids')).toHaveValue('1.2.3.4');
    await expect(page.locator('#roots-state')).toHaveText('roots.pem');
    await expect(page.locator('#settings')).toHaveAttribute('open', '');
    await expect(page.locator('#roots-file')).toHaveValue(/roots\.pem/);
    await expect(page.locator('#archive-file')).toHaveValue(/fixture\.zip/);

    await page.locator('#archive-file').setInputFiles({
      name: 'malformed.zip',
      mimeType: 'application/zip',
      buffer: Buffer.from('not a zip archive')
    });
    await page.locator('#verify-button').click();
    await expect(page.locator('#run-state')).toHaveAttribute('data-kind', 'error', { timeout: 60000 });
    await expect(page.locator('#results')).toBeHidden();
    await page.locator('#error-detail summary').click();
    await expect(page.locator('#error-detail')).toContainText(/archive|invalid/i);
    expect(externalRequests).toEqual([]);
    let providerCalls = 0;
    let holdProviders = false;
    let releaseProviders;
    const providerGate = new Promise(resolve => { releaseProviders = resolve; });
    await page.context().route('https://*.invalid/**', async route => {
      providerCalls++;
      if (holdProviders) await providerGate;
      await route.fulfill({ status: 503, headers: { 'Access-Control-Allow-Origin': '*' }, body: 'unavailable' });
    });
    await page.locator('#providers').fill('https://esplora-a.invalid/api\nhttps://esplora-b.invalid/api');
    await page.locator('#online-check').check();
    await page.locator('#archive-file').setInputFiles(path.join(fixtureDir, 'anchored.zip'));
    await page.locator('#verify-button').click();
    await expect(page.locator('#locations-body tr').first().locator('td').nth(4).locator('.status')).toHaveText('error', { timeout: 60000 });
    expect(providerCalls).toBe(2);
    await expect(page.locator('#run-state')).toHaveAttribute('data-kind', 'info');
    const onlineDownload = page.waitForEvent('download');
    await page.locator('#download-report').click();
    const onlineText = fs.readFileSync(await (await onlineDownload).path(), 'utf8');
    expect(JSON.parse(onlineText).overall).toBe('error');
    expect(onlineText).not.toContain('.invalid');
    holdProviders = true;
    await page.locator('#new-verification').click();
    await page.locator('#verify-button').click();
    await expect.poll(() => providerCalls).toBeGreaterThan(2);
    await page.locator('#cancel-button').click();
    releaseProviders();
    await expect(page.locator('#verify-button')).toBeEnabled();
    await expect(page.locator('#results')).toBeHidden();
    expect(pageErrors).toEqual([]);
  } finally {
    fs.rmSync(fixtureDir, { recursive: true, force: true });
  }
});

test('synthetic reports expose status, policy, aggregates, print, and detail semantics', async ({ page }) => {
  await installSyntheticWorker(page);
  await page.goto(pathToFileURL(indexPath).href);
  await page.evaluate(report => { window.__verifyWorkerPlan = { type: 'result', report }; }, syntheticReport());
  await selectSyntheticArchive(page);
  await page.locator('#verify-button').click();

  await expect(page.locator('#results')).toBeVisible();
  await expect(page.locator('#verify-form')).toBeHidden();
  await expect(page.locator('#result-title')).toBeFocused();
  await expect(page.locator('#run-state')).toHaveAttribute('data-kind', 'info');
  await expect(page.locator('#run-message')).toHaveText('');
  await expect(page.locator('#outcome-message')).toContainText(/invalid|failed|cannot/i);
  await expect(page.locator('#report-file')).toContainText('synthetic.zip');
  await expect(page.locator('#report-time')).toContainText('2026');
  await expect(page.locator('#report-roots')).toContainText(/configured/i);
  await expect(page.locator('#report-oids')).toContainText('1.2.3.4');
  await expect(page.locator('#report-network')).toContainText(/online/i);
  await expect(page.locator('#report-confirmations')).toContainText('6');
  await expect(page.locator('#report-scope')).toContainText(/entire archive.*53 locations/i);

  const expectedCounts = {
    invalid: [18, 18, 0], degraded: [18, 18, 0], verified: [17, 17, 35],
    pending: [0, 0, 18]
  };
  for (const [status, expected] of Object.entries(expectedCounts)) {
    const row = page.locator(`#counts-body [data-status="${status}"]`);
    await expect(row).toBeVisible();
    const numbers = await row.locator('td').allTextContents();
    expect(numbers.length).toBe(3);
    expect(numbers.map(value => Number(value.trim()))).toEqual(expected);
  }
  await page.locator('#status-filter').selectOption('invalid');
  await expect(page.locator('#locations-body tr')).toHaveCount(18);
  await expect(page.locator('#counts-body [data-status="verified"]')).toBeVisible();

  const detail = page.getByRole('button', { name: /Details for location/ }).first();
  await detail.focus();
  await page.keyboard.press('Enter');
  await expect(page.locator('#detail-dialog')).toBeVisible();
  await expect(page.locator('#detail-dialog')).toContainText('TSA evidence');
  await page.keyboard.press('Escape');
  await expect(page.locator('#detail-dialog')).toBeHidden();

  await page.evaluate(() => { window.__printed = 0; window.print = () => { window.__printed++; }; });
  await page.locator('#print-report').click();
  await expect.poll(() => page.evaluate(() => window.__printed)).toBe(1);
  await page.locator('#theme-toggle').click();
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
  await page.emulateMedia({ media: 'print' });
  await expect(page.locator('#print-report')).toBeHidden();
  await expect(page.locator('#location-results')).toBeHidden();
  await expect(page.locator('#counts-body [data-status="verified"]')).toBeVisible();
  await expect(page.locator('body')).toHaveCSS('background-color', 'rgb(255, 255, 255)');
  await page.emulateMedia({ media: 'screen' });

  for (const status of ['verified', 'degraded', 'pending', 'inconclusive', 'invalid', 'error', 'missing', 'unknown']) {
    await page.locator('#new-verification').click();
    await page.evaluate(report => { window.__verifyWorkerPlan = { type: 'result', report }; }, reportForOverall(status));
    await page.locator('#verify-button').click();
    await expect(page.locator('#results')).toBeVisible();
    await expect(page.locator('#result-title')).toBeFocused();
    await expect(page.locator('#summary-overall .status')).toHaveText(status);
    await expect(page.locator('#run-state')).toHaveAttribute('data-kind', 'info');
    await expect(page.locator('#run-message')).toHaveText('');
  }
  await page.locator('#new-verification').click();
  await page.evaluate(report => { window.__verifyWorkerPlan = { type: 'result', report }; }, reportForOverall('verified', 0));
  await page.locator('#verify-button').click();
  await expect(page.locator('#summary-overall .status')).toHaveText('inconclusive');
  await expect(page.locator('#result-title')).toBeFocused();
});

test('synthetic verifier errors are inspectable, retryable, and cancellable', async ({ page }) => {
  await installSyntheticWorker(page);
  await page.goto(pathToFileURL(indexPath).href);
  await selectSyntheticArchive(page);
  await page.evaluate(() => { window.__verifyWorkerPlan = { type: 'error', message: 'invalid_options' }; });
  await page.locator('#verify-button').click();
  await expect(page.locator('#run-state')).toHaveAttribute('data-kind', 'error');
  await expect(page.locator('#results')).toBeHidden();
  await expect(page.locator('#error-detail')).toBeVisible();
  await page.locator('#error-detail summary').click();
  await expect(page.locator('#error-detail')).toContainText('invalid_options');
  await expect(page.locator('#verify-button')).toBeEnabled();

  await page.evaluate(report => { window.__verifyWorkerPlan = { type: 'result', report }; }, syntheticReport(1));
  await page.locator('#verify-button').click();
  await expect(page.locator('#results')).toBeVisible();
  await page.locator('#new-verification').click();
  await expect(page.locator('#verify-form')).toBeVisible();
  await expect(page.locator('#archive-file')).toHaveValue(/synthetic\.zip/);

  await page.evaluate(() => { window.__verifyWorkerPlan = { type: 'hold' }; });
  await page.locator('#verify-button').click();
  await expect(page.locator('#cancel-button')).toBeVisible();
  await page.locator('#cancel-button').click();
  await expect(page.locator('#cancel-button')).toBeHidden();
  await expect(page.locator('#verify-button')).toBeEnabled();
  await expect(page.locator('#run-state')).toHaveAttribute('data-kind', 'info');
  await expect(page.locator('#run-message')).toContainText(/cancel/i);
});

test('verifier input and action controls fit the mobile first fold', async ({ page }) => {
  await page.setViewportSize({ width: 412, height: 839 });
  await page.goto(pathToFileURL(indexPath).href);
  await expect(page.getByRole('heading', { name: 'Location proof verification', exact: true })).toBeVisible();
  await selectSyntheticArchive(page);
  await expect(page.locator('#drop-area')).toBeVisible();
  await expect(page.locator('#verify-button')).toBeVisible();
  const fold = await page.evaluate(() => {
    const rect = id => document.getElementById(id).getBoundingClientRect();
    return { drop: rect('drop-area'), verify: rect('verify-button'), viewport: innerHeight };
  });
  expect(fold.drop.top).toBeGreaterThanOrEqual(0);
  expect(fold.drop.bottom).toBeLessThanOrEqual(fold.viewport);
  expect(fold.verify.top).toBeGreaterThanOrEqual(0);
  expect(fold.verify.bottom).toBeLessThanOrEqual(fold.viewport);
});
