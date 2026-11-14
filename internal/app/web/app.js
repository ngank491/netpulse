'use strict';
/* NetPulse web UI – plain JS, no build step. DOM is built with h()/s(),
   never innerHTML, so router-supplied names can't inject markup. */

// ---------- DOM helpers ----------
function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  setAttrs(el, attrs);
  append(el, kids);
  return el;
}
const SVGNS = 'http://www.w3.org/2000/svg';
function s(tag, attrs, ...kids) {
  const el = document.createElementNS(SVGNS, tag);
  setAttrs(el, attrs);
  append(el, kids);
  return el;
}
function setAttrs(el, attrs) {
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.setAttribute('class', v);
    else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
    else if (k === 'value') el.value = v;
    else if (k === 'checked') el.checked = !!v;
    else if (k === 'selected') el.selected = !!v;
    else el.setAttribute(k, v === true ? '' : v);
  }
}
function append(el, kids) {
  for (const c of kids.flat(Infinity)) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : String(c));
  }
}
const $ = (sel, el = document) => el.querySelector(sel);
const clear = el => { while (el.firstChild) el.removeChild(el.firstChild); return el; };
const fill = (el, ...kids) => { clear(el); append(el, kids); return el; };

// ---------- API ----------
async function api(method, path, body) {
  const opt = { method, headers: { 'X-Requested-With': 'netpulse' }, credentials: 'same-origin' };
  if (body !== undefined) { opt.headers['Content-Type'] = 'application/json'; opt.body = JSON.stringify(body); }
  let r;
  try { r = await fetch(path, opt); } catch { throw new Error('Không kết nối được máy chủ NetPulse'); }
  const ct = r.headers.get('content-type') || '';
  const data = ct.includes('json') ? await r.json().catch(() => ({})) : await r.blob();
  if (!r.ok) {
    const e = new Error((data && data.error) || ('Lỗi ' + r.status));
    e.status = r.status;
    if (r.status === 401 && me && !path.startsWith('/api/me/password')) { me = null; showAuth(); }
    throw e;
  }
  return data;
}

function toast(msg, kind = '') {
  const t = h('div', { class: 'toast ' + kind }, msg);
  $('#toasts').append(t);
  setTimeout(() => t.remove(), kind === 'bad' ? 7000 : 3500);
}

// ---------- formatting ----------
const fmtBps = v => v == null ? '–' : v >= 1e9 ? (v / 1e9).toFixed(2) + ' Gbps' : v >= 1e6 ? (v / 1e6).toFixed(1) + ' Mbps' : v >= 1e3 ? (v / 1e3).toFixed(0) + ' kbps' : Math.round(v) + ' bps';
function fmtBytes(v) {
  v = Number(v); if (!isFinite(v)) return '–';
  const u = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']; let i = 0;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return (i ? v.toFixed(1) : v) + ' ' + u[i];
}
function rosSec(s) {
  if (!s) return null; let t = 0; const c = /(\d+):(\d+):(\d+)/.exec(s);
  if (c) { t += +c[1] * 3600 + +c[2] * 60 + +c[3]; s = s.replace(c[0], ''); }
  for (const m of s.matchAll(/(\d+)(ms|us|w|d|h|m|s)/g)) t += +m[1] * ({ w: 604800, d: 86400, h: 3600, m: 60, s: 1, ms: .001, us: 0 })[m[2]];
  return t;
}
function fmtDur(sec) {
  if (sec == null || !isFinite(sec)) return '–';
  sec = Math.max(0, Math.floor(sec));
  const d = Math.floor(sec / 86400), hr = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
  if (d) return `${d} ngày ${hr} giờ`;
  if (hr) return `${hr} giờ ${m} phút`;
  if (m) return `${m} phút`;
  return `${sec} giây`;
}
const since = ms => ms ? fmtDur((Date.now() - ms) / 1000) : '–';
function ago(ms) { if (!ms) return 'chưa có'; const s = (Date.now() - ms) / 1000; return s < 5 ? 'vừa xong' : fmtDur(s) + ' trước'; }
function fmtTime(ms) {
  if (!ms) return '–';
  const d = new Date(ms), today = new Date();
  const t = d.toLocaleTimeString('vi-VN', { hour: '2-digit', minute: '2-digit', second: '2-digit' });
  return d.toDateString() === today.toDateString() ? t : t + ' ' + d.toLocaleDateString('vi-VN', { day: '2-digit', month: '2-digit' });
}
const pct = v => v == null ? '–' : Math.round(v) + '%';
function ipKey(ip) { const p = String(ip).split('.'); return p.length === 4 ? p.reduce((a, x) => a * 256 + (+x || 0), 0) : Number.MAX_SAFE_INTEGER; }
const truthy = v => v === 'true' || v === 'yes';

const ST_TEXT = { up: 'Hoạt động', down: 'Mất kết nối', waiting: 'Chờ dữ liệu', disabled: 'Đã tắt', unknown: 'Chưa rõ', error: 'Lỗi kiểm tra' };
const badge = (st, text) => h('span', { class: 'st ' + (st || 'unknown') }, text || ST_TEXT[st] || st || '–');
const KIND = { device: ['PC', 'Máy tính / thiết bị'], switch: ['SW', 'Switch'], ap: ['AP', 'Wi-Fi AP'], camera: ['CAM', 'Camera'], server: ['SRV', 'Máy chủ / NAS'], printer: ['PRN', 'Máy in'], phone: ['TEL', 'Điện thoại IP'], virtual: ['—', 'Nút ảo (không ping)'] };

function meterRow(label, v) {
  const val = v == null ? 0 : Math.min(100, Math.max(0, v));
  const cls = val >= 90 ? 'crit' : val >= 75 ? 'hi' : '';
  const bar = h('i', { class: cls }); bar.style.width = val + '%';
  return h('div', { class: 'meter' }, h('span', { class: 'muted' }, label), h('div', { class: 'bar' }, bar), h('span', { class: 'right' }, pct(v)));
}

// ---------- sortable / filterable table ----------
// cols: [{t:'Title', v: row => sortValue, r: row => cell content, num:bool}]
function table(cols, rows, opts = {}) {
  const st = opts.state || {};
  let list = rows;
  if (st.q) {
    const q = st.q.toLowerCase();
    list = list.filter(r => cols.some(c => String(c.v ? c.v(r) ?? '' : '').toLowerCase().includes(q)));
  }
  if (st.sort != null && cols[st.sort] && cols[st.sort].v) {
    const c = cols[st.sort], dir = st.desc ? -1 : 1;
    list = [...list].sort((a, b) => {
      const x = c.v(a), y = c.v(b);
      if (typeof x === 'number' || typeof y === 'number') return ((x ?? -Infinity) - (y ?? -Infinity)) * dir;
      return String(x ?? '').localeCompare(String(y ?? ''), 'vi', { numeric: true }) * dir;
    });
  }
  const head = h('tr', null, cols.map((c, i) => h('th', {
    class: (c.num ? 'num ' : '') + (c.v ? 'sort' : ''),
    onclick: c.v ? () => { if (st.sort === i) st.desc = !st.desc; else { st.sort = i; st.desc = !!c.num; } opts.rerender && opts.rerender(); } : null,
  }, c.t, st.sort === i ? (st.desc ? ' ▾' : ' ▴') : '')));
  const body = list.length ? list.map(r => h('tr', { class: opts.rowClass ? opts.rowClass(r) : null },
    cols.map(c => h('td', { class: c.num ? 'num' : null }, c.r ? c.r(r) : (c.v(r) ?? '–'))))) :
    h('tr', null, h('td', { colspan: cols.length, class: 'empty' }, opts.empty || 'Không có dữ liệu'));
  return h('div', { class: 'tw' }, h('table', null, h('thead', null, head), h('tbody', null, body)));
}

// A panel with title, optional search box and a body re-rendered on refresh.
function tablePanel(title, key, extra) {
  const state = ui.tables[key] || (ui.tables[key] = { q: '' });
  const body = h('div');
  const count = h('span', { class: 'muted small' });
  const search = h('input', { class: 'search', type: 'search', placeholder: 'Tìm…', value: state.q, oninput: e => { state.q = e.target.value; panel.redraw(); } });
  const el = h('div', { class: 'panel' }, h('div', { class: 'panel-h' }, h('h2', null, title), count, h('div', { class: 'grow' }), extra || null, search), body);
  const panel = {
    el, state,
    last: null,
    draw(cols, rows, opts = {}) { this.last = [cols, rows, opts]; fill(body, table(cols, rows, { ...opts, state, rerender: () => this.redraw() })); count.textContent = rows.length ? `(${rows.length})` : ''; },
    redraw() { if (this.last) this.draw(...this.last); },
  };
  return panel;
}

// ---------- modal ----------
function modal(title, content, actions) {
  const bg = h('div', { class: 'modal-bg' });
  const close = () => { bg.remove(); ui.modal = false; };
  const foot = h('div', { class: 'modal-foot' });
  for (const a of actions || []) foot.append(h('button', { class: a.cls || '', type: a.submit ? 'submit' : 'button', onclick: a.submit ? null : () => a.fn(close) }, a.t));
  const form = h('form', { class: 'panel modal', onsubmit: e => { e.preventDefault(); const a = (actions || []).find(x => x.submit); if (a) a.fn(close); } },
    h('div', { class: 'panel-h' }, h('h2', { class: 'grow' }, title), h('button', { type: 'button', class: 'link', onclick: close, 'aria-label': 'Đóng' }, '✕')),
    h('div', { class: 'panel-b' }, content), foot);
  bg.addEventListener('mousedown', e => { if (e.target === bg) close(); });
  bg.append(form);
  document.body.append(bg);
  ui.modal = true;
  const first = form.querySelector('input:not([type=checkbox]),select');
  if (first) first.focus();
  return close;
}
const field = (label, input, cls) => h('label', { class: 'f ' + (cls || '') }, label, input);
const check = (label, input) => h('label', { class: 'cb' }, input, label);

async function busy(btn, fn) {
  btn.disabled = true;
  try { return await fn(); } catch (e) { toast(e.message, 'bad'); } finally { btn.disabled = false; }
}

