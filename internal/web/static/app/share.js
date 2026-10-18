(function () {
  const cfg = JSON.parse(document.getElementById('cfg').textContent);
  const ui = window.mytrackerUI;
  const base = location.pathname.replace(/\/$/, '');
  const map = L.map('map');
  window.mytrackerMap = map;
  L.tileLayer(cfg.tileURL, { maxZoom: 19, attribution: cfg.tileAttr, referrerPolicy: 'strict-origin' }).addTo(map);
  map.setView([cfg.lat != null ? cfg.lat : 37.5665, cfg.lon != null ? cfg.lon : 126.978], cfg.lat != null ? 13 : 11);
  let line = null, points = null, display = null, controller = null, loaded = false, debounce = null, initialFitPending = true, fitPending = false, unavailable = false, stopStream = null, refreshPending = false, stopped = false;

  function overlay(message, icon = 'info', hide = false, retry) {
    const state = document.getElementById('map-state'); state.hidden = hide;
    if (!hide) {
      state.innerHTML = `<i data-lucide="${icon}"${icon === 'loader-circle' ? ' class="spin"' : ''}></i><span></span>`;
      state.querySelector('span').textContent = message;
      if (retry) {
        const button = document.createElement('button'); button.type = 'button'; button.className = 'icon-button'; button.setAttribute('aria-label', 'Retry track'); button.innerHTML = '<i data-lucide="refresh-cw"></i>';
        button.onclick = event => ui.withButton(event.currentTarget, retry);
        state.append(button);
      }
      ui.icons?.(state);
    }
  }
  function clear() {
    if (line) map.removeLayer(line); if (points) map.removeLayer(points); line = points = null;
  }
  function pointMarker(item, index, lastIndex, styles, coordinate) {
    const [lat, lon] = coordinate || [item.geometry.coordinates[1], item.geometry.coordinates[0]];
    const properties = item.properties || {};
    const current = typeof properties.current === 'boolean' ? properties.current : index === lastIndex;
    const marker = L.circleMarker([lat, lon], { radius: current ? 8 : 4, color: current ? '#c43a42' : styles.getPropertyValue('--map-line').trim(), fillColor: styles.getPropertyValue('--surface').trim(), fillOpacity: .95, weight: 2 });
    const popup = document.createElement('div');
    const title = document.createElement('strong'); title.textContent = current ? 'Current' : 'Location';
    const time = document.createElement('div'); time.textContent = properties.time || '';
    popup.append(title, time);
    popup.tabIndex = -1;
    marker.bindPopup(popup);
    marker.on('popupopen', () => marker.getPopup()?.getElement()?.querySelector('[tabindex="-1"]')?.focus());
    return marker;
  }
  function markKeyboard(marker, item, index) {
    const element = marker.getElement();
    if (!element) return;
    const properties = item.properties || {};
    element.setAttribute('tabindex', '0');
    element.setAttribute('role', 'button');
    element.setAttribute('focusable', 'true');
    element.setAttribute('aria-label', `Location ${properties.time || index + 1}; open details`);
    element.addEventListener('keydown', event => {
      if (event.key !== 'Enter' && event.key !== ' ') return;
      event.preventDefault(); marker.openPopup();
    });
  }
  async function loadTrack() {
    clearTimeout(debounce);
    refreshPending = false;
    if (unavailable) return;
    controller?.abort();
    controller = null;
    const current = controller = new AbortController();
    overlay(loaded ? 'Refreshing' : 'Loading shared track', 'loader-circle');
    try {
      const collection = await ui.request(`${base}/api/track`, { signal: current.signal });
      if (current !== controller || unavailable) return;
      clear();
      const feature = collection.features.find(item => item.properties?.kind === 'track');
      const locations = collection.features.filter(item => item.properties?.kind === 'point');
      if (!feature?.geometry || !feature.geometry.coordinates?.length) {
        display = null;
        overlay('No recent locations', 'map-pin-off'); loaded = true; ui.setUpdated(); return;
      }
      const styles = getComputedStyle(document.documentElement);
      display = window.mytrackerMapGeometry.create(feature.geometry, locations, map.getCenter().lng);
      line = L.featureGroup(display.paths.map(coords => L.polyline(coords, { color: styles.getPropertyValue('--map-line').trim(), weight: 4, opacity: .88 }))).addTo(map);
      points = L.layerGroup();
      const markers = locations.map((item, index) => pointMarker(item, index, locations.length - 1, styles, display.points[index]).addTo(points));
      points.addTo(map);
      markers.forEach((marker, index) => markKeyboard(marker, locations[index], index));
      if (initialFitPending || fitPending) {
        map.fitBounds(display.bounds || line.getBounds(), { padding: [40, 40] });
        initialFitPending = false;
        fitPending = false;
      }
      loaded = true; overlay('', 'info', true); ui.setUpdated();
    } catch (error) {
      if (current !== controller || ui.isCanceled?.(error)) return;
      if (error.status === 401 || error.status === 404 || error.status === 410) {
        unavailable = true;
        clear();
        document.getElementById('fit-track').disabled = true;
        stopStream?.();
        ui.setConnection('unavailable');
        overlay('This share is no longer available', 'circle-alert');
        return;
      }
      overlay('Shared track unavailable. Retry to load it.', 'circle-alert', false, loadTrack);
    } finally {
      if (current === controller) {
        controller = null;
        if (refreshPending && !current.signal.aborted) refreshTrack();
      }
    }
  }
  function refreshTrack() {
    if (stopped || unavailable || ui.isPaused?.()) {
      refreshPending = false;
      return;
    }
    if (controller) {
      refreshPending = true;
      return;
    }
    loadTrack();
  }

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
  let streamOpened = false;
  stopStream = ui.stream(`${base}/api/events`, {
    event: 'track',
    onOpen: () => {
      if (streamOpened && !unavailable) refreshTrack();
      streamOpened = true;
    },
    onSafety: () => refreshTrack(),
    onEvent() { if (unavailable) return; clearTimeout(debounce); debounce = setTimeout(refreshTrack, 500); }
  });
  loadTrack();
  window.addEventListener('pagehide', () => {
    stopped = true;
    refreshPending = false;
    stopStream?.();
    controller?.abort();
    clearTimeout(debounce);
  });
})();
