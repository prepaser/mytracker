const { test, expect } = require('@playwright/test');
const AxeBuilder = require('@axe-core/playwright').default;

const auth = { Authorization: `Basic ${Buffer.from('owntracks:test-pub-pass').toString('base64')}` };

async function accessible(page) {
  const results = await new AxeBuilder({ page }).analyze();
  expect(results.violations.filter(item => ['critical', 'serious'].includes(item.impact))).toEqual([]);
}

async function ingest(page, device, tst = Math.floor(Date.now() / 1000), lat = 37.5, lon = 127) {
  const response = await page.request.post(`/pub?d=${device}`, {
    headers: auth,
    data: { _type: 'location', lat, lon, tst }
  });
  expect(response.ok()).toBeTruthy();
}

async function signIn(page) {
  await page.goto('/login');
  await page.getByLabel('Password').fill('testpass');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page).toHaveURL('/');
}

async function expectNoDownloadArtifacts(page) {
  await expect.poll(() => page.evaluate(() => ({
    frames: document.querySelectorAll('iframe[name^="download-"]').length,
    cookies: document.cookie.split('; ').filter(value => value.startsWith('mytracker-download-')).length
  }))).toEqual({ frames: 0, cookies: 0 });
}

test.beforeEach(async ({ page }) => {
  await page.goto('/login');
  await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
  await accessible(page);
  await page.getByLabel('Password').fill('testpass');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page).toHaveURL('/');
  await expect(page.locator('[data-connection]').first()).toHaveText('Connected');
});

test('dashboard handles empty and null lists', async ({ page }) => {
  await page.route('**/api/status', async route => {
    const response = await route.fetch();
    const body = await response.json();
    body.data.proof_verification.providers = null;
    body.data.maintenance.jobs = null;
    await route.fulfill({ response, json: body });
  });
  for (const path of ['stamp-jobs', 'maintenance-runs', 'locations']) {
    await page.route(`**/api/${path}?*`, route => route.fulfill({ json: { data: null, next_cursor: '' } }));
  }
  await page.route('**/api/geofence-events?*', route => route.fulfill({ contentType: 'application/json', body: 'null' }));
  await page.reload();
  for (const [id, message] of Object.entries({
    'proof-providers': 'No provider checks yet', 'maintenance-jobs': 'No maintenance jobs',
    'maintenance-runs': 'No maintenance runs yet', 'failed-jobs': 'No failed stamp jobs',
    'recent-locations': 'No locations yet', 'geofence-events': 'No geofence events'
  })) {
    await expect(page.locator(`#${id}`)).toContainText(message);
  }
  await expect(page.locator('#toast-region .error')).toHaveCount(0);
});

test('stream failures become stale and throttle safety refresh', async ({ page }) => {
  await page.clock.install();
  await page.evaluate(() => {
    const Original = window.EventSource;
    class LocalSource extends EventTarget {
      constructor() { super(); window.testSource = this; }
      close() {}
    }
    window.EventSource = LocalSource;
    window.testSafety = 0;
    window.testOpens = 0;
    window.testStopStream = window.mytrackerUI.stream('/api/events', {
      onSafety: () => { window.testSafety++; },
      onOpen: () => { window.testOpens++; }
    });
    window.EventSource = Original;
    window.testSource.dispatchEvent(new Event('open'));
    window.testSource.dispatchEvent(new Event('error'));
    window.testErrors = setInterval(() => window.testSource.dispatchEvent(new Event('error')), 3000);
  });
  await page.clock.runFor(47000);
  await expect(page.locator('[data-connection]').first()).toHaveText('Stale');
  expect(await page.evaluate(() => window.testSafety)).toBe(0);
  await page.clock.runFor(23000);
  expect(await page.evaluate(() => window.testSafety)).toBe(1);
  await expect(page.locator('[data-connection]').first()).toHaveText('Stale');
  await page.evaluate(() => {
    clearInterval(window.testErrors);
    window.testSource.dispatchEvent(new Event('open'));
  });
  await expect(page.locator('[data-connection]').first()).toHaveText('Connected');
  expect(await page.evaluate(() => window.testOpens)).toBe(2);
  await page.evaluate(() => window.testStopStream());
});

test('dashboard renders raw geofence event arrays', async ({ page }) => {
  await page.route('**/api/geofence-events?*', route => route.fulfill({ json: [{
    tst: 1700000000, name: 'Home', event: 'enter', user: 'owntracks', device: 'phone'
  }] }));
  await page.reload();
  const row = page.locator('#geofence-events tr').first();
  await expect(row).toContainText('Home');
  await expect(row).toContainText('enter');
  await expect(row).toContainText('owntracks/phone');
});

test('failed panels are retryable and stale proofs are warnings', async ({ page }) => {
  await page.route('**/api/status', route => route.fulfill({ status: 500, json: { error: { code: 'internal', message: 'Status unavailable', request_id: 'panel-check' } } }));
  await page.route('**/api/locations?*', route => route.fulfill({ status: 500, json: { error: { message: 'Locations unavailable' } } }));
  await page.reload();
  await expect(page.locator('#status-resource')).toContainText('Unable to load status');
  await expect(page.locator('#recent-locations').getByRole('button', { name: 'Retry' })).toBeVisible();
  await expect(page.locator('#health-summary')).toContainText('Attention required');
  await page.unroute('**/api/status');
  await page.locator('#status-resource').getByRole('button', { name: 'Retry' }).click();
  await expect(page.locator('#status-resource')).toContainText('Last updated');
  await page.route('**/api/verify/123', route => route.fulfill({ json: { data: {
    id: 123, payload_ok: true, digest: 'test-digest', overall: 'verified', tsa_stale: true,
    tsa: { result: 'verified' }, ots: { result: 'verified' }, jobs: []
  } } }));
  await page.route('**/api/verify/123/history?*', route => route.fulfill({ json: { data: [] } }));
  await page.evaluate(() => window.mytrackerUI.createVerificationDialog({ csrf: '' }).open(123));
  await expect(page.locator('#verify-out .status-stale')).toHaveCount(2);
  await expect(page.locator('#verify-out .status-verified')).toHaveCount(1);
});

test('expired session can resume without replacing settings drafts', async ({ page, context }) => {
  await page.goto('/settings#proofs');
  await page.getByLabel('TSA URL').fill('https://draft.invalid/tsa');
  const originalCSRF = await page.locator('input[name="csrf"]').first().inputValue();
  await page.route('**/api/status', route => route.fulfill({ status: 401, json: { error: { code: 'unauthorized', message: 'authentication required', request_id: 'resume-check' } } }));
  const error = await page.evaluate(async () => {
    try { await window.mytrackerUI.request('/api/status'); }
    catch (err) { return { status: err.status, code: err.code, requestId: err.requestId }; }
  });
  expect(error).toEqual({ status: 401, code: 'unauthorized', requestId: 'resume-check' });
  await expect(page.locator('#session-notice')).toBeVisible();
  expect(await page.evaluate(() => window.mytrackerUI.isPaused())).toBe(true);
  await page.unroute('**/api/status');
  await context.clearCookies();
  const login = await context.newPage();
  await login.goto('/login');
  await login.getByLabel('Password').fill('testpass');
  await login.getByRole('button', { name: 'Sign in' }).click();
  await expect(login).toHaveURL('/');
  await page.getByRole('button', { name: 'Resume', exact: true }).click();
  await expect(page.locator('#session-notice')).toBeHidden();
  await expect(page.getByLabel('TSA URL')).toHaveValue('https://draft.invalid/tsa');
  await expect(page.locator('#dirty-state')).toHaveText('Unsaved changes');
  await expect(page.locator('input[name="csrf"]').first()).not.toHaveValue(originalCSRF);
  await expect(page.locator('[data-connection]')).toHaveText('Connected');
  await page.getByRole('tab', { name: 'Privacy' }).click();
  const revoked = page.waitForResponse(response => response.url().endsWith('/api/sessions/revoke-others'));
  await page.getByRole('button', { name: 'Revoke others' }).click();
  await page.locator('#ui-confirm-action').click();
  expect((await revoked).status()).toBe(200);
  await expect(page.getByRole('button', { name: 'Revoke others' })).toBeFocused();
  await login.close();
});