// ---------- state & routing ----------
let me = null;
const ui = { tables: {}, modal: false, drag: false, hist: { hours: 6, iface: '' } };
let page = null, timer = null;

async function boot() {
  try { me = await api('GET', '/api/me'); } catch { return showAuth(); }
  if (!boot.bound) {
    boot.bound = true;
    window.addEventListener('hashchange', route);
    document.addEventListener('visibilitychange', () => { if (!document.hidden && me) refresh(); });
  }
  route();
}

function shell() {
  const navItems = [['#/', 'Tổng quan'], ['#/events', 'Sự kiện']];
  if (me.role === 'admin') navItems.push(['#/admin/routers', 'Router'], ['#/admin/users', 'Người dùng'], ['#/admin/settings', 'Cài đặt']);
  const hash = location.hash || '#/';
  const nav = h('nav', { class: 'nav' }, navItems.map(([href, t]) => h('a', {
    href, class: (href === '#/' ? hash === '#/' || hash.startsWith('#/r/') : hash.startsWith(href)) ? 'on' : null,
  }, t)));
  const top = h('header', { class: 'top' }, h('div', { class: 'top-in' },
    h('a', { class: 'logo', href: '#/' }, h('img', { src: 'favicon.svg', alt: '' }), 'NetPulse'), nav,
    h('div', { class: 'user' },
      h('a', { href: '#/account', class: 'name', title: 'Tài khoản' }, me.username + (me.role === 'admin' ? ' (admin)' : '')),
      h('button', { class: 'sm', onclick: async () => { await api('POST', '/api/logout').catch(() => { }); me = null; showAuth(); } }, 'Đăng xuất'))));
  const main = h('main');
  fill($('#app'), top, main);
  return main;
}

function route() {
  if (!me) return;
  clearInterval(timer);
  const parts = (location.hash || '#/').slice(2).split('?')[0].split('/');
  const main = shell();
  let p;
  if (parts[0] === 'r' && parts[1]) p = routerPage(+parts[1], parts[2] || 'overview');
  else if (parts[0] === 'events') p = eventsPage();
  else if (parts[0] === 'account') p = accountPage();
  else if (parts[0] === 'admin' && me.role === 'admin') p = ({ routers: adminRoutersPage, users: usersPage, settings: settingsPage })[parts[1]]?.();
  p = p || overviewPage();
  page = p;
  p.mount(main);
  refresh();
  if (p.every) timer = setInterval(() => { if (!document.hidden && !ui.modal && !ui.drag) refresh(); }, p.every);
}

async function refresh() {
  const p = page;
  if (!p || !p.load || p.loading) return;
  p.loading = true;
  try { const d = await p.load(); if (page === p) p.update(d); }
  catch (e) { if (e.status !== 401 && page === p) toast(e.message, 'bad'); }
  finally { p.loading = false; }
}

// ---------- auth screens ----------
async function showAuth() {
  clearInterval(timer);
  let needed = false;
  try { needed = (await api('GET', '/api/setup')).needed; } catch { }
  needed ? showSetup() : showLogin();
}

function authForm(title, sub, fields, btnText, submit) {
  const err = h('div', { class: 'err-line' });
  const btn = h('button', { class: 'primary', type: 'submit' }, btnText);
  const form = h('form', { class: 'panel stack', onsubmit: async e => {
    e.preventDefault(); err.textContent = ''; btn.disabled = true;
    try { await submit(); } catch (x) { err.textContent = x.message; } finally { btn.disabled = false; }
  } }, h('h1', null, h('img', { src: 'favicon.svg', alt: '' }), title), h('p', { class: 'muted' }, sub), fields, err, btn);
  fill($('#app'), h('div', { class: 'auth' }, form));
  form.querySelector('input').focus();
}

function showLogin() {
  const u = h('input', { autocomplete: 'username', required: true, maxlength: 64 });
  const p = h('input', { type: 'password', autocomplete: 'current-password', required: true, maxlength: 256 });
  authForm('NetPulse', 'Giám sát MikroTik', [field('Tên đăng nhập', u), field('Mật khẩu', p)], 'Đăng nhập', async () => {
    me = await api('POST', '/api/login', { username: u.value, password: p.value });
    boot();
  });
}

function showSetup() {
  const c = h('input', { required: true, placeholder: 'XXXX-XXXX', autocomplete: 'off' });
  const u = h('input', { value: 'admin', required: true, autocomplete: 'username' });
  const p = h('input', { type: 'password', required: true, minlength: 10, autocomplete: 'new-password' });
  const p2 = h('input', { type: 'password', required: true, autocomplete: 'new-password' });
  authForm('Khởi tạo NetPulse', 'Nhập mã khởi tạo hiển thị trong cửa sổ/log của NetPulse, rồi tạo tài khoản quản trị.',
    [field('Mã khởi tạo', c), field('Tên quản trị', u), field('Mật khẩu (≥ 10 ký tự)', p), field('Nhập lại mật khẩu', p2)], 'Tạo tài khoản', async () => {
      if (p.value !== p2.value) throw new Error('Hai mật khẩu không khớp');
      me = await api('POST', '/api/setup', { code: c.value, username: u.value, password: p.value });
      location.hash = me.role === 'admin' ? '#/admin/routers' : '#/';
      boot();
    });
}

// ---------- overview ----------
function overviewPage() {
  let grid, evBox;
  return {
    every: 5000,
    mount(main) {
      grid = h('div', { class: 'grid cards' });
      evBox = h('div');
      main.append(h('div', { class: 'stack' }, grid, h('div', { class: 'panel' }, h('div', { class: 'panel-h' }, h('h2', null, 'Sự kiện gần đây'), h('div', { class: 'grow' }), h('a', { href: '#/events' }, 'Xem tất cả')), evBox)));
    },
    load: () => api('GET', '/api/overview'),
    update(d) {
      clear(grid);
      if (!d.routers.length) {
        grid.append(h('div', { class: 'panel panel-b' }, me.role === 'admin' ? ['Chưa có router nào. ', h('a', { href: '#/admin/routers' }, 'Thêm router đầu tiên →')] : 'Bạn chưa được cấp quyền xem router nào.'));
      }
      for (const r of d.routers) grid.append(routerCard(r));
      fill(evBox, eventList(d.events, Object.fromEntries(d.routers.map(r => [r.id, r.name]))));
    },
  };
}

function routerCard(r) {
  const res = r.resource || {};
  r.wans = r.wans || []; r.downList = r.downList || [];
  const chips = r.wans.filter(w => w.configured || r.wans.every(x => !x.configured)).map(w => {
    const st = !w.configured ? (w.running ? 'up' : w.running === false ? 'down' : '') : (w.link.status === 'down' || w.inet.status === 'down') ? 'down' : w.inet.status === 'up' || (w.link.status === 'up' && w.inet.status === 'unknown') ? 'up' : '';
    return h('span', { class: 'chip ' + st, title: w.iface }, (st === 'down' ? '✕ ' : st === 'up' ? '✓ ' : '') + (w.label || w.iface));
  });
  return h('a', { class: 'panel rcard ' + (r.status === 'down' ? 'down' : ''), href: `#/r/${r.id}/overview` },
    h('div', { class: 'rc-head' }, h('span', { class: 'dot ' + r.status }), h('b', { class: 'grow' }, r.name), badge(r.status)),
    h('div', { class: 'rc-sub' }, [r.identity, res['board-name'], res.version && 'v' + res.version.split(' ')[0]].filter(Boolean).join(' · ') || (r.mode === 'push' ? 'Chế độ push' : r.mode === 'demo' ? 'Router mô phỏng' : '')),
    r.status === 'down' || r.status === 'waiting' ? h('div', { class: 'err-line' }, r.lastErr || (r.status === 'waiting' ? 'Đang chờ dữ liệu đầu tiên…' : 'Không nhận được dữ liệu')) : null,
    meterRow('CPU', r.cpu), meterRow('RAM', r.mem),
    chips.length ? h('div', { class: 'chips' }, chips) : null,
    r.downList.length ? h('ul', { class: 'downlist' }, r.downList.slice(0, 4).map(x => h('li', null, '● ' + x))) : null,
    h('div', { class: 'rc-foot' },
      h('span', null, r.monitorsTotal ? `Thiết bị: ${r.monitorsTotal - r.monitorsDown}/${r.monitorsTotal} hoạt động` : 'Chưa theo dõi thiết bị'),
      h('span', null, res.uptime ? 'Uptime ' + fmtDur(rosSec(res.uptime)) : ago(r.lastSeen))));
}

function eventList(events, names) {
  if (!events.length) return h('div', { class: 'empty' }, 'Chưa có sự kiện');
  return h('div', { class: 'tw' }, h('table', null, h('tbody', null, events.map(e => h('tr', null,
    h('td', { class: 'nowrap muted small' }, fmtTime(e.at)),
    names ? h('td', { class: 'nowrap' }, h('a', { href: `#/r/${e.routerId}/events` }, names[e.routerId] || '#' + e.routerId)) : null,
    h('td', null, h('span', { class: 'dot ' + (e.level === 'warning' ? 'down' : 'up') }), ' ', e.msg))))));
}

// ---------- events page ----------
function eventsPage() {
  let box, names = {};
  return {
    every: 10000,
    mount(main) { box = h('div'); main.append(h('div', { class: 'panel' }, h('div', { class: 'panel-h' }, h('h2', null, 'Nhật ký sự kiện')), box)); },
    async load() {
      const [ov, ev] = await Promise.all([api('GET', '/api/overview'), api('GET', '/api/events?limit=500')]);
      names = Object.fromEntries(ov.routers.map(r => [r.id, r.name]));
      return ev;
    },
    update(ev) { fill(box, eventList(ev, names)); },
  };
}

// ---------- router page ----------
const TABS = [['overview', 'Tổng quan'], ['wan', 'WAN'], ['devices', 'Thiết bị'], ['map', 'Sơ đồ'], ['ports', 'Cổng'], ['clients', 'Client'], ['ppp', 'PPPoE'], ['ip', 'IP & định tuyến'], ['events', 'Sự kiện']];

