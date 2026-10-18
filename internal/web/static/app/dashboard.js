(function () {
  const cfg = JSON.parse(document.getElementById('cfg').textContent);
  const ui = window.mytrackerUI;
  const csrfHeaders = { 'X-CSRF-Token': cfg.csrf, 'Content-Type': 'application/json' };
  const requests = new Map();
  const executions = new Map();
  const automaticPending = new Set();
  const cache = new Map();
  const pendingJobs = new Map();
  const stampLocations = new Set();
  const stampLocationGenerations = new Map();
  const stampJobLimit = 200;
  const bulkRetryTrackLimit = 50;
  const bulkRetryPollLimit = 5;
  const bulkRetryConcurrency = 2;
  let bulkRetryState = null;
  let bulkRetryTimer = null;
  let statusLoaded = false;
  let statusStale = false;
  let stopped = false;

  async function latest(key, url) {
    automaticPending.delete(key);
    requests.get(key)?.abort();
    const controller = new AbortController();
    requests.set(key, controller);
    try {
      return await ui.request(url, { signal: controller.signal });
    } finally {
      if (requests.get(key) === controller) requests.delete(key);
    }
  }

  const text = (id, value) => {
    const node = document.getElementById(id);
    if (node) node.textContent = value ?? '0';
  };
  const time = value => value ? new Date(value * 1000).toLocaleString() : 'Never';
  const cell = (row, label, value, className = '') => {
    const td = row.insertCell();
    td.dataset.label = label;
    td.textContent = value ?? '';
    if (className) td.className = className;
    return td;
  };
  const actionButton = (icon, label, run) => {
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'icon-text-button small';
    button.innerHTML = `<i data-lucide="${icon}"></i><span>${label}</span>`;
    button.onclick = () => ui.withButton(button, () => run(button), label).catch(error => {
      if (!ui.isCanceled?.(error) && error.name !== 'AbortError') ui.toast(error.message, 'error');
    });
    ui.icons?.(button);
    return button;
  };

  function empty(body, colspan, message, retry) {
    body.replaceChildren();
    body.dataset.state = message === 'Loading' ? 'loading' : 'empty';
    const row = body.insertRow();
    const td = row.insertCell();
    td.colSpan = colspan;
    td.className = 'empty';
    td.textContent = message;
    if (retry) td.append(' ', actionButton('refresh-cw', 'Retry', retry));
  }

  function resourceNotice(body, state, message, retry) {
    const host = body.closest('.table-wrap') || body.parentElement;
    if (!host) return;
    let notice = host.querySelector(':scope > .resource-state');
    if (!notice) {
      notice = document.createElement('div');
      notice.className = 'resource-state';
      host.prepend(notice);
    }
    notice.dataset.state = state;
    notice.replaceChildren();
    notice.append(document.createTextNode(message));
    if (retry) notice.append(' ', actionButton('refresh-cw', 'Retry', retry));
  }

  function resourceError(body, colspan, error, retry, stale) {
    body.dataset.state = stale ? 'stale' : 'error';
    if (stale) {
      body.parentElement?.classList.add('is-stale');
      const stamp = cache.get(`${body.id}:updated`);
      resourceNotice(body, 'stale', `Stale data${stamp ? `; last updated ${time(stamp)}` : ''}`, retry);
      return;
    }
    body.replaceChildren();
    const row = body.insertRow();
    const td = row.insertCell();
    td.colSpan = colspan;
    td.className = 'error-state';
    td.append(document.createTextNode(error?.message || 'Unable to load data.'));
    td.append(' ', actionButton('refresh-cw', 'Retry', retry));
    resourceNotice(body, 'error', error?.message || 'Unable to load data.', retry);
  }

  async function resource(key, url, bodyID, colspan, render, extract = result => result?.data) {
    const body = document.getElementById(bodyID);
    if (!body) return;
    const hadData = cache.has(key);
    if (!hadData) {
      empty(body, colspan, 'Loading');
      resourceNotice(body, 'loading', 'Loading data');
    }
    try {
      const result = await latest(key, url);
      const data = extract(result);
      cache.set(key, data);
      cache.set(`${bodyID}:updated`, Math.floor(Date.now() / 1000));
      body.dataset.state = 'ready';
      body.parentElement?.classList.remove('is-stale');
      resourceNotice(body, 'ready', `Last updated ${time(cache.get(`${bodyID}:updated`))}`);
      render(data);
      return data;
    } catch (error) {
      if (ui.isCanceled?.(error) || error.name === 'AbortError') return;
      resourceError(body, colspan, error, () => run(key, () => resource(key, url, bodyID, colspan, render, extract)), hadData);
      if (!hadData) ui.toast(error.message, 'error');
    }
  }

  function renderProviders(providers = []) {
    const body = document.getElementById('proof-providers');
    if (!providers?.length) return empty(body, 6, 'No provider checks yet');
    const evidence = (status, legacy) => {
      if (status === 'valid' || (!status && legacy === true)) return 'Valid';
      if (status === 'invalid') return 'Invalid';
      return 'Not checked';
    };
    body.replaceChildren();
    providers.forEach(provider => {
      const row = body.insertRow();
      [provider.provider_id, provider.result, evidence(provider.pow_status, provider.pow_valid), provider.confirmations || 0,
        `${provider.duration_ms} ms`, time(provider.checked_at)].forEach(value => row.insertCell().textContent = value);
    });
  }

  function renderMaintenanceJobs(jobs = []) {
    const body = document.getElementById('maintenance-jobs');
    if (!jobs?.length) return empty(body, 6, 'No maintenance jobs');
    body.replaceChildren();
    jobs.forEach(job => {
      const row = body.insertRow();
      cell(row, 'Task', job.kind);
      cell(row, 'State', job.state);
      cell(row, 'Next run', time(job.next_run_at));
      cell(row, 'Last success', time(job.last_success_at));
      cell(row, 'Error', job.last_error || '');
      const action = cell(row, 'Actions', '', 'actions');
      if (job.state === 'failed') action.append(actionButton('rotate-ccw', 'Retry', async () => {
        const response = await ui.request(`/api/maintenance-jobs/${job.id}/retry`, { method: 'POST', headers: csrfHeaders, body: '{}' });
        rememberJob(response?.data?.job, job.kind);
        ui.toast(`${job.kind} queued`, 'success');
        await run('status', loadStatus);
      }));
    });
    ui.icons?.(body);
  }

  function renderMaintenanceRuns(runs = []) {
    const body = document.getElementById('maintenance-runs');
    if (!runs?.length) return empty(body, 5, 'No maintenance runs yet');
    body.replaceChildren();
    runs.forEach(run => {
      const row = body.insertRow();
      [run.kind, run.trigger, run.result, `${run.duration_ms} ms`, time(run.finished_at)]
        .forEach(value => row.insertCell().textContent = value);
    });
  }

  function renderStampJobs(jobs = []) {
    const failed = jobs.filter(job => job.state === 'failed');
    const body = document.getElementById('failed-jobs');
    if (!failed.length) return empty(body, 5, 'No failed stamp jobs');
    body.replaceChildren();
    failed.forEach(job => {
      const row = body.insertRow();
      cell(row, 'Job', job.id);
      cell(row, 'Kind', job.kind);
      cell(row, 'Attempts', `${job.attempts}/${job.max_attempts}`);
      cell(row, 'Error', job.last_error || '');
      cell(row, 'Actions', '', 'actions').append(actionButton('rotate-ccw', 'Retry', async () => {
        const response = await ui.request(`/api/stamps/${job.location_id}/retry`, { method: 'POST', headers: csrfHeaders, body: '{}' });
        rememberStampJobs(response?.data?.jobs);
        ui.toast('Stamp retry queued', 'success');
        await run('stamp', loadFailedJobs);
      }));
    });
    ui.icons?.(body);
  }

  function statusBadge(raw, stale = false) {
    const span = document.createElement('span');
    const value = String(raw || 'unknown').toLowerCase();
    span.className = `badge status-${stale || value.includes('stale') ? 'stale' : value}`;
    span.textContent = `${raw || 'unknown'}${stale ? ' (stale)' : ''}`;
    return span;
  }

  function renderLocations(locations = []) {
    const body = document.getElementById('recent-locations');
    if (!locations?.length) return empty(body, 6, 'No locations yet');
    body.replaceChildren();
    locations.forEach(location => {
      const row = body.insertRow();
      cell(row, 'Time', time(location.Tst));
      cell(row, 'Identity', `${location.User}/${location.Device}`);
      cell(row, 'Coordinates', `${Number(location.Lat).toFixed(5)}, ${Number(location.Lon).toFixed(5)}`);
      cell(row, 'TSA', '').append(statusBadge(location.TsaStatus));
      cell(row, 'OTS', '').append(statusBadge(location.OtsStatus));
      const actions = cell(row, 'Actions', '', 'actions');
      actions.append(actionButton('shield-check', 'Details', button => verification.open(location.ID, button)));
      if (location.TsaStatus === 'fail' || location.OtsStatus === 'fail') {
        actions.append(actionButton('rotate-ccw', 'Retry', async () => {
          const response = await ui.request(`/api/stamps/${location.ID}/retry`, { method: 'POST', headers: csrfHeaders, body: '{}' });
          rememberStampJobs(response?.data?.jobs);
          ui.toast('Stamp retry queued', 'success');
        }));
      }
    });
    ui.icons?.(body);
  }

  function renderGeofenceEvents(values = []) {
    const body = document.getElementById('geofence-events');
    if (!values?.length) return empty(body, 4, 'No geofence events');
    body.replaceChildren();
    values.forEach(value => {
      const row = body.insertRow();
      cell(row, 'Time', time(value.tst));
      cell(row, 'Geofence', value.name);
      cell(row, 'Event', value.event);
      cell(row, 'Identity', `${value.user}/${value.device}`);
    });
  }

  function rememberJob(job, kind) {
    if (!job || job.id == null || job.state === 'done') return;
    const locationID = Number(job.location_id);
    const key = `${kind}:${job.id}`;
    pendingJobs.set(key, {
      kind,
      jobKind: job.kind,
      locationID: Number.isSafeInteger(locationID) && locationID > 0 ? locationID : null,
      generation: Number(job.generation || 0),
      submittedAt: Math.floor(Date.now() / 1000)
    });
  }

  function rememberStampJobs(jobs) {
    const locations = new Set();
    (jobs || []).forEach(job => {
      rememberJob(job, 'stamp');
      const locationID = Number(job?.location_id);
      if (Number.isSafeInteger(locationID) && locationID > 0) locations.add(locationID);
    });
    locations.forEach(locationID => trackStampLocation(locationID, true));
  }

  function stopBulkRetryTracking() {
    clearTimeout(bulkRetryTimer);
    bulkRetryTimer = null;
    bulkRetryState = null;
  }

  function scheduleBulkRetryPoll(state) {
    if (bulkRetryState !== state || stopped || !state.pending.size) return;
    bulkRetryTimer = setTimeout(() => { bulkRetryTimer = null; pollBulkRetry(state); }, 1000);
  }

  function noteBulkRetryPending(state) {
    if (state.pendingNotice || !state.pending.size) return;
    state.pendingNotice = true;
    ui.toast(`${state.pending.size} stamp retries still pending`, 'warning');
  }

  function finishBulkRetry(state) {
    if (bulkRetryState !== state) return;
    bulkRetryState = null;
    const parts = [];
    if (state.completed) parts.push(`${state.completed} stamp retries completed`);
    if (state.failed) parts.push(`${state.failed} stamp retries failed`);
    if (state.unknown) parts.push(`${state.unknown} stamp retries unavailable`);
    if (parts.length) ui.toast(parts.join('; '), state.failed || state.unknown ? 'warning' : 'success');
  }

  async function pollBulkRetry(state, external = false) {
    if (bulkRetryState !== state || stopped || ui.isPaused?.() || !state.pending.size) return;
    if (state.busy) {
      state.followup = true;
      return;
    }
    if (!external && state.polls-- <= 0) {
      noteBulkRetryPending(state);
      automatic('stamp', loadFailedJobs);
      return;
    }
    state.busy = true;
    const locations = [...state.locations].filter(locationID => [...state.pending.values()].some(job => job.locationID === locationID));
    let next = 0;
    try {
      const readLocation = async () => {
        while (next < locations.length) {
          const locationID = locations[next++];
          try {
            const result = await latest(`stamp-bulk-location:${locationID}`, `/api/stamp-jobs?location_id=${encodeURIComponent(locationID)}`);
            if (bulkRetryState !== state) return;
            if (!Array.isArray(result?.data)) continue;
            const current = new Map(result.data.map(job => [String(job.id), job]));
            for (const [id, pending] of state.pending) {
              if (pending.locationID !== locationID) continue;
              const job = current.get(id);
              if (!job) {
                state.pending.delete(id);
                state.unknown++;
              } else if (job.state === 'done') {
                state.pending.delete(id);
                state.completed++;
              } else if (job.state === 'failed') {
                state.pending.delete(id);
                state.failed++;
              }
            }
          } catch (error) {
            if (ui.isCanceled?.(error) || error.name === 'AbortError') return;
          }
        }
      }
      await Promise.all(Array.from({ length: Math.min(bulkRetryConcurrency, locations.length) }, readLocation));
      if (bulkRetryState !== state || stopped) return;
      if (!state.pending.size) return finishBulkRetry(state);
      if (state.polls <= 0) noteBulkRetryPending(state);
      else scheduleBulkRetryPoll(state);
    } finally {
      state.busy = false;
      if (bulkRetryState === state && state.followup && state.pending.size) {
        state.followup = false;
        clearTimeout(bulkRetryTimer);
        bulkRetryTimer = null;
        queueMicrotask(() => pollBulkRetry(state, true));
      }
    }
  }

  function refreshBulkRetryTracking() {
    const state = bulkRetryState;
    if (!state || !state.pending.size || stopped) return;
    clearTimeout(bulkRetryTimer);
    bulkRetryTimer = null;
    pollBulkRetry(state, true);
  }

  function startBulkRetryTracking(jobs, retried) {
    if (!(Number(retried) > 0)) return;
    stopBulkRetryTracking();
    const pending = new Map();
    const locations = new Set();
    (jobs || []).filter(job => job?.id != null).slice(0, bulkRetryTrackLimit).forEach(job => {
      const locationID = Number(job.location_id);
      if (!Number.isSafeInteger(locationID) || locationID < 1) return;
      pending.set(String(job.id), { locationID });
      locations.add(locationID);
    });
    if (!pending.size) return;
    const state = bulkRetryState = { pending, locations, polls: bulkRetryPollLimit, completed: 0, failed: 0, unknown: 0, busy: false, followup: false, pendingNotice: false };
    pollBulkRetry(state);
  }

  function reconcileMaintenance(jobs = []) {
    jobs.forEach(job => {
      const key = pendingJobs.has(`maintenance:${job.id}`) ? `maintenance:${job.id}` : `${job.kind}:${job.id}`;
      const pending = pendingJobs.get(key);
      if (!pending || pending.kind === 'stamp') return;
      if (Number(job.generation || 0) < pending.generation) return;
      if (job.state === 'failed') {
        pendingJobs.delete(key);
        ui.toast(`${job.kind} failed`, 'error');
      } else if (job.state === 'scheduled' && job.last_success_at > 0) {
        pendingJobs.delete(key);
        ui.toast(`${job.kind} completed`, 'success');
      }
    });
  }

  function reconcileStampJobs(jobs = [], locationID = null) {
    const current = new Map(jobs.map(job => [String(job.id), job]));
    for (const [key, pending] of pendingJobs) {
      if (pending.kind !== 'stamp' || pending.locationID == null) continue;
      if (locationID != null && pending.locationID !== locationID) continue;
      const job = current.get(key.slice('stamp:'.length));
      if (job?.state === 'failed') {
        pendingJobs.delete(key);
        ui.toast(`${pending.jobKind.toUpperCase()} stamp failed`, 'error');
      } else if (job?.state === 'done') {
        pendingJobs.delete(key);
        ui.toast(`${pending.jobKind.toUpperCase()} stamp completed`, 'success');
      }
    }
    if (locationID != null && ![...pendingJobs.values()].some(job => job.kind === 'stamp' && job.locationID === locationID)) {
      stampLocations.delete(locationID);
      stampLocationGenerations.delete(locationID);
    }
  }

  function clearStampTracking(locationID) {
    let tracked = stampLocations.delete(locationID);
    stampLocationGenerations.delete(locationID);
    for (const [key, pending] of pendingJobs) {
      if (pending.kind === 'stamp' && pending.locationID === locationID) {
        pendingJobs.delete(key);
        tracked = true;
      }
    }
    return tracked;
  }

  const stampLocationKey = locationID => `stamp-location:${locationID}`;
  async function loadStampLocation(locationID, generation = stampLocationGenerations.get(locationID)) {
    try {
      const result = await latest(stampLocationKey(locationID), `/api/stamp-jobs?location_id=${encodeURIComponent(locationID)}`);
      if (stampLocationGenerations.get(locationID) !== generation) return;
      if (!Array.isArray(result?.data)) return;
      if (!result.data.length) {
        if (clearStampTracking(locationID)) ui.toast('Stamp job is no longer available', 'info');
        return;
      }
      reconcileStampJobs(result.data, locationID);
    } catch (error) {
      if (stampLocationGenerations.get(locationID) !== generation) return;
      if (!ui.isCanceled?.(error) && error.name !== 'AbortError' && !stopped) ui.toast(error.message, 'error');
    }
  }

  function trackStampLocation(locationID, restart = false) {
    if (!Number.isSafeInteger(locationID) || locationID < 1 || (!restart && stampLocations.has(locationID))) return;
    stampLocations.add(locationID);
    const generation = Symbol(`stamp-location:${locationID}`);
    stampLocationGenerations.set(locationID, generation);
    run(stampLocationKey(locationID), () => loadStampLocation(locationID, generation));
  }

  function refreshTrackedStampLocations() {
    for (const locationID of stampLocations) {
      automatic(stampLocationKey(locationID), () => loadStampLocation(locationID));
    }
  }

  function updateHealth(status) {
    const issues = [];
    if (!statusLoaded || statusStale) issues.push('status snapshot unavailable');
    if (!status?.workers?.healthy) issues.push('stamp workers');
    if (status?.mqtt?.enabled && !status.mqtt.connected) issues.push('MQTT');
    if ((status?.stamp_jobs?.failed || 0) > 0) issues.push(`${status.stamp_jobs.failed} stamp failures`);
    const proof = status?.proof_verification || {};
    if ((proof.results?.invalid || 0) > 0) issues.push(`${proof.results.invalid} invalid proofs`);
    if ((proof.results?.error || 0) > 0) issues.push(`${proof.results.error} proof errors`);
    if ((proof.results?.missing || 0) > 0) issues.push(`${proof.results.missing} missing proofs`);
    if ((proof.jobs?.failed || 0) > 0) issues.push(`${proof.jobs.failed} verification failures`);
    if (!proof.worker_healthy) issues.push('verification workers');
    if (!status?.maintenance?.worker_healthy) issues.push('maintenance worker');
    if (!status?.timeline?.worker_healthy) issues.push('timeline worker');
    if (!status?.geofences?.worker_healthy) issues.push('geofence worker');
    if ((status?.geofences?.delivery_failures || 0) > 0) issues.push(`${status.geofences.delivery_failures} delivery failures`);
    if ((status?.maintenance?.status?.failed || 0) > 0) issues.push(`${status.maintenance.status.failed} maintenance failures`);
    const backupState = status?.backup?.state;
    if (backupState === 'missing') issues.push('missing backup');
    if (backupState === 'invalid') issues.push('invalid backup');
    const staleProofs = Number(proof.stale) || 0;
    const policyStaleProofs = Number(proof.policy_stale) || 0;
    if (staleProofs > 0) {
      issues.push(`${staleProofs} stale proofs${policyStaleProofs > 0 && policyStaleProofs !== staleProofs ? ` (${policyStaleProofs} policy stale)` : ''}`);
    } else if (policyStaleProofs > 0) {
      issues.push(`${policyStaleProofs} policy-stale proofs`);
    }
    const strip = document.getElementById('health-summary');
    strip.dataset.tone = issues.length ? 'warning' : 'success';
    strip.querySelector('strong').textContent = issues.length ? 'Attention required' : 'All core services healthy';
    strip.querySelector('span').textContent = issues.length ? issues.join(' · ') : 'Queues and local workers are operating normally.';
    const actions = document.getElementById('health-actions');
    if (actions) {
      actions.replaceChildren();
      if ((status?.stamp_jobs?.failed || 0) > 0) actions.append(actionButton('triangle-alert', 'Stamp failures', () => document.getElementById('failure-heading')?.scrollIntoView({ behavior: 'smooth' })));
      if ((proof.results?.invalid || 0) + (proof.results?.error || 0) + (proof.results?.missing || 0) + (proof.jobs?.failed || 0) + staleProofs + policyStaleProofs > 0) actions.append(actionButton('shield-alert', 'Proof quality', () => document.getElementById('proof-heading')?.scrollIntoView({ behavior: 'smooth' })));
      if ((status?.maintenance?.status?.failed || 0) > 0) actions.append(actionButton('wrench', 'Maintenance', () => document.getElementById('maintenance-heading')?.scrollIntoView({ behavior: 'smooth' })));
      if ((status?.geofences?.delivery_failures || 0) > 0) actions.append(actionButton('map-pin-x', 'Geofence delivery', () => document.getElementById('geofence-heading')?.scrollIntoView({ behavior: 'smooth' })));
      if (backupState === 'missing' || backupState === 'invalid') {
        const backup = document.getElementById('create-backup');
        if (backup) actions.append(actionButton('database-backup', 'Backup', () => backup.click()));
      }
      ui.icons?.(actions);
    }
  }

  async function loadStatus() {
    try {
      const status = (await latest('status', '/api/status')).data;
      cache.set('status', status);
      cache.set('status:updated', Math.floor(Date.now() / 1000));
      statusLoaded = true;
      statusStale = false;
      const stats = status.locations || {};
      text('stat-total', stats.Total); text('stat-tsa-ok', stats.TsaOK); text('stat-ots-pending', stats.OtsPending);
      text('stat-ots-ok', stats.OtsOK); text('stat-tsa-fail', stats.TsaFail); text('stat-ots-fail', stats.OtsFail);
      text('ops-workers', status.workers?.healthy ? 'Healthy' : 'Unhealthy');
      text('ops-mqtt', status.mqtt?.enabled ? (status.mqtt.connected ? 'Connected' : 'Disconnected') : 'Disabled');
      text('ops-gc', status.proof_gc?.pending || 0);
      text('ops-backup', status.backup?.state === 'invalid' ? 'Invalid' : (status.backup?.created_at || 'Never'));
      text('ops-timeline', status.timeline?.worker_healthy ? 'Healthy' : 'Unhealthy');
      text('ops-geofence', status.geofences?.worker_healthy ? 'Healthy' : 'Unhealthy');
      text('ops-delivery', status.geofences?.delivery_failures || 0);
      const proof = status.proof_verification || {};
      text('proof-verified', proof.results?.verified || 0); text('proof-degraded', proof.results?.degraded || 0);
      text('proof-inconclusive', proof.results?.inconclusive || 0); text('proof-invalid', proof.results?.invalid || 0);
      text('proof-stale', proof.stale || 0); text('proof-policy-stale', proof.policy_stale || 0);
      const jobs = proof.jobs || {};
      text('verify-queued', jobs.queued || 0); text('verify-running', jobs.running || 0);
      text('verify-retry', jobs.retry || 0); text('verify-failed', jobs.failed || 0);
      text('verify-workers', proof.worker_healthy ? 'Healthy' : 'Unhealthy');
      text('verify-oldest', jobs.oldest_queued ? `${Math.max(0, Math.floor(Date.now() / 1000 - jobs.oldest_queued))}s` : '0s');
      renderProviders(proof.providers); renderMaintenanceJobs(status.maintenance?.jobs);
      for (const id of ['proof-providers', 'maintenance-jobs']) {
        const body = document.getElementById(id);
        body.parentElement?.classList.remove('is-stale');
        resourceNotice(body, 'ready', `Last updated ${time(cache.get('status:updated'))}`);
        cache.set(`${id}:updated`, cache.get('status:updated'));
      }
      reconcileMaintenance(status.maintenance?.jobs || []);
      const statusNode = document.getElementById('status-resource');
      if (statusNode) {
        statusNode.dataset.state = 'ready';
        statusNode.textContent = `Last updated ${time(Math.floor(Date.now() / 1000))}`;
      }
      updateHealth(status); ui.setUpdated?.();
    } catch (error) {
      if (ui.isPaused()) { statusStale = true; updateHealth(cache.get('status') || {}); return; }
      if (ui.isCanceled?.(error) || error.name === 'AbortError') return;
      statusStale = true;
      updateHealth(cache.get('status') || {});
      for (const id of ['proof-providers', 'maintenance-jobs']) resourceError(document.getElementById(id), 6, error, () => run('status', loadStatus), statusLoaded);
      const statusNode = document.getElementById('status-resource');
      if (statusNode) {
        statusNode.dataset.state = statusLoaded ? 'stale' : 'error';
        statusNode.replaceChildren(document.createTextNode(statusLoaded ? 'Status snapshot is stale.' : 'Unable to load status.'));
        statusNode.append(' ', actionButton('refresh-cw', 'Retry', () => run('status', loadStatus)));
      }
      if (!statusLoaded) ui.toast(error.message, 'error');
    }
  }

  const loadFailedJobs = () => resource('stamp', `/api/stamp-jobs?state=failed&limit=${stampJobLimit}`, 'failed-jobs', 5, jobs => {
    renderStampJobs(jobs || []);
  });
  const loadMaintenanceRuns = () => resource('runs', '/api/maintenance-runs?limit=10', 'maintenance-runs', 5, renderMaintenanceRuns);
  const loadLocations = () => resource('locations', '/api/locations?limit=30', 'recent-locations', 6, renderLocations);
  const loadGeofenceEvents = () => resource('geofences', '/api/geofence-events?limit=20', 'geofence-events', 4, renderGeofenceEvents, result => result);

  const loaders = [
    ['status', loadStatus],
    ['stamp', loadFailedJobs],
    ['runs', loadMaintenanceRuns],
    ['locations', loadLocations],
    ['geofences', loadGeofenceEvents]
  ];
  function run(key, load) {
    const execution = Symbol(key);
    executions.set(key, execution);
    automaticPending.delete(key);
    const result = load();
    return Promise.resolve(result).finally(() => {
      if (executions.get(key) !== execution) return;
      if (automaticPending.delete(key)) automatic(key, load);
    });
  }
  function automatic(key, load) {
    if (stopped || ui.isPaused?.()) return;
    if (key.startsWith('stamp-location:') && !stampLocations.has(Number(key.slice('stamp-location:'.length)))) return;
    if (requests.has(key)) {
      automaticPending.add(key);
      return;
    }
    run(key, load);
  }
  const refresh = (isAutomatic = false) => {
    const result = Promise.all(loaders.map(([key, load]) => isAutomatic ? automatic(key, load) : run(key, load)));
    refreshTrackedStampLocations();
    refreshBulkRetryTracking();
    return result;
  };
  function postAction(button, url, pending, success, kind) {
    return ui.withButton(button, async () => {
      const bulkJobs = kind === 'stamp_bulk' ? (cache.get('stamp') || []).filter(job => job.state === 'failed') : [];
      const response = await ui.request(url, { method: 'POST', headers: csrfHeaders, body: '{}' });
      rememberJob(response?.data?.job, kind);
      const count = response?.data?.retried;
      if (kind === 'stamp_bulk') startBulkRetryTracking(bulkJobs, count);
      const message = kind === 'stamp_bulk' ? `${count || 0} failed stamps queued` : (response?.data?.coalesced ? `${success} (already queued)` : `${success} queued`);
      ui.toast(message, 'success');
      if (kind === 'stamp_bulk') await run('stamp', loadFailedJobs);
      await run('status', loadStatus);
    }, pending).catch(error => { if (!ui.isCanceled?.(error) && error.name !== 'AbortError') ui.toast(error.message, 'error'); });
  }

  const verification = ui.createVerificationDialog({ csrf: cfg.csrf });
  document.getElementById('create-backup').onclick = event => postAction(event.currentTarget, '/api/backups', 'Queuing', 'Backup', 'backup');
  document.getElementById('upgrade-ots').onclick = event => postAction(event.currentTarget, '/api/ots/upgrade', 'Queuing', 'OTS upgrade', 'ots_upgrade');
  document.getElementById('retry-all').onclick = event => postAction(event.currentTarget, '/api/stamps/retry', 'Queuing', 'Failed stamps', 'stamp_bulk');

  ui.onResume?.(() => { refresh(); verification.refresh?.(); });

  let topics = new Set(), ids = new Set(), all = false, debounce;
  function applyChanges() {
    const changed = topics; const changedIDs = ids; const refreshAll = all;
    topics = new Set(); ids = new Set(); all = false; debounce = null;
    if (['locations', 'stamp_jobs', 'verification', 'maintenance', 'mqtt', 'config', 'timeline', 'geofences'].some(topic => changed.has(topic))) automatic('status', loadStatus);
    if (changed.has('locations') || changed.has('stamp_jobs')) automatic('locations', loadLocations);
    if (changed.has('stamp_jobs') || changed.has('locations')) {
      if (changed.has('stamp_jobs')) automatic('stamp', loadFailedJobs);
      if (bulkRetryState && (refreshAll || changedIDs.size === 0 || [...changedIDs].some(id => bulkRetryState.locations.has(Number(id))))) refreshBulkRetryTracking();
      const locations = refreshAll || changedIDs.size === 0 ? stampLocations : changedIDs;
      for (const value of locations) {
        const locationID = Number(value);
        if (stampLocations.has(locationID)) automatic(stampLocationKey(locationID), () => loadStampLocation(locationID));
      }
    }
    if (changed.has('maintenance')) automatic('runs', loadMaintenanceRuns);
    if (changed.has('geofences')) automatic('geofences', loadGeofenceEvents);
    if (changed.has('verification') || changed.has('locations')) {
      verification.handleChange?.({ topics: [...changed], location_ids: [...changedIDs], all: refreshAll });
    }
  }

  let streamOpened = false;
  const stream = ui.stream('/api/events', {
    onOpen: () => {
      if (streamOpened) refresh(true);
      streamOpened = true;
    },
    onSafety: () => { refresh(true); verification.refresh?.(); },
    onEvent(change) {
      (change.topics || []).forEach(topic => topics.add(topic));
      if (change.all) { all = true; ids.clear(); } else if (!all) (change.location_ids || []).forEach(id => ids.add(id));
      clearTimeout(debounce); debounce = setTimeout(applyChanges, 150);
    }
  });
  window.addEventListener('pagehide', () => {
    stopped = true;
    stopBulkRetryTracking();
    automaticPending.clear();
    if (typeof stream === 'function') stream(); else stream?.close?.();
    verification.stop?.();
    requests.forEach(controller => controller.abort());
  });
  refresh();
})();
