(function () {
  const fullTurn = 360;

  function normalize(lon) {
    return ((lon + 180) % fullTurn + fullTurn) % fullTurn - 180;
  }

  function pathsFor(geometry) {
    if (!geometry || !Array.isArray(geometry.coordinates)) return [];
    return geometry.type === 'MultiLineString' ? geometry.coordinates : [geometry.coordinates];
  }

  function coordinateOf(item) {
    const value = item?.geometry?.coordinates;
    return Array.isArray(value) && value.length >= 2 && Number.isFinite(Number(value[0])) && Number.isFinite(Number(value[1]))
      ? [Number(value[0]), Number(value[1])]
      : null;
  }

  function originFor(longitudes) {
    if (!longitudes.length) return -180;
    const sorted = [...new Set(longitudes.map(normalize))].sort((a, b) => a - b);
    if (sorted.length === 1) return sorted[0];
    let largest = -1;
    let index = 0;
    sorted.forEach((value, i) => {
      const next = i + 1 < sorted.length ? sorted[i + 1] : sorted[0] + fullTurn;
      const gap = next - value;
      if (gap > largest) { largest = gap; index = (i + 1) % sorted.length; }
    });
    return sorted[index];
  }

  function create(geometry, locations = [], preferredLongitude) {
    const sourcePaths = pathsFor(geometry).map(path => (Array.isArray(path) ? path : []).map(value => {
      const lon = Number(value?.[0]);
      const lat = Number(value?.[1]);
      return Number.isFinite(lon) && Number.isFinite(lat) ? [lon, lat] : null;
    }).filter(Boolean));
    const sourcePoints = locations.map(coordinateOf);
    const longitudes = sourcePaths.flat().map(value => value[0]).concat(sourcePoints.filter(Boolean).map(value => value[0]));
    const origin = originFor(longitudes);
    const canonical = lon => {
      let value = normalize(Number(lon));
      while (value < origin) value += fullTurn;
      while (value >= origin + fullTurn) value -= fullTurn;
      return value;
    };
    const canonicalLongitudes = longitudes.map(canonical);
    const arcCenter = canonicalLongitudes.length
      ? (Math.min(...canonicalLongitudes) + Math.max(...canonicalLongitudes)) / 2
      : Number.isFinite(preferredLongitude) ? preferredLongitude : origin;
    const worldShift = canonicalLongitudes.length && Number.isFinite(preferredLongitude)
      ? Math.round((preferredLongitude - arcCenter) / fullTurn) * fullTurn
      : 0;
    const displayCenter = arcCenter + worldShift;
    const toDisplayLongitude = lon => {
      const value = normalize(Number(lon));
      return value + Math.round((displayCenter - value) / fullTurn) * fullTurn;
    };
    const windowStart = origin + worldShift;
    const windowEnd = windowStart + fullTurn;
    const splitPath = path => {
      if (!path.length) return [];
      const display = path.map(([lon, lat]) => [lat, toDisplayLongitude(lon)]);
      const segments = [[display[0]]];
      for (let index = 1; index < display.length; index++) {
        const previous = display[index - 1];
        const current = display[index];
        const delta = current[1] - previous[1];
        if (Math.abs(delta) <= 180) {
          segments[segments.length - 1].push(current);
          continue;
        }
        const target = current[1] + (delta > 0 ? -fullTurn : fullTurn);
        const boundary = delta > 0 ? windowStart : windowEnd;
        const fraction = (boundary - previous[1]) / (target - previous[1]);
        const boundaryLat = previous[0] + (current[0] - previous[0]) * fraction;
        const segment = segments[segments.length - 1];
        segment.push([boundaryLat, boundary]);
        segments.push([[boundaryLat, delta > 0 ? windowEnd : windowStart], current]);
      }
      return segments;
    };
    const displayPaths = sourcePaths.flatMap(splitPath);
    const displayPoints = sourcePoints.map(value => value && [value[1], toDisplayLongitude(value[0])]);
    const all = displayPaths.flat().concat(displayPoints.filter(Boolean));
    let bounds = null;
    if (all.length) {
      const latitudes = all.map(value => value[0]);
      const longitudes = all.map(value => value[1]);
      bounds = [[Math.min(...latitudes), Math.min(...longitudes)], [Math.max(...latitudes), Math.max(...longitudes)]];
    }
    return { paths: displayPaths, points: displayPoints, bounds, longitude: toDisplayLongitude };
  }

  window.mytrackerMapGeometry = { create };
})();
