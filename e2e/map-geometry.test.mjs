import assert from 'node:assert/strict';
import fs from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const context = { window: {} };
vm.runInNewContext(fs.readFileSync(new URL('../internal/web/static/app/map-geometry.js', import.meta.url), 'utf8'), context);
const create = context.window.mytrackerMapGeometry.create;

test('keeps dateline tracks and nearby geofences in one world copy', () => {
  const geometry = { type: 'LineString', coordinates: [[179.9, -17], [-179.9, -17]] };
  const locations = geometry.coordinates.map(coordinates => ({ geometry: { coordinates } }));
  const view = create(geometry, locations, -179.9);
  assert.ok(Math.abs(view.paths[0][1][1] - view.paths[0][0][1]) < 1);
  assert.equal(view.points[0][1], view.paths[0][0][1]);
  assert.equal(view.points[1][1], view.paths[0][1][1]);

  const normal = create({ type: 'LineString', coordinates: [[127, 37], [129, 37]] }, [], 127);
  assert.equal(normal.longitude(126), 126);

  const empty = create({ type: 'LineString', coordinates: [] }, [], 127);
  assert.equal(empty.longitude(126), 126);
});

test('unwraps long routes without drawing a reverse-world segment', () => {
  const geometry = { type: 'LineString', coordinates: [[127, 37.5], [-122, 37.8], [0, 51.5], [127, 37.5]] };
  const locations = geometry.coordinates.map((coordinates, index) => ({ geometry: { coordinates }, properties: { user: index % 2 ? 'b' : 'a' } }));
  const view = create(geometry, locations, 127);
  for (const path of view.paths) {
    for (let index = 1; index < path.length; index++) assert.ok(Math.abs(path[index][1] - path[index - 1][1]) <= 180);
  }
  locations.forEach((location, index) => assert.equal(view.points[index][1], view.longitude(location.geometry.coordinates[0])));
  assert.ok(view.bounds[1][1] - view.bounds[0][1] <= 360);
});
