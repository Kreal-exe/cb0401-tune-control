'use strict';
// Floor plan + live motion. Plan units are metres; y grows downward on
// screen (top view). Shared with view3d.js through window.sensing.

const S = window.sensing = {
  plan: { walls: [], router: null, devices: {}, names: {} },
  state: null,            // latest snapshot from /api/stream
  planListeners: [],      // called after every plan change
  view: '2d',
};

const canvas = document.getElementById('plan');
const ctx = canvas.getContext('2d');
const $ = (id) => document.getElementById(id);

// ---------------------------------------------------------------- view ---
let scale = 50;           // px per metre
let origin = { x: 0, y: 0 }; // screen px of world (0,0)
let editing = false;
let tool = 'wall';
let wallStart = null;     // [x,y] while drawing a chain of walls
let cursor = null;        // world position under the pointer
let placing = null;       // MAC waiting for a click to be placed
let dragging = null;      // {kind:'router'|'device', mac}
let panning = null;

const toWorld = (px, py) => [(px - origin.x) / scale, (py - origin.y) / scale];
const toScreen = (x, y) => [origin.x + x * scale, origin.y + y * scale];

function resize() {
  const r = canvas.getBoundingClientRect();
  const dpr = window.devicePixelRatio || 1;
  canvas.width = Math.round(r.width * dpr);
  canvas.height = Math.round(r.height * dpr);
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
}

function planBounds() {
  const pts = [];
  for (const w of S.plan.walls) pts.push([w[0], w[1]], [w[2], w[3]]);
  if (S.plan.router) pts.push(S.plan.router);
  for (const p of Object.values(S.plan.devices)) pts.push(p);
  if (!pts.length) return [-5, -4, 5, 4];
  let [x1, y1, x2, y2] = [Infinity, Infinity, -Infinity, -Infinity];
  for (const [x, y] of pts) { x1 = Math.min(x1, x); y1 = Math.min(y1, y); x2 = Math.max(x2, x); y2 = Math.max(y2, y); }
  return [x1 - 1, y1 - 1, x2 + 1, y2 + 1];
}

function fit() {
  const r = canvas.getBoundingClientRect();
  const [x1, y1, x2, y2] = planBounds();
  scale = Math.max(10, Math.min(r.width / Math.max(2, x2 - x1), r.height / Math.max(2, y2 - y1)) * 0.9);
  origin = { x: r.width / 2 - ((x1 + x2) / 2) * scale, y: r.height / 2 - ((y1 + y2) / 2) * scale };
}

// ------------------------------------------------------------- helpers ---
// The plan as drawn, or - before anything is placed - a stand-in: the router
// in the middle and every captured device 3 m around it, marked as not yet
// placed, so links and zones show from the first minute. Nothing is saved.
function effPlan() {
  if (S.plan.router) return { router: S.plan.router, devices: S.plan.devices, virtual: false };
  const devices = {};
  const links = ((S.state && S.state.links) || []).map((l) => l.mac).sort();
  links.forEach((mac, i) => { const a = (i / Math.max(1, links.length)) * Math.PI * 2 - Math.PI / 2; devices[mac] = [Math.round(Math.cos(a) * 30) / 10, Math.round(Math.sin(a) * 30) / 10]; });
  return { router: [0, 0], devices, virtual: true };
}
S.effPlan = effPlan;
const escapeHtml = (t) => String(t).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const linkOf = (mac) => (S.state && S.state.links || []).find((l) => l.mac === mac);
const stationOf = (mac) => (S.state && S.state.stations || []).find((s) => s.mac === mac);
function labelOf(mac) {
  const st = stationOf(mac);
  return S.plan.names[mac] || (st && st.name) || mac;
}
let frameT = performance.now();
function updateAnim() {
  const now = performance.now(), dt = Math.min(0.2, (now - frameT) / 1000); frameT = now;
  updateBalls(dt);
}

