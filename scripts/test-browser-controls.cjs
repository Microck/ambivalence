// No browser/device required: exercise the shipped host against a track that implements
// Image Capture constraints, including browsers that silently ignore a constraint.
const { readFileSync } = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const { test } = require('node:test');
const html = readFileSync('internal/hub/web/index.html', 'utf8');
const source = html.slice(html.indexOf('  async function applyTrackChanges'), html.indexOf('  function captureJpeg'));
function host(ignore = false) {
  const caps = { focusMode: ['manual', 'continuous'], focusDistance: { min: 0.1, max: 100, step: 0.01 },
    exposureMode: ['manual', 'continuous'], exposureTime: { min: 1, max: 10000, step: 1 }, iso: { min: 50, max: 1600, step: 1 },
    exposureCompensation: { min: -2, max: 2, step: 0.1 }, whiteBalanceMode: ['manual', 'continuous'], colorTemperature: { min: 2000, max: 8000, step: 1 } };
  const defaults = { focusMode: 'continuous', exposureMode: 'continuous', whiteBalanceMode: 'continuous' };
  const settings = { ...defaults };
  let constraints = { deviceId: { exact: 'tele-id' }, width: { ideal: 1920 } };
  let calls = 0;
  // Like MediaStreamTrack, this fixture replaces constraints and restores omitted
  // properties to defaults, rather than treating applyConstraints as a patch.
  const track = { getCapabilities: () => caps, getSettings: () => settings, getConstraints: () => constraints,
    applyConstraints: async c => {
      calls++;
      constraints = c;
      if (!ignore) {
        for (const key of Object.keys(settings)) delete settings[key];
        Object.assign(settings, defaults, c.advanced[0]);
      }
    } };
  const context = vm.createContext({ stream: { getVideoTracks: () => [track] } });
  vm.runInContext(source, context);
  return { context, caps, settings, track, calls: () => calls };
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
test('independent groups, torch and zoom retain constraints; auto clears only its group', async () => {
  const h = host();
  await h.context.applyControls({ focusMode: 'manual', focusPosition: 0 });
  await h.context.applyControls({ exposureMode: 'manual', exposureTime: 0.01, iso: 100 });
  await h.context.applyControls({ whiteBalanceMode: 'manual', whiteBalanceTemperature: 5600 });
  await h.context.applyTrackChanges(h.track, { torch: true });
  await h.context.applyTrackChanges(h.track, { zoom: 2 });
  assert.equal(h.settings.focusMode, 'manual');
  assert.equal(h.settings.focusDistance, 100);
  assert.equal(h.settings.exposureTime, 100);
  assert.equal(h.settings.colorTemperature, 5600);
  assert.equal(h.settings.torch, true);
  assert.equal(h.settings.zoom, 2);
  assert.equal(h.track.getConstraints().deviceId.exact, 'tele-id');
  assert.equal(h.track.getConstraints().width.ideal, 1920);
  await h.context.applyControls({ exposureMode: 'auto' });
  assert.equal(h.settings.exposureMode, 'continuous');
  assert.equal(h.settings.exposureTime, undefined);
  assert.equal(h.settings.iso, undefined);
  assert.equal(h.settings.exposureCompensation, 0);
  assert.equal(h.settings.focusMode, 'manual');
  assert.equal(h.settings.whiteBalanceMode, 'manual');
  assert.equal(h.settings.torch, true);
});
test('explicit exposure modes clear a prior EV bias', async () => {
  const h = host();
  await h.context.applyControls({ exposureMode: 'auto', exposureCompensation: 1 });
  await h.context.applyControls({ exposureMode: 'manual', exposureTime: 0.01, iso: 100 });
  assert.equal(h.settings.exposureCompensation, 0);
  await h.context.applyControls({ exposureMode: 'auto', exposureCompensation: 1 });
  await h.context.applyControls({ exposureMode: 'auto' });
  assert.equal(h.settings.exposureCompensation, 0);
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
test('failed camera acquisition clears the stopped stream and rejects frozen captures', async () => {
  let stopped = false;
  const track = { readyState: 'live', getSettings: () => ({ deviceId: 'old-camera' }), stop: () => { stopped = true; } };
  const stream = { getTracks: () => [track], getVideoTracks: () => [track] };
  const context = vm.createContext({ navigator: { mediaDevices: { getUserMedia: async () => { throw new Error('camera unavailable'); } } },
    window: { isSecureContext: true }, stream, facing: 'environment', video: { srcObject: stream, videoWidth: 1920, videoHeight: 1080 },
    setTorch: () => {}, zoomLevel: 2 });
  vm.runInContext(html.slice(html.indexOf('  async function startCamera'), html.indexOf('  async function applyTorch')), context);
  vm.runInContext(html.slice(html.indexOf('  function captureJpeg'), html.indexOf('  const send =')), context);
  await assert.rejects(context.startCamera('gone-camera'), /camera unavailable/);
  assert.equal(stopped, true);
  assert.equal(context.stream, null);
  assert.equal(context.video.srcObject, null);
  assert.equal(context.zoomLevel, 1);
  await assert.rejects(context.captureJpeg(80, 320), /camera not ready/);
});
test('repeated active device and default selector keep the live stream and controls', async () => {
  let opens = 0, stops = 0, constraints;
  const flip = {};
  const settings = { deviceId: 'back-id', focusMode: 'manual', focusDistance: 10 };
  const track = { readyState: 'live', getSettings: () => settings, stop: () => { stops++; } };
  const stream = { getTracks: () => [track], getVideoTracks: () => [track] };
  const context = vm.createContext({ navigator: { mediaDevices: { getUserMedia: async c => { opens++; constraints = c; return stream; } } },
    window: { isSecureContext: true }, stream, cameraSelection: 'environment', facing: 'environment',
    video: { srcObject: stream, play: async () => {} }, setTorch: () => {}, zoomLevel: 2, $: () => flip });
  vm.runInContext(html.slice(html.indexOf('  async function startCamera'), html.indexOf('  async function applyTorch')), context);
  await context.startCamera();
  await context.startCamera('back-id');
  assert.equal(opens, 0);
  assert.equal(stops, 0);
  assert.equal(context.zoomLevel, 2);
  assert.equal(context.stream, stream);
  assert.equal(settings.focusMode, 'manual');
  // An alias after selecting another exact device must still select the default.
  context.cameraSelection = 'tele-id';
  context.facing = 'environment';
  await context.startCamera();
  assert.equal(opens, 1);
  assert.equal(stops, 1);
  // The flip button can use that selector even when facingMode is hidden.
  vm.runInContext(html.slice(html.indexOf('  $("flip").onclick'), html.indexOf('  torchBtn.onclick')), context);
  await flip.onclick();
  assert.equal(opens, 2);
  assert.equal(stops, 2);
  assert.equal(constraints.video.facingMode.ideal, 'user');
});

test('invalid hub URLs are reported without throwing or closing the current socket', () => {
  const connectSource = html.slice(html.indexOf('  function connect()'), html.indexOf('  $("connect").onclick'));
  for (const value of ['ws://', 'wss://example.test/#fragment', 'ws://example.test/v1#', 'ws://user:pass@example.test/v1']) {
    let status = '', closed = false;
    const context = vm.createContext({ URL, hubEl: { value, blur() {} },
      ws: { close() { closed = true; } }, setStatus(s) { status = s; },
      WebSocket: class { constructor() { throw new Error('must not open a socket'); } } });
    vm.runInContext(connectSource, context);
    assert.doesNotThrow(() => context.connect(), value);
    assert.match(status, /invalid hub URL/);
    assert.equal(closed, false);
  }
});

test('repeated same-size captures reuse the canvas but draw every frame', async () => {
  let width = 0, height = 0, resets = 0, draws = 0;
  const canvas = {
    get width() { return width; }, set width(v) { width = v; resets++; },
    get height() { return height; }, set height(v) { height = v; resets++; },
    getContext() { return { drawImage() { draws++; } }; },
    toBlob(callback) { callback({ size: 1 }); },
  };
  const context = vm.createContext({ canvas, video: { videoWidth: 1920, videoHeight: 1080 },
    stream: { getVideoTracks() { return [{ readyState: 'live' }]; } } });
  vm.runInContext(html.slice(html.indexOf('  function captureJpeg'), html.indexOf('  const send =')), context);
  const first = await context.captureJpeg(80, 1280);
  const second = await context.captureJpeg(80, 1280);
  assert.equal(first.w, 1280); assert.equal(second.h, 720);
  assert.equal(resets, 2); assert.equal(draws, 2);
  await context.captureJpeg(80, 640);
  assert.equal(width, 640); assert.equal(height, 360);
  assert.equal(resets, 4); assert.equal(draws, 3);
});

test('late close from a replaced socket does not reset the current link', () => {
  const sockets = [];
  let cancelled = 0;
  class Socket {
    constructor() { sockets.push(this); }
    close() {}
  }
  const context = vm.createContext({ URL, WebSocket: Socket, ws: null,
    hubEl: { value: 'ws://127.0.0.1:17842/v1', blur() {} }, setStatus() {},
    cancelConfirm() { cancelled++; }, agentHere: true, seal: { active: true },
    clearTimeout() {}, squatTimer: null, challenge: null, pairing: null,
    setTimeout() {}, send() {}, onMessage() {}, ident: { device: 'fixture' } });
  vm.runInContext(html.slice(html.indexOf('  function connect()'), html.indexOf('  $("connect").onclick')), context);
  context.connect(); context.connect();
  context.agentHere = true; context.seal = {active:true}; cancelled = 0;
  sockets[0].onclose();
  assert.equal(cancelled, 0); assert.equal(context.agentHere, true);
  assert.equal(context.seal.active, true);
});

test('replacement socket clears the previous peer authentication state', () => {
  const context = vm.createContext({ URL, WebSocket: class { close() {} },
    ws: {close() {}}, hubEl: {value:'ws://127.0.0.1:17842/v1',blur() {}},
    setStatus() {}, cancelConfirm() {}, clearTimeout() {}, setTimeout() {},
    agentHere:true, seal:{old:true}, challenge:{old:true}, pairing:{old:true}, squatTimer:123,
    ident:{device:'fixture'}, send() {}, onMessage() {} });
  vm.runInContext(html.slice(html.indexOf('  function connect()'),html.indexOf('  $("connect").onclick')),context);
  context.connect();
  assert.equal(context.agentHere,false);
  for(const field of ['seal','challenge','pairing']) assert.equal(context[field],null);
});