function routerPage(id, tab) {
  if (!TABS.some(t => t[0] === tab)) tab = 'overview';
  let head, tabsEl, body, view;
  const t = ({ overview: tabOverview, wan: tabWan, devices: tabDevices, map: tabMap, ports: tabPorts, clients: tabClients, ppp: tabPpp, ip: tabIp, events: tabEvents })[tab](id);
  return {
    every: 5000,
    mount(main) {
      head = h('div'); tabsEl = h('nav', { class: 'tabs' }); body = h('div');
      main.append(head, tabsEl, body);
      t.mount(body);
    },
    load: () => api('GET', `/api/routers/${id}`),
    update(d) {
      view = d.router;
      // tolerate missing collections (new router, no devices yet, no data yet)
      view.ifaces = view.ifaces || []; view.monitors = view.monitors || []; view.sections = view.sections || {};
      view.wans = view.wans || []; view.detectedWans = view.detectedWans || []; view.downList = view.downList || [];
      const res = view.resource || {};
      fill(head, h('div', { class: 'rhead' },
        h('span', { class: 'dot ' + view.status }), h('h1', null, view.name), badge(view.status),
        h('span', { class: 'muted small' }, [view.identity, res['board-name'], res.version && 'RouterOS ' + res.version, view.mode === 'push' ? 'push' : view.mode === 'demo' ? 'demo' : 'REST API'].filter(Boolean).join(' · ')),
        h('div', { class: 'grow' }),
        h('span', { class: 'muted small' }, 'Cập nhật ' + ago(view.lastSeen)),
        me.role === 'admin' ? h('a', { class: 'btn', href: `#/admin/routers?edit=${id}` }, 'Cấu hình') : null),
        view.lastErr && view.status !== 'up' ? h('div', { class: 'err-line' }, view.lastErr) : null);
      const down = view.monitorsDown, wanDown = view.wans.filter(w => w.configured && (w.inet.status === 'down' || w.link.status === 'down')).length;
      const counts = { devices: [view.monitorsTotal, down], wan: [view.wans.length, wanDown], clients: [mergeClients(view.sections).length, 0], ppp: [(view.sections.ppp_active || []).length, 0] };
      fill(tabsEl, TABS.map(([k, label]) => h('a', { href: `#/r/${id}/${k}`, class: k === tab ? 'on' : null }, label,
        counts[k] && counts[k][0] ? h('span', { class: 'n ' + (counts[k][1] ? 'bad' : '') }, counts[k][1] ? counts[k][1] + '/' + counts[k][0] : counts[k][0]) : null)));
      t.update(view);
    },
  };
}

// --- overview tab with history charts
function tabOverview(id) {
  let tiles, trafficBox, cpuBox, ifSel, lastHist = 0, view;
  const loadHist = async () => {
    lastHist = Date.now();
    const iface = ui.hist.iface;
    const d = await api('GET', `/api/routers/${id}/history?hours=${ui.hist.hours}&iface=${encodeURIComponent(iface)}`).catch(e => { toast(e.message, 'bad'); return null; });
    if (!d) return;
    const pts = d.points;
    fill(trafficBox, pts.length && iface ? lineChart([
      { cls: 'rx', name: 'Tải xuống (rx)', pts: pts.map(p => [p.t, p.rx]), area: true },
      { cls: 'tx', name: 'Tải lên (tx)', pts: pts.map(p => [p.t, p.tx]), area: true }], { fmt: fmtBps, gap: d.bucket * 3 }) :
      h('div', { class: 'empty' }, iface ? 'Chưa có lịch sử (dữ liệu được ghi mỗi phút).' : 'Chọn một interface.'));
    fill(cpuBox, pts.length ? lineChart([
      { cls: 'cpu', name: 'CPU', pts: pts.map(p => [p.t, p.cpu]) },
      { cls: 'mem', name: 'RAM', pts: pts.map(p => [p.t, p.mem]) }], { fmt: v => Math.round(v) + '%', max: 100, gap: d.bucket * 3, height: 160 }) :
      h('div', { class: 'empty' }, 'Chưa có lịch sử.'));
  };
  const rangeBtns = [[1, '1 giờ'], [6, '6 giờ'], [24, '24 giờ'], [168, '7 ngày'], [720, '30 ngày']];
  return {
    mount(el) {
      tiles = h('div', { class: 'grid tiles' });
      trafficBox = h('div', { class: 'panel-b' }); cpuBox = h('div', { class: 'panel-b' });
      ifSel = h('select', { class: 'search', onchange: e => { ui.hist.iface = e.target.value; loadHist(); } });
      const seg = h('div', { class: 'seg' }, rangeBtns.map(([v, t]) => h('button', { type: 'button', class: ui.hist.hours === v ? 'on' : null, onclick: e => {
        ui.hist.hours = v; [...seg.children].forEach(b => b.classList.remove('on')); e.target.classList.add('on'); loadHist();
      } }, t)));
      el.append(h('div', { class: 'stack' }, tiles,
        h('div', { class: 'panel' }, h('div', { class: 'panel-h' }, h('h2', null, 'Lưu lượng'), ifSel, h('div', { class: 'legend' }, h('span', null, h('i', { class: 'rx' }), 'rx (vào)'), h('span', null, h('i', { class: 'tx' }), 'tx (ra)')), h('div', { class: 'grow' }), seg), trafficBox),
        h('div', { class: 'panel' }, h('div', { class: 'panel-h' }, h('h2', null, 'CPU & RAM'), h('div', { class: 'legend' }, h('span', null, h('i', { class: 'cpu' }), 'CPU'), h('span', null, h('i', { class: 'mem' }), 'RAM'))), cpuBox)));
    },
    update(v) {
      view = v;
      const r = v.resource || {};
      const tile = (k, val, sm) => h('div', { class: 'tile' }, h('div', { class: 'k' }, k), h('div', { class: 'v' + (sm ? ' sm' : '') }, val));
      const hdd = r['total-hdd-space'] ? 100 - (r['free-hdd-space'] / r['total-hdd-space']) * 100 : null;
      const health = (v.sections.health || []).filter(x => /temperature|voltage/.test(x.name)).slice(0, 3);
      fill(tiles, 
        tile('CPU', pct(v.cpu) + (r['cpu-count'] ? ` · ${r['cpu-count']} nhân` : '')),
        tile('RAM', pct(v.mem) + (r['total-memory'] ? ' / ' + fmtBytes(r['total-memory']) : '')),
        tile('Thời gian chạy', fmtDur(rosSec(r.uptime)), true),
        tile('RouterOS', r.version || '–', true),
        tile('Thiết bị', r['board-name'] || '–', true),
        tile('Bộ nhớ lưu trữ', hdd == null ? '–' : pct(hdd), true),
        health.map(x => tile(x.name, x.value + (x.type === 'C' ? ' °C' : x.type === 'V' ? ' V' : ''), true)),
        tile('Thiết bị theo dõi', v.monitorsTotal ? `${v.monitorsTotal - v.monitorsDown}/${v.monitorsTotal} hoạt động` : 'chưa có', true));
      const names = v.ifaces.filter(i => !i.disabled).map(i => i.name);
      if (!ui.hist.iface || !names.includes(ui.hist.iface)) {
        const wan = v.wans.find(w => names.includes(w.iface));
        ui.hist.iface = wan ? wan.iface : names[0] || '';
      }
      const cur = [...ifSel.options].map(o => o.value).join('|');
      if (cur !== names.join('|')) {
        fill(ifSel, names.map(n => h('option', { value: n, selected: n === ui.hist.iface }, n)));
      }
      ifSel.value = ui.hist.iface;
      if (Date.now() - lastHist > 60000) loadHist();
    },
  };
}

// --- WAN tab
function tabWan(id) {
  let p, note;
  return {
    mount(el) {
      p = tablePanel('Đường truyền WAN', 'wan');
      note = h('div', { class: 'hint' });
      el.append(h('div', { class: 'stack' }, p.el, note));
    },
    update(v) {
      const st = w => w.configured ? (w.link.status === 'down' ? 'down' : w.inet.status) : (w.running ? 'up' : w.running === false ? 'down' : 'unknown');
      p.draw([
        { t: 'WAN', v: w => w.label || w.iface, r: w => h('div', null, h('b', null, w.label || w.iface), w.label ? h('div', { class: 'muted small' }, w.iface) : null) },
        { t: 'Link', v: w => w.running ? 1 : 0, r: w => w.running == null ? badge('error', 'Không thấy') : badge(w.running ? 'up' : 'down', w.running ? 'Có link' : 'Mất link') },
        { t: 'Internet', v: w => st(w), r: w => w.configured ? badge(w.inet.status, w.inet.status === 'up' ? 'Thông' : w.inet.status === 'down' ? 'Mất Internet' : null) : h('span', { class: 'muted small' }, 'chưa theo dõi') },
        { t: 'Ping', num: true, v: w => w.inet.rtt, r: w => w.inet.rtt > 0 ? w.inet.rtt.toFixed(1) + ' ms' : '–' },
        { t: 'Vào', num: true, v: w => w.rxBps, r: w => fmtBps(w.rxBps) },
        { t: 'Ra', num: true, v: w => w.txBps, r: w => fmtBps(w.txBps) },
        { t: 'Từ lúc', v: w => w.inet.since, r: w => w.configured && w.inet.since ? since(w.inet.since) : '–' },
      ], v.wans, { empty: 'Không phát hiện WAN nào', rowClass: w => w.configured ? null : 'off' });
      fill(note, v.wans.some(w => !w.configured) ?
        ['WAN mờ là gợi ý tự phát hiện, chưa được kiểm tra Internet. ', me.role === 'admin' ? h('a', { href: `#/admin/routers?edit=${id}` }, 'Chọn WAN cần theo dõi →') : 'Liên hệ quản trị để bật theo dõi.'] :
        `Internet được kiểm tra bằng ping ${v.wanTarget} qua từng WAN mỗi 30 giây; báo mất sau 2 lần liên tiếp thất bại.`);
    },
  };
}