// Balls: the moving bodies the server's tracker reports, eased between
// updates, each leaving a fading trail of where it went.
S.balls = {};
let trailT = 0;
function updateBalls(dt) {
  const tr = S.state && S.state.track;
  const bodies = (tr && tr.bodies) || [];
  const seen = new Set();
  const now = performance.now();
  const addTrail = now - trailT > 150;
  if (addTrail) trailT = now;
  for (const b of bodies) {
    seen.add(b.id);
    let o = S.balls[b.id];
    if (!o) o = S.balls[b.id] = { x: b.x, y: b.y, k: 0, trail: [] };
    const a = Math.min(1, dt / 0.35);
    o.x += (b.x - o.x) * a; o.y += (b.y - o.y) * a;
    const target = Math.min(1, 0.35 + b.share);
    o.k += (target - o.k) * Math.min(1, dt / 0.3);
    o.spread = b.spread; o.speed = b.speed;
  }
  for (const [id, o] of Object.entries(S.balls)) {
    if (!seen.has(+id)) o.k -= o.k * Math.min(1, dt / 0.8);
    if (addTrail) { o.trail.push([o.x, o.y, now]); if (o.trail.length > 60) o.trail.shift(); }
    if (!seen.has(+id) && o.k < 0.02) delete S.balls[id];
  }
}
function distToSeg(p, a, b) {
  const [px, py] = p, [ax, ay] = a, [bx, by] = b;
  const dx = bx - ax, dy = by - ay, L = dx * dx + dy * dy;
  const t = L ? Math.max(0, Math.min(1, ((px - ax) * dx + (py - ay) * dy) / L)) : 0;
  return Math.hypot(px - ax - t * dx, py - ay - t * dy);
}

// snap: 10 cm grid, existing wall ends within 25 cm, near-horizontal/vertical from the start point
function snap(p, from) {
  let [x, y] = p;
  let best = null;
  for (const w of S.plan.walls) for (const e of [[w[0], w[1]], [w[2], w[3]]]) {
    const d = Math.hypot(e[0] - x, e[1] - y);
    if (d < 0.25 && (!best || d < best.d)) best = { d, e };
  }
  if (best) return best.e.slice();
  x = Math.round(x * 10) / 10; y = Math.round(y * 10) / 10;
  if (from) {
    const ang = Math.abs(Math.atan2(y - from[1], x - from[0]) * 180 / Math.PI);
    if (ang < 10 || ang > 170) y = from[1];
    else if (Math.abs(ang - 90) < 10) x = from[0];
  }
  return [x, y];
}

// --------------------------------------------------------------- saving ---
let saveTimer = null;
function planChanged(auto) {
  if (!auto) S.plan.auto = false;
  S.planListeners.forEach((f) => f());
  clearTimeout(saveTimer);
  $('saved').textContent = 'saving…';
  saveTimer = setTimeout(async () => {
    try {
      const r = await fetch('api/plan', { method: 'PUT', headers: { 'content-type': 'application/json' }, body: JSON.stringify(S.plan) });
      $('saved').textContent = r.ok ? 'saved' : 'not saved: ' + r.status;
    } catch (e) { $('saved').textContent = 'not saved'; }
  }, 400);
}

