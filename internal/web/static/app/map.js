(function () {
  const cfg = JSON.parse(document.getElementById('cfg').textContent);
  const ui = window.mytrackerUI;
  let identities = Array.isArray(cfg.identities) ? cfg.identities : [];
  const user = document.getElementById('user');
  const device = document.getElementById('device');
  const hours = document.getElementById('hours');
  const from = document.getElementById('from');
  const to = document.getElementById('to');
  const limit = document.getElementById('limit');
  const customPeriods = document.querySelectorAll('.custom-period');
  const initial = new URLSearchParams(location.search);
  const periodEdited = { from: false, to: false };

  function fillUsers(selected = user.value) {
    ui.fillIdentitySelect(user, identities, 'User', selected);
  }
  function fillDevices(selected = device.value) {
    const values = identities.filter(identity => !user.value || ui.identityValue(identity, 'User') === user.value);
    ui.fillIdentitySelect(device, values, 'Device', selected);
  }
  function localDateTime(value) {
    const rawValue = String(value ?? '').trim();
    if (!rawValue) return '';
    const raw = Number(rawValue);
    const date = Number.isFinite(raw) && raw >= 0 ? new Date(raw * 1000) : new Date(rawValue);
    if (Number.isNaN(date.getTime())) return '';
    const pad = number => String(number).padStart(2, '0');
    return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
  }
  function periodSeconds(input, key) {
    const raw = initial.get(key);
    if (!periodEdited[key] && raw != null && /^\d+$/.test(raw)) return { value: Number(raw), raw };
    const value = input.value ? Math.floor(new Date(input.value).getTime() / 1000) : NaN;
    return { value, raw: Number.isFinite(value) ? String(value) : '' };
  }
  const custom = initial.has('from') || initial.has('to') || initial.get('hours') === 'custom';
  const knownHours = new Set([...hours.options].map(option => option.value));
  const initialHours = initial.get('hours');
  if (/^\d+$/.test(initialHours || '') && Number(initialHours) >= 1 && Number(initialHours) <= 87600 && !knownHours.has(initialHours)) {
    hours.add(new Option(`${initialHours} hours`, initialHours));
    knownHours.add(initialHours);
  }
  if (initial.has('hours') && (custom || knownHours.has(initial.get('hours')))) hours.value = custom ? 'custom' : initial.get('hours');
  if (custom) hours.value = 'custom';
  if (initial.has('from')) from.value = localDateTime(initial.get('from'));
  if (initial.has('to')) to.value = localDateTime(initial.get('to'));
  const initialUser = initial.get('user') || '';
  const initialDevice = initial.get('device') || '';
  fillUsers(initialUser);
  fillDevices(initialDevice);
  const initialLimit = initial.get('limit');
  if (/^\d+$/.test(initialLimit || '') && Number(initialLimit) >= 2 && Number(initialLimit) <= 20000) {
    if (![...limit.options].some(option => option.value === initialLimit)) limit.add(new Option(Number(initialLimit).toLocaleString(), initialLimit));
    limit.value = initialLimit;
  }

  function updatePeriodFields() { customPeriods.forEach(period => { period.hidden = hours.value !== 'custom'; }); }
  updatePeriodFields();

  const map = L.map('map', { zoomControl: true });
  window.mytrackerMap = map;
  L.tileLayer(cfg.tileURL, { maxZoom: 19, attribution: cfg.tileAttr, referrerPolicy: 'strict-origin' }).addTo(map);
  map.setView(cfg.lastLat != null && cfg.lastLon != null ? [cfg.lastLat, cfg.lastLon] : [37.5665, 126.978], cfg.lastLat != null ? 13 : 11);
  let line = null, points = null, display = null, geofenceValues = [], geofences = L.layerGroup().addTo(map), controller = null, geofenceController = null, identityController = null, identityPending = false, identityDebounce = null, debounce = null, loaded = false, initialFitPending = true, fitPending = false;
  let refreshPending = false, stopped = false;
  const verification = ui.createVerificationDialog?.({ csrf: cfg.csrf });

  function setGeofenceError(message) {
    const state = document.getElementById('geofence-state');
    if (!state) return;
    state.hidden = !message;
    state.replaceChildren();
    if (message) {
      const text = document.createElement('span'); text.textContent = message;
      const retry = document.createElement('button'); retry.type = 'button'; retry.className = 'icon-button'; retry.setAttribute('aria-label', 'Retry geofences'); retry.innerHTML = '<i data-lucide="refresh-cw"></i>';
      retry.onclick = event => ui.withButton(event.currentTarget, loadGeofences);
      state.append(text, retry);
      ui.icons?.(state);
    }
  }
  async function loadGeofences() {
    geofenceController?.abort();
    const current = geofenceController = new AbortController();
    try {
      const values = await ui.request('/api/geofences', { signal: current.signal });
      if (current !== geofenceController) return;
      geofenceValues = values;
      renderGeofences();
      setGeofenceError('');
    } catch (error) {
      if (!ui.isCanceled?.(error) && current === geofenceController) setGeofenceError('Geofences unavailable. Retry to load them.');
    } finally { if (current === geofenceController) geofenceController = null; }
  }
  async function loadIdentities() {
    if (identityController) { identityPending = true; return; }
    const current = identityController = new AbortController();
    try {
      const response = await ui.request('/api/identities', { signal: current.signal });
      if (current !== identityController) return;
      const values = Array.isArray(response) ? response : response?.data;
      if (!Array.isArray(values)) return;
      identities = values;
      const selectedUser = user.value;
      const selectedDevice = device.value;
      fillUsers(selectedUser);
      fillDevices(selectedDevice);
    } catch (error) {
      if (!ui.isCanceled?.(error) && current === identityController) return;
    } finally {
      if (current === identityController) {
        identityController = null;
        if (identityPending && !stopped) {
          identityPending = false;
          scheduleIdentities();
        }
      }
    }
  }
  function scheduleIdentities() {
    clearTimeout(identityDebounce);
    identityDebounce = setTimeout(() => {
      identityDebounce = null;
      loadIdentities();
    }, 250);
  }
  function renderGeofences() {
    geofences.clearLayers();
    geofenceValues.filter(x => x.enabled).forEach(x => {
      const lon = display?.longitude(x.lon) ?? x.lon;
      const label = document.createElement('span'); label.textContent = x.name;
      L.circle([x.lat, lon], { radius: x.radius_m, color: '#c48a18', weight: 2, fillOpacity: .08 }).bindTooltip(label).addTo(geofences);
    });
  }
  function overlay(message, icon = 'info', hide = false, retry) {
    const state = document.getElementById('map-state');
    state.hidden = hide;
    if (!hide) {
      state.innerHTML = `<i data-lucide="${icon}"${icon === 'loader-circle' ? ' class="spin"' : ''}></i><span></span>`;
      state.querySelector('span').textContent = message;
      if (retry) {
        const button = document.createElement('button'); button.type = 'button'; button.className = 'icon-button'; button.setAttribute('aria-label', 'Retry track'); button.innerHTML = '<i data-lucide="refresh-cw"></i>';
        button.onclick = event => ui.withButton(event.currentTarget, retry);
        state.append(button);
      }
    }
    ui.icons?.(state);
  }
  function clear() {
    if (line) map.removeLayer(line);
    if (points) map.removeLayer(points);
    line = points = null;
  }
  function query() {
    const params = new URLSearchParams();
    if (hours.value === 'custom') {
      const startPeriod = periodSeconds(from, 'from');
      const endPeriod = periodSeconds(to, 'to');
      const start = startPeriod.value;
      const end = endPeriod.value;
      if (!Number.isFinite(start) && !Number.isFinite(end)) { overlay('Set a start or end time for the custom period', 'calendar-x'); return null; }
      if ((Number.isFinite(start) && start < 0) || (Number.isFinite(end) && end < 0)) { overlay('Custom period dates must be valid', 'calendar-x'); return null; }
      if (Number.isFinite(start) && Number.isFinite(end) && start > end) { overlay('The custom period start must be before its end', 'calendar-x'); return null; }
      if (Number.isFinite(start)) params.set('from', startPeriod.raw);
      if (Number.isFinite(end)) params.set('to', endPeriod.raw);
    } else if (hours.value !== '0') params.set('hours', hours.value);
    if (user.value) params.set('user', user.value);
    if (device.value) params.set('device', device.value);
    params.set('limit', limit.value);
    const persisted = new URLSearchParams(params);
    persisted.set('hours', hours.value);
    history.replaceState(null, '', `?${persisted}`);
    return params;
  }
  function proofButton(properties) {
    const box = document.createElement('div');
    const title = document.createElement('strong'); title.textContent = properties.time;
    const identity = document.createElement('div'); identity.textContent = `${properties.user}/${properties.device}`;
    const proof = document.createElement('div'); proof.textContent = `TSA: ${properties.tsa_status} · OTS: ${properties.ots_status}`;
    const button = document.createElement('button'); button.type = 'button'; button.className = 'popup-action'; button.textContent = 'Proof details';
    button.onclick = () => verification?.open(properties.id);
    box.append(title, identity, proof, button);
    return box;
  }
  async function loadTrack() {
    clearTimeout(debounce);
    refreshPending = false;
    controller?.abort();
    controller = null;
    const params = query();
    if (!params) { clear(); loaded = false; return; }
    controller = new AbortController();
    const current = controller;
    overlay(loaded ? 'Refreshing track' : 'Loading track', 'loader-circle');
    try {
      const collection = await ui.request(`/api/track?${params}`, { signal: current.signal });
      if (current !== controller) return;
      clear();
      const feature = collection.features.find(item => item.properties?.kind === 'track');
      const locations = collection.features.filter(item => item.properties?.kind === 'point');
      const metadata = collection.metadata || {};
      const total = metadata.total_points ?? locations.length;
      const shown = metadata.returned_points ?? locations.length;
      document.getElementById('info').textContent = metadata.sampled ? `${shown.toLocaleString()} of ${total.toLocaleString()} points` : `${shown.toLocaleString()} points`;
      if (!feature?.geometry || !feature.geometry.coordinates?.length) {
        display = null;
        renderGeofences();
        overlay('No locations match these filters', 'map-pin-off'); loaded = true; ui.setUpdated(); return;
      }
      const styles = getComputedStyle(document.documentElement);
      display = window.mytrackerMapGeometry.create(feature.geometry, locations, map.getCenter().lng);
      line = L.featureGroup(display.paths.map(coords => L.polyline(coords, { color: styles.getPropertyValue('--map-line').trim(), weight: 4, opacity: .88 }))).addTo(map);
      renderGeofences();
      points = L.layerGroup();
      const markers = locations.map((item, index) => {
        const [lat, lon] = display.points[index] || [item.geometry.coordinates[1], item.geometry.coordinates[0]];
        const endpoint = index === 0 || index === locations.length - 1;
        const marker = L.circleMarker([lat, lon], { radius: endpoint ? 7 : 4, color: endpoint ? (index === 0 ? '#23844d' : '#c43a42') : styles.getPropertyValue('--map-line').trim(), fillColor: styles.getPropertyValue('--surface').trim(), fillOpacity: .95, weight: 2 })
          .bindPopup(proofButton(item.properties)).addTo(points);
        marker.on('popupopen', () => marker.getPopup()?.getElement()?.querySelector('.popup-action')?.focus());
        return marker;
      });
      points.addTo(map);
      markers.forEach((marker, index) => {
        const element = marker.getElement();
        if (!element) return;
        const properties = locations[index].properties || {};
        element.setAttribute('tabindex', '0');
        element.setAttribute('role', 'button');
        element.setAttribute('focusable', 'true');
        element.setAttribute('aria-label', `Location ${properties.time || index + 1}; open proof details`);
        element.addEventListener('keydown', event => {
          if (event.key !== 'Enter' && event.key !== ' ') return;
          event.preventDefault();
          marker.openPopup();
        });
      });
      if (initialFitPending || fitPending) {
        map.fitBounds(display.bounds || line.getBounds(), { padding: [40, 40] });
        initialFitPending = false;
        fitPending = false;
      }
      loaded = true; overlay('', 'info', true); ui.setUpdated();
    } catch (error) {
      if (current !== controller || ui.isCanceled?.(error)) return;
      overlay('Track unavailable. Retry to load it.', 'circle-alert', false, loadTrack);
    } finally {
      if (current === controller) {
        controller = null;
        if (refreshPending && !current.signal.aborted) refreshTrack();
      }
    }
  }
  function refreshTrack() {
    if (stopped || ui.isPaused?.()) { refreshPending = false; return; }
    if (controller) { refreshPending = true; return; }
    loadTrack();
  }

  document.getElementById('reload').onclick = event => ui.withButton(event.currentTarget, loadTrack);
  document.getElementById('fit-track').onclick = () => {
    if (controller) { fitPending = true; return; }
    if (line) {
      map.fitBounds(display?.bounds || line.getBounds(), { padding: [40, 40] });
      initialFitPending = false;
      fitPending = false;
      return;
    }
    fitPending = true;
    loadTrack();
  };
  hours.onchange = () => { updatePeriodFields(); loadTrack(); };
  user.onchange = () => { fillDevices(''); loadTrack(); };
  device.onchange = () => loadTrack();
  from.oninput = () => { periodEdited.from = true; };
  to.oninput = () => { periodEdited.to = true; };
  from.onchange = () => { periodEdited.from = true; if (hours.value === 'custom') loadTrack(); };
  to.onchange = () => { periodEdited.to = true; if (hours.value === 'custom') loadTrack(); };
  limit.onchange = () => loadTrack();
  document.getElementById('filter-toggle').onclick = event => {
    const open = event.currentTarget.getAttribute('aria-expanded') !== 'true';
    event.currentTarget.setAttribute('aria-expanded', String(open));
    document.getElementById('filter-fields').classList.toggle('open', open);
  };
  let streamOpened = false;
  const stopStream = ui.stream('/api/events', {
    onOpen: () => {
      if (streamOpened) refreshTrack();
      streamOpened = true;
      loadIdentities();
      loadGeofences();
      verification?.refresh?.();
    },
    onSafety: () => { refreshTrack(); loadIdentities(); loadGeofences(); verification?.refresh?.(); },
    onEvent(change) {
      verification?.handleChange(change);
      if ((change.topics || []).includes('locations')) {
        clearTimeout(debounce);
        debounce = setTimeout(refreshTrack, 500);
        scheduleIdentities();
      }
      if ((change.topics || []).includes('geofences')) loadGeofences();
    }
  });
  ui.onResume?.(() => { loadTrack(); loadIdentities(); loadGeofences(); });
  window.addEventListener('pagehide', () => {
    stopped = true;
    refreshPending = false;
    stopStream?.();
    controller?.abort();
    geofenceController?.abort();
    identityController?.abort();
    verification?.stop?.();
    clearTimeout(debounce);
    clearTimeout(identityDebounce);
  });
  loadTrack();
  loadGeofences();
})();