test('one-time secrets wait for Done and survive a late close event', async ({ page }) => {
  await page.goto('/settings#privacy');
  await page.getByRole('tab', { name: 'Privacy' }).focus();
  await page.evaluate(() => {
    window.mytrackerUI.secret('First URL', 'https://example.invalid/first');
    window.mytrackerUI.secret('Second URL', 'https://example.invalid/second');
  });
  const secret = page.locator('#ui-secret');
  await expect(secret.getByLabel('One-time URL')).toHaveValue('https://example.invalid/first');
  await secret.getByRole('button', { name: 'Done' }).click();
  await expect(secret.getByLabel('One-time URL')).toHaveValue('https://example.invalid/second');
  await page.evaluate(() => {
    document.querySelector('#ui-secret [data-secret-close]').click();
    window.mytrackerUI.secret('Late URL', 'https://example.invalid/late');
  });
  await expect(secret).toBeVisible();
  await expect(secret.getByLabel('One-time URL')).toHaveValue('https://example.invalid/late');
  await page.keyboard.press('Escape');
  await expect(secret).toBeHidden();
  await expect(secret.getByLabel('One-time URL')).toHaveValue('');
  await expect(page.getByRole('tab', { name: 'Privacy' })).toBeFocused();
});

test('session expiry releases proof dialogs and keeps Resume reachable', async ({ page }) => {
  await page.route('**/api/verify/123', route => route.fulfill({ json: { data: {
    id: 123, payload_ok: true, overall: 'pending', jobs: []
  } } }));
  await page.route('**/api/verify/123/history?*', route => route.fulfill({ json: { data: [] } }));
  await page.evaluate(() => {
    window.testVerification = window.mytrackerUI.createVerificationDialog({ csrf: '' });
    return window.testVerification.open(123);
  });
  await expect(page.locator('#verify-dialog')).toBeVisible();
  await page.route('**/api/status', route => route.fulfill({ status: 401, json: { error: {
    code: 'unauthorized', message: 'authentication required'
  } } }));
  await page.evaluate(() => window.mytrackerUI.request('/api/status').catch(() => {}));
  await expect(page.locator('#verify-dialog')).toBeHidden();
  await expect(page.locator('#session-resume')).toBeFocused();
  await page.evaluate(() => window.testVerification.open(123));
  await expect(page.locator('#verify-dialog')).toBeHidden();
  await expect(page.locator('#session-resume')).toBeFocused();
});

test('proof dialog restores focus after a live row is replaced', async ({ page }) => {
  const first = `focus-before-${Date.now()}`;
  await ingest(page, first);
  await expect(page.locator('#recent-locations tr').first()).toContainText(first);
  await page.locator('#recent-locations').getByRole('button', { name: 'Details' }).first().click();
  const dialog = page.getByRole('dialog', { name: 'Proof verification' });
  await expect(dialog).toBeVisible();
  const next = `focus-after-${Date.now()}`;
  await ingest(page, next);
  await expect(page.locator('#recent-locations tr').first()).toContainText(next);
  await page.locator('#verify-close').click();
  await expect(dialog).toBeHidden();
  await expect(page.locator('.table-wrap').filter({ has: page.locator('#recent-locations') })).toBeFocused();
});

test('live identity filters preserve selections and settings drafts', async ({ page, context }) => {
  const selected = `selected-${Date.now()}`;
  await ingest(page, selected);
  await page.goto('/timeline');
  await page.locator('#timeline-device').selectOption(selected);
  const settings = await context.newPage();
  try {
    await settings.goto('/settings#privacy');
    await settings.locator('#share-device').selectOption(selected);
    await settings.getByRole('tab', { name: 'Proofs' }).click();
    await settings.getByLabel('TSA URL').fill('https://draft.invalid/tsa');
    const added = `added-${Date.now()}`;
    await ingest(page, added);
    await expect(page.locator('#timeline-device option').filter({ hasText: added })).toHaveCount(1);
    await expect(settings.locator('#share-device option').filter({ hasText: added })).toHaveCount(1);
    await expect(page.locator('#timeline-device')).toHaveValue(selected);
    await expect(settings.locator('#share-device')).toHaveValue(selected);
    await expect(settings.getByLabel('TSA URL')).toHaveValue('https://draft.invalid/tsa');
    await expect(settings.locator('#dirty-state')).toHaveText('Unsaved changes');
  } finally {
    await settings.close();
  }
});

test('timeline cursor pages keep their range and open custom map periods', async ({ page }, testInfo) => {
  const requests = [];
  const now = Math.floor(Date.now() / 1000);
  await page.route('**/api/timeline?*', route => {
    const url = new URL(route.request().url());
    requests.push(url.searchParams);
    const second = url.searchParams.has('cursor');
    const start = now - (second ? 3600 : 1200) - 17;
    return route.fulfill({ json: {
      summary: { trips: 101, stays: 0, distance_m: 1000, total_segments: 101 },
      segments: [{ id: second ? 1 : 2, kind: 'trip', user: 'owntracks', device: `cursor-${testInfo.project.name}`, start_tst: start, end_tst: start + 317, distance_m: 1000, point_count: 5 }],
      next_cursor: second ? '' : 'second-page'
    } });
  });
  await page.goto('/timeline?hours=0');
  await expect(page.locator('#timeline-list .timeline-row')).toHaveCount(1);
  const initial = requests.at(-1);
  expect(initial.get('limit')).toBe('100');
  expect(initial.get('to')).toBeTruthy();
  await page.getByRole('button', { name: 'Next', exact: true }).click();
  await expect(page.locator('#timeline-page')).toHaveText('Page 2');
  expect(requests.at(-1).get('to')).toBe(initial.get('to'));
  await ingest(page, `cursor-event-${testInfo.project.name}`, now);
  await expect(page.locator('#timeline-changed')).toBeVisible();
  await expect(page.locator('#timeline-page')).toHaveText('Page 2');
  await page.getByRole('link', { name: 'View on map' }).click();
  if (testInfo.project.name === 'mobile') await page.getByRole('button', { name: 'Toggle filters' }).click();
  await expect(page.getByLabel('Period')).toHaveValue('custom');
  await expect(page.locator('#from')).toBeVisible();
  await expect(page.locator('#to')).toBeVisible();
  const url = new URL(page.url());
  const start = Math.floor(new Date(await page.locator('#from').inputValue()).getTime() / 1000);
  const end = Math.floor(new Date(await page.locator('#to').inputValue()).getTime() / 1000);
  expect(start).toBe(Number(url.searchParams.get('from')));
  expect(end).toBe(Number(url.searchParams.get('to')));
  await accessible(page);
});

test.describe('map custom periods across DST', () => {
  test.use({ timezoneId: 'America/New_York' });

  test('keeps untouched URL timestamps exact and uses local semantics after editing', async ({ page }) => {
    const from = 1793514600;
    const to = from + 3600;
    const requests = [];
    const empty = { type: 'FeatureCollection', features: [], metadata: { total_points: 0, returned_points: 0, sampled: false } };
    await page.route(/\/api\/track\?/, route => {
      requests.push(new URL(route.request().url()).searchParams);
      return route.fulfill({ json: empty });
    });
    await page.goto(`/map?hours=custom&from=${from}&to=${to}`);
    await expect.poll(() => requests.length).toBeGreaterThan(0);
    expect(requests[0].get('from')).toBe(String(from));
    expect(requests[0].get('to')).toBe(String(to));

    if (await page.locator('#filter-toggle').isVisible()) await page.locator('#filter-toggle').click();
    await page.locator('#from').fill('2026-11-01T01:31');
    await page.locator('#from').blur();
    await expect.poll(() => requests.length).toBeGreaterThan(1);
    const edited = await page.evaluate(() => Math.floor(new Date('2026-11-01T01:31:00').getTime() / 1000));
    expect(requests.at(-1).get('from')).toBe(String(edited));
  });
});