// --------------------------------------------------------------- drawing ---
function draw() {
  requestAnimationFrame(draw);
  updateAnim();
  if (S.view !== '2d') return;
  const r = canvas.getBoundingClientRect();
  ctx.clearRect(0, 0, r.width, r.height);

  // grid, 1 m
  ctx.lineWidth = 1;
  ctx.strokeStyle = '#141c26';
  const [wx1, wy1] = toWorld(0, 0), [wx2, wy2] = toWorld(r.width, r.height);
  for (let x = Math.floor(wx1); x <= wx2; x++) { const [sx] = toScreen(x, 0); ctx.beginPath(); ctx.moveTo(sx, 0); ctx.lineTo(sx, r.height); ctx.stroke(); }
  for (let y = Math.floor(wy1); y <= wy2; y++) { const [, sy] = toScreen(0, y); ctx.beginPath(); ctx.moveTo(0, sy); ctx.lineTo(r.width, sy); ctx.stroke(); }

  const EP = effPlan();
  const R = EP.router;

  // walls
  ctx.strokeStyle = '#c9d3de'; ctx.lineCap = 'round';
  ctx.lineWidth = Math.max(3, 0.12 * scale);
  for (const w of S.plan.walls) {
    const [ax, ay] = toScreen(w[0], w[1]), [bx, by] = toScreen(w[2], w[3]);
    ctx.beginPath(); ctx.moveTo(ax, ay); ctx.lineTo(bx, by); ctx.stroke();
  }
  if (editing && tool === 'wall' && wallStart && cursor) {
    const e = snap(cursor, wallStart);
    const [ax, ay] = toScreen(wallStart[0], wallStart[1]), [bx, by] = toScreen(e[0], e[1]);
    ctx.strokeStyle = 'rgba(201,211,222,0.5)';
    ctx.beginPath(); ctx.moveTo(ax, ay); ctx.lineTo(bx, by); ctx.stroke();
    ctx.fillStyle = '#8494a6'; ctx.font = '12px sans-serif';
    ctx.fillText(Math.hypot(e[0] - wallStart[0], e[1] - wallStart[1]).toFixed(1) + ' m', (ax + bx) / 2 + 6, (ay + by) / 2 - 6);
  }

  // moving bodies: a glowing ball each, with the trail of where it went
  for (const o of Object.values(S.balls)) {
    const now = performance.now();
    ctx.lineCap = 'round';
    for (let i = 1; i < o.trail.length; i++) {
      const [x1, y1] = toScreen(o.trail[i - 1][0], o.trail[i - 1][1]), [x2, y2] = toScreen(o.trail[i][0], o.trail[i][1]);
      const age = (now - o.trail[i][2]) / 9000;
      ctx.strokeStyle = `rgba(127,243,255,${Math.max(0, 0.5 * (1 - age)) * o.k})`;
      ctx.lineWidth = 3;
      ctx.beginPath(); ctx.moveTo(x1, y1); ctx.lineTo(x2, y2); ctx.stroke();
    }
    const [sx, sy] = toScreen(o.x, o.y);
    const rad = Math.max(18, Math.min(1.6, 0.35 + (o.spread || 0)) * scale);
    const g = ctx.createRadialGradient(sx, sy, 0, sx, sy, rad);
    g.addColorStop(0, `rgba(255,255,255,${0.95 * o.k})`);
    g.addColorStop(0.2, `rgba(127,243,255,${0.7 * o.k})`);
    g.addColorStop(1, 'rgba(127,243,255,0)');
    ctx.fillStyle = g; ctx.beginPath(); ctx.arc(sx, sy, rad, 0, Math.PI * 2); ctx.fill();
    ctx.fillStyle = `rgba(240,253,255,${o.k})`; ctx.beginPath(); ctx.arc(sx, sy, 7, 0, Math.PI * 2); ctx.fill();
  }

  // devices
  ctx.font = '12px sans-serif';
  for (const [mac, p] of Object.entries(EP.devices)) {
    const [sx, sy] = toScreen(p[0], p[1]);
    const l = linkOf(mac);
    ctx.fillStyle = l && !l.stale ? '#5ee0c1' : '#5a6878';
    ctx.beginPath(); ctx.arc(sx, sy, 7, 0, Math.PI * 2); ctx.fill();
    ctx.fillStyle = '#dde6ef'; ctx.fillText(labelOf(mac) + (EP.virtual ? ' (not placed)' : ''), sx + 10, sy + 4);
  }
  // router
  if (R) {
    const [sx, sy] = toScreen(R[0], R[1]);
    ctx.fillStyle = '#f2b35b'; ctx.fillRect(sx - 9, sy - 9, 18, 18);
    ctx.fillStyle = '#0b0f14'; ctx.font = 'bold 12px sans-serif'; ctx.fillText('R', sx - 4, sy + 4);
    ctx.fillStyle = '#dde6ef'; ctx.font = '12px sans-serif'; ctx.fillText('router', sx + 12, sy + 4);
  }
  // placing preview
  if (editing && placing && cursor) {
    const [sx, sy] = toScreen(cursor[0], cursor[1]);
    ctx.strokeStyle = '#5ee0c1'; ctx.beginPath(); ctx.arc(sx, sy, 9, 0, Math.PI * 2); ctx.stroke();
  }
}

// ---------------------------------------------------------------- input ---
function pointerWorld(ev) {
  const r = canvas.getBoundingClientRect();
  return toWorld(ev.clientX - r.left, ev.clientY - r.top);
}
function hitMarker(p) {
  if (S.plan.router && Math.hypot(S.plan.router[0] - p[0], S.plan.router[1] - p[1]) < 14 / scale) return { kind: 'router' };
  for (const [mac, q] of Object.entries(S.plan.devices)) if (Math.hypot(q[0] - p[0], q[1] - p[1]) < 12 / scale) return { kind: 'device', mac };
  return null;
}