// --- devices tab
function tabDevices(id) {
  let p, view;
  return {
    mount(el) {
      const extra = me.role === 'admin' ? h('div', { class: 'row' },
        h('button', { class: 'primary', onclick: () => monitorForm(id, view, {}) }, '+ Thêm thiết bị'),
        h('button', { onclick: () => suggestForm(id, view) }, 'Thêm từ DHCP/neighbor')) : null;
      p = tablePanel('Thiết bị theo dõi (ping từ router)', 'dev-' + id, extra);
      el.append(p.el);
    },
    update(v) {
      view = v;
      const parentName = m => m.parentId ? (v.monitors.find(x => x.id === m.parentId) || {}).name || '?' : v.name;
      const cols = [
        { t: 'Trạng thái', v: m => m.kind === 'virtual' ? 'z' : m.state?.status || 'unknown', r: m => m.kind === 'virtual' ? h('span', { class: 'muted small' }, 'nút ảo') : !m.enabled ? badge('disabled', 'Tạm dừng') : badge(m.state?.status || 'unknown') },
        { t: 'Tên', v: m => m.name, r: m => h('div', null, h('b', null, m.name), h('div', { class: 'muted small' }, KIND[m.kind]?.[1] || m.kind)) },
        { t: 'IP', v: m => ipKey(m.ip), r: m => h('span', { class: 'mono' }, m.ip || '–') },
        { t: 'Ping', num: true, v: m => m.state?.rtt, r: m => m.state?.rtt > 0 ? m.state.rtt.toFixed(1) + ' ms' : '–' },
        { t: 'Sẵn sàng 24h', num: true, v: m => m.avail24, r: m => m.avail24 == null ? '–' : m.avail24.toFixed(m.avail24 === 100 ? 0 : 2) + '%' },
        { t: 'Từ lúc', v: m => m.state?.since, r: m => m.state?.since ? since(m.state.since) : '–' },
        { t: 'Nối với', v: parentName },
        { t: 'Báo Telegram', v: m => m.notify ? 1 : 0, r: m => m.kind === 'virtual' ? '–' : m.notify ? (m.repeatMin ? `Có, nhắc ${m.repeatMin}′` : 'Có') : 'Không' },
      ];
      if (me.role === 'admin') cols.push({ t: '', r: m => h('div', { class: 'row' }, h('button', { class: 'sm', onclick: () => monitorForm(id, view, m) }, 'Sửa')) });
      p.draw(cols, v.monitors, { empty: me.role === 'admin' ? 'Chưa có thiết bị. Bấm “Thêm thiết bị” hoặc chọn từ danh sách DHCP.' : 'Chưa có thiết bị', rowClass: m => m.enabled ? null : 'off' });
    },
  };
}

function descendants(monitors, id) {
  const out = new Set([id]); let grew = true;
  while (grew) { grew = false; for (const m of monitors) if (m.parentId && out.has(m.parentId) && !out.has(m.id)) { out.add(m.id); grew = true; } }
  return out;
}

function monitorForm(routerId, view, m) {
  const isNew = !m.id;
  const name = h('input', { value: m.name || '', required: true, maxlength: 64 });
  const ip = h('input', { value: m.ip || '', placeholder: '192.168.88.10', class: 'mono' });
  const kind = h('select', null, Object.entries(KIND).map(([k, [, t]]) => h('option', { value: k, selected: (m.kind || 'device') === k }, t)));
  const blocked = isNew ? new Set() : descendants(view.monitors, m.id);
  const parent = h('select', null, h('option', { value: '' }, view.name + ' (router)'),
    view.monitors.filter(x => !blocked.has(x.id)).map(x => h('option', { value: x.id, selected: m.parentId === x.id }, x.name + (x.ip ? ` (${x.ip})` : ''))));
  const enabled = h('input', { type: 'checkbox', checked: isNew ? true : m.enabled });
  const notify = h('input', { type: 'checkbox', checked: isNew ? true : m.notify });
  const repeat = h('select', null, [[0, 'Chỉ báo một lần'], [15, 'Nhắc lại mỗi 15 phút'], [30, 'Nhắc lại mỗi 30 phút'], [60, 'Nhắc lại mỗi giờ'], [120, 'Nhắc lại mỗi 2 giờ']].map(([v, t]) => h('option', { value: v, selected: (m.repeatMin || 0) === v }, t)));
  const syncKind = () => { const virt = kind.value === 'virtual'; ip.disabled = virt; ip.required = !virt; };
  kind.addEventListener('change', syncKind); syncKind();
  const actions = [{ t: 'Huỷ', fn: c => c() }, { t: isNew ? 'Thêm' : 'Lưu', cls: 'primary', submit: true, fn: async close => {
    const body = { name: name.value, ip: ip.value, kind: kind.value, parentId: parent.value ? +parent.value : null, enabled: enabled.checked, notify: notify.checked, repeatMin: +repeat.value };
    try {
      await api(isNew ? 'POST' : 'PUT', isNew ? `/api/admin/routers/${routerId}/monitors` : `/api/admin/monitors/${m.id}`, body);
      close(); toast('Đã lưu thiết bị', 'ok'); refresh();
    } catch (e) { toast(e.message, 'bad'); }
  } }];
  if (!isNew) actions.unshift({ t: 'Xoá', cls: 'danger', fn: async close => {
    if (!confirm(`Xoá “${m.name}”? Các thiết bị nối với nó sẽ chuyển lên nút cha.`)) return;
    try { await api('DELETE', `/api/admin/monitors/${m.id}`); close(); refresh(); } catch (e) { toast(e.message, 'bad'); }
  } });
  modal(isNew ? 'Thêm thiết bị' : 'Sửa thiết bị', h('div', { class: 'form' },
    field('Tên hiển thị', name), field('Địa chỉ IP', ip), field('Loại', kind), field('Nối với (trên sơ đồ)', parent),
    h('div', { class: 'full row' }, check('Ping định kỳ', enabled), check('Báo Telegram khi mất kết nối', notify)), field('Khi vẫn chưa khắc phục', repeat),
    h('p', { class: 'hint full' }, 'Router ping 2 gói mỗi 30 giây. Thiết bị chỉ bị báo mất kết nối sau 2 lần liên tiếp không phản hồi; lỗi chạy lệnh ping không được tính.')), actions);
}

function suggestForm(routerId, view) {
  const have = new Set(view.monitors.map(m => m.ip));
  const items = [];
  for (const n of view.sections.neighbor || []) if (n.address && !have.has(n.address)) items.push({ name: n.identity || n.board || n.address, ip: n.address, kind: /CRS|CSS|switch/i.test(n.board || '') ? 'switch' : /cAP|wAP|hAP|AP/i.test(n.board || '') ? 'ap' : 'device', src: 'neighbor ' + (n.platform || '') });
  for (const c of mergeClients(view.sections)) if (!have.has(c.ip) && !items.some(i => i.ip === c.ip)) items.push({ name: c.host || c.ip, ip: c.ip, kind: 'device', src: c.dhcp ? 'DHCP' : 'ARP' });
  const boxes = items.map(i => ({ i, cb: h('input', { type: 'checkbox' }) }));
  modal('Thêm thiết bị từ dữ liệu router', boxes.length ? h('div', { class: 'tw' }, h('table', null, h('tbody', null, boxes.map(({ i, cb }) => h('tr', null,
    h('td', null, cb), h('td', null, i.name), h('td', { class: 'mono' }, i.ip), h('td', { class: 'muted small' }, i.src)))))) :
    h('div', { class: 'empty' }, 'Không còn địa chỉ nào chưa theo dõi.'), [{ t: 'Đóng', fn: c => c() }, { t: 'Thêm mục đã chọn', cls: 'primary', submit: true, fn: async close => {
      const chosen = boxes.filter(b => b.cb.checked);
      for (const { i } of chosen) {
        try { await api('POST', `/api/admin/routers/${routerId}/monitors`, { name: i.name, ip: i.ip, kind: i.kind, enabled: true, notify: true, repeatMin: 0, parentId: null }); }
        catch (e) { toast(i.ip + ': ' + e.message, 'bad'); }
      }
      close(); if (chosen.length) toast(`Đã thêm ${chosen.length} thiết bị`, 'ok'); refresh();
    } }]);
}