test('timeline coalesces automatic refreshes and preserves a later page', async ({ page }, testInfo) => {
  let requests = 0;
  let pageRequests = 0;
  let releaseInitial;
  let releaseFollowup;
  let releasePage;
  const initial = new Promise(resolve => { releaseInitial = resolve; });
  const followup = new Promise(resolve => { releaseFollowup = resolve; });
  const pageLoad = new Promise(resolve => { releasePage = resolve; });
  let initialReleased = false;
  await page.route('**/api/timeline?*', async route => {
    const number = ++requests;
    const secondPage = new URL(route.request().url()).searchParams.has('cursor');
    if (secondPage) {
      pageRequests++;
      await pageLoad;
    }
    else if (!initialReleased) await initial;
    else await followup;
    await route.fulfill({ json: {
      summary: { trips: 1, stays: 0, distance_m: 1000, total_segments: 1 },
      segments: [{ id: number, kind: 'trip', user: 'fixture', device: `coalesce-${testInfo.project.name}`, start_tst: 1, end_tst: 301, distance_m: 1000, point_count: 2 }],
      next_cursor: secondPage ? '' : 'second-page'
    } });
  });
  try {
    await page.goto('/timeline?hours=0');
    await expect.poll(() => requests).toBe(1);
    const now = Math.floor(Date.now() / 1000);
    for (let i = 0; i < 2; i++) {
      await ingest(page, `timeline-coalesce-${testInfo.project.name}`, now + i);
      await page.waitForTimeout(750);
    }
    expect(requests).toBe(1);
    initialReleased = true;
    releaseInitial();
    await expect.poll(() => requests).toBe(2);
    releaseFollowup();
    await expect(page.getByRole('button', { name: 'Next', exact: true })).toBeEnabled();
    await page.getByRole('button', { name: 'Next', exact: true }).click();
    await expect.poll(() => pageRequests).toBe(1);
    await ingest(page, `timeline-page-event-${testInfo.project.name}`, now + 2);
    await page.waitForTimeout(300);
    releasePage();
    await expect(page.locator('#timeline-page')).toHaveText('Page 2');
    await expect(page.locator('#timeline-changed')).toBeVisible();
  } finally {
    releaseInitial();
    releaseFollowup();
    releasePage();
    await page.unroute('**/api/timeline?*');
  }
});

test('map tiles send only the site origin as referrer', async ({ page, context }) => {
  const origin = new URL(page.url()).origin + '/';
  const referrers = [];
  await context.route(/^https:\/\/(?:[abc]\.)?tile\.openstreetmap\.org\//, async route => {
    referrers.push(await route.request().headerValue('referer'));
    await route.fulfill({
      contentType: 'image/png',
      body: Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aD1sAAAAASUVORK5CYII=', 'base64')
    });
  });
  const csrf = await page.locator('#cfg').evaluate(el => JSON.parse(el.textContent).csrf);
  const created = await page.request.post('/api/shares', {
    headers: { 'X-CSRF-Token': csrf }, data: { label: 'tile referrer check' }
  });
  expect(created.ok()).toBeTruthy();
  const share = await created.json();
  for (const path of ['/map?user=private-user&device=private-device', share.url]) {
    referrers.length = 0;
    const response = await page.goto(path);
    expect(response.headers()['referrer-policy']).toBe('no-referrer');
    await expect(page.locator('.leaflet-tile-loaded').first()).toBeVisible();
    expect(referrers.length).toBeGreaterThan(0);
    expect(referrers.every(value => value === origin)).toBe(true);
  }
});

test('dashboard actions remain live and accessible', async ({ page }, testInfo) => {
  const pageErrors = [];
  page.on('pageerror', error => pageErrors.push(error.message));
  await expect(page.getByRole('heading', { name: 'Dashboard' })).toBeVisible();
  await expect(page.locator('#health-summary')).toBeVisible();
  await accessible(page);

  const initialTheme = await page.locator('html').getAttribute('data-theme');
  await page.locator('[data-theme-toggle]').first().click();
  const changedTheme = initialTheme === 'dark' ? 'light' : 'dark';
  await expect(page.locator('html')).toHaveAttribute('data-theme', changedTheme);
  await page.reload();
  await expect(page.locator('html')).toHaveAttribute('data-theme', changedTheme);

  await page.waitForTimeout(1000);
  const idleRequests = [];
  page.on('request', request => {
    const path = new URL(request.url()).pathname;
    if (['/api/status', '/api/stamp-jobs', '/api/maintenance-runs'].includes(path)) {
      idleRequests.push({ path, at: Date.now() });
    }
  });
  await page.waitForTimeout(2500);
  const pollingCycle = idleRequests.some(({ at }) => {
    const nearby = new Set(idleRequests.filter(request => Math.abs(request.at - at) <= 150).map(request => request.path));
    return nearby.size === 3;
  });
  expect(pollingCycle).toBe(false);

  const backup = page.waitForResponse(response => response.url().endsWith('/api/backups'));
  await page.getByRole('button', { name: 'Create backup' }).click();
  expect((await backup).status()).toBe(202);
  await expect(page.getByText('Backup queued')).toBeVisible();

  await ingest(page, `dashboard-${testInfo.project.name}`);
  await expect(page.locator('#recent-locations tr').first()).toContainText(`dashboard-${testInfo.project.name}`, { timeout: 20000 });
  await page.route(/\/api\/stamps\/\d+\/retry$/, route => route.fulfill({
    status: 500,
    contentType: 'application/json',
    body: JSON.stringify({ error: { message: 'simulated retry failure' } })
  }));
  const failedRetry = page.locator('#failed-jobs').getByRole('button', { name: 'Retry' }).first();
  await expect(failedRetry).toBeVisible({ timeout: 20000 });
  await failedRetry.click();
  await expect(page.getByText('simulated retry failure')).toBeVisible();
  await page.unroute(/\/api\/stamps\/\d+\/retry$/);

  await page.locator('#recent-locations').getByRole('button', { name: 'Details' }).first().click();
  const dialog = page.getByRole('dialog', { name: 'Proof verification' });
  await expect(dialog).toBeVisible();
  await expect(page.locator('#verify-out')).toContainText('Overall');
  await expect(page.locator('#verify-out')).toContainText('Trust policy');
  const verify = page.waitForResponse(response => response.request().method() === 'POST' && /\/api\/verify\/\d+$/.test(response.url()));
  await page.getByRole('button', { name: 'Reverify' }).click();
  expect((await verify).status()).toBe(202);
  await page.keyboard.press('Escape');
  await expect(dialog).toBeHidden();
  expect(pageErrors).toEqual([]);
});

test('dashboard tracks retried stamp jobs by location', async ({ page }) => {
  let failedReads = 0;
  let targetedReads = 0;
  await page.route(/\/api\/stamp-jobs\?state=failed&limit=200$/, route => {
    failedReads++;
    return route.fulfill({ json: { data: [{
      id: 101, location_id: 42, kind: 'tsa', state: 'failed', attempts: 3, max_attempts: 3, last_error: 'expired'
    }] } });
  });
  await page.route(/\/api\/stamps\/42\/retry$/, route => route.fulfill({ json: { data: { jobs: [
    { id: 101, location_id: 42, kind: 'tsa', state: 'queued', attempts: 0, max_attempts: 3 },
    { id: 102, location_id: 42, kind: 'ots', state: 'done', attempts: 1, max_attempts: 3 }
  ] } } }));
  await page.route(/\/api\/stamp-jobs\?location_id=42$/, route => {
    targetedReads++;
    const state = targetedReads > 1 ? 'done' : 'queued';
    return route.fulfill({ json: { data: [
      { id: 101, location_id: 42, kind: 'tsa', state, attempts: 1, max_attempts: 3 },
      { id: 102, location_id: 42, kind: 'ots', state: 'done', attempts: 1, max_attempts: 3 }
    ] } });
  });
  await page.addInitScript(() => {
    class TestEventSource extends EventTarget {
      constructor() { super(); window.testEventSource = this; }
      close() {}
    }
    window.EventSource = TestEventSource;
  });
  await page.reload();
  await expect(page.locator('#failed-jobs').getByRole('button', { name: 'Retry' })).toBeVisible({ timeout: 20000 });
  expect(failedReads).toBeGreaterThan(0);
  await page.locator('#failed-jobs').getByRole('button', { name: 'Retry' }).click();
  await expect(page.getByText('Stamp retry queued')).toBeVisible();
  await expect.poll(() => targetedReads).toBe(1);
  expect(await page.locator('#toast-region').getByText('OTS stamp completed').count()).toBe(0);
  await page.evaluate(() => window.testEventSource.dispatchEvent(new MessageEvent('change', {
    data: JSON.stringify({ topics: ['stamp_jobs'], location_ids: [42], all: false })
  })));
  await expect.poll(() => targetedReads).toBe(2);
  await expect(page.locator('#toast-region').getByText('TSA stamp completed')).toBeVisible();
  expect(await page.locator('#toast-region').getByText('OTS stamp completed').count()).toBe(0);
});