canvas.addEventListener('pointerdown', (ev) => {
  const p = pointerWorld(ev);
  cursor = p;
  if (!editing) { panning = { x: ev.clientX, y: ev.clientY, o: { ...origin } }; canvas.setPointerCapture(ev.pointerId); return; }
  if (placing) { S.plan.devices[placing] = snap(p); placing = null; setHint(); planChanged(); renderDevices(); return; }
  if (tool === 'wall') {
    const q = snap(p, wallStart);
    if (!wallStart) { wallStart = q; setHint(); return; }
    if (Math.hypot(q[0] - wallStart[0], q[1] - wallStart[1]) > 0.05) { S.plan.walls.push([wallStart[0], wallStart[1], q[0], q[1]]); planChanged(); }
    wallStart = q; return;
  }
  if (tool === 'router') { S.plan.router = snap(p); planChanged(); return; }
  if (tool === 'move') {
    dragging = hitMarker(p);
    if (!dragging) panning = { x: ev.clientX, y: ev.clientY, o: { ...origin } };
    canvas.setPointerCapture(ev.pointerId); return;
  }
  if (tool === 'scale') {
    let best = -1, bd = 14 / scale;
    S.plan.walls.forEach((w, i) => { const d = distToSeg(p, [w[0], w[1]], [w[2], w[3]]); if (d < bd) { bd = d; best = i; } });
    if (best < 0) return;
    const w = S.plan.walls[best], len = Math.hypot(w[2] - w[0], w[3] - w[1]);
    const v = prompt(`This wall is drawn ${len.toFixed(1)} m long. How long is it really, in metres?`, len.toFixed(1));
    const real = v && parseFloat(v.replace(',', '.'));
    if (!real || real <= 0 || !len) return;
    const f = real / len, o = S.plan.router || [w[0], w[1]];
    const sc = (q) => [Math.round((o[0] + (q[0] - o[0]) * f) * 100) / 100, Math.round((o[1] + (q[1] - o[1]) * f) * 100) / 100];
    S.plan.walls = S.plan.walls.map((x) => [...sc([x[0], x[1]]), ...sc([x[2], x[3]])]);
    if (S.plan.router) S.plan.router = sc(S.plan.router);
    for (const k of Object.keys(S.plan.devices)) S.plan.devices[k] = sc(S.plan.devices[k]);
    planChanged(); userMovedView = false; fit();
    return;
  }
  if (tool === 'erase') {
    const m = hitMarker(p);
    if (m && m.kind === 'router') { S.plan.router = null; planChanged(); return; }
    if (m && m.kind === 'device') { delete S.plan.devices[m.mac]; planChanged(); renderDevices(); return; }
    let best = -1, bd = 0.3;
    S.plan.walls.forEach((w, i) => { const d = distToSeg(p, [w[0], w[1]], [w[2], w[3]]); if (d < bd) { bd = d; best = i; } });
    if (best >= 0) { S.plan.walls.splice(best, 1); planChanged(); }
  }
});
canvas.addEventListener('pointermove', (ev) => {
  cursor = pointerWorld(ev);
  if (panning) { userMovedView = true; origin = { x: panning.o.x + ev.clientX - panning.x, y: panning.o.y + ev.clientY - panning.y }; return; }
  if (dragging) {
    const q = snap(cursor);
    if (dragging.kind === 'router') S.plan.router = q; else S.plan.devices[dragging.mac] = q;
  }
});
canvas.addEventListener('pointerup', () => {
  if (dragging) { dragging = null; planChanged(); }
  panning = null;
});
canvas.addEventListener('dblclick', () => { wallStart = null; setHint(); });
canvas.addEventListener('contextmenu', (ev) => { ev.preventDefault(); wallStart = null; setHint(); });
window.addEventListener('keydown', (ev) => { if (ev.key === 'Escape') { wallStart = null; placing = null; setHint(); } });
canvas.addEventListener('wheel', (ev) => {
  ev.preventDefault();
  zoomAt(ev.offsetX, ev.offsetY, ev.deltaY < 0 ? 1.15 : 1 / 1.15);
}, { passive: false });
function zoomAt(px, py, f) {
  userMovedView = true;
  const [wx, wy] = toWorld(px, py);
  scale = Math.max(8, Math.min(400, scale * f));
  origin = { x: px - wx * scale, y: py - wy * scale };
}
function zoomButton(f) {
  if (S.view === '3d') { if (S.zoom3d) S.zoom3d(f); return; }
  const r = canvas.getBoundingClientRect(); zoomAt(r.width / 2, r.height / 2, f);
}
$('zoomIn').onclick = () => zoomButton(1.3);
$('zoomOut').onclick = () => zoomButton(1 / 1.3);
// two-finger pinch on the 2D plan
const touches = new Map();
let pinch = null;
canvas.addEventListener('pointerdown', (ev) => {
  if (ev.pointerType !== 'touch') return;
  touches.set(ev.pointerId, [ev.clientX, ev.clientY]);
  if (touches.size === 2) {
    const [p1, p2] = [...touches.values()];
    pinch = { d: Math.hypot(p1[0] - p2[0], p1[1] - p2[1]) };
    panning = null; dragging = null; wallStart = null;
  }
}, true);
canvas.addEventListener('pointermove', (ev) => {
  if (!touches.has(ev.pointerId)) return;
  touches.set(ev.pointerId, [ev.clientX, ev.clientY]);
  if (pinch && touches.size === 2) {
    const [p1, p2] = [...touches.values()];
    const d = Math.hypot(p1[0] - p2[0], p1[1] - p2[1]);
    const r = canvas.getBoundingClientRect();
    zoomAt((p1[0] + p2[0]) / 2 - r.left, (p1[1] + p2[1]) / 2 - r.top, d / pinch.d);
    pinch.d = d;
    ev.stopImmediatePropagation();
  }
}, true);
const endTouch = (ev) => { touches.delete(ev.pointerId); if (touches.size < 2) pinch = null; };
canvas.addEventListener('pointerup', endTouch, true);
canvas.addEventListener('pointercancel', endTouch, true);
$('zoomFit').onclick = () => { userMovedView = false; fit(); S.planListeners.forEach((f) => f('fit')); };