// --- topology tab
function tabMap(id) {
  let wrap, svg, saveBtn, view, pos = {}, dirty = false;
  const NW = 170, NH = 50;
  const key = n => n.id === 0 ? 'r' : 'm' + n.id;

  function autoLayout(nodes) {
    // simple top-down tree: router on top, children spread by subtree width
    const kids = {};
    for (const n of nodes) if (n.id !== 0) { const p = n.parentId && nodes.some(x => x.id === n.parentId) ? n.parentId : 0; (kids[p] = kids[p] || []).push(n); }
    const out = {}; let cursor = 0;
    const place = (n, depth) => {
      const ch = kids[n.id] || [];
      if (!ch.length) { out[key(n)] = { x: cursor * (NW + 30), y: depth * 110 }; cursor++; return; }
      const start = cursor;
      ch.forEach(c => place(c, depth + 1));
      out[key(n)] = { x: (start + cursor - 1) / 2 * (NW + 30), y: depth * 110 };
    };
    place(nodes[0], 0);
    return out;
  }

  function draw() {
    const nodes = [{ id: 0, name: view.name, ip: view.identity || '', kind: 'router', status: view.status, x: view.mapX, y: view.mapY },
      ...view.monitors.map(m => ({ ...m, status: m.kind === 'virtual' ? 'virtual' : !m.enabled ? 'unknown' : m.state?.status || 'unknown' }))];
    const auto = autoLayout(nodes);
    for (const n of nodes) if (!pos[key(n)]) pos[key(n)] = n.x != null && n.y != null ? { x: n.x, y: n.y } : auto[key(n)];
    const xs = nodes.map(n => pos[key(n)].x), ys = nodes.map(n => pos[key(n)].y);
    const minX = Math.min(...xs) - 30, minY = Math.min(...ys) - 30, maxX = Math.max(...xs) + NW + 30, maxY = Math.max(...ys) + NH + 30;
    svg = s('svg', { class: 'map', viewBox: `${minX} ${minY} ${maxX - minX} ${maxY - minY}` });
    svg.style.maxWidth = Math.max(640, (maxX - minX) * 1.1) + 'px';
    const edges = s('g'), layer = s('g');
    svg.append(edges, layer);
    const edgeEls = [];
    for (const n of nodes) {
      if (n.id === 0) continue;
      const p = n.parentId && nodes.some(x => x.id === n.parentId) ? nodes.find(x => x.id === n.parentId) : nodes[0];
      const line = s('path', { class: 'edge' + (n.status === 'down' ? ' down' : '') });
      edgeEls.push({ line, a: key(p), b: key(n) });
      edges.append(line);
    }
    const layoutEdges = () => {
      for (const e of edgeEls) {
        const a = pos[e.a], b = pos[e.b];
        const x1 = a.x + NW / 2, y1 = a.y + NH, x2 = b.x + NW / 2, y2 = b.y, my = (y1 + y2) / 2;
        e.line.setAttribute('d', `M${x1},${y1} C${x1},${my} ${x2},${my} ${x2},${y2}`);
      }
    };
    for (const n of nodes) {
      const k = key(n), [abbr] = n.id === 0 ? ['RT'] : KIND[n.kind] || ['?'];
      const g = s('g', { class: `node ${n.status} ${n.kind === 'virtual' ? 'virtual' : ''} ${n.id === 0 ? 'router' : ''} ${me.role === 'admin' ? 'drag' : ''}`, transform: `translate(${pos[k].x},${pos[k].y})` },
        s('title', null, `${n.name}${n.ip ? ' – ' + n.ip : ''}\n${ST_TEXT[n.status] || ''}`),
        s('rect', { class: 'bg', width: NW, height: NH, rx: 9 }),
        s('rect', { class: 'kind', x: 8, y: 12, width: 30, height: 26, rx: 6 }),
        s('text', { class: 'kind', x: 23, y: 29, 'text-anchor': 'middle' }, abbr),
        s('text', { x: 46, y: 22 }, n.name.length > 17 ? n.name.slice(0, 16) + '…' : n.name),
        s('text', { class: 'ip', x: 46, y: 38 }, n.ip || (n.kind === 'virtual' ? 'không ping' : '')));
      if (me.role === 'admin') {
        g.addEventListener('pointerdown', e => {
          e.preventDefault(); g.setPointerCapture(e.pointerId); ui.drag = true;
          const m = svg.getScreenCTM().inverse();
          const pt = (ev) => { const p = new DOMPoint(ev.clientX, ev.clientY).matrixTransform(m); return p; };
          const start = pt(e), origin = { ...pos[k] };
          const move = ev => {
            const p = pt(ev);
            pos[k] = { x: Math.round((origin.x + p.x - start.x) / 10) * 10, y: Math.round((origin.y + p.y - start.y) / 10) * 10 };
            g.setAttribute('transform', `translate(${pos[k].x},${pos[k].y})`); layoutEdges();
            if (!dirty) { dirty = true; saveBtn.classList.remove('hidden'); }
          };
          const up = () => { g.removeEventListener('pointermove', move); g.removeEventListener('pointerup', up); g.removeEventListener('pointercancel', up); ui.drag = false; };
          g.addEventListener('pointermove', move); g.addEventListener('pointerup', up); g.addEventListener('pointercancel', up);
        });
      }
      layer.append(g);
    }
    layoutEdges();
    fill(wrap, svg);
  }

  return {
    mount(el) {
      wrap = h('div', { class: 'map-wrap' });
      saveBtn = h('button', { class: 'primary hidden', onclick: e => busy(e.target, async () => {
        const nodes = Object.entries(pos).map(([k, p]) => ({ id: k === 'r' ? 0 : +k.slice(1), x: p.x, y: p.y }));
        await api('PUT', `/api/admin/routers/${id}/layout`, { nodes });
        dirty = false; saveBtn.classList.add('hidden'); toast('Đã lưu sơ đồ', 'ok');
      }) }, 'Lưu bố cục');
      const tools = me.role === 'admin' ? h('div', { class: 'row' }, saveBtn,
        h('button', { onclick: () => { if (!view) return; pos = autoLayout([{ id: 0 }, ...view.monitors].map(n => n)); dirty = true; saveBtn.classList.remove('hidden'); draw(); } }, 'Tự sắp xếp'),
        h('button', { onclick: () => view && monitorForm(id, view, {}) }, '+ Thêm nút')) : null;
      el.append(h('div', { class: 'panel' }, h('div', { class: 'panel-h' }, h('h2', null, 'Sơ đồ mạng'), h('span', { class: 'muted small' }, me.role === 'admin' ? 'Kéo thả để sắp xếp. Đường nối theo mục “Nối với” của thiết bị.' : ''), h('div', { class: 'grow' }), tools), wrap));
    },
    update(v) {
      view = v;
      if (dirty || ui.drag) return; // don't move nodes under the user's hands
      pos = {};
      draw();
    },
  };
}

// --- ports tab
function tabPorts(id) {
  let p;
  return {
    mount(el) { p = tablePanel('Interface', 'ports'); el.append(p.el); },
    update(v) {
      const wans = new Set(v.wans.filter(w => w.configured).map(w => w.iface));
      p.draw([
        { t: '', v: i => i.running ? 1 : 0, r: i => h('span', { class: 'dot ' + (i.disabled ? '' : i.running ? 'up' : 'down') }) },
        { t: 'Tên', v: i => i.name, r: i => h('div', null, h('b', null, i.name), wans.has(i.name) ? h('span', { class: 'chip', title: 'WAN' }, ' WAN') : null, i.comment ? h('div', { class: 'muted small' }, i.comment) : null) },
        { t: 'Loại', v: i => i.type },
        { t: 'Trạng thái', v: i => i.disabled ? 'tắt' : i.running ? 'chạy' : 'không link', r: i => i.disabled ? badge('disabled', 'Tắt') : badge(i.running ? 'up' : 'down', i.running ? 'Có link' : 'Không link') },
        { t: 'Vào', num: true, v: i => i.rxBps, r: i => fmtBps(i.rxBps) },
        { t: 'Ra', num: true, v: i => i.txBps, r: i => fmtBps(i.txBps) },
        { t: 'Tổng vào', num: true, v: i => i.rxBytes, r: i => fmtBytes(i.rxBytes) },
        { t: 'Tổng ra', num: true, v: i => i.txBytes, r: i => fmtBytes(i.txBytes) },
        { t: 'MAC', v: i => i.mac, r: i => h('span', { class: 'mono small' }, i.mac || '') },
      ], v.ifaces, { rowClass: i => i.disabled ? 'off' : null });
    },
  };
}

// --- clients tab
function mergeClients(sec) {
  const m = new Map();
  for (const l of sec.lease || []) {
    const ip = l['active-address'] || l.address; if (!ip) continue;
    m.set(ip, { ip, mac: l['active-mac-address'] || l['mac-address'] || '', host: l['host-name'] || l.comment || '', dhcp: l.status || '', server: l.server || '', iface: '', arp: '', lastSeen: l['last-seen'] || '' });
  }
  for (const a of sec.arp || []) {
    if (!a.address) continue;
    const c = m.get(a.address) || { ip: a.address, mac: '', host: '', dhcp: '', server: '', lastSeen: '' };
    c.mac = c.mac || a['mac-address'] || ''; c.iface = a.interface || ''; c.arp = a.status || (truthy(a.complete) ? 'complete' : '');
    m.set(a.address, c);
  }
  return [...m.values()].sort((a, b) => ipKey(a.ip) - ipKey(b.ip));
}

function tabClients(id) {
  let p, view;
  return {
    mount(el) { p = tablePanel('Client (DHCP + ARP)', 'clients'); el.append(p.el); },
    update(v) {
      view = v;
      const watched = new Set(v.monitors.map(m => m.ip));
      const cols = [
        { t: 'IP', v: c => ipKey(c.ip), r: c => h('span', { class: 'mono' }, c.ip) },
        { t: 'Tên máy', v: c => c.host },
        { t: 'MAC', v: c => c.mac, r: c => h('span', { class: 'mono small' }, c.mac) },
        { t: 'DHCP', v: c => c.dhcp, r: c => c.dhcp ? badge(c.dhcp === 'bound' ? 'up' : 'unknown', c.dhcp) : '–' },
        { t: 'ARP', v: c => c.arp, r: c => c.arp ? badge(['reachable', 'permanent', 'complete'].includes(c.arp) ? 'up' : c.arp === 'failed' ? 'down' : 'unknown', c.arp) : '–' },
        { t: 'Interface', v: c => c.iface || c.server },
        { t: 'Thấy lần cuối', v: c => rosSec(c.lastSeen), r: c => c.lastSeen ? fmtDur(rosSec(c.lastSeen)) + ' trước' : '–' },
      ];
      if (me.role === 'admin') cols.push({ t: '', r: c => watched.has(c.ip) ? h('span', { class: 'muted small' }, 'đang theo dõi') : h('button', { class: 'sm', onclick: () => monitorForm(id, view, { name: c.host || c.ip, ip: c.ip }) }, '+ Theo dõi') });
      p.draw(cols, mergeClients(v.sections), { empty: 'Chưa có dữ liệu DHCP/ARP (cập nhật mỗi phút).' });
    },
  };
}

// --- PPPoE tab
function tabPpp(id) {
  let a, c;
  return {
    mount(el) { a = tablePanel('Phiên PPP đang hoạt động', 'ppp'); c = tablePanel('PPPoE client (đường ra)', 'pppc'); el.append(h('div', { class: 'stack' }, a.el, c.el)); },
    update(v) {
      a.draw([
        { t: 'Tài khoản', v: x => x.name, r: x => h('b', null, x.name) },
        { t: 'Dịch vụ', v: x => x.service },
        { t: 'Caller ID (MAC)', v: x => x['caller-id'], r: x => h('span', { class: 'mono small' }, x['caller-id'] || '') },
        { t: 'IP', v: x => ipKey(x.address), r: x => h('span', { class: 'mono' }, x.address || '') },
        { t: 'Thời gian', num: true, v: x => rosSec(x.uptime), r: x => fmtDur(rosSec(x.uptime)) },
      ], v.sections.ppp_active || [], { empty: 'Không có phiên PPP (hoặc router không chạy PPP server).' });
      c.draw([
        { t: 'Tên', v: x => x.name, r: x => h('b', null, x.name) },
        { t: 'Trên cổng', v: x => x.interface },
        { t: 'Trạng thái', v: x => x.running, r: x => truthy(x.disabled) ? badge('disabled', 'Tắt') : badge(truthy(x.running) ? 'up' : 'down', truthy(x.running) ? 'Đã kết nối' : 'Chưa kết nối') },
        { t: 'Route mặc định', v: x => x['add-default-route'], r: x => truthy(x['add-default-route']) ? 'Có' : 'Không' },
      ], v.sections.pppoe_client || [], { empty: 'Không có PPPoE client.' });
    },
  };
}