test('dashboard restarts stamp observation after a second retry', async ({ page }) => {
  let retryPosts = 0;
  const observations = [];
  await page.addInitScript(() => {
    class TestEventSource extends EventTarget {
      constructor() { super(); window.testEventSource = this; }
      close() {}
    }
    window.EventSource = TestEventSource;
  });
  await page.route(/\/api\/stamp-jobs\?state=failed&limit=200$/, route => route.fulfill({ json: { data: [
    { id: 101, location_id: 42, kind: 'tsa', state: 'failed', attempts: 3, max_attempts: 3, last_error: 'expired' }
  ] } }));
  await page.route(/\/api\/stamps\/42\/retry$/, route => {
    retryPosts++;
    return route.fulfill({ json: { data: { jobs: [
      { id: 101, location_id: 42, kind: 'tsa', state: 'queued', attempts: 0, max_attempts: 3 }
    ] } } });
  });
  await page.route(/\/api\/stamp-jobs\?location_id=42$/, async route => {
    const release = await new Promise(resolve => observations.push(resolve));
    const state = release === 'failed' ? 'failed' : 'done';
    try {
      await route.fulfill({ json: { data: [
        { id: 101, location_id: 42, kind: 'tsa', state, attempts: 1, max_attempts: 3 }
      ] } });
    } catch (error) {
      if (!/aborted|handled|closed/i.test(error.message)) throw error;
    }
  });
  await page.reload();
  await expect(page.locator('#failed-jobs').getByRole('button', { name: 'Retry' })).toBeVisible({ timeout: 20000 });
  await page.locator('#failed-jobs').getByRole('button', { name: 'Retry' }).click();
  await expect.poll(() => observations.length).toBe(1);
  await expect(page.getByText('Stamp retry queued')).toBeVisible();
  await page.locator('#failed-jobs').getByRole('button', { name: 'Retry' }).click();
  await expect.poll(() => retryPosts).toBe(2);
  await expect.poll(() => observations.length).toBe(2);
  observations[0]('failed');
  await page.waitForTimeout(100);
  expect(await page.locator('#toast-region').getByText('TSA stamp failed').count()).toBe(0);
  observations[1]('done');
  await expect(page.getByText('TSA stamp completed')).toBeVisible();
});

test('dashboard tracks bounded bulk stamp retries without SSE', async ({ page }) => {
  let retried = false;
  let targetedReads = 0;
  let completed = false;
  await page.addInitScript(() => {
    class TestEventSource extends EventTarget {
      constructor() { super(); window.bulkSource = this; }
      close() {}
    }
    window.EventSource = TestEventSource;
  });
  await page.route(/\/api\/stamp-jobs\?state=failed&limit=200$/, route => route.fulfill({ json: {
    data: retried ? [] : [{ id: 201, location_id: 52, kind: 'tsa', state: 'failed', attempts: 3, max_attempts: 3, last_error: 'expired' }]
  } }));
  await page.route(/\/api\/stamp-jobs\?location_id=52$/, route => {
    targetedReads++;
    return route.fulfill({ json: { data: [
      { id: 201, location_id: 52, kind: 'tsa', state: completed ? 'done' : 'queued', attempts: 0, max_attempts: 3 },
      { id: 999, location_id: 999, kind: 'tsa', state: 'running', attempts: 1, max_attempts: 3 }
    ] } });
  });
  await page.route('**/api/stamps/retry', route => {
    const count = retried ? 0 : 1;
    retried = true;
    return route.fulfill({ json: { data: { retried: count } } });
  });
  await page.reload();
  await expect(page.locator('#retry-all')).toBeVisible();
  await page.locator('#retry-all').click();
  await expect(page.getByText('1 failed stamps queued')).toBeVisible();
  await page.locator('#retry-all').click();
  await expect(page.getByText('0 failed stamps queued')).toBeVisible();
  await expect(page.getByText('1 stamp retries still pending')).toBeVisible({ timeout: 10000 });
  expect(targetedReads).toBe(5);
  await expect(page.getByText('1 stamp retries completed')).toHaveCount(0);
  completed = true;
  await page.evaluate(() => window.bulkSource.dispatchEvent(new MessageEvent('change', {
    data: JSON.stringify({ topics: ['stamp_jobs'], location_ids: [52], all: false })
  })));
  await expect(page.getByText('1 stamp retries completed')).toBeVisible();
  expect(await page.locator('#failed-jobs').getByText('No failed stamp jobs').count()).toBe(1);
});

test('map coalesces automatic refreshes without canceling an active request', async ({ page }, testInfo) => {
  const name = 'R&D <HQ>';
  await page.route('**/api/geofences', route => route.fulfill({ json: [
    { id: 1, name, enabled: true, lat: 37.5, lon: 127, radius_m: 100 }
  ] }));
  const empty = { type: 'FeatureCollection', features: [], metadata: { total_points: 0, returned_points: 0, sampled: false } };
  let requests = 0, releaseFirst, releaseSecond;
  const first = new Promise(resolve => { releaseFirst = resolve; });
  const second = new Promise(resolve => { releaseSecond = resolve; });
  await page.route(/\/api\/track\?/, async route => {
    const number = ++requests;
    if (number === 1) await first;
    if (number === 2) await second;
    await route.fulfill({ json: empty }).catch(() => {});
  });
  try {
    await page.goto('/map');
    await expect.poll(() => requests).toBe(1);
    const now = Math.floor(Date.now() / 1000);
    for (let i = 0; i < 2; i++) {
      await ingest(page, `coalesce-${testInfo.project.name}`, now + i);
      await page.waitForTimeout(750);
    }
    expect(requests).toBe(1);
    releaseFirst();
    await expect(page.locator('#info')).toHaveText('0 points');
    await expect.poll(() => requests).toBe(2);
    await expect.poll(() => page.evaluate(() => {
      let label;
      window.mytrackerMap.eachLayer(layer => {
        const content = layer.getTooltip?.()?.getContent();
        if (content instanceof HTMLElement) label = { text: content.textContent, children: content.children.length };
      });
      return label;
    })).toEqual({ text: name, children: 0 });
    if (testInfo.project.name === 'mobile') await page.getByRole('button', { name: 'Toggle filters' }).click();
    await page.getByLabel('Period').selectOption('168');
    await expect.poll(() => requests).toBe(3);
    await expect(page.locator('#map-state')).toContainText('No locations match these filters');
  } finally {
    releaseFirst();
    releaseSecond();
  }
});