// --------------------------------------------------------------- toolbar ---
function setHint() {
  const h = {
    wall: wallStart ? 'Click each corner. Double-click, right-click or Esc to finish.' : 'Click where a wall starts. Grid squares are 1 m.',
    router: 'Click where the router stands.',
    move: 'Drag the router or devices. Drag empty space to pan.',
    erase: 'Click a wall, the router or a device to remove it.',
    scale: 'Click a wall whose real length you know: the whole plan is resized to match.',
  }[tool];
  $('hint').textContent = placing ? `Click on the plan where "${labelOf(placing)}" is.` : h;
}
document.querySelectorAll('#toolbar [data-tool]').forEach((b) => b.onclick = () => {
  tool = b.dataset.tool; wallStart = null; placing = null;
  document.querySelectorAll('#toolbar [data-tool]').forEach((x) => x.classList.toggle('on', x === b));
  setHint();
});
$('undoWall').onclick = () => { if (S.plan.walls.length) { S.plan.walls.pop(); planChanged(); } };
$('editToggle').onclick = () => {
  editing = !editing; wallStart = null; placing = null;
  $('toolbar').hidden = !editing;
  document.body.classList.toggle('editing', editing);
  $('editToggle').classList.toggle('on', editing);
  $('editToggle').textContent = editing ? 'Done' : 'Edit plan';
  if (editing && S.view !== '2d') setView('2d');
  setHint(); renderDevices(); setTimeout(resize, 0);
};
function setView(v) {
  S.view = v;
  $('view2d').classList.toggle('on', v === '2d'); $('view3d').classList.toggle('on', v === '3d');
  canvas.hidden = v !== '2d'; $('three').hidden = v !== '3d';
  if (v === '2d') setTimeout(resize, 0);
  S.planListeners.forEach((f) => f('view'));
}
$('view2d').onclick = () => setView('2d');
$('view3d').onclick = () => { if (editing) $('editToggle').click(); setView('3d'); };