// --- IP tab
function tabIp(id) {
  let a, r, d, n;
  return {
    mount(el) {
      a = tablePanel('Địa chỉ IP', 'addr'); r = tablePanel('Route mặc định', 'routes'); d = tablePanel('DHCP client', 'dhcpc'); n = tablePanel('Thiết bị lân cận (MNDP/LLDP/CDP)', 'neigh');
      el.append(h('div', { class: 'stack' }, a.el, r.el, h('div', { class: 'grid two' }, d.el, n.el)));
    },
    update(v) {
      const sec = v.sections;
      a.draw([
        { t: 'Địa chỉ', v: x => ipKey(x.address), r: x => h('span', { class: 'mono' }, x.address) },
        { t: 'Interface', v: x => x.interface },
        { t: 'Loại', v: x => truthy(x.dynamic) ? 'động' : 'tĩnh' },
        { t: 'Ghi chú', v: x => x.comment },
      ], sec.address || [], { rowClass: x => truthy(x.disabled) || truthy(x.invalid) ? 'off' : null });
      r.draw([
        { t: 'Gateway', v: x => x.gateway, r: x => h('span', { class: 'mono' }, x['immediate-gw'] || x.gateway) },
        { t: 'Distance', num: true, v: x => +x.distance },
        { t: 'Bảng', v: x => x['routing-table'] || 'main' },
        { t: 'Trạng thái', v: x => x.active, r: x => truthy(x.disabled) ? badge('disabled', 'Tắt') : truthy(x.active) ? badge('up', 'Đang dùng') : badge('unknown', 'Dự phòng') },
        { t: 'Ghi chú', v: x => x.comment },
      ], sec.route || []);
      d.draw([
        { t: 'Interface', v: x => x.interface },
        { t: 'Trạng thái', v: x => x.status, r: x => badge(x.status === 'bound' ? 'up' : 'unknown', x.status || '–') },
        { t: 'Địa chỉ', v: x => x.address, r: x => h('span', { class: 'mono' }, x.address || '') },
        { t: 'Gateway', v: x => x.gateway, r: x => h('span', { class: 'mono' }, x.gateway || '') },
      ], sec.dhcp_client || []);
      n.draw([
        { t: 'Tên', v: x => x.identity },
        { t: 'IP', v: x => ipKey(x.address), r: x => h('span', { class: 'mono' }, x.address || '') },
        { t: 'Thiết bị', v: x => [x.platform, x.board].filter(Boolean).join(' ') },
        { t: 'Cổng', v: x => x.interface },
      ], sec.neighbor || []);
    },
  };
}

// --- events tab
function tabEvents(id) {
  let box, last = 0;
  return {
    mount(el) { box = h('div'); el.append(h('div', { class: 'panel' }, h('div', { class: 'panel-h' }, h('h2', null, 'Sự kiện của router')), box)); },
    async update() {
      if (Date.now() - last < 10000) return;
      last = Date.now();
      try { fill(box, eventList(await api('GET', `/api/events?router=${id}&limit=300`))); } catch (e) { toast(e.message, 'bad'); }
    },
  };
}

// ---------- chart ----------
function lineChart(series, o = {}) {
  const W = 900, H = o.height || 200, L = 70, R = 12, T = 10, B = 24;
  const all = series.flatMap(x => x.pts.filter(p => p[1] != null));
  if (!all.length) return h('div', { class: 'empty' }, 'Chưa có dữ liệu');
  const t0 = Math.min(...series.flatMap(x => x.pts.map(p => p[0]))), t1 = Math.max(...series.flatMap(x => x.pts.map(p => p[0])));
  let vmax = o.max || Math.max(...all.map(p => p[1]));
  if (!vmax || vmax <= 0) vmax = 1;
  vmax *= o.max ? 1 : 1.1;
  const X = t => L + (t1 === t0 ? 0.5 : (t - t0) / (t1 - t0)) * (W - L - R);
  const Y = v => T + (1 - v / vmax) * (H - T - B);
  const svg = s('svg', { class: 'chart', viewBox: `0 0 ${W} ${H}`, role: 'img' });
  for (let i = 0; i <= 4; i++) {
    const v = vmax * i / 4, y = Y(v);
    svg.append(s('line', { class: 'grid-l', x1: L, x2: W - R, y1: y, y2: y }), s('text', { x: L - 6, y: y + 4, 'text-anchor': 'end' }, o.fmt ? o.fmt(v) : v.toFixed(0)));
  }
  const span = t1 - t0;
  for (let i = 0; i <= 5; i++) {
    const t = t0 + span * i / 5, d = new Date(t);
    const lbl = span > 2 * 86400000 ? d.toLocaleDateString('vi-VN', { day: '2-digit', month: '2-digit' }) + ' ' + d.toLocaleTimeString('vi-VN', { hour: '2-digit' }) + 'h' : d.toLocaleTimeString('vi-VN', { hour: '2-digit', minute: '2-digit' });
    svg.append(s('text', { x: X(t), y: H - 6, 'text-anchor': i === 0 ? 'start' : i === 5 ? 'end' : 'middle' }, lbl));
  }
  for (const se of series) {
    // break the line on missing values or gaps longer than o.gap
    let seg = [];
    const flush = () => {
      if (seg.length > 1) {
        const d = seg.map((p, i) => (i ? 'L' : 'M') + X(p[0]).toFixed(1) + ',' + Y(p[1]).toFixed(1)).join('');
        if (se.area) svg.append(s('path', { class: 'ar ' + se.cls, d: d + `L${X(seg.at(-1)[0]).toFixed(1)},${Y(0)}L${X(seg[0][0]).toFixed(1)},${Y(0)}Z` }));
        svg.append(s('path', { class: 'ln ' + se.cls, d }));
      } else if (seg.length === 1) svg.append(s('circle', { class: se.cls, cx: X(seg[0][0]), cy: Y(seg[0][1]), r: 2 }));
      seg = [];
    };
    let prev = null;
    for (const p of se.pts) {
      if (p[1] == null || (prev && o.gap && p[0] - prev[0] > o.gap)) flush();
      if (p[1] != null) seg.push(p);
      prev = p;
    }
    flush();
  }
  // hover readout
  const cur = s('line', { class: 'cur hidden', y1: T, y2: H - B });
  const tipText = s('text', { x: 8, y: 16 });
  const tipBox = s('rect', { rx: 5, width: 10, height: 10 });
  const tip = s('g', { class: 'tip hidden' }, tipBox, tipText);
  const hit = s('rect', { x: L, y: T, width: W - L - R, height: H - T - B, fill: 'transparent' });
  svg.append(cur, tip, hit);
  const times = series[0].pts.map(p => p[0]);
  hit.addEventListener('pointermove', e => {
    const p = new DOMPoint(e.clientX, e.clientY).matrixTransform(svg.getScreenCTM().inverse());
    const t = t0 + (p.x - L) / (W - L - R) * (t1 - t0);
    let i = 0, best = Infinity;
    times.forEach((x, j) => { const d = Math.abs(x - t); if (d < best) { best = d; i = j; } });
    const x = X(times[i]);
    cur.setAttribute('x1', x); cur.setAttribute('x2', x); cur.classList.remove('hidden');
    fill(tipText, s('tspan', { x: 8, dy: 0 }, new Date(times[i]).toLocaleString('vi-VN', { hour: '2-digit', minute: '2-digit', day: '2-digit', month: '2-digit' })),
      series.map(se => s('tspan', { x: 8, dy: 16 }, `${se.name}: ${se.pts[i] && se.pts[i][1] != null ? (o.fmt ? o.fmt(se.pts[i][1]) : se.pts[i][1]) : '–'}`)));
    const bw = 170, bh = 22 + series.length * 16;
    tipBox.setAttribute('width', bw); tipBox.setAttribute('height', bh);
    const tx = x + bw + 12 > W ? x - bw - 8 : x + 8;
    tip.setAttribute('transform', `translate(${tx},${T + 4})`); tip.classList.remove('hidden');
  });
  hit.addEventListener('pointerleave', () => { cur.classList.add('hidden'); tip.classList.add('hidden'); });
  return svg;
}

// ---------- admin: routers ----------
function adminRoutersPage() {
  let p, list = [], uninstall = '';
  const editId = +(new URLSearchParams(location.hash.split('?')[1] || '').get('edit') || 0);
  let opened = false;
  return {
    every: 10000,
    mount(main) {
      p = tablePanel('Router', 'adm-routers', h('button', { class: 'primary', onclick: () => routerForm({}) }, '+ Thêm router'));
      main.append(p.el);
    },
    async load() {
      const [a, ov] = await Promise.all([api('GET', '/api/admin/routers'), api('GET', '/api/overview')]);
      uninstall = a.uninstall;
      return { routers: a.routers, views: Object.fromEntries(ov.routers.map(v => [v.id, v])) };
    },
    update(d) {
      list = d.routers;
      p.draw([
        { t: 'Tên', v: r => r.name, r: r => h('a', { href: `#/r/${r.id}/overview` }, h('b', null, r.name)) },
        { t: 'Trạng thái', v: r => d.views[r.id]?.status, r: r => badge(d.views[r.id]?.status || 'unknown') },
        { t: 'Kết nối', v: r => r.mode, r: r => r.mode === 'pull' ? `REST ${r.tls ? 'https' : 'http'}://${r.host}${r.port ? ':' + r.port : ''}` : r.mode === 'push' ? (r.pushPaired ? 'Router tự gửi (đã cài script)' : 'Router tự gửi – chưa cài script') : 'Mô phỏng' },
        { t: 'WAN theo dõi', v: r => r.wans.length, r: r => r.wans.map(w => w.label || w.iface).join(', ') || '–' },
        { t: 'Lỗi gần nhất', v: r => d.views[r.id]?.lastErr, r: r => h('span', { class: 'small', style: null }, d.views[r.id]?.status !== 'up' ? d.views[r.id]?.lastErr || '' : '') },
        { t: '', r: r => h('div', { class: 'row' }, h('button', { class: 'sm', onclick: () => routerForm(r) }, 'Sửa'), r.mode === 'push' ? h('button', { class: 'sm', onclick: () => pushDialog(r, uninstall) }, 'Script cài') : null) },
      ], list, { empty: 'Chưa có router. Bấm “Thêm router”.', rowClass: r => r.enabled ? null : 'off' });
      if (editId && !opened) { opened = true; const r = list.find(x => x.id === editId); if (r) routerForm(r, d.views[r.id]); }
    },
  };
}

