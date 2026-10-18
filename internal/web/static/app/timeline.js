(() => {
  const cfg = JSON.parse(document.getElementById('cfg')?.textContent || '{}');
  const ui = window.mytrackerUI;
  const user = document.getElementById('timeline-user');
  const device = document.getElementById('timeline-device');
  const hours = document.getElementById('timeline-hours');
  const list = document.getElementById('timeline-list');
  const summary = document.getElementById('timeline-summary');
  const updated = document.getElementById('timeline-updated');
  const changed = document.getElementById('timeline-changed');
  const previous = document.getElementById('timeline-previous');
  const next = document.getElementById('timeline-next');
  const refresh = document.getElementById('timeline-refresh');
  const changedRefresh = document.getElementById('timeline-changed-refresh');
  const pageLabel = document.getElementById('timeline-page');
  const filterForm = document.getElementById('timeline-filters');
  const esc = value => String(value ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  let identities = Array.isArray(cfg.identities) ? cfg.identities : [];
  const pageSize = 100;
  let controller = null;
  let sequence = 0;
  let currentCursor = '';
  let nextCursor = '';
  let pageNumber = 1;
  let previousCursors = [];
  let windowRange = null;
  let hasData = false;
  let streamOpened = false;
  let eventTimer = null;
  let navigationBusy = false;
  let refreshPending = false;
  let stopped = false;
  let identityController = null;
  let identityPending = false;
  let identityDebounce = null;

  function renderIcons(root) {
    if (ui?.icons) ui.icons(root);
    else window.lucide?.createIcons({ root: root || document, attrs: { 'aria-hidden': 'true' } });
  }

  function setState(kind, message) {
    list.dataset.state = kind;
    if (kind === 'loading' && !hasData) list.innerHTML = `<p class="empty-state"><i data-lucide="loader-circle" class="spin"></i><span>${esc(message || 'Loading timeline...')}</span></p>`;
    if (kind === 'empty') list.innerHTML = `<p class="empty-state"><i data-lucide="calendar-x"></i><span>${esc(message || 'No timeline segments in this period.')}</span></p>`;
    if (kind === 'error' && !hasData) list.innerHTML = `<div class="error-state"><i data-lucide="circle-alert"></i><p>${esc(message || 'Timeline unavailable.')}</p><button type="button" id="timeline-error-retry" class="icon-text-button"><i data-lucide="refresh-cw"></i><span>Retry</span></button></div>`;
    const retry = document.getElementById('timeline-error-retry');
    if (retry) retry.onclick = () => loadPage({ reset: true });
    renderIcons(list);
  }

  function renderSummary(value) {
    const s = value || {};
    const trips = Number(s.trips) || 0;
    const stays = Number(s.stays) || 0;
    const distance = Number(s.distance_m) || 0;
    const staySeconds = Number(s.stay_seconds) || 0;
    const total = Number(s.total_segments) || 0;
    const stayTime = staySeconds >= 3600 ? `${(staySeconds / 3600).toFixed(1)} h` : `${Math.round(staySeconds / 60)} min`;
    summary.innerHTML = `<div class="metric"><span>Trips</span><strong>${trips.toLocaleString()}</strong></div><div class="metric"><span>Stays</span><strong>${stays.toLocaleString()}</strong></div><div class="metric"><span>Distance</span><strong>${(distance / 1000).toFixed(1)} km</strong></div><div class="metric"><span>Stay time</span><strong>${stayTime}</strong></div><div class="metric"><span>Total segments</span><strong>${total.toLocaleString()}</strong></div>`;
  }

  function mapLink(segment) {
    const params = new URLSearchParams();
    if (segment.user) params.set('user', segment.user);
    if (segment.device) params.set('device', segment.device);
    if (Number.isFinite(Number(segment.start_tst))) params.set('from', String(segment.start_tst));
    if (Number.isFinite(Number(segment.end_tst))) params.set('to', String(segment.end_tst));
    return `/map?${params}`;
  }

  function renderSegments(segments) {
    if (!segments.length) {
      hasData = false;
      setState('empty');
      return;
    }
    hasData = true;
    list.dataset.state = 'ready';
    list.innerHTML = segments.map(segment => {
      const stay = segment.kind === 'stay';
      const duration = Math.max(0, Number(segment.end_tst) - Number(segment.start_tst));
      const distance = Number(segment.distance_m) || 0;
      const gap = segment.gap_before ? ' · after gap' : '';
      const durationText = duration >= 3600 ? `${(duration / 3600).toFixed(1)} h` : `${Math.round(duration / 60)} min`;
      return `<article class="data-row timeline-row"><i data-lucide="${stay ? 'map-pin' : 'route'}" aria-hidden="true"></i><div><strong>${stay ? 'Stay' : 'Trip'}</strong><span>${esc(segment.user)} / ${esc(segment.device)}</span></div><div><strong>${esc(new Date(Number(segment.start_tst) * 1000).toLocaleString())}</strong><span>${durationText} · ${(distance / 1000).toFixed(2)} km · ${Number(segment.point_count) || 0} points${gap}</span></div><a class="icon-text-button small" href="${esc(mapLink(segment))}"><i data-lucide="map" aria-hidden="true"></i><span>View on map</span></a></article>`;
    }).join('');
    renderIcons(list);
  }

  function setChanged(value, message = 'New timeline data is available.') {
    changed.hidden = !value;
    changed.querySelector('span').textContent = message;
    if (value) renderIcons(changed);
  }

  function syncPagination() {
    previous.disabled = navigationBusy || pageNumber <= 1;
    next.disabled = navigationBusy || !nextCursor;
    pageLabel.textContent = `Page ${pageNumber}`;
  }

  function updateURL() {
    const params = new URLSearchParams();
    if (user.value) params.set('user', user.value);
    if (device.value) params.set('device', device.value);
    params.set('hours', hours.value);
    history.replaceState(null, '', params.toString() ? `?${params}` : location.pathname);
  }

  function freezeWindow() {
    const selected = Number(hours.value);
    const to = Math.floor(Date.now() / 1000);
    windowRange = { from: selected ? to - selected * 3600 : 0, to };
  }

  function query(cursor) {
    const params = new URLSearchParams({ user: user.value, device: device.value, limit: String(pageSize) });
    if (windowRange?.from) params.set('from', String(windowRange.from));
    if (windowRange?.to) params.set('to', String(windowRange.to));
    if (cursor) params.set('cursor', cursor);
    return params;
  }

  async function loadPage({ reset = false, cursor, page, history, automatic = false } = {}) {
    if (ui.isPaused?.()) return;
    if (automatic && controller) {
      refreshPending = true;
      return;
    }
    refreshPending = false;
    if (!automatic) {
      clearTimeout(eventTimer);
      eventTimer = null;
      controller?.abort();
    }
    if (reset) {
      cursor = '';
      page = 1;
      history = [];
      freezeWindow();
      updateURL();
      setChanged(false);
    } else {
      cursor ??= currentCursor;
      page ??= pageNumber;
      history ??= previousCursors;
    }
    const current = ++sequence;
    const request = controller = new AbortController();
    navigationBusy = true;
    syncPagination();
    if (!hasData) setState('loading');
    else list.dataset.state = 'refreshing';
    try {
      const raw = await ui.request(`/api/timeline?${query(cursor)}`, { signal: request.signal });
      if (current !== sequence) return;
      const data = raw?.segments == null && raw?.data?.segments ? raw.data : raw;
      const segments = Array.isArray(data?.segments) ? data.segments : [];
      renderSummary(data?.summary);
      renderSegments(segments);
      currentCursor = cursor || '';
      nextCursor = typeof data?.next_cursor === 'string' ? data.next_cursor : '';
      pageNumber = page;
      previousCursors = history;
      syncPagination();
      updated.textContent = `Updated ${new Date().toLocaleTimeString()}`;
      updated.dataset.stale = 'false';
      setChanged(false);
    } catch (error) {
      if (!ui.isCanceled?.(error) && error.name !== 'AbortError' && current === sequence) {
        if (hasData) {
          list.dataset.state = 'stale';
          updated.textContent = 'Refresh failed';
          updated.dataset.stale = 'true';
          setChanged(true, 'Timeline is stale. Refresh failed.');
        } else setState('error', error.message);
      }
    } finally {
      if (current === sequence) {
        navigationBusy = false;
        syncPagination();
      }
      if (request === controller) {
        controller = null;
        if (refreshPending && !request.signal.aborted) refreshPage();
      }
    }
  }

  function refreshPage() {
    if (stopped || ui.isPaused?.()) {
      refreshPending = false;
      return;
    }
    if (pageNumber > 1) {
      refreshPending = false;
      setChanged(true);
      return;
    }
    if (controller) {
      refreshPending = true;
      return;
    }
    loadPage({ reset: true, automatic: true });
  }

  function reloadFromStart() {
    loadPage({ reset: true });
  }

  function fillUsers(selected = user.value) {
    ui.fillIdentitySelect(user, identities, 'User', selected);
  }

  function fillDevices(selected = device.value) {
    const values = identities.filter(item => !user.value || ui.identityValue(item, 'User') === user.value);
    ui.fillIdentitySelect(device, values, 'Device', selected);
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
      const selectedUser = user.value;
      const selectedDevice = device.value;
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

  function restoreFilters() {
    const params = new URLSearchParams(location.search);
    const requestedUser = params.get('user') || '';
    const requestedDevice = params.get('device') || '';
    const requestedHours = params.get('hours');
    if (requestedUser && ![...user.options].some(option => option.value === requestedUser)) user.add(new Option(requestedUser, requestedUser));
    user.value = requestedUser;
    fillDevices(requestedDevice);
    if (requestedDevice && ![...device.options].some(option => option.value === requestedDevice)) device.add(new Option(requestedDevice, requestedDevice));
    device.value = requestedDevice;
    if ([...hours.options].some(option => option.value === requestedHours)) hours.value = requestedHours;
  }

  user.addEventListener('change', () => { fillDevices(''); reloadFromStart(); });
  device.addEventListener('change', reloadFromStart);
  hours.addEventListener('change', reloadFromStart);
  filterForm?.addEventListener('submit', event => { event.preventDefault(); reloadFromStart(); });
  refresh.onclick = () => ui.withButton(refresh, () => loadPage({ reset: true }), 'Refreshing');
  changedRefresh.onclick = () => refresh.click();
  previous.onclick = () => {
    if (navigationBusy || pageNumber <= 1) return;
    const history = previousCursors.slice(0, -1);
    const cursor = previousCursors[previousCursors.length - 1] || '';
    loadPage({ cursor, page: pageNumber - 1, history });
  };
  next.onclick = () => {
    if (navigationBusy || !nextCursor) return;
    loadPage({ cursor: nextCursor, page: pageNumber + 1, history: [...previousCursors, currentCursor] });
  };

  function onChange(change) {
    if (ui.isPaused?.()) return;
    if (!(change?.all || change?.topics?.some(topic => topic === 'timeline' || topic === 'locations'))) return;
    if (change?.all || change?.topics?.includes('locations')) scheduleIdentityRefresh();
    clearTimeout(eventTimer);
    eventTimer = setTimeout(() => {
      if (pageNumber === 1) refreshPage();
      else setChanged(true);
    }, 150);
  }

  fillUsers();
  restoreFilters();
  const closeEvents = ui.stream('/api/events', {
    onOpen: () => { scheduleIdentityRefresh(); if (streamOpened) onChange({ all: true }); streamOpened = true; },
    onEvent: onChange,
    onSafety: () => { scheduleIdentityRefresh(); onChange({ all: true }); }
  });
  const removeResume = ui.onResume?.(() => { scheduleIdentityRefresh(); onChange({ all: true }); });
  window.addEventListener('beforeunload', () => {
    stopped = true;
    refreshPending = false;
    clearTimeout(eventTimer);
    clearTimeout(identityDebounce);
    removeResume?.();
    closeEvents?.();
    controller?.abort();
    identityController?.abort();
  });
  loadIdentities();
  loadPage({ reset: true });
})();