test('map refresh preserves viewport and filters', async ({ page }, testInfo) => {
  const pageErrors = [];
  page.on('pageerror', error => pageErrors.push(error.message));
  const now = Math.floor(Date.now() / 1000);
  await ingest(page, `seed-a-${testInfo.project.name}`, now, 37.5, 127);
  await ingest(page, `seed-b-${testInfo.project.name}`, now, 35.1, 129.1);
  let initialTracks = 0;
  page.on('request', request => {
    if (new URL(request.url()).pathname === '/api/track') initialTracks++;
  });
  let delayInitialTrack = true;
  await page.route(/\/api\/track\?/, async route => {
    if (delayInitialTrack) {
      delayInitialTrack = false;
      await new Promise(resolve => setTimeout(resolve, 400));
    }
    await route.continue();
  });

  await page.getByRole('link', { name: 'Map', exact: true }).click();
  await expect(page.locator('#map')).toBeVisible();
  if (testInfo.project.name === 'mobile') await page.getByRole('button', { name: 'Toggle filters' }).click();
  await expect(page.getByLabel('Point limit')).toBeVisible();
  await expect(page.locator('#info')).toContainText('point');
  await expect.poll(() => initialTracks).toBe(1);
  await page.unroute(/\/api\/track\?/);
  await expect.poll(() => page.evaluate(() => window.mytrackerMap.getZoom())).toBeLessThan(10);

  await page.evaluate(() => window.mytrackerMap.setView([0, 0], 2));
  await page.getByRole('button', { name: 'Fit track', exact: true }).click();
  await expect.poll(() => page.evaluate(() => window.mytrackerMap.getZoom())).toBeGreaterThan(2);

  const before = await page.evaluate(() => ({ center: window.mytrackerMap.getCenter(), zoom: window.mytrackerMap.getZoom() }));
  const autoRefresh = page.waitForResponse(response => new URL(response.url()).pathname === '/api/track');
  await ingest(page, `map-${testInfo.project.name}`, Math.floor(Date.now() / 1000) + 1, 35.1, 129.1);
  await autoRefresh;
  await expect(page.locator('#device')).toContainText(`map-${testInfo.project.name}`);
  const after = await page.evaluate(() => ({ center: window.mytrackerMap.getCenter(), zoom: window.mytrackerMap.getZoom() }));
  expect(after).toEqual(before);

  let releaseRefresh;
  let holdRefresh = true;
  const refreshGate = new Promise(resolve => { releaseRefresh = resolve; });
  await page.route(/\/api\/track\?/, async route => {
    if (holdRefresh) await refreshGate;
    await route.continue();
  });
  const pendingRefresh = page.waitForResponse(response => new URL(response.url()).pathname === '/api/track');
  await ingest(page, `map-fit-${testInfo.project.name}`, Math.floor(Date.now() / 1000) + 2, 34.1, 126.1);
  await expect(page.locator('#map-state')).toContainText('Refreshing track');
  await page.evaluate(() => window.mytrackerMap.setView([0, 0], 2));
  await page.getByRole('button', { name: 'Fit track', exact: true }).click();
  holdRefresh = false;
  releaseRefresh();
  await pendingRefresh;
  await expect.poll(() => page.evaluate(() => window.mytrackerMap.getZoom())).toBeGreaterThan(2);
  await page.unroute(/\/api\/track\?/);

  const proofPoint = page.locator('#map [role="button"][aria-label^="Location"]').first();
  await expect(proofPoint).toBeVisible();
  await proofPoint.focus();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('button', { name: 'Proof details', exact: true })).toBeVisible();

  const allTrack = page.waitForResponse(response => {
    const url = new URL(response.url());
    return url.pathname === '/api/track' && !url.searchParams.has('hours');
  });
  const allTrackRequest = page.waitForRequest(request => {
    const url = new URL(request.url());
    return url.pathname === '/api/track' && !url.searchParams.has('hours');
  });
  await page.getByLabel('Period').selectOption('0');
  const [allTrackResponse] = await Promise.all([allTrack, allTrackRequest]);
  expect(allTrackResponse.status()).toBe(200);
  await expect(page).toHaveURL(/[?&]hours=0(?:&|$)/);
  await page.reload();
  await expect(page.getByLabel('Period')).toHaveValue('0');
  await page.goto('/map?hours=48&limit=20000');
  if (testInfo.project.name === 'mobile') await page.getByRole('button', { name: 'Toggle filters' }).click();
  await expect(page.getByLabel('Period')).toHaveValue('48');
  await expect(page.getByLabel('Point limit')).toHaveValue('20000');
  await accessible(page);
  expect(pageErrors).toEqual([]);
});

test('admin and shared tracks fit across the date line', async ({ page, context }) => {
  const coordinates = [[179.9, -17], [-179.9, -17]];
  const track = { type: 'FeatureCollection', features: [
    { type: 'Feature', geometry: { type: 'LineString', coordinates }, properties: { kind: 'track' } },
    ...coordinates.map((coordinate, index) => ({ type: 'Feature', geometry: { type: 'Point', coordinates: coordinate },
      properties: { kind: 'point', id: index + 1, tst: 1, user: 'fixture', device: 'date-line', tsa_status: 'pending', ots_status: 'pending' } }))
  ], metadata: { total_points: 2, returned_points: 2, sampled: false } };
  await page.route(/\/api\/track\?/, route => route.fulfill({ json: track }));
  await page.goto('/map');
  await expect(page.locator('#info')).toContainText('2');
  await expect.poll(() => page.evaluate(() => window.mytrackerMap.getZoom())).toBeGreaterThan(8);
  expect(await page.evaluate(() => Math.abs(window.mytrackerMap.getCenter().lng))).toBeCloseTo(180, 1);
  await page.goto('/settings#privacy');
  const token = await page.locator('input[name="csrf"]').first().inputValue();
  const created = await page.request.post('/api/shares', {
    headers: { 'X-CSRF-Token': token },
    form: { csrf: token, label: 'date-line fixture', hours: '24', expires_days: '7', delay_minutes: '0', precision_m: '100' }
  });
  expect(created.ok()).toBeTruthy();
  const { url } = await created.json();
  const shared = await context.newPage();
  await shared.route(/\/share\/[^/]+\/api\/track/, route => route.fulfill({ json: track }));
  await shared.goto(url);
  await expect.poll(() => shared.evaluate(() => window.mytrackerMap.getZoom())).toBeGreaterThan(8);
  expect(await shared.evaluate(() => Math.abs(window.mytrackerMap.getCenter().lng))).toBeCloseTo(180, 1);
  await expect(shared.locator('#map [role="button"][aria-label^="Location"]')).toHaveCount(2);
  await shared.close();
});

test('timeline loads derived summaries without client errors', async ({ page }, testInfo) => {
  const pageErrors = [];
  page.on('pageerror', error => pageErrors.push(error.message));
  const now = Math.floor(Date.now() / 1000);
  await ingest(page, `timeline-${testInfo.project.name}`, now - 700, 37.5, 127);
  await ingest(page, `timeline-${testInfo.project.name}`, now, 37.5001, 127.0001);
  await page.route('**/api/timeline?*', route => route.fulfill({ json: {
    summary: { trips: 0, stays: 0, distance_m: 0, stay_seconds: 0, total_segments: 0 }, segments: null
  } }));
  await page.getByRole('link', { name: 'Timeline' }).click();
  await expect(page.getByRole('heading', { name: 'Timeline' })).toBeVisible();
  await expect(page.locator('body')).toHaveClass(/app-shell/);
  await expect(page.locator('#timeline-summary')).toContainText('Total segments');
  await expect(page.locator('#timeline-list')).toContainText('No timeline segments in this period.');
  await expect(page.locator('#timeline-list')).not.toContainText('mytrackerUI.api');
  await accessible(page);
  await page.unroute('**/api/timeline?*');
  await page.reload();
  await expect(page.locator('#timeline-summary')).toContainText('Total segments');
  await expect(page.locator('#timeline-list .timeline-row').first()).toBeVisible({ timeout: 20000 });
  await accessible(page);
  expect(pageErrors).toEqual([]);
});