// -------------------------------------------------------------- devices ---
function stateText(l, st) {
  if (st && !st.captured && !l) return ['not captured', ''];
  if (!l || l.stale) return ['no data', ''];
  if (l.learning) return ['learning…', ''];
  if (!l.motion) return ['quiet', 'quiet'];
  return [l.speed > 0 ? `motion · ${l.speed.toFixed(1)} m/s` : 'motion', 'motion'];
}

// Doppler history per link: how fast the moving reflection's path length
// changes, over the last ~18 s, drawn as a small waterfall under the device.
const DOP_KEEP = 60;
S.dopHist = {};
function recordDoppler(st) {
  for (const l of st.links || []) {
    if (!l.doppler || l.stale) continue;
    const h = (S.dopHist[l.mac] = S.dopHist[l.mac] || []);
    h.push(l.doppler);
    if (h.length > DOP_KEEP) h.shift();
  }
}
function drawDoppler(cv, hist) {
  const w = cv.clientWidth || 240, h = 44;
  cv.width = w; cv.height = h;
  const g = cv.getContext('2d');
  g.fillStyle = '#0b0f14'; g.fillRect(0, 0, w, h);
  const cw = w / DOP_KEEP, off = DOP_KEEP - hist.length;
  hist.forEach((col, i) => {
    const rh = h / col.length;
    col.forEach((db, j) => {
      const k = Math.max(0, Math.min(1, (db - 4) / 16));
      if (k <= 0) return;
      g.fillStyle = `rgba(${Math.round(94 + 150 * k)},${Math.round(224 + 29 * k)},${Math.round(193 + 62 * k)},${k})`;
      g.fillRect((off + i) * cw, h - (j + 1) * rh, Math.ceil(cw), Math.ceil(rh));
    });
  });
  g.strokeStyle = 'rgba(255,255,255,0.12)'; g.beginPath(); g.moveTo(0, h / 2); g.lineTo(w, h / 2); g.stroke();
}
function renderDevices() {
  const ul = $('devices');
  const st = (S.state && S.state.stations) || [];
  const macs = new Set([...st.map((s) => s.mac), ...((S.state && S.state.links) || []).map((l) => l.mac), ...Object.keys(S.plan.devices)]);
  const rows = [...macs].map((mac) => ({ mac, st: stationOf(mac), l: linkOf(mac) }));
  rows.sort((a, b) => (!!b.l - !!a.l) || labelOf(a.mac).localeCompare(labelOf(b.mac)));
  ul.innerHTML = '';
  for (const { mac, st: s, l } of rows) {
    const [txt, cls] = stateText(l, s);
    const li = document.createElement('li');
    const band = s && s.ghz ? (s.ghz < 3 ? '2.4 GHz' : '5 GHz') : '';
    const meta = [mac, band, s && s.rssi ? `signal ${s.rssi} dBm` : '', l && !l.stale ? `${l.rate.toFixed(0)} frames/s` : '', s && s.power_save ? 'power save' : '', l && l.noisy ? 'noisy link: needs a big change to count' : ''].filter(Boolean).join(' · ');
    li.innerHTML = `<div class="dev-top"><span class="dev-name"></span><span class="dev-state ${cls}">${txt}</span></div>
      <div class="dev-meta">${meta}</div>
      <div class="bar"><i style="width:${l && !l.stale ? Math.min(100, l.score / (2 * l.threshold) * 100) : 0}%"></i></div>`;
    li.querySelector('.dev-name').textContent = labelOf(mac);
    if (l && !l.stale && S.dopHist[mac]) {
      const wrap = document.createElement('div'); wrap.className = 'dop';
      wrap.innerHTML = '<canvas></canvas><span class="dop-l">+2.5 m/s</span><span class="dop-l dop-b">−2.5</span>';
      li.append(wrap);
      requestAnimationFrame(() => drawDoppler(wrap.firstChild, S.dopHist[mac]));
    }
    if (editing) {
      const acts = document.createElement('div'); acts.className = 'dev-actions';
      const place = document.createElement('button');
      place.textContent = S.plan.devices[mac] ? 'Move on plan' : 'Place on plan';
      place.onclick = () => { placing = mac; setHint(); };
      const rename = document.createElement('button');
      rename.textContent = 'Rename';
      rename.onclick = () => {
        const v = prompt('Device name', labelOf(mac));
        if (v !== null) { if (v.trim()) S.plan.names[mac] = v.trim(); else delete S.plan.names[mac]; planChanged(); renderDevices(); }
      };
      acts.append(place, rename); li.append(acts);
    }
    ul.append(li);
  }
}