function routerForm(r, view) {
  const isNew = !r.id;
  const mode = h('select', null, [['pull', 'NetPulse đọc router qua REST API (khuyên dùng)'], ['push', 'Router tự gửi dữ liệu (script, cho router sau NAT)']].concat(me.demo ? [['demo', 'Mô phỏng (demo)']] : [])
    .map(([v, t]) => h('option', { value: v, selected: (r.mode || 'pull') === v }, t)));
  const name = h('input', { value: r.name || '', required: true, maxlength: 64 });
  const host = h('input', { value: r.host || '', placeholder: '192.168.88.1', class: 'mono' });
  const tls = h('input', { type: 'checkbox', checked: isNew ? true : r.tls });
  const verify = h('input', { type: 'checkbox', checked: !!r.verifyTls });
  const port = h('input', { type: 'number', min: 0, max: 65535, value: r.port || '', placeholder: 'mặc định 443/80' });
  const user = h('input', { value: r.username || 'netpulse', autocomplete: 'off' });
  const pass = h('input', { type: 'password', autocomplete: 'new-password', placeholder: r.hasPassword ? '(giữ nguyên)' : '' });
  const interval = h('input', { type: 'number', min: 5, max: 300, value: r.interval || 15 });
  const target = h('input', { value: r.wanTarget || '8.8.8.8', class: 'mono' });
  const chat = h('input', { value: r.chatId || '', placeholder: '-100123456789 (tuỳ chọn)', class: 'mono' });
  const enabled = h('input', { type: 'checkbox', checked: isNew ? true : r.enabled });
  const ifList = h('datalist', { id: 'np-ifaces' });
  const known = new Set([...(view?.wans || []).map(w => w.iface)]);
  const fillIfaces = names => { names.forEach(n => known.add(n)); fill(ifList, [...known].map(n => h('option', { value: n }))); };
  fillIfaces([]);
  const wanRows = h('div', { class: 'stack' });
  const addWan = (w = {}) => {
    const iface = h('input', { value: w.iface || '', placeholder: 'pppoe-out1 / ether1', list: 'np-ifaces', class: 'mono' });
    const label = h('input', { value: w.label || '', placeholder: 'Tên nhà mạng (VNPT, Viettel…)', maxlength: 40 });
    const row = h('div', { class: 'row wan-row' }, h('div', { class: 'grow' }, iface), h('div', { class: 'grow' }, label), h('button', { type: 'button', class: 'sm danger', onclick: () => row.remove() }, 'Bỏ'));
    row._get = () => ({ iface: iface.value.trim(), label: label.value.trim() });
    wanRows.append(row);
  };
  (r.wans || []).forEach(addWan);
  const detected = (view?.detectedWans || []).filter(n => !(r.wans || []).some(w => w.iface === n));
  const testOut = h('div');
  const pullBox = h('div', { class: 'form full' },
    field('Địa chỉ router (IP/tên miền)', host), field('Cổng (để trống = mặc định)', port), field('User router (chỉ đọc)', user), field('Mật khẩu', pass),
    h('div', { class: 'full row' }, check('Dùng HTTPS (www-ssl)', tls), check('Kiểm tra chứng chỉ (bỏ chọn nếu router dùng chứng chỉ tự ký)', verify)),
    h('div', { class: 'full row' }, h('button', { type: 'button', onclick: e => busy(e.target, async () => {
      const res = await api('POST', '/api/admin/routers/test', { ...collect(), id: r.id || 0 });
      fillIfaces(res.interfaces || []);
      fill(testOut, h('div', { class: 'box ok' }, `✓ Kết nối được: ${res.identity || ''} · ${res.board || ''} · RouterOS ${res.version || ''} · ${res.interfaces.length} interface`, res.pingWarning ? h('div', { class: 'err-line' }, res.pingWarning) : null));
    }).catch(() => { }) }, 'Kiểm tra kết nối'), testOut),
    h('details', { class: 'full' }, h('summary', { class: 'hint' }, 'Cách tạo user chỉ-đọc trên router (RouterOS 7.x)'),
      h('pre', null, `/user group add name=netpulse policy=read,test,api,rest-api
/user add name=netpulse group=netpulse password="MatKhauManh" address=<IP-may-NetPulse>/32
# HTTPS (khuyên dùng):
/certificate add name=netpulse-www common-name=router key-usage=tls-server
/certificate sign netpulse-www
/ip service set www-ssl certificate=netpulse-www disabled=no address=<IP-may-NetPulse>/32
# hoặc HTTP trong LAN tin cậy:
/ip service set www disabled=no address=<IP-may-NetPulse>/32`),
      h('p', { class: 'hint' }, 'User chỉ có quyền đọc và ping; không thể thay đổi cấu hình router. Nếu RouterOS cũ báo không có policy rest-api, bỏ nó khỏi lệnh đầu.')));
  const pushNote = h('p', { class: 'hint full' }, 'Sau khi lưu, bấm “Script cài” để tải file .rsc và nhập vào router. Router cần RouterOS 7.17+ và gọi được tới địa chỉ NetPulse.');
  const syncMode = () => { pullBox.classList.toggle('hidden', mode.value !== 'pull'); pushNote.classList.toggle('hidden', mode.value !== 'push'); };
  mode.addEventListener('change', syncMode); syncMode();
  function collect() {
    const body = { name: name.value, mode: mode.value, host: host.value, port: +port.value || 0, tls: tls.checked, verifyTls: verify.checked, username: user.value,
      interval: +interval.value || 15, wans: [...wanRows.children].map(x => x._get()).filter(w => w.iface), wanTarget: target.value, chatId: chat.value, enabled: enabled.checked };
    if (pass.value) body.password = pass.value;
    return body;
  }
  const actions = [{ t: 'Huỷ', fn: c => c() }, { t: isNew ? 'Thêm router' : 'Lưu', cls: 'primary', submit: true, fn: async close => {
    try {
      const saved = await api(isNew ? 'POST' : 'PUT', isNew ? '/api/admin/routers' : `/api/admin/routers/${r.id}`, collect());
      close(); toast('Đã lưu router', 'ok'); refresh();
      if (isNew && saved.mode === 'push') pushDialog(saved, '');
    } catch (e) { toast(e.message, 'bad'); }
  } }];
  if (!isNew) actions.unshift({ t: 'Xoá router', cls: 'danger', fn: async close => {
    if (!confirm(`Xoá router “${r.name}” cùng toàn bộ lịch sử, thiết bị và sự kiện?`)) return;
    try { await api('DELETE', `/api/admin/routers/${r.id}`); close(); toast('Đã xoá'); refresh(); } catch (e) { toast(e.message, 'bad'); }
  } });
  modal(isNew ? 'Thêm router' : 'Sửa router: ' + r.name, h('div', { class: 'form' },
    field('Tên hiển thị', name), field('Cách kết nối', mode, 'full'), pullBox, pushNote,
    field('Chu kỳ đọc (giây)', interval), field('IP kiểm tra Internet', target), field('Chat ID nhóm Telegram riêng', chat),
    h('div', { class: 'full stack' }, h('h3', null, 'WAN cần theo dõi'),
      h('p', { class: 'hint' }, 'NetPulse báo khi WAN mất link hoặc mất Internet (ping IP kiểm tra qua đúng interface đó). Với PPPoE, chọn interface pppoe-out.'),
      wanRows, ifList,
      h('div', { class: 'row' }, h('button', { type: 'button', class: 'sm', onclick: () => addWan() }, '+ Thêm WAN'),
        detected.length ? h('span', { class: 'hint' }, 'Phát hiện: ', detected.map(n => h('button', { type: 'button', class: 'link', onclick: () => addWan({ iface: n }) }, n))) : null)),
    h('div', { class: 'full' }, check('Đang giám sát (bỏ chọn để tạm dừng)', enabled))), actions);
}

function pushDialog(r, uninstall) {
  modal('Cài đặt script cho ' + r.name, h('div', { class: 'stack' },
    h('div', { class: 'box' }, h('b', null, 'Các bước:'), h('ol', null,
      h('li', null, 'Kiểm tra router cho phép fetch & scheduler: ', h('code', null, '/system device-mode print'), '. Nếu đang tắt: ', h('code', null, '/system device-mode update fetch=yes scheduler=yes'), ' rồi bấm nút reset/ngắt điện trong 5 phút để xác nhận.'),
      h('li', null, 'Bấm “Tải script” bên dưới (mỗi lần tải sẽ tạo khoá mới, khoá cũ hết hiệu lực).'),
      h('li', null, 'Kéo file .rsc vào mục Files của WinBox/WebFig.'),
      h('li', null, 'Mở Terminal và chạy: ', h('code', null, `/import file-name=netpulse-${r.id}.rsc`)),
      h('li', null, 'Sau khoảng 1 phút router sẽ hiển thị “Hoạt động”.'))),
    h('p', { class: 'hint' }, 'Script chỉ đọc dữ liệu (policy read,test) và chỉ tạo script/scheduler tên “netpulse”. Địa chỉ NetPulse trong script lấy theo địa chỉ bạn đang mở trang này, hoặc mục “Địa chỉ cho router” trong Cài đặt.'),
    uninstall ? h('details', null, h('summary', { class: 'hint' }, 'Gỡ cài đặt khỏi router'), h('pre', null, uninstall)) : null),
    [{ t: 'Đóng', fn: c => c() }, { t: 'Tải script', cls: 'primary', fn: async () => {
      if (r.pushPaired && !confirm('Router này đã có script. Tải mới sẽ thu hồi khoá cũ – cần import lại trên router. Tiếp tục?')) return;
      try {
        const blob = await api('POST', `/api/admin/routers/${r.id}/push-script`);
        const a = h('a', { href: URL.createObjectURL(blob), download: `netpulse-${r.id}.rsc` });
        document.body.append(a); a.click(); a.remove();
        r.pushPaired = true; toast('Đã tải script', 'ok');
      } catch (e) { toast(e.message, 'bad'); }
    } }]);
}