test('settings and share workflows remain accessible', async ({ page, context }, testInfo) => {
  const pageErrors = [];
  page.on('pageerror', error => pageErrors.push(error.message));
  const shareDevice = `share-${testInfo.project.name}-${Date.now()}`;
  await ingest(page, shareDevice, Math.floor(Date.now() / 1000) - 60, 37.5, 127);
  await ingest(page, shareDevice, Math.floor(Date.now() / 1000), 35.1, 129.1);
  await page.getByRole('link', { name: 'Settings' }).click();
  await expect(page.getByRole('heading', { name: 'Settings' })).toBeVisible();
  await expect(page.locator('#export-link')).toHaveAttribute('href', '/api/export?hours=24');
  await page.evaluate(() => {
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async () => { throw new Error('denied'); } } });
    Object.defineProperty(document, 'execCommand', { configurable: true, value: () => false });
  });
  await page.getByRole('button', { name: 'Reveal and copy' }).click();
  await expect(page.locator('#owntracks-url')).not.toHaveText('Hidden');
  await expect(page.locator('.toast.warning')).toContainText('Copy it manually');
  await expect(page.locator('[data-connection]')).toHaveText('Connected');
  await expect(page.locator('.connection-line')).toHaveAttribute('data-state', 'connected');
  const tracking = page.getByRole('tab', { name: 'Tracking' });
  await tracking.focus();
  await page.keyboard.press('End');
  await expect(page.getByRole('tab', { name: 'Operations' })).toHaveAttribute('aria-selected', 'true');
  await expect(page).toHaveURL(/#operations$/);

  await page.getByRole('tab', { name: 'Proofs' }).click();
  const tsaURL = page.getByLabel('TSA URL');
  const savedTSAURL = await tsaURL.inputValue();
  await tsaURL.fill('');
  await page.getByRole('tab', { name: 'Operations' }).click();
  await page.getByRole('button', { name: 'Save settings' }).click();
  await expect(page.getByRole('tab', { name: 'Proofs' })).toHaveAttribute('aria-selected', 'true');
  await tsaURL.fill(savedTSAURL);
  await page.getByRole('tab', { name: 'Operations' }).click();
  await page.getByLabel('Backup interval').fill('not-a-duration');
  await page.getByRole('tab', { name: 'Tracking' }).click();
  await page.locator('#pub-pass').fill('must-not-be-echoed');
  await page.locator('#geofence-lat').fill('91');
  await page.getByRole('tab', { name: 'Operations' }).click();
  await page.getByRole('button', { name: 'Save settings' }).click();
  await expect(page.getByLabel('Backup interval')).toHaveValue('not-a-duration');
  await expect(page.locator('#dirty-state')).toHaveText('Unsaved changes');
  await expect(page.locator('#pub-pass')).toHaveValue('');
  await expect(page.getByRole('tab', { name: 'Operations' })).toHaveAttribute('aria-selected', 'true');
  await page.getByLabel('Backup interval').fill('24h');
  await expect(page.locator('#dirty-state')).toHaveText('Unsaved changes');
  await page.getByRole('button', { name: 'Save settings' }).click();
  await expect(page).toHaveURL(/\/settings\?saved=1#operations$/);
  await expect(page.locator('[data-connection]')).toHaveText('Connected');
  await page.route(/\/api\/import$/, route => route.fulfill({
    status: 202,
    contentType: 'application/json',
    body: JSON.stringify({ imported: 2, skipped: 0, proofs: 2, cleanup_pending: true })
  }));
  await page.locator('#import-file').setInputFiles({ name: 'export.zip', mimeType: 'application/zip', buffer: Buffer.from('fixture') });
  await page.getByRole('button', { name: 'Import', exact: true }).click();
  await expect(page.locator('.toast.warning')).toContainText('cleanup pending');
  await page.unroute(/\/api\/import$/);
  await page.getByRole('tab', { name: 'Proofs' }).click();
  await expect(page.getByLabel('Esplora API URLs, one per line')).toHaveValue(/blockstream\.info/);

  await page.getByRole('tab', { name: 'Tracking' }).click();
  const geofenceName = `<b>Home-${testInfo.project.name}-${Date.now()}</b>`;
  await page.locator('#geofence-name').fill(geofenceName);
  await page.locator('#geofence-lat').fill('37.5');
  await page.locator('#geofence-lon').fill('127');
  await page.getByRole('button', { name: 'Add geofence' }).click();
  const geofenceRow = page.locator('#geofence-list .data-row').filter({ hasText: geofenceName });
  await expect(geofenceRow).toBeVisible();
  await expect(page.locator('#geofence-list b')).toHaveCount(0);
  let toggleRequest;
  let releaseToggle;
  const toggleReady = new Promise(resolve => { toggleRequest = resolve; });
  const toggleHold = new Promise(resolve => { releaseToggle = resolve; });
  await page.route(/\/api\/geofences\/\d+$/, async route => {
    if (route.request().method() === 'PATCH') {
      toggleRequest(route.request().postDataJSON());
      await toggleHold;
    }
    await route.continue();
  });
  await geofenceRow.getByRole('checkbox').uncheck();
  expect(await toggleReady).toEqual({ enabled: false });
  const fences = await (await page.request.get('/api/geofences')).json();
  const fence = fences.find(value => value.name === geofenceName);
  const csrf = await page.locator('input[name="csrf"]').first().inputValue();
  const editedName = `${geofenceName}-edited`;
  const edited = await page.request.patch(`/api/geofences/${fence.id}`, {
    headers: { 'X-CSRF-Token': csrf }, data: { ...fence, name: editedName }
  });
  expect(edited.ok()).toBeTruthy();
  releaseToggle();
  await expect(geofenceRow).toContainText(editedName);
  await expect(geofenceRow.getByRole('checkbox')).not.toBeChecked();
  await page.unroute(/\/api\/geofences\/\d+$/);
  await page.route(/\/api\/geofences\/\d+$/, route => route.request().method() === 'DELETE'
    ? route.fulfill({ status: 500, contentType: 'application/json', body: JSON.stringify({ error: { message: 'simulated geofence delete failure' } }) })
    : route.continue());
  await geofenceRow.getByRole('button', { name: 'Delete' }).click();
  await page.locator('#ui-confirm-action').click();
  await expect(page.locator('.toast.error').filter({ hasText: 'simulated geofence delete failure' })).toBeVisible();
  await page.unroute(/\/api\/geofences\/\d+$/);

  const label = `smoke-${testInfo.project.name}-${Date.now()}`;
  await page.getByRole('tab', { name: 'Privacy' }).click();
  await page.locator('#share-device').selectOption(shareDevice);
  await page.locator('#share-label').fill(label);
  await page.locator('#share-delay').selectOption('0');
  await page.locator('#share-precision').selectOption('500');
  await page.getByRole('button', { name: 'Create link' }).click();
  const secret = page.getByRole('dialog', { name: 'New share URL' });
  await expect(secret).toBeVisible();
  const textareaCount = await page.locator('textarea').count();
  const copyFallback = await page.evaluate(async () => {
    let insideDialog = false;
    Object.defineProperty(document, 'execCommand', { configurable: true, value: () => {
      insideDialog = Boolean(document.querySelector('dialog[open] textarea'));
      throw new Error('simulated copy failure');
    } });
    const result = await window.mytrackerUI.copy('fallback-check');
    return { result, insideDialog };
  });
  expect(copyFallback).toEqual({ result: false, insideDialog: true });
  await expect(page.locator('textarea')).toHaveCount(textareaCount);
  const shareURL = await secret.getByLabel('One-time URL').inputValue();
  expect(shareURL).toContain('/share/');
  await page.waitForTimeout(300);
  await expect(secret.getByLabel('One-time URL')).toHaveValue(shareURL);
  await secret.getByRole('button', { name: 'Done' }).click();
  await expect(secret).toBeHidden();
  await expect(page.getByRole('cell', { name: label })).toBeVisible();
  await expect(page.locator('#share-list')).not.toContainText(shareURL);
  await accessible(page);
  await page.screenshot({ path: testInfo.outputPath('settings.png'), fullPage: true });

  const shared = await context.newPage();
  const sharedErrors = [];
  let shareTracks = 0;
  shared.on('pageerror', error => sharedErrors.push(error.message));
  shared.on('request', request => {
    if (new URL(request.url()).pathname.endsWith('/api/track')) shareTracks++;
  });
  await shared.goto(shareURL);
  await expect(shared.locator('[data-connection]')).toHaveText('Connected');
  await expect(shared.getByText('Last updated')).toBeVisible();
  await expect(shared.locator('#map')).toBeVisible();
  await expect.poll(() => shareTracks).toBe(1);
  const sharedPoints = shared.locator('#map [role="button"][aria-label^="Location"]');
  await expect(sharedPoints).toHaveCount(2);
  await shared.evaluate(() => window.mytrackerMap.setView([0, 0], 2));
  await shared.getByRole('button', { name: 'Fit track', exact: true }).click();
  await expect.poll(() => shared.evaluate(() => window.mytrackerMap.getZoom())).toBeGreaterThan(2);
  expect(shareTracks).toBe(1);

  let releaseShareRefresh;
  let holdShareRefresh = true;
  const shareRefreshGate = new Promise(resolve => { releaseShareRefresh = resolve; });
  await shared.route(/\/share\/[^/]+\/api\/track$/, async route => {
    if (holdShareRefresh) await shareRefreshGate;
    await route.continue();
  });
  const shareRefresh = shared.waitForResponse(response => new URL(response.url()).pathname.endsWith('/api/track'));
  await ingest(page, shareDevice, Math.floor(Date.now() / 1000) - 120, 34.1, 126.1);
  await expect(shared.locator('#map-state')).toContainText('Refreshing');
  await expect.poll(() => shareTracks).toBe(2);
  await shared.evaluate(() => window.mytrackerMap.setView([0, 0], 2));
  await shared.getByRole('button', { name: 'Fit track', exact: true }).click();
  holdShareRefresh = false;
  releaseShareRefresh();
  await shareRefresh;
  await expect.poll(() => shared.evaluate(() => window.mytrackerMap.getZoom())).toBeGreaterThan(2);
  await shared.unroute(/\/share\/[^/]+\/api\/track$/);

  const popup = shared.locator('.leaflet-popup-content');
  await sharedPoints.first().focus();
  await shared.keyboard.press('Enter');
  await expect(popup).toHaveCount(1);
  await expect(popup.locator('strong')).toHaveText('Location');
  await shared.locator('.leaflet-popup-close-button').click();
  await expect(popup).toHaveCount(0);
  await sharedPoints.last().focus();
  await shared.keyboard.press('Enter');
  await expect(popup).toHaveCount(1);
  await expect(popup.locator('strong')).toHaveText('Current');
  await accessible(shared);
  await shared.screenshot({ path: testInfo.outputPath('share.png') });
  expect(pageErrors).toEqual([]);
  expect(sharedErrors).toEqual([]);
});

