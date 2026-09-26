// 3D view of the same plan: extruded walls, router, devices, links that
// light up with motion, and the glowing dot. Plan (x, y) maps to (x, 0, y).
import * as THREE from 'three';
import { OrbitControls } from 'three/addons/controls/OrbitControls.js';

const S = window.sensing;
const host = document.getElementById('three');
const WALL_H = 2.5;

let epSig = '';
let renderer, scene, camera, controls, planGroup, linkObjs = {}, dot, dotLight, halo;

function init() {
  renderer = new THREE.WebGLRenderer({ antialias: true });
  renderer.setPixelRatio(Math.min(2, window.devicePixelRatio || 1));
  host.appendChild(renderer.domElement);
  scene = new THREE.Scene();
  scene.background = new THREE.Color(0x0b0f14);
  scene.fog = new THREE.Fog(0x0b0f14, 25, 60);
  camera = new THREE.PerspectiveCamera(50, 1, 0.1, 200);
  controls = new OrbitControls(camera, renderer.domElement);
  controls.enableDamping = true;
  controls.maxPolarAngle = Math.PI * 0.49;
  scene.add(new THREE.AmbientLight(0x8899aa, 0.55));
  const sun = new THREE.DirectionalLight(0xffffff, 0.6);
  sun.position.set(5, 12, 8);
  scene.add(sun);

  dot = new THREE.Mesh(new THREE.SphereGeometry(0.22, 24, 16),
    new THREE.MeshBasicMaterial({ color: 0xe8fdff, transparent: true, opacity: 0 }));
  dot.position.y = 1.0;
  dotLight = new THREE.PointLight(0x7ff3ff, 0, 6, 1.5);
  dot.add(dotLight);
  const haloTex = (() => {
    const c = document.createElement('canvas'); c.width = c.height = 128;
    const g = c.getContext('2d'), gr = g.createRadialGradient(64, 64, 0, 64, 64, 64);
    gr.addColorStop(0, 'rgba(255,255,255,1)'); gr.addColorStop(0.3, 'rgba(127,243,255,0.6)'); gr.addColorStop(1, 'rgba(127,243,255,0)');
    g.fillStyle = gr; g.fillRect(0, 0, 128, 128);
    return new THREE.CanvasTexture(c);
  })();
  halo = new THREE.Sprite(new THREE.SpriteMaterial({ map: haloTex, transparent: true, opacity: 0, depthWrite: false }));
  halo.scale.set(2.2, 2.2, 1);
  dot.add(halo);
  scene.add(dot);

  rebuild('fit');
  animate();
}

let zoneTexCache = null;
function zoneTex() {
  if (zoneTexCache) return zoneTexCache;
  const c = document.createElement('canvas'); c.width = c.height = 128;
  const g = c.getContext('2d'), gr = g.createRadialGradient(64, 64, 0, 64, 64, 64);
  gr.addColorStop(0, 'rgba(255,255,255,1)'); gr.addColorStop(0.6, 'rgba(255,255,255,0.45)'); gr.addColorStop(1, 'rgba(255,255,255,0)');
  g.fillStyle = gr; g.fillRect(0, 0, 128, 128);
  return (zoneTexCache = new THREE.CanvasTexture(c));
}

function bounds() {
  const pts = [], EP = S.effPlan();
  for (const w of S.plan.walls) pts.push([w[0], w[1]], [w[2], w[3]]);
  if (EP.router) pts.push(EP.router);
  for (const p of Object.values(EP.devices)) pts.push(p);
  if (!pts.length) return [-5, -4, 5, 4];
  let b = [Infinity, Infinity, -Infinity, -Infinity];
  for (const [x, y] of pts) b = [Math.min(b[0], x), Math.min(b[1], y), Math.max(b[2], x), Math.max(b[3], y)];
  return b;
}

