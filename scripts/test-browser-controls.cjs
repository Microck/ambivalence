// No browser/device required: exercise the shipped host against a track that implements
// Image Capture constraints, including browsers that silently ignore a constraint.
const { readFileSync } = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const { test } = require('node:test');
const html = readFileSync('internal/hub/web/index.html', 'utf8');
const source = html.slice(html.indexOf('  // Image Capture exposureTime'), html.indexOf('  function captureJpeg'));
function host(ignore = false) {
  const caps = { focusMode: ['manual', 'continuous'], focusDistance: { min: 0.1, max: 100, step: 0.01 },
    exposureMode: ['manual', 'continuous'], exposureTime: { min: 1, max: 10000, step: 1 }, iso: { min: 50, max: 1600, step: 1 },
    exposureCompensation: { min: -2, max: 2, step: 0.1 }, whiteBalanceMode: ['manual', 'continuous'], colorTemperature: { min: 2000, max: 8000, step: 1 } };
  const settings = { focusMode: 'continuous', exposureMode: 'continuous', whiteBalanceMode: 'continuous' };
  let calls = 0;
  const track = { getCapabilities: () => caps, getSettings: () => settings,
    applyConstraints: async c => { calls++; if (!ignore) Object.assign(settings, c.advanced[0]); } };
  const context = vm.createContext({ stream: { getVideoTracks: () => [track] } });
  vm.runInContext(source, context);
  return { context, caps, settings, calls: () => calls };
}
test('seconds convert to browser units; settings convert back', async () => {
  const h = host();
  await h.context.applyControls({ exposureMode: 'manual', exposureTime: 1 / 125, iso: 100 });
  assert.equal(h.settings.exposureTime, 80);
  assert.equal(h.context.controlSettings().exposureTime, 1 / 125);
  assert.equal(h.context.controlCapabilities(h.context.stream.getVideoTracks()[0]).exposureTime.max, 1);
});
test('focus endpoints, white balance, group preservation and auto reset', async () => {
  const h = host();
  await h.context.applyControls({ focusMode: 'manual', focusPosition: 0 });
  assert.equal(h.settings.focusDistance, 100);
  await h.context.applyControls({ focusMode: 'manual', focusPosition: 1 });
  assert.ok(Math.abs(h.settings.focusDistance - 0.1) < 1e-8);
  await h.context.applyControls({ whiteBalanceMode: 'manual', whiteBalanceTemperature: 5600 });
  assert.equal(h.context.controlSettings().focusPosition, 1);
  await h.context.applyControls({ focusMode: 'auto', whiteBalanceMode: 'auto' });
  assert.equal(h.settings.focusMode, 'continuous');
  assert.equal(h.settings.whiteBalanceMode, 'continuous');
});
test('invalid, unsupported and incomplete controls never reach hardware', async () => {
  const h = host();
  for (const c of [{ focusMode: 'manual', focusPosition: 2 }, { exposureMode: 'manual', iso: 100 },
    { exposureMode: 'auto', exposureTime: 0.1 }, { focusPoint: { x: 0.5, y: 0.5 } },
    { whiteBalanceMode: 'manual', whiteBalanceGains: { red: 1, green: 1, blue: 1 } },
    { exposureCompensation: NaN }, { focusMode: 'bogus' }, { unknown: 1 }]) await assert.rejects(h.context.applyControls(c));
  assert.equal(h.calls(), 0);
});
test('ignored advanced constraints return an error', async () => {
  const h = host(true);
  await assert.rejects(h.context.applyControls({ exposureMode: 'manual', exposureTime: 0.01, iso: 100 }), /not applied/);
});
test('lens selection requests an exact device ID; aliases use facing hints', async () => {
  let constraints, stopped = false;
  const track = { stop: () => { stopped = true; }, getSettings: () => ({ deviceId: 'tele-id', facingMode: 'environment' }) };
  const stream = { getTracks: () => [track], getVideoTracks: () => [track] };
  const context = vm.createContext({ navigator: { mediaDevices: { getUserMedia: async c => { constraints = c; return stream; } } },
    window: { isSecureContext: true }, stream, facing: 'environment', video: { play: async () => {} }, setTorch: () => {}, zoomLevel: 2 });
  vm.runInContext(html.slice(html.indexOf('  async function startCamera'), html.indexOf('  async function applyTorch')), context);
  await context.startCamera('tele-id');
  assert.equal(constraints.video.deviceId.exact, 'tele-id');
  assert.equal(constraints.video.facingMode, undefined);
  assert.equal(stopped, true);
  assert.equal(context.zoomLevel, 1);
  await context.startCamera();
  assert.equal(constraints.video.facingMode.ideal, 'environment');
  assert.equal(constraints.video.deviceId, undefined);
});