test('expired share links do not offer rotation', async ({ page }) => {
  await page.route('**/api/shares', route => route.fulfill({ json: [
    { id: 1, label: 'Expired', filter: 'all', hours: 24, expires: 'Expired', active: false },
    { id: 2, label: 'Active', filter: 'all', hours: 24, expires: 'Tomorrow', active: true }
  ] }));
  await page.goto('/settings#privacy');
  const rows = page.locator('#share-list tr');
  await expect(rows.filter({ hasText: 'Expired' }).getByRole('button', { name: 'Rotate' })).toHaveCount(0);
  await expect(rows.filter({ hasText: 'Active' }).getByRole('button', { name: 'Rotate' })).toHaveCount(1);
});

test('withButton restores focus within a replaced list row', async ({ page }) => {
  const focused = await page.evaluate(async () => {
    const section = document.createElement('section');
    const command = section.appendChild(document.createElement('button'));
    command.textContent = 'Retry section';
    const table = document.createElement('table');
    const body = table.createTBody();
    const row = body.insertRow();
    row.insertCell().textContent = 'old';
    const cell = row.insertCell();
    const button = document.createElement('button');
    button.textContent = 'Run';
    cell.append(button);
    section.append(table);
    document.body.append(section);
    button.focus();
    await window.mytrackerUI.withButton(button, async () => {
      row.remove();
      const replacement = body.insertRow();
      replacement.insertCell().textContent = 'new';
      const action = replacement.insertCell().appendChild(document.createElement('button'));
      action.textContent = 'Next';
    });
    const replacementFocus = document.activeElement?.textContent;
    await window.mytrackerUI.withButton(document.activeElement, async () => {
      body.replaceChildren();
      body.insertRow().insertCell().textContent = 'No items';
    });
    return [replacementFocus, document.activeElement?.textContent];
  });
  expect(focused).toEqual(['Next', 'Retry section']);
});