function renderSummary() {
  const s = S.state;
  const el = $('summary');
  if (!s) { el.textContent = 'Waiting for data…'; return; }
  const live = (s.links || []).filter((l) => !l.stale);
  const moving = live.filter((l) => l.motion && !l.learning);
  if (!live.length) { el.innerHTML = 'No capture data from the router.'; return; }
  if (live.every((l) => l.learning)) { el.innerHTML = 'Learning the normal signal level, about 10 seconds…'; return; }
  const tr = s.track || {};
  if (!tr.ready) { el.innerHTML = (moving.length ? '<b>Movement</b> · ' : '') + `<span style="color:#f2b35b">${escapeHtml(tr.reason || '')}</span>`; return; }
  const n = (tr.bodies || []).length;
  el.innerHTML = n ? `<b>${n === 1 ? 'Someone is moving' : n + ' moving bodies'}</b>` + ((tr.bodies[0] && tr.bodies[0].speed) ? ` · about ${tr.bodies[0].speed.toFixed(1)} m/s` : '') : 'Quiet: no movement';
}

// Poll the newest state a few times a second. (An event stream never got
// through the Cloudflare tunnel - it buffers the response.)
function applyState(st) {
  S.state = st;
  recordDoppler(st);
  const pill = $('status');
  if (st.receiving) { pill.textContent = 'live'; pill.className = 'pill ok'; }
  else { pill.textContent = st.error ? 'router unreachable' : 'no capture data'; pill.className = 'pill bad'; }
  renderSummary();
  if (!editing || !document.querySelector('#devices li:hover')) renderDevices();
}
async function poll() {
  try {
    const r = await fetch('api/state', { cache: 'no-store' });
    if (r.ok) { const st = await r.json(); if (st) applyState(st); }
    else { $('status').textContent = r.status === 403 ? 'open the link you were sent' : 'server error ' + r.status; $('status').className = 'pill bad'; }
  } catch (e) { $('status').textContent = 'reconnecting…'; $('status').className = 'pill bad'; }
  setTimeout(poll, 300);
}
function connect() { poll(); recPoll(); }

// ------------------------------------------------------------- recording ---
let recState = { recording: false };
function renderRec() {
  const b = $('recToggle');
  b.textContent = recState.recording ? 'Stop recording' : 'Start recording';
  b.classList.toggle('on', !!recState.recording);
  $('recInfo').textContent = recState.recording ? `${recState.seconds}s · ${recState.mb.toFixed(0)} MB · ${recState.label || 'no label yet'}` : (recState.session ? `saved ${recState.session}` : '');
  document.querySelectorAll('.rec-labels button').forEach((x) => { x.disabled = !recState.recording; x.classList.toggle('on', recState.recording && recState.label === x.dataset.label); });
}
async function recCall(body) {
  try {
    const r = await fetch('api/rec', body ? { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) } : {});
    if (r.ok) { const st = await r.json(); recState = { ...st, session: st.session || recState.session }; renderRec(); }
  } catch (e) {}
}
function recPoll() { recCall(); setTimeout(recPoll, 1000); }
$('recToggle').onclick = () => recCall({ action: recState.recording ? 'stop' : 'start' });
document.querySelectorAll('.rec-labels button').forEach((b) => b.onclick = () => recCall({ action: 'mark', label: b.dataset.label }));

let userMovedView = false; // after the user pans or zooms, stop auto-fitting
async function init() {
  try { const r = await fetch('api/plan'); if (r.ok) S.plan = await r.json(); } catch (e) {}
  S.plan.walls = S.plan.walls || []; S.plan.devices = S.plan.devices || {}; S.plan.names = S.plan.names || {};
  // Size and fit once the stage has its real size (it can still be 0 x 0
  // at this point), and again whenever it changes until the user moves the view.
  new ResizeObserver(() => { resize(); if (!userMovedView) fit(); }).observe(document.getElementById('stage'));
  setHint(); renderSummary();
  S.planListeners.forEach((f) => f());
  connect(); draw();
}
init();
