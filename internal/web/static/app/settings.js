(function () {
  const cfg = JSON.parse(document.getElementById('cfg').textContent);
  const ui = window.mytrackerUI;
  const csrf = cfg.csrf;
  const formHeaders = { 'X-CSRF-Token': csrf, 'Content-Type': 'application/x-www-form-urlencoded' };
  const tabs = [...document.querySelectorAll('[role="tab"]')];
  const panels = [...document.querySelectorAll('[role="tabpanel"]')];

  function selectTab(name, focus = false) {
    if (!tabs.some(tab => tab.dataset.tab === name)) name = 'tracking';
    tabs.forEach(tab => {
      const active = tab.dataset.tab === name;
      tab.setAttribute('aria-selected', String(active));
      tab.tabIndex = active ? 0 : -1;
      if (active && focus) tab.focus();
    });
    panels.forEach(panel => { panel.hidden = panel.dataset.panel !== name; });
    document.getElementById('active-tab').value = name;
    history.replaceState(null, '', `#${name}`);
  }

  tabs.forEach((tab, index) => {
    tab.onclick = () => selectTab(tab.dataset.tab);
    tab.onkeydown = event => {
      let next = null;
      if (event.key === 'ArrowRight' || event.key === 'ArrowDown') next = (index + 1) % tabs.length;
      if (event.key === 'ArrowLeft' || event.key === 'ArrowUp') next = (index - 1 + tabs.length) % tabs.length;
      if (event.key === 'Home') next = 0;
      if (event.key === 'End') next = tabs.length - 1;
      if (next != null) { event.preventDefault(); selectTab(tabs[next].dataset.tab, true); }
    };
  });
  selectTab(location.hash.slice(1) || cfg.activeTab);

  const settingsForm = document.getElementById('settings-form');
  const dirty = document.getElementById('dirty-state');
  let changed = false;
  function markDirty(event) {
    if (event.target.form !== settingsForm || !event.target.name || ['csrf', 'active_tab'].includes(event.target.name)) return;
    changed = true;
    dirty.textContent = 'Unsaved changes';
    dirty.dataset.dirty = 'true';
  }
  document.addEventListener('input', markDirty, true);
  settingsForm.addEventListener('input', markDirty);
  let saving = false;
  settingsForm.addEventListener('submit', async event => {
    event.preventDefault();
    if (saving) return;
    if (ui.isPaused?.()) { ui.toast('Sign in to resume before saving settings', 'warning'); return; }
    const body = new URLSearchParams(new FormData(settingsForm));
    const controls = [...settingsForm.elements].filter(control => !control.disabled);
    saving = true;
    controls.forEach(control => { control.disabled = true; });
    settingsForm.setAttribute('aria-busy', 'true');
    dirty.textContent = 'Saving';
    let saved = false;
    try {
      const result = await ui.request('/api/settings', {
        method: 'POST', headers: formHeaders, body
      });
      changed = false;
      const target = new URL(result.data.redirect, location.href);
      const reload = target.origin === location.origin && target.pathname === location.pathname && target.search === location.search;
      location.assign(target.href);
      if (reload) location.reload();
      saved = true;
    } catch (error) {
      changed = true;
      dirty.textContent = 'Unsaved changes';
      dirty.dataset.dirty = 'true';
      [...settingsForm.elements].filter(input => input.type === 'password').forEach(input => { input.value = ''; });
      if (error.status !== 401 && !ui.isCanceled(error)) {
        if (error.details?.committed) {
          settingsForm.elements.settings_revision.value = error.details.settings_revision;
          settingsForm.elements.regen_pub_pass.checked = false;
        }
        let notice = document.querySelector('.notice.error');
        if (!notice) {
          notice = document.createElement('div');
          notice.className = 'notice error';
          notice.setAttribute('role', 'alert');
          document.querySelector('.settings-layout').before(notice);
        }
        notice.textContent = `${error.message} Secret fields were cleared; re-enter them if needed.`;
      }
      errorToast(error);
    } finally {
      if (!saved) {
        controls.forEach(control => { control.disabled = false; });
        settingsForm.removeAttribute('aria-busy');
        saving = false;
      }
    }
  });
  document.addEventListener('invalid', event => {
    if (event.target.form !== settingsForm) return;
    const panel = event.target.closest('[role="tabpanel"]');
    if (panel) selectTab(panel.dataset.panel);
  }, true);
  if (cfg.saveError) {
    changed = true;
    dirty.textContent = 'Unsaved changes';
    dirty.dataset.dirty = 'true';
  }
  addEventListener('beforeunload', event => { if (changed) event.preventDefault(); });

  const exportHours = document.getElementById('export-hours');
  const exportLink = document.getElementById('export-link');
  const updateExport = () => {
    const hours = exportHours.value;
    exportLink.href = hours === '0' ? '/api/export' : `/api/export?hours=${hours}`;
  };
  exportHours.onchange = updateExport;
  updateExport();

  function downloadError(frame) {
    const raw = frame.contentDocument?.body?.textContent?.trim();
    if (!raw) return null;
    try {
      const body = JSON.parse(raw);
      return body.error || { message: raw };
    } catch (_) {
      return { message: raw.slice(0, 240) };
    }
  }

  let forwardingDownload = false;
  const downloads = new Set();
  function nativeDownload(event) {
    if (forwardingDownload || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    if (ui.isPaused?.()) {
      ui.toast('Sign in to resume before downloading', 'warning');
      return;
    }
    const link = event.currentTarget;
    const id = [...crypto.getRandomValues(new Uint32Array(4))].join('-');
    const frame = createDownloadFrame(id);
    const target = link.target;
    const href = link.getAttribute('href');
    const url = new URL(link.href);
    url.searchParams.set('download_id', id);
    link.href = url.href;
    link.target = frame.name;
    forwardingDownload = true;
    try { link.click(); } finally {
      forwardingDownload = false;
      link.setAttribute('href', href);
      if (target) link.target = target;
      else link.removeAttribute('target');
    }
  }

  function createDownloadFrame(id) {
    const frame = document.createElement('iframe');
    frame.name = `download-${id}`;
    frame.title = '';
    frame.setAttribute('aria-hidden', 'true');
    frame.style.cssText = 'position:absolute;width:1px;height:1px;border:0;left:-10000px;top:-10000px';
    const cookie = `mytracker-download-${id}`;
    let poll;
    const cleanup = () => {
      clearInterval(poll);
      frame.remove();
      downloads.delete(cleanup);
      document.cookie = `${cookie}=; Max-Age=0; Path=/; SameSite=Strict`;
    };
    const onLoad = () => {
      if (frame.contentDocument?.location?.href === 'about:blank') return;
      const error = downloadError(frame);
      if (!error) {
        cleanup();
        return;
      }
      cleanup();
      if (error.code === 'unauthorized') {
        ui.toast('Sign in to resume before downloading', 'error');
        ui.request('/api/sessions', { cache: 'no-store' }).catch(() => {});
      } else {
        ui.toast(error.message || 'Download failed', 'error');
      }
    };
    frame.addEventListener('load', onLoad);
    frame.addEventListener('error', () => {
      cleanup();
      ui.toast('Download failed', 'error');
    }, { once: true });
    poll = setInterval(() => {
      if (document.cookie.split('; ').some(value => value === `${cookie}=1`)) cleanup();
    }, 250);
    downloads.add(cleanup);
    document.body.append(frame);
    return frame;
  }

  addEventListener('pagehide', () => { downloads.forEach(cleanup => cleanup()); });

  exportLink.addEventListener('click', nativeDownload);

  const shareUser = document.getElementById('share-user');
  const shareDevice = document.getElementById('share-device');
  let identities = Array.isArray(cfg.identities) ? cfg.identities : [];
  let identityController = null;
  let identityPending = false;
  let identityDebounce = null;
  let stopped = false;
  function fillUsers(selected = shareUser.value) {
    ui.fillIdentitySelect(shareUser, identities, 'User', selected);
  }
  function fillDevices(selected = shareDevice.value) {
    const values = identities.filter(identity => !shareUser.value || ui.identityValue(identity, 'User') === shareUser.value);
    ui.fillIdentitySelect(shareDevice, values, 'Device', selected);
  }
  async function loadIdentities() {
    if (stopped) return;
    if (identityController) {
      identityPending = true;
      return;
    }
    const request = identityController = new AbortController();
    try {
      const response = await ui.request('/api/identities', { signal: request.signal });
      if (request !== identityController) return;
      const values = Array.isArray(response) ? response : response?.data;
      if (!Array.isArray(values)) return;
      identities = values;
      const selectedUser = shareUser.value;
      const selectedDevice = shareDevice.value;
      fillUsers(selectedUser);
      fillDevices(selectedDevice);
    } catch (error) {
      if (!ui.isCanceled?.(error) && request === identityController) return;
    } finally {
      if (request !== identityController) return;
      identityController = null;
      if (identityPending && !stopped) {
        identityPending = false;
        scheduleIdentityRefresh();
      }
    }
  }
  function scheduleIdentityRefresh() {
    if (stopped) return;
    clearTimeout(identityDebounce);
    identityDebounce = setTimeout(() => {
      identityDebounce = null;
      loadIdentities();
    }, 250);
  }
  shareUser.onchange = () => fillDevices('');
  fillUsers();
  fillDevices();

  async function formRequest(method, path, values) {
    const options = { method, headers: { 'X-CSRF-Token': csrf } };
    if (values) { options.headers['Content-Type'] = formHeaders['Content-Type']; options.body = new URLSearchParams(values); }
    return ui.request(path, options);
  }
  async function jsonRequest(method, path, value) {
    return ui.request(path, { method, headers: { 'X-CSRF-Token': csrf, 'Content-Type': 'application/json' }, body: JSON.stringify(value) });
  }
  const errorToast = error => { if (!ui.isCanceled?.(error)) ui.toast(error.message, 'error'); };
  const time = value => value ? new Date(value * 1000).toLocaleString() : 'Never';
  const cell = (row, label, value, className = '') => {
    const td = row.insertCell(); td.dataset.label = label; td.textContent = value; if (className) td.className = className; return td;
  };
  const empty = (body, columns, message) => {
    body.replaceChildren(); const row = body.insertRow(); const td = row.insertCell(); td.colSpan = columns; td.className = 'empty'; td.textContent = message;
  };
  const listError = (host, message, retry, columns = 1) => {
    host.dataset.stale = 'true';
    host.querySelectorAll('.resource-error').forEach(node => node.remove());
    const hadData = host.children.length > 0 && !host.querySelector('.empty');
    if (host.tagName === 'TBODY') {
      if (!hadData) host.replaceChildren();
      const row = host.insertRow(0); row.className = 'resource-error'; const td = row.insertCell(); td.colSpan = columns; td.className = 'error-state';
      td.textContent = message || 'Unable to load';
      const action = document.createElement('button'); action.type = 'button'; action.className = 'icon-text-button small'; action.textContent = 'Retry'; action.onclick = retry; td.append(' ', action);
    } else {
      if (!hadData) host.replaceChildren();
      const notice = document.createElement('div'); notice.className = 'notice error resource-error'; notice.setAttribute('role', 'alert'); notice.textContent = message || 'Unable to load';
      const action = document.createElement('button'); action.type = 'button'; action.className = 'icon-text-button small'; action.textContent = 'Retry'; action.onclick = retry; notice.append(' ', action); host.append(notice);
    }
    ui.icons?.(host);
  };
  const button = (icon, label, action, danger = false) => {
    const el = document.createElement('button'); el.type = 'button'; el.className = `${danger ? 'btn-danger' : 'icon-text-button'} small`;
    el.innerHTML = `<i data-lucide="${icon}"></i><span>${label}</span>`; el.onclick = () => { const returnFocus = document.activeElement; return ui.withButton(el, () => action(returnFocus), label).catch(errorToast); }; return el;
  };

  let sharesController = null;
  async function loadShares() {
    sharesController?.abort();
    const request = sharesController = new AbortController();
    try {
      const shares = await ui.request('/api/shares', { signal: request.signal });
      if (request !== sharesController) return;
      const body = document.getElementById('share-list');
      body.removeAttribute('data-stale');
      if (!shares.length) return empty(body, 5, 'No share links');
      body.replaceChildren();
      shares.forEach(share => {
        const row = body.insertRow();
        cell(row, 'Label', share.label || 'Unlabeled'); cell(row, 'Filter', share.filter);
        cell(row, 'Hours', `${share.hours}h`); cell(row, 'Expires', share.active ? share.expires : 'Expired');
        const actions = cell(row, 'Actions', '', 'actions');
        if (share.active) actions.append(button('refresh-cw', 'Rotate', async returnFocus => {
          if (!await ui.confirm({ title: 'Rotate share link?', message: 'The old URL will stop working immediately.', confirmLabel: 'Rotate' })) return;
          try {
            const result = await formRequest('POST', `/api/shares/${share.id}/rotate`, { csrf });
            await ui.secret('New share URL', result.url, returnFocus);
            await loadShares();
          } catch (error) { errorToast(error); }
        }));
        actions.append(button('unlink', 'Revoke', async () => {
          if (!await ui.confirm({ title: 'Revoke share link?', message: 'Anyone using this URL will lose access.', confirmLabel: 'Revoke', danger: true })) return;
          try { await formRequest('DELETE', `/api/shares/${share.id}`); ui.toast('Share link revoked', 'success'); await loadShares(); }
          catch (error) { errorToast(error); }
        }, true));
      });
      window.lucide?.createIcons();
    } catch (error) { if (!request.signal.aborted && !ui.isCanceled?.(error) && request === sharesController) { listError(document.getElementById('share-list'), error.message, loadShares, 5); ui.toast(error.message, 'error'); } }
    finally { if (request === sharesController) sharesController = null; }
  }

  document.getElementById('share-form').onsubmit = event => {
    event.preventDefault();
    if (!event.currentTarget.reportValidity()) return;
    const createButton = document.getElementById('share-create');
    const returnFocus = event.submitter || createButton;
    return ui.withButton(createButton, async () => {
    const exact = document.getElementById('share-precision').value === '0';
    const forever = document.getElementById('share-expires').value === '0';
    let confirmRisk = false;
    if (exact || forever) {
      confirmRisk = await ui.confirm({ title: 'Create a high-risk link?', message: 'This link exposes exact coordinates or never expires.', confirmLabel: 'Create link', danger: true });
      if (!confirmRisk) return;
    }
    try {
      const result = await formRequest('POST', '/api/shares', {
        label: document.getElementById('share-label').value, user: shareUser.value, device: shareDevice.value,
        hours: document.getElementById('share-hours').value, expires_days: document.getElementById('share-expires').value,
        delay_minutes: document.getElementById('share-delay').value, precision_m: document.getElementById('share-precision').value,
        csrf, confirm_risk: confirmRisk ? '1' : '0'
      });
      await ui.secret('New share URL', result.url, returnFocus);
      await loadShares();
    } catch (error) { errorToast(error); }
    }, 'Creating');
  };

  let sessionsController = null;
  async function loadSessions() {
    sessionsController?.abort();
    const request = sessionsController = new AbortController();
    try {
      const sessions = await ui.request('/api/sessions', { signal: request.signal });
      if (request !== sessionsController) return;
      const body = document.getElementById('sessions');
      body.removeAttribute('data-stale');
      if (!sessions.length) return empty(body, 4, 'No active sessions');
      body.replaceChildren();
      sessions.forEach(session => {
        const row = body.insertRow();
        cell(row, 'Created', time(session.created_at)); cell(row, 'Last used', time(session.last_seen_at)); cell(row, 'Expires', time(session.expires_at));
        const actions = cell(row, 'Actions', '', 'actions');
        if (session.current) actions.textContent = 'Current';
        else actions.append(button('log-out', 'Revoke', async () => {
          if (!await ui.confirm({ title: 'Revoke session?', message: 'That browser will be signed out immediately.', confirmLabel: 'Revoke', danger: true })) return;
          try { await formRequest('DELETE', `/api/sessions/${session.id}`); ui.toast('Session revoked', 'success'); await loadSessions(); }
          catch (error) { errorToast(error); }
        }, true));
      });
      window.lucide?.createIcons();
    } catch (error) { if (!request.signal.aborted && !ui.isCanceled?.(error) && request === sessionsController) { listError(document.getElementById('sessions'), error.message, loadSessions, 4); ui.toast(error.message, 'error'); } }
    finally { if (request === sessionsController) sessionsController = null; }
  }

  document.getElementById('sessions-revoke-others').onclick = event => ui.withButton(event.currentTarget, async () => {
    if (!await ui.confirm({ title: 'Revoke other sessions?', message: 'Every other browser will be signed out.', confirmLabel: 'Revoke others', danger: true })) return;
    try { await formRequest('POST', '/api/sessions/revoke-others', { csrf }); ui.toast('Other sessions revoked', 'success'); await loadSessions(); }
    catch (error) { errorToast(error); }
  }, 'Revoking');

  document.getElementById('import-form').onsubmit = event => {
    event.preventDefault();
    if (!event.currentTarget.reportValidity()) return;
    return ui.withButton(document.getElementById('import-submit'), async () => {
    const input = document.getElementById('import-file');
    if (!input.files.length) { ui.toast('Choose an export ZIP first', 'error'); input.focus(); return; }
    const body = new FormData(); body.append('file', input.files[0]);
    try {
      const result = await ui.request('/api/import', { method: 'POST', headers: { 'X-CSRF-Token': csrf }, body });
      const cleanupPending = Boolean(result.cleanup_pending);
      ui.toast(`Imported ${result.imported}; skipped ${result.skipped}; proofs ${result.proofs}${cleanupPending ? '; cleanup pending' : ''}`, cleanupPending ? 'warning' : 'success');
      input.value = '';
    } catch (error) { errorToast(error); }
    }, 'Importing');
  };

  const gpxUser = document.getElementById('gpx-user');
  const gpxDevice = document.getElementById('gpx-device');
  const gpxExport = document.getElementById('gpx-export');
  function updateGPXExport() { gpxExport.href = `/api/export/gpx?${new URLSearchParams({ user: gpxUser.value, device: gpxDevice.value })}`; }
  gpxUser.oninput = gpxDevice.oninput = updateGPXExport;
  gpxExport.addEventListener('click', nativeDownload);
  document.getElementById('gpx-import-form').onsubmit = event => {
    event.preventDefault();
    if (!event.currentTarget.reportValidity()) return;
    return ui.withButton(document.getElementById('gpx-import'), async () => {
    const input = document.getElementById('gpx-file');
    if (!input.files.length || !gpxUser.value.trim() || !gpxDevice.value.trim()) { ui.toast('Choose a GPX file and enter user and device', 'error'); return; }
    const body = new FormData(); body.append('file', input.files[0]); body.append('user', gpxUser.value.trim()); body.append('device', gpxDevice.value.trim());
    try { const result = await ui.request('/api/import/gpx', { method: 'POST', headers: { 'X-CSRF-Token': csrf }, body }); ui.toast(`Imported ${result.imported}; skipped ${result.skipped}`, 'success'); input.value = ''; }
    catch (error) { errorToast(error); }
    }, 'Importing');
  };

  let geofencesController = null;
  let editingGeofence = 0;
  const geofenceField = id => document.getElementById(id);
  function clearGeofenceForm() {
    editingGeofence = 0;
    ['geofence-name', 'geofence-lat', 'geofence-lon', 'geofence-user', 'geofence-device'].forEach(id => { geofenceField(id).value = ''; });
    geofenceField('geofence-radius').value = '200';
    geofenceField('geofence-enabled').checked = true;
    geofenceField('geofence-create').querySelector('span').textContent = 'Add geofence';
    geofenceField('geofence-cancel').hidden = true;
  }
  function editGeofence(value) {
    editingGeofence = value.id;
    geofenceField('geofence-name').value = value.name;
    geofenceField('geofence-lat').value = value.lat;
    geofenceField('geofence-lon').value = value.lon;
    geofenceField('geofence-radius').value = value.radius_m;
    geofenceField('geofence-user').value = value.user || '';
    geofenceField('geofence-device').value = value.device || '';
    geofenceField('geofence-enabled').checked = Boolean(value.enabled);
    geofenceField('geofence-create').querySelector('span').textContent = 'Save geofence';
    geofenceField('geofence-cancel').hidden = false;
    geofenceField('geofence-name').focus();
  }
  function readGeofence() {
    const latRaw = geofenceField('geofence-lat').value.trim();
    const lonRaw = geofenceField('geofence-lon').value.trim();
    if (!latRaw || !lonRaw) throw new Error('Latitude and longitude are required');
    const value = { name: geofenceField('geofence-name').value, lat: Number(latRaw), lon: Number(lonRaw), radius_m: Number(geofenceField('geofence-radius').value), user: geofenceField('geofence-user').value, device: geofenceField('geofence-device').value, enabled: geofenceField('geofence-enabled').checked };
    if (![value.lat, value.lon, value.radius_m].every(Number.isFinite)) throw new Error('Coordinates and radius must be numbers');
    const bytes = value => new TextEncoder().encode(value).length;
    if (bytes(value.name.trim()) < 1 || bytes(value.name.trim()) > 80) throw new Error('Name must be 1 to 80 bytes');
    if (bytes(value.user.trim()) > 128 || bytes(value.device.trim()) > 128) throw new Error('User and device filters must be at most 128 bytes');
    if (value.lat < -90 || value.lat > 90 || value.lon < -180 || value.lon > 180) throw new Error('Coordinates are out of range');
    if (value.radius_m < 25 || value.radius_m > 100000) throw new Error('Radius must be between 25 and 100000 m');
    return value;
  }
  async function loadGeofences() {
    geofencesController?.abort();
    const request = geofencesController = new AbortController();
    try { const values = await ui.request('/api/geofences', { signal: request.signal }) || []; if (request !== geofencesController) return; const host = document.getElementById('geofence-list'); host.removeAttribute('data-stale'); host.replaceChildren();
      if (!values.length) { host.innerHTML = '<p class="empty">No geofences</p>'; return; }
      values.forEach(value => {
        const row = document.createElement('div'); row.className = 'data-row';
        const info = document.createElement('div'); const name = document.createElement('strong'); const detail = document.createElement('span');
        name.textContent = value.name; detail.textContent = `${value.radius_m} m · ${value.user || 'all users'} / ${value.device || 'all devices'}`; info.append(name, detail);
        const actions = document.createElement('div'); actions.className = 'actions';
        actions.append(button('pencil', 'Edit', () => editGeofence(value)));
        const enabled = document.createElement('label'); enabled.className = 'checkbox'; const toggle = document.createElement('input'); toggle.type = 'checkbox'; toggle.checked = Boolean(value.enabled); toggle.setAttribute('aria-label', `${value.name} enabled`); enabled.append(toggle, ' Enabled');
        toggle.onchange = async () => {
          toggle.disabled = true;
          try { await jsonRequest('PATCH', `/api/geofences/${value.id}`, { enabled: toggle.checked }); ui.toast(toggle.checked ? 'Geofence enabled' : 'Geofence disabled', 'success'); await loadGeofences(); }
    catch (error) { toggle.checked = !toggle.checked; errorToast(error); }
          finally { toggle.disabled = false; }
        };
        actions.append(enabled);
        actions.append(button('trash-2', 'Delete', async () => { if (!await ui.confirm({ title: 'Delete geofence?', message: 'Stored events for this geofence will also be removed.', danger: true, confirmLabel: 'Delete' })) return; await formRequest('DELETE', `/api/geofences/${value.id}`); if (editingGeofence === value.id) clearGeofenceForm(); await loadGeofences(); }, true));
        row.append(info, actions); host.append(row);
      });
      ui.icons?.(host);
    } catch (error) { if (!request.signal.aborted && !ui.isCanceled?.(error) && request === geofencesController) { listError(document.getElementById('geofence-list'), error.message, loadGeofences); ui.toast(error.message, 'error'); } }
    finally { if (request === geofencesController) geofencesController = null; }
  }
  document.getElementById('geofence-form').onsubmit = event => {
    event.preventDefault();
    if (!event.currentTarget.reportValidity()) return;
    const submit = document.getElementById('geofence-create');
    return ui.withButton(submit, async () => {
    let value;
    try { value = readGeofence(); } catch (error) { errorToast(error); return; }
    try { await jsonRequest(editingGeofence ? 'PATCH' : 'POST', editingGeofence ? `/api/geofences/${editingGeofence}` : '/api/geofences', value); ui.toast(editingGeofence ? 'Geofence updated' : 'Geofence created', 'success'); clearGeofenceForm(); await loadGeofences(); }
    catch (error) { errorToast(error); }
    }, 'Saving');
  };
  document.getElementById('geofence-cancel').onclick = clearGeofenceForm;

  function unix(id) {
    const value = document.getElementById(id).value;
    if (!value) return 0;
    const seconds = Math.floor(new Date(value).getTime() / 1000);
    if (!Number.isFinite(seconds) || seconds <= 0) throw new Error('Date must be after 1970-01-01');
    return seconds;
  }
  document.getElementById('delete-form').onsubmit = event => {
    event.preventDefault();
    return ui.withButton(document.getElementById('delete-preview'), async () => {
    try {
      const from = unix('delete-from'); const to = unix('delete-to');
      if (!from && !to) { ui.toast('From or To is required', 'error'); return; }
      if (from && to && from > to) { ui.toast('From must not exceed To', 'error'); return; }
      const preview = await ui.request(`/api/delete-preview?${new URLSearchParams({ from, to })}`);
      if (!preview.locations) { ui.toast('No locations match this range', 'info'); return; }
      const purge = document.getElementById('delete-purge-backups').checked;
      const oldest = preview.oldest_backup_at ? ` Oldest backup: ${preview.oldest_backup_at}.` : '';
      const message = `${preview.locations} locations and about ${preview.proofs} proofs will be deleted. ${preview.backups} existing backups remain.${oldest}${purge ? ' Existing backups will be purged after a verified post-delete backup.' : ''}`;
      if (!await ui.confirm({ title: 'Delete location range?', message, confirmLabel: 'Delete locations', danger: true, requiredText: 'DELETE' })) return;
      const result = await formRequest('POST', '/api/delete', { from, to, confirm: 'DELETE', purge_backups: purge ? '1' : '0' });
      ui.toast(`${result.deleted} locations deleted${result.purge_pending ? '; cleanup pending' : ''}`, result.purge_pending ? 'info' : 'success');
    } catch (error) { errorToast(error); }
    }, 'Checking');
  };

  const refreshLists = (topics, all = false) => {
    if (topics.includes('shares')) loadShares();
    if (topics.includes('config') || topics.includes('sessions')) loadSessions();
    if (topics.includes('geofences')) loadGeofences();
    if (all || topics.includes('locations')) scheduleIdentityRefresh();
  };
  const refreshAllLists = () => { loadShares(); loadSessions(); loadGeofences(); scheduleIdentityRefresh(); };
  const stopEvents = ui.stream('/api/events', { onEvent: change => refreshLists(change.topics || [], change.all), onOpen: refreshAllLists, onSafety: refreshAllLists });
  const removeResume = ui.onResume?.(refreshAllLists);
  addEventListener('pagehide', () => {
    stopped = true;
    removeResume?.();
    stopEvents?.();
    clearTimeout(identityDebounce);
    identityController?.abort();
    sharesController?.abort();
    sessionsController?.abort();
    geofencesController?.abort();
  }, { once: true });
  loadShares();
  loadSessions();
	loadGeofences();
  loadIdentities();
})();