function rebuild(reason) {
  if (!scene) return;
  if (planGroup) { scene.remove(planGroup); planGroup.traverse((o) => { o.geometry && o.geometry.dispose(); }); }
  planGroup = new THREE.Group();
  linkObjs = {};
  const [x1, y1, x2, y2] = bounds();
  const cx = (x1 + x2) / 2, cz = (y1 + y2) / 2, w = x2 - x1 + 4, d = y2 - y1 + 4;

  const floor = new THREE.Mesh(new THREE.PlaneGeometry(w, d), new THREE.MeshStandardMaterial({ color: 0x121821, roughness: 1 }));
  floor.rotation.x = -Math.PI / 2; floor.position.set(cx, 0, cz);
  planGroup.add(floor);
  const grid = new THREE.GridHelper(Math.ceil(Math.max(w, d)), Math.ceil(Math.max(w, d)), 0x1d2733, 0x161e28);
  grid.position.set(cx, 0.002, cz);
  planGroup.add(grid);

  const wallMat = new THREE.MeshStandardMaterial({ color: 0xc9d3de, transparent: true, opacity: 0.55, roughness: 0.8 });
  for (const [ax, ay, bx, by] of S.plan.walls) {
    const len = Math.hypot(bx - ax, by - ay);
    if (len < 0.01) continue;
    const m = new THREE.Mesh(new THREE.BoxGeometry(len, WALL_H, 0.12), wallMat);
    m.position.set((ax + bx) / 2, WALL_H / 2, (ay + by) / 2);
    m.rotation.y = -Math.atan2(by - ay, bx - ax);
    planGroup.add(m);
  }
  const EP = S.effPlan();
  epSig = JSON.stringify(EP);
  const R = EP.router;
  if (R) {
    const r = new THREE.Mesh(new THREE.BoxGeometry(0.3, 0.4, 0.3), new THREE.MeshStandardMaterial({ color: 0xf2b35b, emissive: 0x3a2610 }));
    r.position.set(R[0], 0.9, R[1]);
    planGroup.add(r);
  }
  for (const [mac, p] of Object.entries(EP.devices)) {
    const s = new THREE.Mesh(new THREE.SphereGeometry(0.12, 16, 12), new THREE.MeshStandardMaterial({ color: 0x5ee0c1, emissive: 0x0d2a24 }));
    s.position.set(p[0], 0.8, p[1]);
    planGroup.add(s);
    if (R) {
      const g = new THREE.BufferGeometry().setFromPoints([new THREE.Vector3(R[0], 0.9, R[1]), new THREE.Vector3(p[0], 0.8, p[1])]);
      const line = new THREE.Line(g, new THREE.LineBasicMaterial({ color: 0x5a6878, transparent: true, opacity: 0.5 }));
      planGroup.add(line);
      const z = S.zoneOf(R, p);
      const zone = new THREE.Mesh(new THREE.CircleGeometry(1, 64), new THREE.MeshBasicMaterial({
        map: zoneTex(), color: 0x7ff3ff, transparent: true, opacity: 0, depthWrite: false, blending: THREE.AdditiveBlending }));
      zone.scale.set(z.a, z.b, 1);
      zone.rotation.set(-Math.PI / 2, 0, -z.angle);
      zone.position.set(z.cx, 0.03, z.cy);
      planGroup.add(zone);
      linkObjs[mac] = { line, zone };
    }
  }
  scene.add(planGroup);
  if (reason === 'fit' || !camera.userData.placed) {
    const span = Math.max(x2 - x1, y2 - y1, 6);
    camera.position.set(cx + span * 0.3, span * 1.1, cz + span * 1.25);
    controls.target.set(cx, 0.5, cz);
    camera.userData.placed = true;
  }
  resize();
}

function resize() {
  if (!renderer || host.hidden) return;
  const r = host.getBoundingClientRect();
  if (!r.width || !r.height) return;
  renderer.setSize(r.width, r.height, false);
  camera.aspect = r.width / r.height;
  camera.updateProjectionMatrix();
}

const cool = new THREE.Color(0x5a6878), live = new THREE.Color(0x5ee0c1), hot = new THREE.Color(0xe8fdff);
function animate() {
  requestAnimationFrame(animate);
  if (S.view !== '3d') return;
  if (JSON.stringify(S.effPlan()) !== epSig) rebuild(); // placeholder layout follows the captured devices
  controls.update();
  const st = S.state;
  const links = (st && st.links) || [];
  for (const [mac, o] of Object.entries(linkObjs)) {
    const l = links.find((x) => x.mac === mac);
    const on = l && !l.stale;
    const k = on ? Math.min(1, l.score / (2 * l.threshold)) : 0;
    o.line.material.color.copy(on ? live.clone().lerp(hot, k) : cool);
    o.line.material.opacity = on ? 0.45 + 0.55 * k : 0.35;
    o.zone.material.opacity = 0.8 * S.zoneLevel(mac);
  }
  const D = S.dot;
  const I = D ? D.k * (D.rough ? 0.5 : 1) : 0;
  if (D) dot.position.set(D.x, 1.0, D.y);
  dot.visible = I > 0.02;
  const pulse = 1 + 0.08 * Math.sin(performance.now() / 250);
  dot.material.opacity = D && D.rough ? 0 : I;
  halo.material.opacity = 0.9 * I;
  halo.scale.setScalar((D && D.rough ? 3.5 : 1.6 + 1.6 * I) * pulse);
  dotLight.intensity = 4 * I;
  renderer.render(scene, camera);
}

S.zoom3d = (f) => {
  if (!camera) return;
  const v = camera.position.clone().sub(controls.target).multiplyScalar(1 / f);
  camera.position.copy(controls.target).add(v);
};

S.planListeners.push((reason) => {
  if (!renderer && S.view === '3d') init();
  else rebuild(reason);
  if (reason === 'view') setTimeout(resize, 0);
});
window.addEventListener('resize', resize);