// ---------- admin: users ----------
function usersPage() {
  let p, routers = [];
  return {
    mount(main) {
      p = tablePanel('Người dùng', 'users', h('button', { class: 'primary', onclick: () => userForm({}, routers) }, '+ Thêm người dùng'));
      main.append(h('div', { class: 'stack' }, p.el, h('p', { class: 'hint' }, 'Quản trị viên xem và cấu hình mọi router. Người xem chỉ thấy các router được chọn và không thay đổi được gì.')));
    },
    async load() {
      const [u, r] = await Promise.all([api('GET', '/api/admin/users'), api('GET', '/api/admin/routers')]);
      routers = r.routers; return u;
    },
    update(users) {
      const rn = Object.fromEntries(routers.map(r => [r.id, r.name]));
      p.draw([
        { t: 'Tên đăng nhập', v: u => u.username, r: u => h('b', null, u.username) },
        { t: 'Vai trò', v: u => u.role, r: u => u.role === 'admin' ? 'Quản trị' : 'Người xem' },
        { t: 'Router được xem', v: u => u.routers.length, r: u => u.role === 'admin' ? 'Tất cả' : u.routers.map(id => rn[id]).filter(Boolean).join(', ') || '–' },
        { t: 'Trạng thái', v: u => u.disabled, r: u => u.disabled ? badge('disabled', 'Đã khoá') : badge('up', 'Hoạt động') },
        { t: '', r: u => h('button', { class: 'sm', onclick: () => userForm(u, routers) }, 'Sửa') },
      ], users);
    },
  };
}

function userForm(u, routers) {
  const isNew = !u.id;
  const name = h('input', { value: u.username || '', required: isNew, disabled: !isNew, maxlength: 32 });
  const role = h('select', null, [['viewer', 'Người xem (chỉ đọc)'], ['admin', 'Quản trị viên']].map(([v, t]) => h('option', { value: v, selected: (u.role || 'viewer') === v }, t)));
  const pass = h('input', { type: 'password', autocomplete: 'new-password', placeholder: isNew ? '≥ 10 ký tự' : '(để trống = giữ nguyên)' });
  const disabled = h('input', { type: 'checkbox', checked: !!u.disabled });
  const boxes = routers.map(r => ({ id: r.id, cb: h('input', { type: 'checkbox', checked: (u.routers || []).includes(r.id) }), name: r.name }));
  const rbox = h('div', { class: 'full stack' }, h('h3', null, 'Router được xem'), boxes.length ? h('div', { class: 'form' }, boxes.map(b => check(b.name, b.cb))) : h('p', { class: 'hint' }, 'Chưa có router.'));
  const sync = () => rbox.classList.toggle('hidden', role.value === 'admin');
  role.addEventListener('change', sync); sync();
  const actions = [{ t: 'Huỷ', fn: c => c() }, { t: 'Lưu', cls: 'primary', submit: true, fn: async close => {
    try {
      await api(isNew ? 'POST' : 'PUT', isNew ? '/api/admin/users' : `/api/admin/users/${u.id}`, { username: name.value, role: role.value, password: pass.value, disabled: disabled.checked, routers: boxes.filter(b => b.cb.checked).map(b => b.id) });
      close(); toast('Đã lưu', 'ok'); refresh();
    } catch (e) { toast(e.message, 'bad'); }
  } }];
  if (!isNew && u.id !== me.id) actions.unshift({ t: 'Xoá', cls: 'danger', fn: async close => {
    if (!confirm(`Xoá người dùng ${u.username}?`)) return;
    try { await api('DELETE', `/api/admin/users/${u.id}`); close(); refresh(); } catch (e) { toast(e.message, 'bad'); }
  } });
  modal(isNew ? 'Thêm người dùng' : 'Sửa ' + u.username, h('div', { class: 'form' }, field('Tên đăng nhập', name), field('Vai trò', role), field('Mật khẩu', pass),
    h('div', { class: 'full' }, check('Khoá tài khoản', disabled)), rbox), actions);
}

// ---------- admin: settings ----------
function settingsPage() {
  let box;
  return {
    mount(main) { box = h('div'); main.append(box); },
    load: () => api('GET', '/api/admin/settings'),
    update(d) {
      const st = d.settings;
      const token = h('input', { type: 'password', autocomplete: 'off', placeholder: d.hasToken ? '(đã lưu – để trống để giữ nguyên)' : '123456789:AA…' });
      const removeTok = h('input', { type: 'checkbox' });
      const chat = h('input', { value: st.adminChat, placeholder: 'Chat ID cá nhân hoặc nhóm quản trị', class: 'mono' });
      const sumOn = h('input', { type: 'checkbox', checked: st.summaryOn });
      const sumAt = h('input', { type: 'time', value: st.summaryAt });
      const repeat = h('select', null, [[0, 'Không nhắc lại'], [15, '15 phút'], [30, '30 phút'], [60, '1 giờ'], [120, '2 giờ']].map(([v, t]) => h('option', { value: v, selected: st.repeatMin === v }, t)));
      const tz = h('input', { value: st.timezone });
      const keep = h('input', { type: 'number', min: 1, max: 400, value: st.retentionDays });
      const base = h('input', { value: st.pushBase, placeholder: 'http://192.168.1.10:8080', class: 'mono' });
      const save = h('button', { class: 'primary', type: 'submit' }, 'Lưu cài đặt');
      const form = h('form', { class: 'stack', onsubmit: e => { e.preventDefault(); busy(save, async () => {
        const body = { adminChat: chat.value, summaryOn: sumOn.checked, summaryAt: sumAt.value, repeatMin: +repeat.value, timezone: tz.value, retentionDays: +keep.value, pushBase: base.value };
        if (removeTok.checked) body.tgToken = ''; else if (token.value) body.tgToken = token.value;
        page.update(await api('PUT', '/api/admin/settings', body)); toast('Đã lưu cài đặt', 'ok');
      }); } },
        h('div', { class: 'panel' }, h('div', { class: 'panel-h' }, h('h2', null, 'Cảnh báo Telegram'), d.telegramError ? h('span', { class: 'st down' }, d.telegramError) : null),
          h('div', { class: 'panel-b form' },
            field('Bot Token (tạo bằng @BotFather)', token), field('Chat ID quản trị (nhận mọi cảnh báo)', chat),
            d.hasToken ? h('div', { class: 'full' }, check('Xoá Bot Token đã lưu', removeTok)) : null,
            h('div', { class: 'full row' }, check('Gửi bản tin tổng hợp hằng ngày lúc', sumOn), h('div', null, sumAt)),
            field('Nhắc lại khi WAN/router vẫn lỗi', repeat),
            h('div', { class: 'full row' }, h('button', { type: 'button', onclick: e => busy(e.target, async () => { await api('POST', '/api/admin/settings/telegram-test', { chat: chat.value }); toast('Đã gửi tin thử – kiểm tra Telegram', 'ok'); }) }, 'Gửi tin thử'),
              h('span', { class: 'hint' }, 'Chưa biết Chat ID? Lưu Bot Token, nhắn /start cho bot (hoặc thêm bot vào nhóm và gõ /id) – bot sẽ trả lời Chat ID. Gõ /status để xem tình trạng bất cứ lúc nào.')))),
        h('div', { class: 'panel' }, h('div', { class: 'panel-h' }, h('h2', null, 'Hệ thống')),
          h('div', { class: 'panel-b form' },
            field('Múi giờ', tz), field('Số ngày lưu lịch sử', keep),
            field('Địa chỉ NetPulse cho router (chế độ push)', base, 'full'),
            h('p', { class: 'hint full' }, 'Để trống thì dùng địa chỉ bạn đang mở trang. Phải là địa chỉ router gọi tới được, ví dụ IP LAN hoặc tên miền.'))),
        h('div', { class: 'row' }, save));
      const backup = h('div', { class: 'panel' }, h('div', { class: 'panel-h' }, h('h2', null, 'Sao lưu')),
        h('div', { class: 'panel-b stack' },
          h('p', null, 'Tải bản sao database (đã nhất quán, an toàn khi đang chạy). Để khôi phục: dừng NetPulse, chép file vào ', h('code', null, d.dataDir + '/netpulse.db'), ' và giữ nguyên file ', h('code', null, 'secret.key'), ' (dùng để giải mã mật khẩu router và Bot Token).'),
          h('div', null, h('a', { class: 'btn', href: '/api/admin/backup' }, 'Tải bản sao lưu')),
          h('p', { class: 'hint' }, `NetPulse ${d.version}`)));
      fill(box, h('div', { class: 'stack' }, form, backup));
    },
  };
}

// ---------- account ----------
function accountPage() {
  return {
    mount(main) {
      const old = h('input', { type: 'password', required: true, autocomplete: 'current-password' });
      const p1 = h('input', { type: 'password', required: true, minlength: 10, autocomplete: 'new-password' });
      const p2 = h('input', { type: 'password', required: true, autocomplete: 'new-password' });
      const btn = h('button', { class: 'primary', type: 'submit' }, 'Đổi mật khẩu');
      main.append(h('form', { class: 'panel', onsubmit: e => { e.preventDefault(); busy(btn, async () => {
        if (p1.value !== p2.value) throw new Error('Hai mật khẩu mới không khớp');
        await api('POST', '/api/me/password', { old: old.value, new: p1.value });
        old.value = p1.value = p2.value = ''; toast('Đã đổi mật khẩu. Các thiết bị khác đã bị đăng xuất.', 'ok');
      }); } }, h('div', { class: 'panel-h' }, h('h2', null, 'Tài khoản: ' + me.username)),
        h('div', { class: 'panel-b form' }, field('Mật khẩu hiện tại', old), field('Mật khẩu mới (≥ 10 ký tự)', p1), field('Nhập lại mật khẩu mới', p2), h('div', { class: 'full' }, btn))));
    },
  };
}

boot();