test('stale settings tabs are rejected without losing the draft', async ({ browser }) => {
  const context = await browser.newContext();
  const page = await context.newPage();
  const stale = await context.newPage();
  await signIn(page);
  await page.goto('/settings#proofs');
  await stale.goto('/settings#proofs');
  const originalTSA = await stale.getByLabel('TSA URL').inputValue();
  const originalBackup = await stale.getByLabel('Backup interval').inputValue();

  await page.getByLabel('TSA URL').fill(`https://tsa-${Date.now()}.example/tsr`);
  await page.getByRole('button', { name: 'Save settings' }).click();
  await expect(page).toHaveURL(/\/settings\?saved=1#proofs$/);
  const savedTSA = await page.getByLabel('TSA URL').inputValue();

  await stale.getByRole('tab', { name: 'Operations' }).click();
  await stale.getByLabel('Backup interval').fill('48h');
  const response = await Promise.all([
    stale.waitForResponse(result => result.request().method() === 'POST' && result.url().endsWith('/api/settings')),
    stale.getByRole('button', { name: 'Save settings' }).click()
  ]).then(([result]) => result);
  expect(response.status()).toBe(409);
  await expect(stale).toHaveURL(/\/settings#operations$/);
  await expect(stale.getByLabel('Backup interval')).toHaveValue('48h');
  await expect(stale.locator('.notice.error')).toContainText('changed in another tab');

  const fresh = await context.newPage();
  await fresh.goto('/settings#proofs');
  await expect(fresh.getByLabel('TSA URL')).toHaveValue(savedTSA);
  await fresh.getByRole('tab', { name: 'Operations' }).click();
  await expect(fresh.getByLabel('Backup interval')).toHaveValue(originalBackup);
  await fresh.getByRole('tab', { name: 'Proofs' }).click();
  await fresh.getByLabel('TSA URL').fill(originalTSA);
  const restored = await Promise.all([
    fresh.waitForResponse(result => result.request().method() === 'POST' && result.url().endsWith('/api/settings')),
    fresh.getByRole('button', { name: 'Save settings' }).click()
  ]).then(([result]) => result);
  expect(restored.status()).toBe(200);
  await expect(fresh).toHaveURL(/\/settings\?saved=1#proofs$/);
  await fresh.close();
  await stale.close();
  await context.close();
});

test('settings form stays locked until a pending save navigates', async ({ page }) => {
  await page.goto('/settings#proofs');
  const tsa = page.getByLabel('TSA URL');
  const original = await tsa.inputValue();
  const submittedTSA = `https://saving-${Date.now()}.invalid/tsa`;
  await tsa.fill(submittedTSA);
  let release;
  const gate = new Promise(resolve => { release = resolve; });
  let submitted;
  await page.route('**/api/settings', async route => {
    submitted = new URLSearchParams(route.request().postData());
    await gate;
    await route.continue();
  });
  try {
    await page.getByRole('button', { name: 'Save settings' }).click();
    await expect.poll(() => submitted?.get('tsa_url')).toBe(submittedTSA);
    expect(submitted.get('csrf')).toBeTruthy();
    expect(submitted.get('settings_revision')).toBeTruthy();
    await expect(tsa).toBeDisabled();
    await expect(page.getByLabel('Trust roots')).toBeDisabled();
    await expect(page.getByRole('button', { name: 'Save settings' })).toBeDisabled();
    await page.getByRole('tab', { name: 'Tracking' }).click();
    await expect(page.locator('#pub-pass')).toBeDisabled();
    release();
    await expect(page).toHaveURL(/\/settings\?saved=1#proofs$/);
    await expect(tsa).toHaveValue(submittedTSA);
    await expect(tsa).toBeEnabled();
  } finally {
    release();
    await page.unroute('**/api/settings');
  }
  await tsa.fill(original);
  const restored = await Promise.all([
    page.waitForResponse(response => response.request().method() === 'POST' && response.url().endsWith('/api/settings')),
    page.getByRole('button', { name: 'Save settings' }).click()
  ]).then(([response]) => response);
  expect(restored.status()).toBe(200);
  await expect(page).toHaveURL(/\/settings\?saved=1#proofs$/);
  await expect(tsa).toHaveValue(original);
  await expect(tsa).toBeEnabled();
});

test('failed settings saves unlock controls and preserve disabled fields', async ({ page }) => {
  await page.goto('/settings#proofs');
  const tsa = page.getByLabel('TSA URL');
  const original = await tsa.inputValue();
  await tsa.fill('https://unsaved.invalid/tsa');
  await page.locator('#webhook-secret').evaluate(input => { input.disabled = true; });
  await page.route('**/api/settings', route => route.fulfill({
    status: 500, json: { error: { code: 'internal_error', message: 'Save unavailable' } }
  }));
  try {
    await page.getByRole('button', { name: 'Save settings' }).click();
    await expect(page.locator('.notice.error')).toContainText('Save unavailable');
    await expect(tsa).toBeEnabled();
    await expect(tsa).toHaveValue('https://unsaved.invalid/tsa');
    await expect(page.getByRole('button', { name: 'Save settings' })).toBeEnabled();
    await expect(page.locator('#webhook-secret')).toBeDisabled();
  } finally { await page.unroute('**/api/settings'); }
  await tsa.fill(original);
});

test('settings save keeps drafts when the session is revoked in flight', async ({ browser }) => {
  const ownerContext = await browser.newContext();
  const controlContext = await browser.newContext();
  const owner = await ownerContext.newPage();
  const control = await controlContext.newPage();
  try {
    await signIn(owner);
    await signIn(control);
    await owner.goto('/settings#proofs');
    const originalTSA = await owner.getByLabel('TSA URL').inputValue();
    const draftTSA = `https://draft-${Date.now()}.invalid/tsa`;
    await owner.getByLabel('TSA URL').fill(draftTSA);
    await owner.getByRole('tab', { name: 'Tracking' }).click();
    await owner.locator('#pub-pass').fill('ephemeral-secret');
    await owner.getByRole('tab', { name: 'Proofs' }).click();
    let revoked = false;
    await owner.route('**/api/settings', async route => {
      if (!revoked && route.request().method() === 'POST') {
        revoked = true;
        await control.goto('/settings#privacy');
        const csrf = await control.locator('input[name="csrf"]').first().inputValue();
        const response = await control.request.post('/api/sessions/revoke-others', {
          headers: { 'X-CSRF-Token': csrf }, form: { csrf }
        });
        expect(response.status()).toBe(200);
      }
      await route.continue();
    });
    const rejected = await Promise.all([
      owner.waitForResponse(response => response.request().method() === 'POST' && response.url().endsWith('/api/settings')),
      owner.getByRole('button', { name: 'Save settings' }).click()
    ]).then(([response]) => response);
    expect(rejected.status()).toBe(401);
    await expect(owner).toHaveURL(/\/settings#proofs$/);
    await expect(owner.getByLabel('TSA URL')).toHaveValue(draftTSA);
    await expect(owner.locator('#pub-pass')).toHaveValue('');
    await expect(owner.locator('#session-notice')).toBeVisible();
    await expect(owner.locator('#dirty-state')).toHaveText('Unsaved changes');
    expect(await owner.evaluate(() => window.mytrackerUI.isPaused())).toBe(true);

    const login = await ownerContext.newPage();
    await signIn(login);
    await owner.getByRole('button', { name: 'Resume', exact: true }).click();
    await expect(owner.locator('#session-notice')).toBeHidden();
    await expect(owner.getByLabel('TSA URL')).toHaveValue(draftTSA);
    await owner.getByLabel('TSA URL').fill(originalTSA);
    const restored = await Promise.all([
      owner.waitForResponse(response => response.request().method() === 'POST' && response.url().endsWith('/api/settings')),
      owner.getByRole('button', { name: 'Save settings' }).click()
    ]).then(([response]) => response);
    expect(restored.status()).toBe(200);
    await expect(owner).toHaveURL(/\/settings\?saved=1#proofs$/);
    await expect(owner.getByLabel('TSA URL')).toHaveValue(originalTSA);
    await login.close();
  } finally {
    await ownerContext.close();
    await controlContext.close();
  }
});

test('revoked sessions keep export settings context and pause auth', async ({ browser }) => {
  for (const selector of ['#export-link', '#gpx-export']) {
    const ownerContext = await browser.newContext();
    const owner = await ownerContext.newPage();
    const controlContext = await browser.newContext();
    const control = await controlContext.newPage();
    try {
      await signIn(owner);
      await signIn(control);
      await owner.goto('/settings#operations');
      let exportFailed = false;
      await owner.route('**/api/**', route => {
        const path = new URL(route.request().url()).pathname;
        if (path === '/api/export' || path === '/api/export/gpx' || (path === '/api/sessions' && exportFailed)) return route.continue();
        return route.abort();
      });
      await control.goto('/settings#privacy');
      const csrf = await control.locator('input[name="csrf"]').first().inputValue();
      const revoked = await control.request.post('/api/sessions/revoke-others', {
        headers: { 'X-CSRF-Token': csrf }, form: { csrf }
      });
      expect(revoked.status()).toBe(200);
      const exportPath = selector === '#export-link' ? '/api/export' : '/api/export/gpx';
      const failedDownload = owner.waitForResponse(result => new URL(result.url()).pathname === exportPath && result.status() === 401);
      await owner.locator(selector).click();
      expect((await failedDownload).status()).toBe(401);
      exportFailed = true;
      await expect(owner).toHaveURL(/\/settings#operations$/);
      await expect(owner.getByRole('heading', { name: 'Settings' })).toBeVisible();
      await expect(owner.locator('#session-notice')).toBeVisible();
      await expect(owner.locator('.toast.error')).toContainText('Sign in to resume');
      expect(await owner.evaluate(() => window.mytrackerUI.isPaused())).toBe(true);
      await expectNoDownloadArtifacts(owner);
    } finally {
      await ownerContext.close();
      await controlContext.close();
    }
  }
});

test('export and GPX links produce native attachment downloads', async ({ page }, testInfo) => {
  const device = `download-${testInfo.project.name}-${Date.now()}`;
  await ingest(page, device);
  await page.goto('/settings#operations');

  const zipDownload = await Promise.all([
    page.waitForEvent('download'),
    page.locator('#export-link').click()
  ]).then(([download]) => download);
  expect(zipDownload.suggestedFilename()).toMatch(/^mytracker-export-\d{8}-\d{6}\.zip$/);
  expect(await zipDownload.path()).toBeTruthy();
  await expectNoDownloadArtifacts(page);

  await page.locator('#gpx-user').fill('owntracks');
  await page.locator('#gpx-device').fill(device);
  const gpxDownload = await Promise.all([
    page.waitForEvent('download'),
    page.locator('#gpx-export').click()
  ]).then(([download]) => download);
  expect(gpxDownload.suggestedFilename()).toBe('mytracker.gpx');
  expect(await gpxDownload.path()).toBeTruthy();
  await expectNoDownloadArtifacts(page);
  await expect(page).toHaveURL(/\/settings#operations$/);
});

test('native attachment downloads do not cancel an overlapping request', async ({ page }, testInfo) => {
  const device = `overlap-${testInfo.project.name}-${Date.now()}`;
  await ingest(page, device);
  await page.goto('/settings#operations');

  let zipStarted = false;
  let releaseZip;
  const zipGate = new Promise(resolve => { releaseZip = resolve; });
  await page.route(/\/api\/export\?/, async route => {
    if (!zipStarted) {
      zipStarted = true;
      await zipGate;
    }
    await route.continue();
  });

  const zipDownload = page.waitForEvent('download', download => download.suggestedFilename().endsWith('.zip'));
  const zipClick = page.locator('#export-link').click();
  try {
    await expect.poll(() => zipStarted).toBe(true);
    await page.locator('#gpx-user').fill('owntracks');
    await page.locator('#gpx-device').fill(device);
    const gpxDownload = await Promise.all([
      page.waitForEvent('download', download => download.suggestedFilename() === 'mytracker.gpx'),
      page.locator('#gpx-export').click()
    ]).then(([download]) => download);
    expect(gpxDownload.suggestedFilename()).toBe('mytracker.gpx');
    expect(await gpxDownload.path()).toBeTruthy();
    releaseZip();
    const completedZip = await zipDownload;
    expect(completedZip.suggestedFilename()).toMatch(/^mytracker-export-\d{8}-\d{6}\.zip$/);
    expect(await completedZip.path()).toBeTruthy();
    await zipClick;
    await expectNoDownloadArtifacts(page);
  } finally {
    releaseZip();
    await page.unroute(/\/api\/export\?/);
  }
});
