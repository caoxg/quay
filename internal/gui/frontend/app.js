import { call, on, isMock } from './api.js';
import { t, setLang, lang, detectLang, relTime, has } from './i18n.js';
import { icon } from './icons.js';

// ---------------------------------------------------------------- helpers
const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const short = (c) => (c || '').slice(0, 7);
const repoLabel = (u) => (u || '').startsWith('local:') ? u.slice(6) : (u || '').replace(/^https?:\/\//, '').replace(/^git@/, '').replace(/\.git$/, '');
const keyOf = (b, f) => `${b}/${f || ''}`;
const when = (iso) => (iso ? relTime(Date.parse(iso)) : '');
const plural = (n, word) => `${n} ${word}`;
const mb = (n) => `${Math.round((n || 0) / 1048576).toLocaleString()} MB`;
// A board's name is its hostname: one DNS label, also its network name NAME.local.
const HOSTNAME_RE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;
const mirrorOn = (d) => !!(d?.mirror?.managed || d?.mirror?.pip || (d?.mirror?.debian && !/deb\.debian\.org/.test(d.mirror.debian)));
const hostOf = (u) => (u || '').replace(/^[a-z+]+:\/\//, '').split('/')[0].toLowerCase();
// mirrorOf names the catalog mirror (or the official servers) a URL belongs to.
const mirrorOf = (u) => [...(S.info?.mirrors || []), S.info?.official].find((m) => m && hostOf(u) && [m.pypi, m.debian].some((x) => hostOf(x) === hostOf(u)));
const mirrorName = (m) => (m?.id === 'official' ? t('mirrorOfficialShort') : lang() === 'zh' ? m?.nameZh || m?.name : m?.name) || '';
const works = (r) => !!r && (!r.debian || (r.debian.ok && r.debian.security)) && (!r.pypi || r.pypi.ok);
const speed = (bps) => (bps >= 1048576 ? `${(bps / 1048576).toFixed(1)} MB/s` : `${Math.round((bps || 0) / 1024)} KB/s`);
const tildeKey = (p) => (/[\\/]\.ssh[\\/]/.test(p || '') ? '~/.ssh/' + p.split(/[\\/]/).pop() : p || '');
const keyOn = (st) => !!(S.info?.publicKey && st?.authorizedKeys?.includes(S.info.publicKey.blob));

// ---------------------------------------------------------------- state
const S = {
  info: null, boards: [], def: '', cur: null,
  status: {}, updates: {}, sys: {},
  ops: [], opMap: new Map(), busy: {}, activity: false,
  modals: [], menu: null,
};
const SHOT = isMock && new URLSearchParams(location.search).has('shot'); // README screenshots (build/screenshots.py)
if (isMock) window.__quay = { S, get modals() { return S.modals.map((m) => m.type); } };
if (new URLSearchParams(location.search).has('noanim')) document.head.insertAdjacentHTML('beforeend', '<style>*,*::before,*::after{animation:none!important;transition:none!important}</style>');
const VISIBLE = new Set(['deploy', 'update', 'start', 'stop', 'restart', 'autostart', 'prepare', 'revert', 'mirror', 'backup', 'restore', 'export', 'erase', 'rename', 'sysupdate', 'relay', 'uploadkey', 'policy', 'policyrestore', 'wifi', 'hotspot', 'away', 'sysfix']);

// ---------------------------------------------------------------- rendering scheduler
const dirty = new Set();
let raf = 0, rafTimer = 0;
function invalidate(...parts) {
  parts.forEach((p) => dirty.add(p));
  if (!raf) {
    raf = requestAnimationFrame(flush);
    rafTimer = setTimeout(flush, 40); // rAF is paused while the window is hidden
  }
}
function flush() {
  if (raf) cancelAnimationFrame(raf);
  clearTimeout(rafTimer);
  raf = 0;
  const parts = [...dirty];
  dirty.clear();
  if (parts.includes('sidebar')) renderSidebar();
  if (parts.includes('main')) renderMain();
  if (parts.includes('activity')) renderActivity();
  if (parts.includes('modal')) renderModal();
  if (parts.includes('menu')) renderMenu();
}
const renderAll = () => invalidate('sidebar', 'main', 'activity', 'modal', 'menu');

// ---------------------------------------------------------------- operations
const waiters = new Map();
const pending = new Map();
on('quay:op', (e) => {
  trackOp(e);
  const w = waiters.get(e.op);
  if (!w) {
    if (!pending.has(e.op)) pending.set(e.op, []);
    pending.get(e.op).push(e);
    if (pending.size > 300) pending.delete(pending.keys().next().value);
    return;
  }
  deliver(w, e);
});
function deliver(w, e) {
  try { w.onEvent?.(e); } catch (err) { console.error(err); }
  if (e.type !== 'end') return;
  waiters.delete(e.op);
  if (e.state === 'ok') w.resolve(e.result);
  else {
    const err = new Error(e.error || e.state);
    err.cancelled = e.state === 'cancel';
    err.unsafe = !!e.unsafe;
    w.reject(err);
  }
}
/** runOp awaits a background operation. onId receives its id (for Cancel). */
async function runOp(started, onEvent, onId) {
  const id = await started;
  onId?.(id);
  return new Promise((resolve, reject) => {
    const w = { resolve, reject, onEvent };
    waiters.set(id, w);
    const buf = pending.get(id);
    if (buf) { pending.delete(id); buf.forEach((e) => deliver(w, e)); }
  });
}

/** runForceable starts an op with start(false); if the app's safety probe refuses, it asks and
 *  repeats it with start(true). */
async function runForceable(start, onEvent, onId) {
  try {
    return await runOp(start(false), onEvent, onId);
  } catch (e) {
    if (!e.unsafe || !(await confirm({ icon: 'alert', danger: true, title: t('unsafeTitle'), text: `<b>${esc(e.message)}</b><br><br>${t('unsafeText')}`, ok: t('forceAnyway') }))) throw e;
    return runOp(start(true), onEvent, onId);
  }
}

function opTitle(kind, board, folder) {
  const app = appName(board, folder);
  if (kind === 'autostart' && !folder) return t('op_autostart_none');
  return t('op_' + kind, { app });
}
function appName(board, folder) {
  const a = S.status[board]?.data?.apps?.find((x) => x.folder === folder);
  return a?.name || folder || '';
}

function trackOp(e) {
  if (!VISIBLE.has(e.kind)) return;
  let o = S.opMap.get(e.op);
  if (!o) {
    o = { id: e.op, kind: e.kind, board: e.board, folder: e.folder, state: 'run', steps: [], logs: [], notes: [], started: e.time || Date.now(), open: true, showLog: false, title: opTitle(e.kind, e.board, e.folder) };
    S.opMap.set(e.op, o);
    S.ops.unshift(o);
    S.ops.forEach((x, i) => { if (i > 0 && x.state !== 'run') x.open = false; });
    if (S.ops.length > 40) { const old = S.ops.pop(); S.opMap.delete(old.id); }
    if (e.folder !== undefined) S.busy[keyOf(e.board, e.folder)] = e.op;
  }
  switch (e.type) {
    case 'step': {
      const last = [...o.steps].reverse().find((s) => s.key === e.step);
      if (last && last.state === 'run') { last.state = e.state; if (e.state === 'fail' || e.state === 'ok') last.text = e.text || last.text; }
      else o.steps.push({ key: e.step, state: e.state, text: e.text });
      invalidate('main');
      break;
    }
    case 'progress': { // keep where the step's progress started, for the time left
      const pr = e.data || {}, now = e.time || Date.now();
      const base = o.progress?.step === e.step ? o.progress : { step: e.step, t0: now, p0: pr.percent || 0 };
      o.progress = { ...base, ...pr, t: now };
      invalidate('main');
      break;
    }
    case 'log': o.logs.push(e.text); if (o.logs.length > 3000) o.logs.splice(0, o.logs.length - 3000); break;
    case 'warn': o.notes.push({ kind: 'warn', text: e.text }); break;
    case 'info': if (e.text && !e.text.startsWith('saved ')) o.notes.push({ kind: 'info', text: e.text }); break;
    case 'end':
      o.state = e.state; o.error = e.error; o.result = e.result; o.ended = e.time || Date.now();
      if (o.state === 'fail') { o.open = true; o.showLog = o.logs.length > 0; }
      delete S.busy[keyOf(e.board, e.folder)];
      invalidate('main', 'sidebar');
      break;
  }
  invalidate('activity');
}

function busyOp(board, folder) {
  const id = S.busy[keyOf(board, folder)];
  return id ? S.opMap.get(id) : null;
}
const runningCount = () => S.ops.filter((o) => o.state === 'run').length;

// ---------------------------------------------------------------- toasts
function toast(kind, text, action, timeout = 5000) {
  const box = $('#toasts');
  const el = document.createElement('div');
  el.className = 'toast ' + kind;
  const ico = { ok: 'check', err: 'alert', warn: 'alert', info: 'info' }[kind] || 'info';
  el.innerHTML = `${icon(ico)}<span>${esc(text)}</span>${action ? `<button class="act">${esc(action.label)}</button>` : ''}<button class="close">${icon('x', 'sm')}</button>`;
  const remove = () => { el.style.opacity = '0'; el.style.transition = 'opacity .2s'; setTimeout(() => el.remove(), 200); };
  el.querySelector('.close').onclick = remove;
  if (action) el.querySelector('.act').onclick = () => { remove(); action.fn(); };
  box.appendChild(el);
  if (timeout) setTimeout(remove, kind === 'err' ? timeout * 2 : timeout);
}
function fail(what, err) {
  if (err?.cancelled) return;
  toast('err', `${t('t_failed', { what })}: ${err?.message || err}`, { label: t('viewDetails'), fn: () => { S.activity = true; invalidate('activity', 'main'); } });
}

// ---------------------------------------------------------------- data loading
async function loadBoards() {
  const r = await call('Boards');
  S.boards = r.boards || [];
  S.def = r.default || '';
  if (S.cur && !S.boards.find((b) => b.name === S.cur)) S.cur = null;
  if (!S.cur) S.cur = S.def || S.boards[0]?.name || null;
}
const board = (name) => S.boards.find((b) => b.name === name);
const boardTarget = (b) => (b.kind === 'adb' ? (b.serial ? `USB · ${b.serial}` : 'USB') : `${b.user || 'arduino'}@${b.host}${b.port && b.port !== 22 ? ':' + b.port : ''}`);

async function refresh(name = S.cur) {
  if (!name) return;
  const st = (S.status[name] ||= {});
  if (st.loading) return;
  st.loading = true;
  invalidate('main', 'sidebar');
  try {
    const res = await runOp(call('Status', name), (e) => {
      if (e.type === 'step' && e.step === 'connect') { st.connecting = e.state === 'run'; invalidate('main', 'sidebar'); }
    });
    if (res.board && res.board !== name) { renameLocal(name, res.board); name = res.board; } // renamed on the board
    st.data = res;
    st.error = null;
    checkUpdates(name, st.data.apps || []);
    if (!S.sys[name]) loadSystem(name);
  } catch (e) {
    if (!e.cancelled) { st.error = e.message; st.data = null; }
  } finally {
    st.loading = false; st.connecting = false;
    invalidate('main', 'sidebar');
  }
}
// loadSystem lists the board's pending system updates (refresh: run apt-get update first).
async function loadSystem(name, refresh = false, onEvent) {
  const s = (S.sys[name] ||= {});
  if (s.loading) return s.data;
  s.loading = true; s.error = null;
  invalidate('main', 'modal');
  try { s.data = await runOp(call('SystemCheck', name, refresh), onEvent); } catch (e) { if (!e.cancelled) s.error = e.message || String(e); } finally { s.loading = false; invalidate('main', 'modal'); }
  return s.data;
}

// renameLocal follows a board rename (by Quay, or noticed in the board's status).
function renameLocal(old, nw) {
  if (!old || !nw || old === nw) return;
  for (const m of [S.status, S.updates, S.sys]) if (m[old]) { m[nw] = m[old]; delete m[old]; }
  S.ops.forEach((o) => { if (o.board === old) o.board = nw; });
  if (S.cur === old) S.cur = nw;
  loadBoards().then(() => renderAll());
}

async function checkUpdates(name, apps) {
  if (!apps.some((a) => a.deploy)) return;
  try {
    const list = await runOp(call('CheckUpdates', apps));
    S.updates[name] = Object.fromEntries((list || []).map((u) => [u.folder, u]));
    invalidate('main');
  } catch { /* offline: no badge */ }
}

// ---------------------------------------------------------------- sidebar
function renderSidebar() {
  const items = S.boards.map((b) => {
    const st = S.status[b.name] || {};
    const dot = st.connecting || st.loading ? 'busy' : st.error ? 'err' : st.data ? 'ok' : '';
    return `<div class="board-item ${b.name === S.cur ? 'active' : ''}" data-act="select-board" data-board="${esc(b.name)}">
      <div class="chip">${icon(b.kind === 'adb' ? 'usb' : 'wifi')}</div>
      <div class="meta"><div class="title">${esc(b.name)}${b.name === S.def ? `<span class="star" title="default">★</span>` : ''}${Object.values(S.updates[b.name] || {}).some((u) => u.behind) ? `<span class="reddot" title="${esc(t('updateAvailable'))}"></span>` : ''}</div><div class="sub">${esc(boardTarget(b))}</div></div>
      <span class="dot ${dot}"></span></div>`;
  }).join('');
  $('#sidebar').innerHTML = `
    <div class="drag"></div>
    <div class="brand"><div class="logo">${icon('anchor')}</div><div><div class="name">Quay</div><div class="tag">${t('tagline')}</div></div></div>
    <div class="side-section">
      <div class="side-label"><span>${t('boards')}</span><button data-act="add-board" title="${t('addBoard')}">${icon('plus', 'sm')}</button></div>
      <div class="boards">${items}</div>
      <button class="add-board" data-act="add-board">${icon('plus')} ${t('addBoard')}</button>
    </div>
    <div class="side-foot">
      <button class="side-link" data-act="reveal-backups">${icon('archive')} ${t('backupsFolder')}</button>
      <div class="ver"><button class="ver-link" data-act="about" title="${esc(t('about'))}">Quay${SHOT ? '' : ` ${esc(S.info?.version || '')}${isMock ? ' · preview' : ''}`}</button>
        <span class="seg-mini"><button class="${lang() === 'en' ? 'on' : ''}" data-act="lang" data-lang="en">EN</button><button class="${lang() === 'zh' ? 'on' : ''}" data-act="lang" data-lang="zh">中</button></span></div>
    </div>`;
}

// ---------------------------------------------------------------- main
function renderMain() {
  const el = $('#main');
  const prev = $('#content');
  const scroll = prev ? prev.scrollTop : 0;
  if (!S.boards.length) {
    el.innerHTML = `<header class="topbar"></header><div class="content" id="content"><div class="content-inner">${welcome()}</div></div>`;
    return;
  }
  const b = board(S.cur);
  const st = S.status[S.cur] || {};
  let body;
  if (st.data) body = warnings(st.data) + overview(st.data) + appsSection(st.data);
  else if (st.error && !st.loading) body = errorCard(b, st.error);
  else body = connectingCard(b);
  el.innerHTML = topbar(b, st) + `<div class="content" id="content"><div class="content-inner">${body}</div></div>`;
  const c = $('#content');
  c.scrollTop = scroll;
  c.addEventListener('scroll', () => $('.topbar')?.classList.toggle('scrolled', c.scrollTop > 4), { passive: true });
}

function topbar(b, st) {
  const state = st.connecting ? t('connecting') : st.data ? t('connected') : st.loading ? t('connecting') : t('offline');
  const dot = st.connecting || st.loading ? 'busy' : st.error ? 'err' : st.data ? 'ok' : '';
  const n = runningCount();
  return `<header class="topbar">
    <h1>${esc(b?.name || '')}</h1>
    <div class="where"><span class="dot ${dot}"></span><span>${esc(state)}</span><span>·</span><span>${esc(b ? boardTarget(b) : '')}</span>${st.data?.hostname && st.data.hostname !== b?.name ? `<span>·</span><span>${esc(st.data.hostname)}</span>` : ''}</div>
    <div class="spacer"></div>
    <button class="btn ghost" data-act="toggle-activity">${icon('activity')} ${t('activity')}${n ? `<span class="badge">${n}</span>` : ''}</button>
    <button class="btn icon" data-act="refresh" title="${t('refresh')}">${icon('refresh', st.loading ? 'spin' : '')}</button>
    <button class="btn icon" data-act="board-menu" title="${t('editBoard')}">${icon('more')}</button>
    <button class="btn primary" data-act="install" ${st.data ? '' : 'disabled'}>${icon('plus')} ${t('installApp')}</button>
  </header>`;
}

function welcome() {
  return `<div class="empty welcome">
    <div class="art">${icon('anchor')}</div>
    <h3>${t('welcomeTitle')}</h3><p>${t('welcomeText')}</p>
    <div class="actions"><button class="btn primary" data-act="add-board">${icon('wifi')} ${t('addSSH')}</button><button class="btn" data-act="add-board" data-mode="usb">${icon('usb')} ${t('addUSB')}</button></div>
    <div class="steps-hint">
      <div><span class="n">1</span><b>${t('welcomeStep1')}</b>${t('welcomeStep1d')}</div>
      <div><span class="n">2</span><b>${t('welcomeStep2')}</b>${t('welcomeStep2d')}</div>
      <div><span class="n">3</span><b>${t('welcomeStep3')}</b>${t('welcomeStep3d')}</div>
    </div></div>`;
}

function connectingCard(b) {
  return `<div class="overview">${'<div class="stat"><div class="skeleton" style="height:11px;width:40%;margin-bottom:10px"></div><div class="skeleton" style="height:15px;width:75%"></div></div>'.repeat(4)}</div>
    <div class="state-card"><div>${icon('spinner', 'lg spin')}</div><h3>${t('connectingTo', { name: esc(b?.name) })}</h3><p>${t('firstConnectHint')}</p></div>`;
}

function errorCard(b, err) {
  return `<div class="state-card"><div style="color:var(--danger)">${icon('alert', 'lg')}</div>
    <h3>${t('cantConnect', { name: esc(b?.name) })}</h3><div class="err">${esc(err)}</div>
    <div class="actions"><button class="btn primary" data-act="refresh">${icon('refresh')} ${t('retry')}</button><button class="btn" data-act="edit-board">${icon('edit')} ${t('editBoard')}</button></div></div>`;
}

// A system profile's finding (another tool's setting that matters here), with its fix.
function findingHtml(f, busy) {
  const a = f.action;
  return `<div class="callout warn finding">${icon('info')}<div class="grow">${esc(zhOr(f.text, f.textZh))}</div>${a ? `<button class="btn sm" data-act="sys-fix" data-id="${esc(f.id)}" ${busy ? 'disabled' : ''}>${esc(zhOr(a.label, a.labelZh))}</button>` : ''}</div>`;
}
function warnings(d) {
  let h = (d.findings || []).map((f) => findingHtml(f)).join('');
  if (!d.appcli) h += `<div class="callout warn" style="margin:6px 0 14px">${icon('alert')}<div class="grow">${t('noAppCLI')}</div></div>`;
  else if (!d.isUnoQ) h += `<div class="callout warn" style="margin:6px 0 14px">${icon('info')}<div class="grow">${t('notUnoQ')}</div></div>`;
  return h;
}

function overview(d) {
  const ip = (d.ips || []).find((x) => x.iface?.startsWith('wl')) || (d.ips || [])[0];
  const otherIPs = (d.ips || []).filter((x) => x !== ip).map((x) => x.ip).join(', ');
  const host = (u) => (u || '').replace(/^https?:\/\//, '').split('/')[0];
  const mOn = mirrorOn(d);
  const mCur = mirrorOf(d.mirror?.debian && !/deb\.debian\.org/.test(d.mirror.debian) ? d.mirror.debian : d.mirror?.pip);
  const mBackups = (d.mirror?.debianFallback || []).filter((u) => !/deb\.debian\.org/.test(u)).length;
  const mirrorHosts = mOn ? (mCur ? mirrorName(mCur) : [d.mirror?.pip, d.mirror?.debian].filter(Boolean).map(host).join(', ')) + (mBackups ? ' · ' + t('mirrorBackups', { n: mBackups }) : '') : '';
  const cli = (d.appcliVersion || '').replace(/^arduino[ -]app[ -]cli\s*/i, '').replace(/^version\s*/i, '');
  const sys = S.sys[S.cur] || {};
  const u = sys.data;
  // The board's operating system on the value line, App CLI and the update state below it.
  const osName = d.os?.version ? `Debian ${d.os.version}${d.os.codename ? ` (${d.os.codename})` : ''}` : d.os?.pretty || '';
  const cliS = cli ? `App CLI ${esc(cli)}` : '';
  const sysName = d.system ? `${zhOr(d.system.name, d.system.nameZh)}${d.system.version ? ' ' + d.system.version : ''}` : '';
  const baseS = d.system ? [d.system.base ? t('sysBase', { v: d.system.base }) : '', osName].filter(Boolean).join(' · ') : '';
  let sysV = esc(sysName || osName || cli || '✓'), sysS = [baseS, cliS, t('sysCheck')].filter(Boolean).join(' · '), sysDot = '';
  if (!d.appcli) { sysV = esc(osName) || `<span style="color:var(--danger)">${t('notFound')}</span>`; sysS = `App CLI ${t('notFound')}`; }
  const sysOp = S.ops.find((o) => o.kind === 'sysupdate' && o.board === S.cur && o.state === 'run');
  if (sysOp) {
    const st = [...sysOp.steps].reverse().find((x) => x.state === 'run');
    const p = sysOp.progress?.step === st?.key ? sysOp.progress : null;
    sysS = `<b class="accent-text">${esc(st ? stepLabel(st) : t('running'))}${p ? ' · ' + esc(progressText(p)) : ''}</b>` + (p ? progressBar(p) : '');
  } else if (sys.loading && !u) sysS = [baseS, cliS, t('sysChecking')].filter(Boolean).join(' · ');
  else if (u?.total?.count) { sysS = `${[baseS, cliS].filter(Boolean).join(' · ')} · <b class="accent-text">${esc(t('sysUpdates', { n: u.total.count }))} · ${mb(u.total.size)}</b>`; sysDot = '<span class="reddot"></span>'; }
  else if (u) sysS = `${[baseS, cliS].filter(Boolean).join(' · ')} · ${t('sysUpToDate')}`;
  const relay = d.relay || {};
  const ri = S.relayInfo || {};
  const busy = busyOp(S.cur, undefined) ? 'disabled' : '';
  return `<section class="overview">
    <div class="stat"><div class="k">${icon('cpu', 'sm')} ${t('model')}</div><div class="v" title="${esc(d.model)}">${esc(d.model || '—')}</div><div class="s">${esc(d.hostname)}${d.serial ? ' · ' + esc(t('serial', { s: d.serial })) : ''}</div></div>
    <div class="stat click" data-act="network" title="${esc(t('netSettings'))}"><div class="k">${icon('globe', 'sm')} ${t('network')}</div><div class="v mono">${esc(ip?.ip || t('noIP'))}</div><div class="s">${esc(netLine(d.net) || [ip?.iface, otherIPs].filter(Boolean).join(' · '))}</div></div>
    <div class="stat click" data-act="system" title="${esc(t('systemUpdate'))}"><div class="k">${icon('package', 'sm')} ${t('sysTitle')}${sysDot}${sys.loading ? ` ${icon('spinner', 'sm spin')}` : ''}</div><div class="v" title="${esc(d.os?.pretty || '')}">${sysV}</div><div class="s">${sysS}</div></div>
    <div class="stat toggle"><div class="k">${icon('download', 'sm')} ${t('mirror')}${helpQ('mirror')}</div>
      <div class="row"><div class="v">${mOn ? t('mirrorOn') : t('mirrorOff')}</div><button class="btn sm ghost speed-btn" data-act="mirror-test" ${busy} title="${esc(t('mirrorTestHelp'))}">${icon('activity', 'sm')} ${t('mirrorSpeedTest')}</button><button class="switch ${mOn ? 'on' : ''}" data-act="mirror" ${busy}></button></div>
      <div class="s">${mOn ? `<a class="link" data-act="mirror-info" title="${esc(t('mirrorDetails'))}">${esc(mirrorHosts)}</a>` : esc('pypi.org · deb.debian.org')}</div></div>
    <div class="stat toggle"><div class="k">${icon('link', 'sm')} ${t('relayCard')}${helpQ('relay')}</div>
      <div class="row"><div class="v">${relay.installed ? t('mirrorOn') : t('relayOff')}</div><button class="switch ${relay.installed ? 'on' : ''}" data-act="relay" ${busy}></button></div>
      <div class="s">${relay.installed ? `${relay.active ? t('relayActive') : t('relayIdle')} · <a class="link" data-act="relay-upstream" title="${esc(t('relayChange'))}">${esc(t('relayVia', { p: hostOf(ri.effective) || t('relayDirectShort') }))}</a>` : t('relayOffSub')}</div></div>
  </section>`;
}

function appsSection(d) {
  const apps = d.apps || [];
  const head = `<div class="section-head"><h2>${t('apps')}</h2><span class="count">${apps.length}</span><div class="spacer"></div></div>`;
  if (!apps.length) {
    const cat = (S.info?.catalog || [])[0];
    return head + `<div class="empty"><div class="art">${icon('package')}</div><h3>${t('noAppsTitle')}</h3><p>${t('noAppsText')}</p>
      <div class="actions">${cat ? `<button class="btn primary" data-act="install" data-cat="${esc(cat.id)}">${icon('download')} ${t('install')} ${esc(cat.name)}</button>` : ''}<button class="btn" data-act="install" data-tab="git">${icon('branch')} ${t('installFromGit')}</button></div></div>`;
  }
  return head + `<div class="app-grid">${apps.map((a) => appCard(a, d)).join('')}</div>`;
}

function statusPill(s) {
  const cls = { running: 'ok', failed: 'err', starting: 'warn', stopping: 'warn' }[s] || '';
  return `<span class="pill ${cls}"><span class="dot ${cls === 'ok' ? 'ok' : cls === 'err' ? 'err' : cls === 'warn' ? 'busy' : ''}"></span>${t('st_' + (s || 'stopped'))}</span>`;
}


// A profile's icon wins over the app.yaml emoji (a built-in profile can know the app better). SVG
// icons are drawn through <img>, where markup from a repository's quay.yaml cannot run scripts.
function appIcon(prof, fallback) {
  const ic = (prof?.icon || '').trim();
  if (!ic.startsWith('<svg')) return `<div class="app-icon">${esc(ic || fallback || '📦')}</div>`;
  const svg = ic.includes('xmlns=') ? ic : ic.replace('<svg', '<svg xmlns="http://www.w3.org/2000/svg"');
  return `<div class="app-icon"><img alt="" src="data:image/svg+xml;charset=utf-8,${encodeURIComponent(svg)}"></div>`;
}
function appCard(a, d) {
  const op = busyOp(S.cur, a.folder);
  const upd = S.updates[S.cur]?.[a.folder];
  const running = ['running', 'starting'].includes(a.status);
  const p = a.profile;
  const tags = [];
  if (a.default) tags.push(`<span class="pill accent">${icon('boot', 'sm')} ${t('runsAtBoot')}</span>`);
  if (upd?.behind) tags.push(`<span class="pill accent click" data-act="update" data-folder="${esc(a.folder)}"><span class="reddot"></span>${t('updateAvailable')}</span>`);
  if (p) tags.push(`<span class="pill click" data-act="profile-info" data-folder="${esc(a.folder)}" title="${esc(t('whatIsThis'))}">${icon('shield', 'sm')} ${esc(t('profileChip', { name: p.name }))}</span>`);
  if (p?.system && a.systemReady === true) tags.push(`<span class="pill ok">${icon('check', 'sm')} ${t('setupDone')}</span>`);
  if (a.data?.length) tags.push(`<span class="pill" title="${esc(a.data.join(', '))}">${icon('database', 'sm')} ${t('dataFiles', { n: a.data.length })}</span>`);
  const pols = Object.values(a.policies || {});
  if (p?.policies && pols.length) tags.push(`<span class="pill accent click" data-act="policies" data-folder="${esc(a.folder)}" data-tab="slots" title="${esc(pols.map((x) => x.name).join(', '))}">${icon('cpu', 'sm')} ${esc(t('policiesChip', { n: pols.length }))}</span>`);

  const dep = a.deploy;
  const src = dep
    ? (dep.repo.startsWith('local:')
      ? `<div class="row" title="${esc(dep.repo.slice(6))}">${icon('folder', 'sm')}<span>${t('localFolder')}</span><code>${esc(dep.repo.slice(6).split(/[\\/]/).filter(Boolean).pop() || '')}</code></div>`
      : `<div class="row">${icon('branch', 'sm')}<span class="t">${esc(repoLabel(dep.repo))}</span><span>@</span><code>${esc(dep.ref || '')}</code>${dep.commit ? `<code>${esc(short(dep.commit))}${dep.dirty ? '*' : ''}</code>` : ''}</div>`)
      + `<div class="row">${icon('clock', 'sm')}<span class="t">${esc(dep.subject || '')}${dep.subject ? ' · ' : ''}${esc(t('deployed', { when: when(dep.deployedAt) }))}</span>${upd && !upd.behind ? `<span class="latest">${icon('check', 'sm')}${t('isLatest')}</span>` : ''}</div>`
      + (upd?.behind ? `<div class="row upd" data-act="update" data-folder="${esc(a.folder)}"><span class="reddot"></span><span class="t"><b>${esc(upd.commits > 0 ? t('newCommits', { n: upd.commits }) : t('updateAvailable'))}</b> <code>${esc(short(upd.latest))}</code> ${esc(upd.subject || '')}${upd.date ? ' · ' + esc(when(upd.date)) : ''}</span></div>` : '')
    : `<div class="row">${icon('info', 'sm')}<span class="t">${t('notByQuay')}</span></div>`
      + (upd?.unknown ? `<div class="row" title="${esc(upd.repo)}">${icon('branch', 'sm')}<span class="t">${t('latestOnGitHub')} <code>${esc(short(upd.latest))}</code> ${esc(upd.subject || '')}${upd.date ? ' · ' + esc(when(upd.date)) : ''}</span></div>` : '');
  const setup = p?.system && a.systemReady === false
    ? `<div class="callout warn">${icon('alert')}<div class="grow">${esc(t('setupCallout', { name: p.name, what: firstSentence(zhOr(p.system.description, p.system.descriptionZh)) }))}</div><button class="btn sm" data-act="prepare" data-folder="${esc(a.folder)}">${t('applySetup')}</button></div>` : '';

  let foot;
  if (op) {
    const cur = [...op.steps].reverse().find((s) => s.state === 'run') || op.steps[op.steps.length - 1];
    foot = `<div class="progress">${icon('spinner', 'spin')}<span>${esc(op.title)}${cur ? ' — ' + esc(stepLabel(cur)) : ''}</span></div><div class="spacer"></div>
      <button class="btn sm ghost" data-act="toggle-activity">${t('activity')}</button>`;
  } else {
    foot = (running
      ? `<button class="btn sm" data-act="stop" data-folder="${esc(a.folder)}">${icon('stop', 'sm')} ${t('stop')}</button>`
      : `<button class="btn sm primary" data-act="start" data-folder="${esc(a.folder)}">${icon('play', 'sm')} ${t('start')}</button>`)
      + (a.webUrl ? consoleButton(a, running) : '')
      + (upd?.behind ? `<button class="btn sm has-dot" data-act="update" data-folder="${esc(a.folder)}">${icon('arrowUp', 'sm')} ${t('update')}<span class="reddot"></span></button>` : '')
      + (upd?.unknown ? `<button class="btn sm" data-act="takeover" data-folder="${esc(a.folder)}" title="${esc(t('takeoverHint'))}">${icon('download', 'sm')} ${t('takeover')}</button>` : '')
      + (p?.policies ? `<button class="btn sm" data-act="policies" data-folder="${esc(a.folder)}">${icon('cpu', 'sm')} ${t('policies')}</button>` : '')
      + `<div class="spacer"></div>
      <button class="btn sm ghost icon" data-act="logs" data-folder="${esc(a.folder)}" title="${t('logs')}">${icon('terminal', 'sm')}</button>
      <button class="btn sm ghost icon" data-act="app-menu" data-folder="${esc(a.folder)}" title="…">${icon('more', 'sm')}</button>`;
  }
  return `<article class="app-card ${op ? 'busy' : ''}">
    <div class="app-head">${appIcon(a.profile, a.icon)}
      <div class="app-title"><div class="name">${esc(a.name)} ${statusPill(a.status)}</div><div class="desc">${esc(a.description || '')}</div></div></div>
    <div class="app-meta">${src}<div class="row">${icon('folder', 'sm')}<span class="t mono">~/ArduinoApps/${esc(a.folder)}</span></div>${a.webUrl ? `<div class="row">${icon('globe', 'sm')}<a class="t mono link${running ? '' : ' off'}" data-act="open-web" data-url="${esc(a.webUrl)}" title="${esc(running ? t('openConsole') : t('consoleNeedsRun'))}">${esc(a.webUrl.replace(/^https?:\/\//, ''))}</a></div>` : ''}${actionsRow(a, running)}</div>
    ${tags.length ? `<div class="app-tags">${tags.join('')}</div>` : ''}
    ${setup}
    <div class="app-foot">${foot}</div>
  </article>`;
}

// actionsRow shows the profile's shortcuts (pages of the app's console) while the app runs.
function actionsRow(a, running) {
  const acts = (a.profile?.actions || []).map((x) => ({ x, url: actionURL(a, x) })).filter((y) => y.url);
  if (!acts.length || !running) return '';
  return `<div class="row actions-row">${icon('external', 'sm')}<span class="t">${acts.map(({ x, url }) => `<a class="link" data-act="open-web" data-url="${esc(url)}" title="${esc(zhOr(x.description, x.descriptionZh) || url)}">${esc(zhOr(x.label, x.labelZh))}</a>`).join(' · ')}</span></div>`;
}

// The app's own web console (profile web port, else the first port in app.yaml).
function consoleButton(a, running) {
  return running
    ? `<button class="btn sm" data-act="open-web" data-url="${esc(a.webUrl)}" title="${esc(a.webUrl)}">${icon('globe', 'sm')} ${t('console')}</button>`
    : `<button class="btn sm" disabled title="${esc(t('consoleNeedsRun'))}">${icon('globe', 'sm')} ${t('console')}</button>`;
}

// hooks lists what an app's profile makes Quay do, one line each, for the install preview, the
// catalog and the profile chip.
function hooks(p) {
  if (!p) return [];
  const z = (o, k = 'description') => zhOr(o?.[k], o?.[k + 'Zh']);
  const out = [];
  if (p.system) out.push({ icon: 'alert', title: t('hk_system'), text: z(p.system) });
  if (p.beforeStart) out.push({ icon: 'sliders', title: t('hk_before'), text: z(p.beforeStart) || t('hk_beforeRun', { run: p.beforeStart.run }) });
  if (p.health) out.push({ icon: 'activity', title: t('hk_health'), text: t('hk_healthText', { url: p.health.url, n: p.health.timeout }) });
  if (p.safety) out.push({ icon: 'shield', title: t('hk_safety'), text: t('hk_safetyText', { msg: p.safety.message || p.safety.field }) });
  if (p.web) out.push({ icon: 'globe', title: t('hk_web'), text: t('hk_webText', { port: p.web.port }) });
  if (p.policies) out.push({ icon: 'cpu', title: t('hk_policies'), text: p.policies.slots.map((s) => slotLabel(s)).join(lang() === 'zh' ? '、' : ', ') });
  if (p.actions?.length) out.push({ icon: 'external', title: t('hk_actions'), text: p.actions.map((a) => zhOr(a.label, a.labelZh)).join(lang() === 'zh' ? '、' : ', ') });
  if (p.uninstall) out.push({ icon: 'trash', title: t('hk_uninstall'), text: z(p.uninstall) || t('hk_uninstallRun') });
  return out;
}
function hooksHtml(p) {
  const list = hooks(p);
  if (!list.length) return '';
  const from = p.origin === 'repo' ? t('hooksFromRepo') : t('hooksBuiltin', { name: p.name });
  return `<p class="hooks-from">${esc(from)} ${t('hooksOnlyQuay')}</p>
    <ul class="hooks">${list.map((h) => `<li>${icon(h.icon, 'sm')}<div><b>${esc(h.title)}</b>${h.text ? `<span>${esc(h.text)}</span>` : ''}</div></li>`).join('')}</ul>`;
}
// actionURL resolves a profile action against the app's console (a path) or the board (a full URL).
function actionURL(a, act) {
  if (act.url.startsWith('/')) return a.webUrl ? new URL(act.url, a.webUrl).href : '';
  return act.url.replace('{host}', a.webUrl ? new URL(a.webUrl).hostname : '');
}

const helpQ = (topic) => `<button class="help-q" data-act="help" data-topic="${topic}" title="${esc(t('whatIsThis'))}">?</button>`;
function helpHtml(topic) {
  if (topic === 'mirror') return `<b>${t('mirror')}</b><p>${t('helpMirror')}</p>`;
  if (topic === 'away') return `<b>${t('awayTitle')}</b><p>${t('helpAway')}</p>${AWAY_MODES.map((m) => `<div class="pop-sec">${t('away_' + m)}</div><p>${t('helpAway_' + m)}</p>`).join('')}<p class="muted">${t('helpAwayNote')}</p>`;
  const ri = S.relayInfo || {};
  return `<b>${t('relayCard')}</b><p>${t('helpRelay')}</p>
    <div class="pop-row">${icon('link', 'sm')}<span>${esc(t('relayViaNow', { p: ri.effective || t('relayDirect') }))}${ri.upstream ? '' : ` <span class="muted">${t('relaySystem')}</span>`}</span></div>
    <button class="btn sm" data-act="relay-upstream">${t('relayChange')}</button>`;
}

function firstSentence(s) {
  s = (s || '').trim();
  const m = s.match(/^.+?(\. |。)/);
  return m ? m[0].trim() : s;
}
function stepLabel(s) { return has('s_' + s.key) ? t('s_' + s.key) : s.text; }

// ---------------------------------------------------------------- menus
function openMenu(anchor, items) {
  const r = anchor.getBoundingClientRect();
  S.menu = { x: Math.min(r.right - 230, window.innerWidth - 240), y: r.bottom + 6, items, flipY: r.bottom + 330 > window.innerHeight, top: r.top };
  invalidate('menu');
}
function closeMenu() { if (S.menu) { S.menu = null; invalidate('menu'); } }
function openPopover(anchor, html) {
  const r = anchor.getBoundingClientRect();
  const below = window.innerHeight - r.bottom - 18, above = r.top - 18;
  const up = below < 260 && above > below;
  S.menu = { x: Math.min(r.left - 12, window.innerWidth - 348), y: r.bottom + 6, top: r.top, up, maxH: Math.min(460, up ? above : below), html, items: [] };
  invalidate('menu');
}
function renderMenu() {
  const root = $('#menu-root');
  if (!S.menu) { root.innerHTML = ''; return; }
  const m = S.menu;
  if (m.html) { root.innerHTML = `<div class="menu pop" style="left:${Math.max(8, m.x)}px;${m.up ? `bottom:${window.innerHeight - m.top + 6}px` : `top:${m.y}px`};max-height:${m.maxH}px">${m.html}</div>`; return; }
  root.innerHTML = `<div class="menu" style="left:${Math.max(8, m.x)}px;${m.flipY ? `bottom:${window.innerHeight - m.top + 6}px` : `top:${m.y}px`}">${m.items.map((it, i) => {
    if (it === '-') return '<hr>';
    if (it.hint) return `<div class="hint">${esc(it.hint)}</div>`;
    return `<button data-menu="${i}" class="${it.danger ? 'danger' : ''}" ${it.disabled ? 'disabled' : ''}>${icon(it.icon)} ${esc(it.label)}</button>`;
  }).join('')}</div>`;
}

function appMenu(anchor, a) {
  const running = ['running', 'starting'].includes(a.status);
  const p = a.profile;
  const items = [
    ...(a.webUrl ? [{ icon: 'globe', label: t('openConsole'), disabled: !running, fn: () => call('OpenURL', a.webUrl) },
      ...(p?.actions || []).map((x) => ({ icon: 'external', label: zhOr(x.label, x.labelZh), disabled: !running || !actionURL(a, x), fn: () => call('OpenURL', actionURL(a, x)) })), '-'] : []),
    { icon: 'restart', label: t('restart'), disabled: !running, fn: () => act.restart({ folder: a.folder }) },
    { icon: 'branch', label: t('updateTo'), disabled: !a.deploy || a.deploy.repo.startsWith('local:'), fn: () => openUpdate(a) },
    { icon: 'terminal', label: t('logs'), fn: () => act.logs({ folder: a.folder }) },
    { icon: 'boot', label: a.default ? t('dontRunAtBoot') : t('runAtBoot'), fn: () => doAutostart(a.default ? '' : a.folder) },
    '-',
    { icon: 'archive', label: t('backup'), disabled: !a.data?.length, fn: () => doBackup(a) },
    { icon: 'restore', label: t('restore'), fn: () => doRestore(a) },
    { icon: 'package', label: t('export'), fn: () => doExport(a) },
  ];
  if (p?.policies) items.splice(a.webUrl ? 2 : 0, 0, { icon: 'cpu', label: t('policies') + '…', fn: () => openPolicies(a) });
  if (p?.system) {
    items.push('-');
    items.push(a.systemReady ? { icon: 'restart', label: t('revertSetup'), fn: () => doPrepare(a, true) } : { icon: 'shield', label: t('systemSetup'), fn: () => doPrepare(a, false) });
  }
  items.push('-', { icon: 'trash', label: t('erase'), danger: true, fn: () => openErase(a) });
  openMenu(anchor, items);
}

// ---------------------------------------------------------------- activity
function renderActivity() {
  const root = $('#activity-root');
  if (!S.activity) { root.innerHTML = ''; return; }
  const scrolls = {};
  root.querySelectorAll('.log[data-op]').forEach((el) => { scrolls[el.dataset.op] = el.scrollHeight - el.scrollTop - el.clientHeight < 30; });
  const list = S.ops.length ? S.ops.map(opCard).join('') : `<div class="empty-act">${icon('activity', 'lg')}<p>${t('noActivity')}</p></div>`;
  const head = `<h3>${t('activity')}</h3>
    ${S.ops.some((o) => o.state !== 'run') ? `<button class="btn sm ghost" data-act="clear-ops">${t('clearFinished')}</button>` : ''}
    <button class="btn sm ghost icon" data-act="toggle-activity">${icon('x', 'sm')}</button>`;
  let aside = root.querySelector('aside.activity');
  if (!aside) {
    root.innerHTML = `<aside class="activity enter"><header></header><div class="list"></div></aside>`;
    aside = root.querySelector('aside.activity');
  }
  const listEl = aside.querySelector('.list');
  const keep = listEl.scrollTop;
  aside.querySelector('header').innerHTML = head;
  listEl.innerHTML = list;
  listEl.scrollTop = keep;
  root.querySelectorAll('.log[data-op]').forEach((el) => { if (scrolls[el.dataset.op] !== false) el.scrollTop = el.scrollHeight; });
}

// progressText says how far a step is: percent, amount, and time left once the rate is known.
function progressText(p) {
  const pct = Math.floor(p.percent || 0);
  const amount = p.unit === 'bytes' ? `${mb(p.done)} / ${mb(p.total)}` : t('progressPkgs', { d: p.done, t: p.total });
  const el = (p.t - p.t0) / 1000, done = (p.percent || 0) - p.p0;
  let left = '';
  if (done >= 1 && el >= 3 && pct < 100) {
    const rest = (el / done) * (100 - p.percent);
    left = rest >= 60 ? t('etaMin', { n: Math.round(rest / 60) }) : t('etaSec', { n: Math.max(1, Math.round(rest)) });
  }
  return [`${pct}%`, amount, left].filter(Boolean).join(' · ');
}
const progressBar = (p) => `<div class="pbar"><i style="width:${Math.min(100, p.percent || 0)}%"></i></div>`;

function opCard(o) {
  const ico = o.state === 'run' ? `<span class="state-ico run">${icon('spinner', 'spin')}</span>`
    : o.state === 'ok' ? `<span class="state-ico ok">${icon('check', 'sm')}</span>`
    : o.state === 'cancel' ? `<span class="state-ico cancel">${icon('x', 'sm')}</span>`
    : `<span class="state-ico fail">${icon('x', 'sm')}</span>`;
  const sub = `${esc(o.board)} · ${o.state === 'run' ? t('running') : o.state === 'ok' ? t('done') : o.state === 'cancel' ? t('cancelled') : t('failed')} · ${esc(relTime(o.ended || o.started))}`;
  let body = '';
  if (o.open) {
    const steps = o.steps.map((s) => {
      const si = s.state === 'run' ? icon('spinner', 'sm spin') : s.state === 'ok' ? icon('check', 'sm') : s.state === 'fail' ? icon('x', 'sm') : '–';
      const label = stepLabel(s);
      const detail = s.text && s.text !== label ? `<span class="detail">${esc(s.text)}</span>` : '';
      const prog = s.state === 'run' && o.progress?.step === s.key ? `${progressBar(o.progress)}<span class="pinfo">${esc(progressText(o.progress))}</span>` : '';
      return `<li class="${s.state}"><span class="ico">${si}</span><span class="grow">${esc(label)}${detail}${prog}</span></li>`;
    }).join('');
    const notes = o.notes.length ? `<div class="notes">${o.notes.map((n) => `<div class="note ${n.kind}">${icon(n.kind === 'warn' ? 'alert' : 'info', 'sm')}<span>${esc(n.text)}</span></div>`).join('')}</div>` : '';
    const err = o.state === 'fail' && o.error ? `<div class="err">${esc(o.error)}</div>` : '';
    const logs = o.logs.length ? `<button class="log-toggle" data-act="op-log" data-op="${o.id}">${icon(o.showLog ? 'chevronDown' : 'chevronRight', 'sm')} ${o.showLog ? t('hideLog') : t('showLog')} (${o.logs.length})</button>
      ${o.showLog ? `<div class="log" data-op="${o.id}">${o.logs.slice(-600).map(logLine).join('\n')}</div>` : ''}` : '';
    body = `<div class="body"><ul class="steps">${steps}</ul>${notes}${err}${logs}</div>`;
  }
  return `<div class="op ${o.state === 'run' ? 'running' : ''}">
    <div class="op-head" data-act="op-toggle" data-op="${o.id}">${ico}<div class="ttl"><b>${esc(o.title)}</b><span>${sub}</span></div>
    ${o.state === 'run' ? `<button class="btn sm" data-act="op-cancel" data-op="${o.id}">${t('cancel')}</button>` : icon(o.open ? 'chevronDown' : 'chevronRight', 'sm')}</div>${body}</div>`;
}
function logLine(l) {
  const s = esc(l);
  if (/\b(error|failed|fatal|traceback)\b/i.test(l)) return `<span class="l-err">${s}</span>`;
  if (/\bwarn(ing)?\b/i.test(l)) return `<span class="l-warn">${s}</span>`;
  return s;
}

// ---------------------------------------------------------------- modals
// Dialogs that make sense once: a second click on the same button while one is open is ignored.
const SINGLE_MODALS = new Set(['board', 'install', 'update', 'erase', 'mirror', 'relay', 'rename', 'system', 'restore', 'logs', 'about', 'policies', 'polcheck', 'polindex']);
function pushModal(type, data = {}) {
  if (SINGLE_MODALS.has(type) && S.modals.some((m) => m.type === type)) return Promise.resolve(false);
  return new Promise((resolve) => { S.modals.push({ type, data, resolve }); invalidate('modal'); setTimeout(focusModal, 30); });
}
function topModal() { return S.modals[S.modals.length - 1]; }
function closeModal(value) {
  const m = S.modals.pop();
  m?.resolve(value);
  if (m?.onClose) m.onClose();
  invalidate('modal');
  setTimeout(focusModal, 30);
}
function focusModal() {
  const el = $('#modal-root [autofocus]') || $('#modal-root input:not([type=checkbox])');
  if (el && document.activeElement?.closest('#modal-root') == null) el.focus();
}

const modalViews = {};
function renderModal() {
  const root = $('#modal-root');
  const m = topModal();
  if (!m) { root.innerHTML = ''; return; }
  const v = modalViews[m.type](m.data, m);
  const active = document.activeElement;
  const activeBind = active?.dataset?.bind;
  const sel = active && 'selectionStart' in active ? [active.selectionStart, active.selectionEnd] : null;
  m.id ||= Math.random().toString(36).slice(2);
  const existing = root.querySelector(`.modal[data-mid="${m.id}"]`);
  if (existing) { // same dialog: update in place so the entrance animation does not replay
    existing.className = `modal ${v.cls || ''}`;
    existing.innerHTML = v.html;
  } else {
    const fresh = !root.querySelector('.overlay');
    root.innerHTML = `<div class="overlay ${fresh ? 'enter' : ''}" data-act="overlay"><div class="modal ${v.cls || ''} enter" data-mid="${m.id}" role="dialog">${v.html}</div></div>`;
  }
  if (activeBind) {
    const el = root.querySelector(`[data-bind="${activeBind}"]`);
    if (el) { el.focus(); if (sel && el.setSelectionRange) try { el.setSelectionRange(...sel); } catch { /* */ } }
  }
  root.querySelectorAll('.term').forEach((el) => { if (m.data.follow !== false) el.scrollTop = el.scrollHeight; });
}
const header = (ico, title, sub, cls = '') => `<header><div class="mi ${cls}">${icon(ico)}</div><div><h3>${title}</h3>${sub ? `<p>${sub}</p>` : ''}</div><button class="btn sm ghost icon x" data-act="modal-close">${icon('x', 'sm')}</button></header>`;
const field = (label, input, help = '') => `<div class="field"><label>${label}</label>${input}${help ? `<div class="help">${help}</div>` : ''}</div>`;
const input = (bind, value, ph = '', extra = '') => `<input class="input" data-bind="${bind}" value="${esc(value ?? '')}" placeholder="${esc(ph)}" spellcheck="false" autocomplete="off" ${extra}>`;
const check = (bind, on, label, help = '') => `<label class="check"><input type="checkbox" data-bind="${bind}" ${on ? 'checked' : ''}><span>${label}${help ? `<span class="help">${help}</span>` : ''}</span></label>`;

// --- confirm
modalViews.confirm = (d) => ({
  html: header(d.icon || 'info', d.title, '', d.danger ? 'danger' : d.warn ? 'warn' : '') + `<div class="mbody"><p style="margin:0;color:var(--text-2)">${d.text}</p>${d.extra || ''}</div>
    <footer><div class="spacer"></div><button class="btn" data-act="modal-close">${t('cancel')}</button><button class="btn ${d.danger ? 'danger' : 'primary'}" data-act="modal-ok" data-primary>${d.ok || t('ok')}</button></footer>`,
});
const confirm = (o) => pushModal('confirm', o);

// --- secret prompt (from Go)
// The sudo prompt comes from Go in English ("Mirror on needs sudo. Password for arduino@duck.local:").
const SUDO_WHAT = { 'Mirror on': 'mirror', 'Mirror off': 'mirror', 'Relay on': 'relay', 'Relay off': 'relay', 'Refresh the package lists': 'refresh',
  'Download the updates': 'download', 'Install the updates': 'upgrade', 'Apply system setup': 'system', 'Revert system setup': 'system', 'Rename the board': 'rename',
  'Save Wi-Fi': 'wifi', 'Remove Wi-Fi': 'wifi', 'Mark Wi-Fi': 'wifi', 'Hotspot settings': 'hotspot', 'Away plan': 'away', Hotspot: 'hotspot', 'System fix': 'sysfix' };
function promptText(d) {
  if (d.title === 'SSH login') return [t('passwordTitle'), d.message];
  const m = /^(.*) needs sudo\. Password for (.*):$/.exec(d.message || '');
  if (d.title !== 'Administrator password' || !m) return [d.title || t('passwordTitle'), d.message];
  const what = (SUDO_WHAT[m[1]] ? t('s_' + SUDO_WHAT[m[1]]) : m[1]).replace(/\s*[（(]sudo[)）]$/, '');
  return [t('sudoTitle'), t('sudoMsg', { what, who: m[2] })];
}
modalViews.secret = (d) => ({
  html: header('lock', esc(promptText(d)[0]), esc(promptText(d)[1])) + `<div class="mbody">
    ${field(t('passwordFor'), `<div class="input-group"><input class="input" type="${d.show ? 'text' : 'password'}" data-bind="value" value="${esc(d.value || '')}" autofocus autocomplete="off"><button class="btn" data-act="toggle-show">${t('show')}</button></div>`)}</div>
    <footer><div class="spacer"></div><button class="btn" data-act="prompt-cancel">${t('cancel')}</button><button class="btn primary" data-act="prompt-ok" data-primary>${t('continue')}</button></footer>`,
});
// --- host key prompt (from Go)
modalViews.hostkey = (d) => ({
  html: header('shield', t('trustTitle'), esc(t('trustSub', { host: d.host }))) + `<div class="mbody">
    <div class="fingerprint">${esc(d.keyType)}<br>${esc(d.fingerprint)}</div><p style="color:var(--text-2);margin:8px 0 0">${t('trustText')}</p></div>
    <footer><div class="spacer"></div><button class="btn" data-act="prompt-cancel">${t('cancel')}</button><button class="btn primary" data-act="prompt-ok" data-primary>${t('trust')}</button></footer>`,
});

// --- add / edit board
modalViews.board = (d) => {
  const ssh = d.kind !== 'adb';
  const edit = !!d.oldName;
  const devices = d.devices == null ? `<div class="loading-line">${icon('spinner', 'spin')} ${t('scan')}…</div>`
    : d.devices.length ? `<div class="device-list">${d.devices.map((x) => `<div class="device ${d.serial === x.serial ? 'on' : ''}" data-act="pick-device" data-serial="${esc(x.serial)}">${icon('usb')}<div class="grow"><b>${esc(x.serial)}</b>${x.state !== 'device' ? ` <span class="pill warn">${t('unauthorized')}</span>` : ''}</div>${d.serial === x.serial ? icon('check') : ''}</div>`).join('')}</div>`
    : `<div class="callout warn" style="margin:0 0 12px">${icon('info')}<div class="grow">${d.devicesError ? esc(d.devicesError) : t('noUSB')}</div></div>`;
  let found = '';
  if (ssh && !edit) {
    const list = d.found == null ? `<div class="loading-line">${icon('spinner', 'spin')} ${t('scanning')}</div>`
      : d.found.length ? `<div class="device-list">${d.found.map((b) => {
        const addr = b.host || (b.ips || [])[0] || '';
        const sel = d.host === addr;
        return `<div class="device ${sel ? 'on' : ''}" data-act="pick-found" data-host="${esc(addr)}">${icon('wifi')}<div class="grow"><b>${esc(b.name)}</b> <span class="sub">${esc(addr)}${(b.ips || []).length ? ' · ' + esc(b.ips.join(', ')) : ''}</span><div class="sub">${esc(b.model)}${b.serial ? ' · ' + esc(t('serial', { s: b.serial })) : ''}</div></div>${b.saved ? `<span class="pill">${esc(t('added', { name: b.saved }))}</span>` : sel ? icon('check') : ''}</div>`;
      }).join('')}</div>`
      : `<div class="callout warn" style="margin:0 0 12px">${icon('info')}<div class="grow">${d.foundError ? esc(d.foundError) : t('noneFound')}</div></div>`;
    found = `<div class="field"><label style="display:flex;justify-content:space-between;align-items:center">${t('onNetwork')}<button class="btn sm ghost" data-act="scan-net">${icon('refresh', 'sm')} ${t('scan')}</button></label></div>${list}`;
  }
  const sshFields = `${field(t('host'), input('host', d.host, t('hostPh')))}
    <button class="log-toggle" data-act="board-more">${icon(d.more ? 'chevronDown' : 'chevronRight', 'sm')} ${t('moreOptions')}</button>
    ${d.more ? `<div style="margin-top:10px"><div class="field-row">${field(t('user'), input('user', d.user, 'arduino'))}<div style="width:110px">${field(t('port'), input('port', d.port, '22', 'inputmode="numeric"'))}</div></div>
      ${field(t('keyFile'), `<div class="input-group">${input('keyFile', d.keyFile, '~/.ssh/id_ed25519')}<button class="btn" data-act="pick-key">${t('browse')}</button></div>`, t('keyFilePh'))}</div>` : ''}`;
  const nameNote = edit ? `<div class="callout info" style="margin:0 0 12px">${icon('info')}<div class="grow">${esc(t('nameFromBoard', { name: d.oldName }))}</div><button class="btn sm" data-act="rename-board">${t('renameBoardLink')}</button></div>` : '';
  const progress = d.adding ? `<div class="loading-line" style="margin-top:8px">${icon('spinner', 'spin')} ${esc(t('addingBoard', { host: ssh ? d.host : 'USB' }))}</div>`
    : d.addError ? `<div class="callout warn" style="margin:8px 0 0">${icon('alert')}<div class="grow">${esc(d.addError)}</div></div>` : '';
  return {
    html: header(ssh ? 'wifi' : 'usb', edit ? t('editBoardTitle') : t('addBoardTitle'), edit ? esc(d.oldName) : t('addBoardSub')) + `<div class="mbody">
      ${nameNote}
      <div class="seg"><button class="${ssh ? 'on' : ''}" data-act="board-kind" data-kind="ssh">${icon('wifi')} ${t('viaSSH')}</button><button class="${!ssh ? 'on' : ''}" data-act="board-kind" data-kind="adb">${icon('usb')} ${t('viaUSB')}</button></div>
      ${ssh ? found + sshFields : `<div class="field"><label style="display:flex;justify-content:space-between;align-items:center">${t('usbDevices')}<button class="btn sm ghost" data-act="scan-usb">${icon('refresh', 'sm')} ${t('scan')}</button></label></div>${devices}`}
      ${edit ? '' : check('makeDefault', d.makeDefault, t('makeDefault'))}
      ${edit ? '' : check('uploadKey', d.uploadKey, esc(S.info?.publicKey ? t('uploadKeyAdd', { path: tildeKey(S.info.publicKey.path) }) : t('uploadKeyNew')), t('uploadKeyHelp'))}${progress}</div>
      <footer><div class="spacer"></div><button class="btn" data-act="modal-close">${t('cancel')}</button><button class="btn primary" data-act="save-board" data-primary ${d.adding ? 'disabled' : ''}>${edit ? t('save') : t('addBtn')}</button></footer>`,
  };
};

// --- install
modalViews.install = (d) => {
  const tabs = `<div class="seg"><button class="${d.tab === 'catalog' ? 'on' : ''}" data-act="install-tab" data-tab="catalog">${icon('puzzle')} ${t('tabCatalog')}</button><button class="${d.tab === 'git' ? 'on' : ''}" data-act="install-tab" data-tab="git">${icon('branch')} ${t('tabGit')}</button><button class="${d.tab === 'local' ? 'on' : ''}" data-act="install-tab" data-tab="local">${icon('folder')} ${t('tabLocal')}</button></div>`;
  let src = '';
  if (d.tab === 'catalog') {
    src = `<div class="catalog">${(S.info?.catalog || []).map((p) => `<div class="cat-item ${d.catId === p.id ? 'on' : ''}" data-act="pick-cat" data-cat="${esc(p.id)}">${appIcon(p)}<div class="t"><b>${esc(p.name)}</b><span>${esc(p.description || '')}</span>${hooks(p).length ? `<span class="cat-hooks">${icon('shield', 'sm')} ${esc(t('catHooks', { list: hooks(p).map((h) => h.title).join(' · ') }))}</span>` : ''}</div><span class="pill">${icon('branch', 'sm')} ${esc(repoLabel(p.source.repo).split('/').slice(1).join('/'))}</span></div>`).join('')}</div>`;
  } else if (d.tab === 'git') {
    const r = d.refs;
    const opts = [`<option value="">${esc(t('defaultBranch', { b: r?.defaultBranch || '…' }))}</option>`];
    if (r) {
      const br = r.refs.filter((x) => x.kind === 'branch'), tg = r.refs.filter((x) => x.kind === 'tag');
      if (br.length) opts.push(`<optgroup label="${t('branches')}">${br.map((x) => `<option value="${esc(x.name)}" ${d.ref === x.name ? 'selected' : ''}>${esc(x.name)} · ${short(x.commit)}</option>`).join('')}</optgroup>`);
      if (tg.length) opts.push(`<optgroup label="${t('tags')}">${tg.map((x) => `<option value="${esc(x.name)}" ${d.ref === x.name ? 'selected' : ''}>${esc(x.name)}</option>`).join('')}</optgroup>`);
    }
    src = `${field(t('repo'), `<div class="input-group">${input('repo', d.repo, t('repoPh'), 'autofocus')}<button class="btn" data-act="load-refs">${d.refsLoading ? icon('spinner', 'spin') : icon('refresh')}</button></div>`, d.refsError ? `<span style="color:var(--danger)">${esc(d.refsError)}</span>` : '')}
      <div class="field-row"><div class="field"><label>${t('version')}</label><select class="input" data-bind="ref">${opts.join('')}</select></div></div>
      <button class="log-toggle" data-act="toggle-adv">${icon(d.adv ? 'chevronDown' : 'chevronRight', 'sm')} ${t('advanced')}</button>
      ${d.adv ? `<div style="margin-top:10px">${field(t('subdir'), input('subdir', d.subdir, t('subdirPh')))}</div>` : ''}`;
  } else {
    src = field(t('localPath'), `<div class="input-group">${input('dir', d.dir, t('localPh'), 'autofocus')}<button class="btn" data-act="pick-folder">${t('browse')}</button></div>`);
  }
  let preview = '';
  if (d.inspecting) {
    preview = `<div class="preview"><div class="loading-line">${icon('spinner', 'spin')} ${t('checking')}</div>${d.inspectLog?.length ? `<div class="mini-log">${esc(d.inspectLog.slice(-3).join('\n'))}</div>` : ''}</div>`;
  } else if (d.inspectError) {
    preview = `<div class="callout warn" style="margin:10px 0 0">${icon('alert')}<div class="grow">${esc(d.inspectError)}</div></div>`;
  } else if (d.insp) {
    const b = d.insp.bundle, p = d.insp.profile;
    const existing = S.status[S.cur]?.data?.apps?.find((a) => a.folder === d.folder);
    preview = `<div class="preview">
      <div class="top">${appIcon(p, b.app.icon)}<div style="min-width:0;flex:1"><b style="font-size:14px">${esc(b.app.name)}</b><div style="color:var(--text-2);font-size:12px;margin-top:2px">${esc(b.app.description || '')}</div></div></div>
      <dl class="facts">
        <dt>${t('ref')}</dt><dd><code>${esc(b.ref)}</code> · <code>${esc(short(b.commit))}</code>${b.dirty ? ` <span class="pill warn">${t('dirtyWarn')}</span>` : ''} ${esc(b.subject || '')}</dd>
        <dt>${t('files')}</dt><dd>${b.files} · ${esc(when(b.date))}</dd>
        <dt>${icon('shield', 'sm')}</dt><dd>${p ? `${esc(t('profileFound', { name: p.name }))} · <span style="color:var(--muted)">${p.origin === 'repo' ? t('profileRepo') : t('profileBuiltin')}</span>` : `<span style="color:var(--muted)">${t('noProfile')}</span>`}</dd>
        ${p?.system ? `<dt>${icon('alert', 'sm')}</dt><dd style="white-space:normal;color:var(--warn)">${t('needsSystem')}: ${esc(firstSentence(zhOr(p.system.description, p.system.descriptionZh)))}</dd>` : ''}
      </dl>
      ${p ? `<details class="hooks-box" ${d.hooksOpen !== false ? 'open' : ''}><summary data-act="hooks-toggle">${icon('shield', 'sm')} ${esc(t('hooksSummary', { name: p.name, n: hooks(p).length }))}</summary>${hooksHtml(p)}</details>` : ''}</div>
      ${existing ? `<div class="callout warn" style="margin:10px 0 0">${icon('info')}<div class="grow">${esc(t('existingWarn', { f: d.folder }))}</div></div>` : ''}
      <div style="margin-top:12px">${field(t('installFolder'), input('folder', d.folder, '', 'class="input mono"'), esc(t('folderHelp', { f: d.folder })))}</div>
      ${check('start', d.start, t('startAfter'))}
      ${existing?.data?.length ? check('backup', d.backup, t('backupFirst')) : ''}
      ${check('boot', d.boot, t('bootAfter'))}
      ${p?.system ? check('setup', d.setup, t('setupAfter')) : ''}`;
  }
  const canCheck = d.tab === 'git' ? !!d.repo?.trim() : d.tab === 'local' ? !!d.dir?.trim() : !!d.catId;
  const existing = d.insp && S.status[S.cur]?.data?.apps?.find((a) => a.folder === d.folder);
  return {
    cls: 'wide',
    html: header('download', t('installTitle'), t('installSub')) + `<div class="mbody">${tabs}${src}${preview}</div>
      <footer><div class="spacer"></div><button class="btn" data-act="modal-close">${t('cancel')}</button>
      ${d.insp ? `<button class="btn primary" data-act="do-install" data-primary>${icon('download')} ${existing ? t('reinstall') : t('install')}</button>`
        : `<button class="btn primary" data-act="inspect" data-primary ${canCheck && !d.inspecting ? '' : 'disabled'}>${t('check')}</button>`}</footer>`,
  };
};

// --- update to version
modalViews.update = (d) => {
  const a = d.app, r = d.refs;
  const opts = r ? [`<option value="">${esc(t('defaultBranch', { b: r.defaultBranch }))}</option>`,
    ...r.refs.map((x) => `<option value="${esc(x.name)}" ${d.ref === x.name ? 'selected' : ''}>${x.kind === 'tag' ? '🏷 ' : ''}${esc(x.name)} · ${short(x.commit)}${x.commit === a.deploy.commit ? ' ✓' : ''}</option>`)].join('') : '';
  return {
    html: header('branch', esc(t('updateTitle', { app: a.name })), esc(t('updateSub', { repo: repoLabel(a.deploy.repo) }))) + `<div class="mbody">
      <dl class="facts" style="display:grid;grid-template-columns:auto 1fr;gap:6px 14px;font-size:12px;margin:0 0 14px"><dt style="color:var(--muted)">${t('current')}</dt><dd style="margin:0"><code>${esc(a.deploy.ref)}</code> · <code>${short(a.deploy.commit)}</code> ${esc(a.deploy.subject || '')}</dd></dl>
      ${r ? field(t('version'), `<select class="input" data-bind="ref">${opts}</select>`) : d.error ? `<div class="callout warn">${icon('alert')}<div class="grow">${esc(d.error)}</div></div>` : `<div class="loading-line">${icon('spinner', 'spin')} ${t('checking')}</div>`}
      ${a.data?.length ? `<p style="color:var(--muted);font-size:12px;margin:4px 0 0">${icon('archive', 'sm')} ${t('backupFirst')}</p>` : ''}</div>
      <footer><div class="spacer"></div><button class="btn" data-act="modal-close">${t('cancel')}</button><button class="btn primary" data-act="do-update-ref" data-primary ${r ? '' : 'disabled'}>${t('update')}</button></footer>`,
  };
};

// --- erase
modalViews.erase = (d) => {
  const a = d.app;
  const ok = d.confirm === a.folder;
  const applied = a.profile?.system && a.systemReady;
  return {
    html: header('trash', esc(t('eraseTitle', { app: a.name })), esc(t('eraseSub', { board: S.status[S.cur]?.data?.hostname || S.cur })), 'danger') + `<div class="mbody">
      <div class="danger-list"><div>${icon('stop', 'sm')}<span>${t('eraseL1')}</span></div><div>${icon('trash', 'sm')}<span class="mono">${esc(t('eraseL2', { f: a.folder }))}</span></div>${a.default ? `<div>${icon('boot', 'sm')}<span>${t('eraseL3')}</span></div>` : ''}</div>
      ${a.data?.length ? check('backup', d.backup, esc(t('eraseBackup', { files: a.data.join(', ') }))) : ''}
      ${applied ? check('revert', d.revert, t('eraseRevert'), esc(firstSentence(a.profile.system.description))) : ''}
      <div style="margin-top:10px">${field(esc(t('eraseConfirm', { f: a.folder })), input('confirm', d.confirm, a.folder, 'autofocus class="input mono"'))}</div></div>
      <footer><div class="spacer"></div><button class="btn" data-act="modal-close">${t('cancel')}</button><button class="btn danger" data-act="do-erase" ${ok ? '' : 'disabled'} data-primary>${icon('trash', 'sm')} ${t('eraseBtn')}</button></footer>`,
  };
};

// --- mirror
modalViews.mirror = (d) => {
  const cat = S.info?.mirrors || [], off = S.info?.official;
  const all = [...cat, ...(off ? [off] : [])];
  const order = d.ranked ? d.ranked.map((id) => all.find((m) => m.id === id)).filter(Boolean) : all;
  const top = Math.max(1, ...Object.values(d.results).map((r) => r.debian?.speed || 0));
  const row = (m) => {
    const r = d.results[m.id], ref = m.id === off?.id, sel = !d.custom && d.sel === m.id;
    let deb, lat = '', py = '';
    if (r?.debian) {
      if (!r.debian.ok) deb = `<span class="bad" title="${esc(r.debian.error || '')}">${t('mirrorFailed')}</span>`;
      else {
        deb = `<span class="bar"><i style="width:${Math.max(3, Math.round((100 * r.debian.speed) / top))}%"></i></span><span class="num">${speed(r.debian.speed)}</span>${r.debian.security ? '' : ` <span class="pill warn">${t('mirrorNoSec')}</span>`}`;
        lat = `${r.debian.ms} ms`;
      }
    } else deb = d.testing ? icon('spinner', 'sm spin') : `<span class="muted">${t('mirrorUntested')}</span>`;
    if (r?.pypi) py = r.pypi.ok ? `${r.pypi.ms} ms` : `<span class="bad" title="${esc(r.pypi.error || '')}">${t('mirrorFailed')}</span>`;
    return `<tr class="${sel ? 'on' : ''} ${ref ? 'ref' : ''} ${r && !works(r) ? 'broken' : ''}" ${ref ? `title="${esc(t('mirrorOfficialHelp'))}"` : `data-act="pick-mirror" data-id="${esc(m.id)}"`}>
      <td><span class="radio ${sel ? 'on' : ''} ${ref ? 'none' : ''}"></span>${esc(ref ? t('mirrorOfficial') : mirrorName(m))}</td><td class="sp">${deb}</td><td class="n">${lat}</td><td class="n">${py}</td></tr>`;
  };
  const chosen = d.custom ? null : all.find((m) => m.id === d.sel);
  const ok = (d.usePyPI || d.useDebian) && (d.custom ? (!d.usePyPI || d.index.trim()) && (!d.useDebian || d.debian.trim()) : !!chosen);
  return {
    cls: 'wide',
    html: header('download', t('mirrorTitle'), t('mirrorSub')) + `<div class="mbody">
      <div class="mirror-what">${check('usePyPI', d.usePyPI, t('mirrorPyPIShort'))}${check('useDebian', d.useDebian, t('mirrorDebianShort'))}</div>
      <table class="mirror-table"><thead><tr><th>${t('mirrorName')}</th><th>${t('mirrorDebianSpeed')}</th><th class="n">${t('mirrorLatency')}</th><th class="n">PyPI</th></tr></thead>
        <tbody>${order.map(row).join('')}</tbody></table>
      <div class="mirror-custom ${d.custom ? 'on' : ''}" data-act="mirror-custom"><span class="radio ${d.custom ? 'on' : ''}"></span>${t('mirrorCustom')}</div>
      <div class="indent custom-fields ${d.custom ? '' : 'off'}">${field('PyPI', input('index', d.index, 'https://…/simple', `class="input mono" ${d.custom ? '' : 'disabled'}`))}${field('Debian', input('debian', d.debian, 'https://…/debian', `class="input mono" ${d.custom ? '' : 'disabled'}`))}</div>
      ${d.testError ? `<div class="callout warn" style="margin:8px 0 0">${icon('alert')}<div class="grow">${esc(d.testError)}</div></div>` : ''}
      <p class="sys-note">${t('mirrorFallbackNote')}</p></div>
      <footer>${d.on ? `<button class="btn ghost danger-text" data-act="mirror-off">${t('mirrorTurnOff')}</button>` : ''}<button class="btn ghost" data-act="mirror-retest" ${d.testing ? 'disabled' : ''}>${icon(d.testing ? 'spinner' : 'refresh', d.testing ? 'sm spin' : 'sm')} ${d.testing ? t('mirrorTesting') : t('mirrorRetest')}</button>
        <div class="spacer"></div><button class="btn" data-act="modal-close">${t('cancel')}</button>
        <button class="btn primary" data-act="modal-ok" data-primary ${ok ? '' : 'disabled'}>${esc(chosen ? t('mirrorUse', { name: mirrorName(chosen) }) : t('mirrorUseCustom'))}</button></footer>`,
  };
};

// --- relay
modalViews.relay = (d) => ({
  html: header('link', d.edit ? t('relayUpTitle') : t('relayTitle'), d.edit ? t('relayUpSub') : t('relaySub')) + `<div class="mbody">
    ${d.edit ? '' : `<p style="margin:0 0 12px;color:var(--text-2)">${t('relayHow')}</p>`}
    ${field(t('relayUpstream'), input('upstream', d.upstream, d.effective || t('relayDirect'), 'class="input mono"'), esc(t('relayUpstreamHelp', { p: d.effective || t('relayDirect') })))}</div>
    <footer><div class="spacer"></div><button class="btn" data-act="modal-close">${t('cancel')}</button><button class="btn primary" data-act="modal-ok" data-primary>${d.edit ? t('save') : t('turnOn')}</button></footer>`,
});
async function editRelayUpstream() {
  closeMenu();
  const info = await call('RelaySettings').catch(() => ({}));
  const m = { upstream: info.upstream || '', effective: info.effective || '', edit: true };
  if (!(await pushModal('relay', m))) return;
  try { S.relayInfo = await call('SetRelayUpstream', m.upstream.trim()); toast('ok', t('relaySaved')); invalidate('main'); } catch (e) { toast('err', e.message || String(e)); }
}

// --- network: saved Wi-Fi and the automatic hotspot
const HOTSPOT_IP = '10.42.0.1';
const utf8Len = (s) => new TextEncoder().encode(s || '').length;
const ssidError = (s) => (!s || utf8Len(s) > 32 ? t('ssidLen') : /[;\\\x00-\x1f\x7f]/.test(s) ? t('ssidChars') : s.trim() !== s ? t('ssidSpace') : '');
const pskError = (p) => (/^[0-9a-fA-F]{64}$/.test(p) ? '' : p.length < 8 || p.length > 63 ? t('pskLen') : /[^\x20-\x7e]/.test(p) ? t('pskChars') : '');
// netLine sums up the board's Wi-Fi from its status, for the overview card.
function netLine(n) {
  if (!n) return '';
  if (n.hotspot) return t(n.ours ? 'netOnHotspot' : 'netOnOtherHotspot', { ssid: n.ssid }) + (n.ours && n.stations != null ? ' · ' + t('netDevices', { n: n.stations }) : '');
  const away = n.away && n.away !== 'off' ? t('awayShort', { plan: t('away_' + n.away) }) : '';
  if (n.state === 100 && n.ssid) return [t('netOnWifi', { ssid: n.ssid }), away].filter(Boolean).join(' · ');
  return away;
}
// The web consoles of the apps installed on the board (from their app.yaml ports or profile), as a
// phone on the board's hotspot reaches them.
function hotspotLinks(st) {
  return (st?.apps || []).flatMap((a) => {
    try { const u = new URL(a.webUrl); u.hostname = HOTSPOT_IP; return [{ name: a.name, url: u.toString() }]; } catch { return []; }
  });
}
function hotspotState(h) {
  if (!h.running) return `<span class="danger-text">${t('hsNotRunning')}</span>`;
  const s = h.state?.state;
  if (s === 'hotspot') return esc(t('hsOpen')) + (h.state.stations != null ? ' · ' + esc(t('netDevices', { n: h.state.stations })) : '');
  return esc(t({ wifi: 'hsWifi', 'other-hotspot': 'hsOther', off: 'hsRadioOff' }[s] || 'hsWaiting'));
}
const AWAY_MODES = ['off', 'board', 'phone', 'both'];
const wantsBoard = (m) => m === 'board' || m === 'both';
const wantsPhone = (m) => m === 'phone' || m === 'both';
modalViews.network = (d) => {
  const n = d.net, apps = hotspotLinks(S.status[d.board]?.data);
  let body;
  if (d.error) body = `<div class="callout warn" style="margin:0">${icon('alert')}<div class="grow">${esc(d.error)}</div></div>`;
  else if (!n) body = `<div class="loading-line">${icon('spinner', 'spin')} ${t('netReading')}</div>`;
  else if (!n.nmcli) body = `<div class="callout warn" style="margin:0">${icon('alert')}<div class="grow">${t('netNoNM')}</div></div>`;
  else {
    const h = n.hotspot, act = n.profiles.find((p) => p.active), off = d.busy ? 'disabled' : '', pend = d.pending || {};
    const now = h.active ? `<b>${esc(t('netOnHotspot', { ssid: h.ssid }))}</b> · <span class="mono">${HOTSPOT_IP}</span>`
      : act ? `<b>${esc(t(act.phone ? 'netOnPhone' : 'netOnWifi', { ssid: act.ssid || act.id }))}</b>${n.ip ? ` · <span class="mono selectable">${esc(n.ip)}</span>` : ''}`
      : esc(t('netNoWifi'));
    const row = (p) => {
      const notes = [p.mode === 'ap' ? t('netOwnHotspot') : '', p.hidden ? t('netHidden') : '', p.secure ? '' : t('netOpen'), p.autoconnect || p.phone ? '' : t('netManual')].filter(Boolean);
      return `<div class="net-row">${icon('wifi')}<div class="grow"><b>${esc(p.ssid || p.id)}</b>${p.active ? ` <span class="pill ok">${t('netInUse')}</span>` : ''}${notes.length ? `<div class="sub">${esc(notes.join(' · '))}</div>` : ''}</div>
        <button class="btn sm ghost icon" data-act="wifi-menu" data-uuid="${esc(p.uuid)}" ${off} title="${esc(t('netMore'))}">${icon('more', 'sm')}</button></div>`;
    };
    // A card's header: title, a spinner with what is being done there, and its button.
    const head = (where, title, extra) => `<div class="net-head"><b>${title}</b>${pend.where === where ? `<span class="net-busy">${icon('spinner', 'sm spin')} ${esc(pend.text)}</span>` : ''}<div class="spacer"></div>${extra}</div>`;
    const ordinary = n.profiles.filter((p) => !p.phone), phones = n.profiles.filter((p) => p.phone);
    const wifiCard = `<section class="net-card wifi">${head('wifi', t('netOrdinary'), `<button class="btn sm ghost" data-act="wifi-add" ${off}>${icon('plus', 'sm')} ${t('netAdd')}</button>`)}
      ${ordinary.length ? ordinary.map(row).join('') : `<p class="net-empty">${t('netNoneSaved')}</p>`}
      <p class="net-note">${t('netOrdinaryNote')}</p></section>`;
    const phoneCard = `<section class="net-card phone">${head('phone', t('netPhones'), `<button class="btn sm ghost" data-act="wifi-add" data-phone="1" ${off}>${icon('plus', 'sm')} ${t('netAdd')}</button>`)}
      ${phones.length ? phones.map(row).join('') : `<p class="net-empty">${t('netNoPhones')}</p>`}
      <p class="net-note ${phones.length && !wantsPhone(n.away) ? 'warn' : ''}">${t(wantsPhone(n.away) ? 'netPhonesUsed' : 'netPhonesIdle')}</p></section>`;
    const boardBox = !wantsBoard(n.away) ? '' : !n.ap ? `<div class="callout warn" style="margin:12px 0 0">${icon('info')}<div class="grow">${t('netNoAP')}</div></div>`
      : `<div class="hs-box"><div class="hs-title">${t('netOwnTitle')}</div><div class="hs-grid">
          <span class="lbl">${t('hotspotName')}</span><span><b>${esc(h.ssid)}</b> <a class="link" data-act="hotspot-edit">${t('change')}</a></span>
          <span class="lbl">${t('hotspotNow')}</span><span>${hotspotState(h)}</span>
          <span class="lbl">${t('hotspotAddr')}</span><span class="mono">${HOTSPOT_IP}</span></div>
          ${apps.length ? `<div class="hs-sub">${t('hotspotApps')}</div><div class="hs-grid">${apps.map((l) => `<span class="lbl">${esc(l.name)}</span><a class="link mono" data-act="open-web" data-url="${esc(l.url)}">${esc(l.url)}</a>`).join('')}</div>` : ''}
          <div class="hs-actions">${h.active ? `<button class="btn sm" data-act="hotspot-close" ${off}>${icon('wifi', 'sm')} ${t('hotspotCloseNow')}</button>`
            : `<button class="btn sm" data-act="hotspot-open" ${off}>${icon('wifi', 'sm')} ${t('hotspotOpenNow')}</button>`}</div></div>`;
    const seg = AWAY_MODES.map((m) => {
      const going = pend.where === 'away' && pend.mode === m;
      return `<button class="${(pend.where === 'away' ? going : n.away === m) ? 'on' : ''}" data-act="away-pick" data-mode="${m}" ${off} ${wantsBoard(m) && !n.ap ? 'disabled' : ''}>${going ? icon('spinner', 'sm spin') + ' ' : ''}${t('away_' + m)}</button>`;
    }).join('');
    const awayCard = `<section class="net-card away"><div class="net-head"><b>${t('awayTitle')}</b>${helpQ('away')}<div class="spacer"></div></div>
      <div class="seg away-seg">${seg}</div>
      <p class="net-how">${pend.where === 'away' ? `${icon('spinner', 'sm spin')} ${esc(pend.text)}` : t('awayHow_' + n.away)}</p>${boardBox}</section>`;
    const found = (n.findings || []).map((f) => findingHtml(f, d.busy)).join('');
    body = `<div class="net-now">${icon('wifi')}<span>${now}</span></div>${found}<div class="net-grid">${wifiCard}${phoneCard}</div>${awayCard}`;
  }
  return {
    cls: 'xwide',
    html: header('wifi', t('netTitle'), esc(t('netSub', { name: d.board }))) + `<div class="mbody">${body}</div>
      <footer><button class="btn ghost" data-act="net-refresh" ${(n || d.error) && !d.busy ? '' : 'disabled'}>${icon(d.busy ? 'spinner' : 'refresh', d.busy ? 'sm spin' : 'sm')} ${t('refresh')}</button><div class="spacer"></div><button class="btn primary" data-act="modal-close" data-primary>${t('close')}</button></footer>`,
  };
};
const pskField = (d, help) => field(t('wifiPassword'), `<div class="input-group"><input class="input" type="${d.show ? 'text' : 'password'}" data-bind="psk" value="${esc(d.psk || '')}" autocomplete="new-password" placeholder="${esc(d.pskPh || '')}"><button class="btn" data-act="toggle-show">${t('show')}</button></div>`, `<span class="v-psk">${esc(help)}</span>`);
// netForm checks the Wi-Fi / hotspot dialogs: [ok, name message, password message].
function netForm(m) {
  const d = m.data, sErr = d.ssid ? ssidError(d.ssid) : '';
  const pErr = d.psk ? pskError(d.psk) : m.type === 'hotspot' && !d.configured ? t('pskNeeded') : '';
  const help = m.type === 'hotspot' ? (d.configured ? t('pskKeepHelp') : t('pskHelp')) : t('wifiPskHelp');
  return [!!d.ssid && !sErr && !pErr, sErr || (m.type === 'hotspot' ? t('hotspotNameHelp') : ''), (d.psk && pErr) || help];
}
modalViews.hotspot = (d, m) => {
  const [ok, sMsg, pMsg] = netForm(m);
  return {
    html: header('wifi', t('hotspotEditTitle'), t('hotspotOnSub')) + `<div class="mbody">
      ${field(t('hotspotName'), input('ssid', d.ssid, 'duck', 'autofocus'), `<span class="v-ssid">${esc(sMsg)}</span>`)}
      ${pskField(d, pMsg)}
      ${d.configured ? '' : `<div class="callout info" style="margin:0">${icon('info')}<div class="grow">${t('hotspotOnNote')}</div></div>`}</div>
      <footer><div class="spacer"></div><button class="btn" data-act="modal-close">${t('cancel')}</button><button class="btn primary" data-act="modal-ok" data-primary ${ok ? '' : 'disabled'}>${t('save')}</button></footer>`,
  };
};
modalViews.wifiadd = (d, m) => {
  const [ok, sMsg, pMsg] = netForm(m);
  const list = d.seen == null ? `<div class="loading-line">${icon('spinner', 'spin')} ${t('wifiScanning')}</div>`
    : d.seen.length ? `<div class="device-list scroll">${d.seen.map((x) => {
      const saved = d.saved.includes(x.ssid);
      return `<div class="device ${saved ? 'static' : ''} ${d.ssid === x.ssid ? 'on' : ''}" ${saved ? '' : `data-act="wifi-pick" data-ssid="${esc(x.ssid)}"`}>${icon('wifi')}<div class="grow"><b>${esc(x.ssid)}</b> <span class="sub">${x.signal}%${x.security ? ' · ' + esc(x.security) : ' · ' + t('netOpen')}</span></div>${saved ? `<span class="pill">${t('wifiSavedPill')}</span>` : d.ssid === x.ssid ? icon('check') : ''}</div>`;
    }).join('')}</div>`
    : `<p class="sys-note" style="margin:0 0 12px">${esc(d.seenError || t('wifiNoneInRange'))}</p>`;
  return {
    html: header('wifi', d.phone ? t('wifiAddPhoneTitle') : t('wifiAddTitle'), d.phone ? t('wifiAddPhoneSub') : t('wifiAddSub')) + `<div class="mbody">
      <div class="field"><label class="row-label">${t('wifiInRange')}<button class="btn sm ghost" data-act="wifi-rescan">${icon('refresh', 'sm')} ${t('scan')}</button></label></div>${list}
      ${field(t('wifiName'), input('ssid', d.ssid, d.phone ? t('wifiPhonePh') : t('wifiNamePh')), `<span class="v-ssid">${esc(sMsg)}</span>`)}
      ${pskField(d, pMsg)}
      ${check('phone', d.phone, t('wifiIsPhone'), t('wifiIsPhoneHelp'))}
      ${check('hidden', d.hidden, t('wifiHidden'))}</div>
      <footer><div class="spacer"></div><button class="btn" data-act="modal-close">${t('cancel')}</button><button class="btn primary" data-act="modal-ok" data-primary ${ok ? '' : 'disabled'}>${t('save')}</button></footer>`,
  };
};
async function openNetwork() {
  const d = { board: S.cur, net: null, error: '' };
  const p = pushModal('network', d);
  loadNetwork(d);
  await p;
}
async function loadNetwork(d) {
  d.busy = true; invalidate('modal');
  try { d.net = await runOp(call('Network', d.board)); d.error = ''; } catch (e) { d.error = e.message || String(e); }
  d.busy = false; invalidate('modal');
}
// netOp runs a Wi-Fi change from the network dialog, then reads the board's network again.
async function netOp(d, kind, start, okMsg, pending) {
  d.busy = true; d.pending = pending; invalidate('modal');
  try { await boardOp(kind, start, undefined, okMsg); } catch { /* shown in the activity */ }
  await loadNetwork(d);
  d.pending = null; invalidate('modal');
}
async function openWifiAdd(nd, phone) {
  const d = { ssid: '', psk: '', hidden: false, phone: !!phone, seen: null, saved: (nd.net?.profiles || []).map((x) => x.ssid) };
  const p = pushModal('wifiadd', d);
  scanWifi(d, nd.board);
  if (!(await p)) return;
  netOp(nd, 'wifi', call('WifiAdd', nd.board, d.ssid, d.psk, !!d.hidden, !!d.phone), () => t(d.phone ? 't_phoneSaved' : 't_wifiSaved', { ssid: d.ssid }),
    { where: d.phone ? 'phone' : 'wifi', text: t('netSaving', { ssid: d.ssid }) });
}
async function scanWifi(d, name) {
  d.seen = null; d.seenError = ''; invalidate('modal');
  try { d.seen = await runOp(call('WifiScan', name)); } catch (e) { d.seen = []; d.seenError = e.message || String(e); }
  if (!d.seen.length && S.status[name]?.data?.net?.hotspot) d.seenError = t('wifiNoScanOnHotspot');
  invalidate('modal');
}
// hotspotSettings asks for the board's hotspot name and password; null when cancelled.
async function hotspotSettings(nd) {
  const h = nd.net.hotspot;
  const d = { ssid: h.ssid || S.status[nd.board]?.data?.hostname || nd.board, psk: '', configured: h.configured, pskPh: h.configured ? t('pskKeep') : '' };
  return (await pushModal('hotspot', d)) ? d : null;
}
async function pickAway(mode) {
  const nd = topModal().data, n = nd.net;
  if (!n || n.away === mode) return;
  let ssid = '', psk = '';
  if (wantsBoard(mode) && !n.hotspot.configured) {
    const s = await hotspotSettings(nd);
    if (!s) return;
    ssid = s.ssid; psk = s.psk;
  }
  if (n.hotspot.active && !wantsBoard(mode) && !(await confirm({ icon: 'wifi', title: t('awayLeaveTitle'), text: t('awayLeaveText'), ok: t('away_' + mode) }))) return;
  netOp(nd, 'away', call('Away', nd.board, mode, ssid, psk), () => t('t_away', { plan: t('away_' + mode) }),
    { where: 'away', mode, text: t(wantsBoard(mode) && !wantsBoard(n.away) ? 'awaySwitchingInstall' : 'awaySwitching', { plan: t('away_' + mode) }) });
}
async function editHotspot() {
  const nd = topModal().data, s = await hotspotSettings(nd);
  if (s) netOp(nd, 'away', call('Away', nd.board, nd.net.away, s.ssid, s.psk), () => t('t_hotspotSaved', { ssid: s.ssid }), { where: 'away', mode: nd.net.away, text: t('hotspotSaving') });
}
function wifiMenu(uuid, el) {
  const nd = topModal().data, p = nd.net?.profiles.find((x) => x.uuid === uuid);
  if (!p) return;
  const name = p.ssid || p.id;
  openMenu(el, [
    { icon: 'wifi', label: p.phone ? t('netMarkOrdinary') : t('netMarkPhone'), fn: () => netOp(nd, 'wifi', call('WifiPhone', nd.board, uuid, !p.phone), () => t(p.phone ? 't_markedOrdinary' : 't_markedPhone', { ssid: name }),
      { where: p.phone ? 'wifi' : 'phone', text: t('netMoving', { ssid: name }) }) },
    '-',
    { icon: 'trash', label: t('netRemove'), danger: true, disabled: p.active, fn: async () => {
      if (!(await confirm({ icon: 'trash', danger: true, title: esc(t('wifiRmTitle', { ssid: name })), text: t('wifiRmText'), ok: t('netRemove') }))) return;
      netOp(nd, 'wifi', call('WifiRemove', nd.board, uuid), () => t('t_wifiRemoved', { ssid: name }), { where: p.phone ? 'phone' : 'wifi', text: t('netRemoving', { ssid: name }) });
    } },
  ]);
}
async function systemFix(id) {
  const m = topModal(), nd = m?.type === 'network' ? m.data : null;
  const f = [...(nd?.net?.findings || []), ...(S.status[S.cur]?.data?.findings || [])].find((x) => x.id === id);
  if (!f?.action) return;
  const label = zhOr(f.action.label, f.action.labelZh);
  if (!(await confirm({ icon: 'info', title: esc(label), text: esc(zhOr(f.action.confirm, f.action.confirmZh) || zhOr(f.text, f.textZh)), ok: esc(label) }))) return;
  if (nd) return netOp(nd, 'sysfix', call('SystemFix', nd.board, id), () => t('t_sysfix', { what: label }), { where: 'away', mode: nd.net.away, text: label + '…' });
  boardOp('sysfix', call('SystemFix', S.cur, id), undefined, () => t('t_sysfix', { what: label })).catch(() => {});
}
async function hotspotNow(open) {
  const nd = topModal().data, ssid = nd.net.hotspot.ssid;
  const ok = await confirm({ icon: 'wifi', title: open ? t('hotspotOpenTitle') : t('hotspotCloseTitle'),
    text: esc(open ? t('hotspotOpenText', { ssid, ip: HOTSPOT_IP }) : t('hotspotCloseText')), ok: open ? t('hotspotOpenNow') : t('hotspotCloseNow') });
  if (!ok) return;
  nd.busy = true; invalidate('modal');
  try { await boardOp('hotspot', call('HotspotNow', nd.board, open), undefined, () => (open ? t('t_hotspotOpening', { ssid }) : t('t_hotspotClosing'))); } catch { /* shown */ }
  nd.busy = false;
  closeModal(false);
}

// --- rename
modalViews.rename = (d) => {
  const v = (d.name || '').trim().toLowerCase();
  const ok = HOSTNAME_RE.test(v);
  return {
    html: header('edit', t('renameTitle'), esc(t('renameSub', { name: d.old }))) + `<div class="mbody">
      ${field(t('renameLabel'), input('name', d.name, 'duck', 'autofocus class="input mono"'), `<span class="rename-help">${esc(ok ? t('renameHelp', { name: v }) : t('renameBad'))}</span>`)}</div>
      <footer><div class="spacer"></div><button class="btn" data-act="modal-close">${t('cancel')}</button><button class="btn primary" data-act="modal-ok" data-primary ${ok ? '' : 'disabled'}>${t('renameBtn')}</button></footer>`,
  };
};

// --- system update
modalViews.system = (d) => {
  const s = S.sys[d.board] || {}, u = s.data, st = S.status[d.board]?.data;
  const cli = (u?.appcliVersion || st?.appcliVersion || '').replace(/^arduino[ -]app[ -]cli\s*/i, '').replace(/^version\s*/i, '');
  let body = '', count = 0, size = 0;
  if (s.loading) {
    body = `<div class="loading-line">${icon('spinner', 'spin')} ${t('sysChecking')}</div>${d.log.length ? `<div class="mini-log">${esc(d.log.slice(-4).join('\n'))}</div>` : ''}`;
  } else if (s.error) {
    body = `<div class="callout warn" style="margin:0">${icon('alert')}<div class="grow">${esc(s.error)}</div></div>`;
  } else if (u && !u.total.count) {
    body = `<div class="ok-line">${icon('check')} ${t('sysUpToDate')}${cli ? ` · App CLI ${esc(cli)}` : ''}</div>`;
  } else if (u) {
    const pick = u.packages.filter((p) => !(d.mode === 'arduino' && !p.arduino) && !(d.skipDesktop && p.desktop));
    count = pick.length; size = pick.reduce((a, p) => a + p.size, 0);
    const arduino = u.packages.filter((p) => p.arduino && p.from).map((p) => `<span class="pkg"><code>${esc(p.name)}</code> ${esc(p.from)} → <b>${esc(p.to)}</b></span>`);
    const desk = u.packages.filter((p) => p.desktop && !(d.mode === 'arduino' && !p.arduino)).reduce((a, p) => a + p.size, 0);
    const tips = [];
    if (!(st?.mirror?.debian && !/deb\.debian\.org/.test(st.mirror.debian))) tips.push(`<div class="tip">${icon('download', 'sm')}<span class="grow">${t('sysTipMirror')}</span><button class="btn sm" data-act="mirror">${t('turnOn')}</button></div>`);
    if (!st?.relay?.installed) tips.push(`<div class="tip">${icon('link', 'sm')}<span class="grow">${t('sysTipRelay')}</span><button class="btn sm" data-act="relay">${t('turnOn')}</button></div>`);
    body = `<div class="sys-sum"><b>${esc(t('sysUpdates', { n: u.total.count }))}</b><span>${esc(t('sysDownload', { size: mb(u.total.size) }))}</span></div>
      <table class="sys-src">${u.sources.map((g) => `<tr><td>${esc(g.host === '(cached)' ? t('sysCached') : g.host)}</td><td class="n">${esc(t('sysPackages', { n: g.count }))}</td><td class="n">${g.host === '(cached)' ? '' : mb(g.size)}</td></tr>`).join('')}</table>
      ${arduino.length ? `<div class="sys-pkgs"><span class="lbl">${t('sysArduino')}</span> ${arduino.join(' ')}</div>` : ''}
      <div class="seg"><button class="${d.mode !== 'arduino' ? 'on' : ''}" data-act="sys-mode" data-mode="all">${t('sysModeAll')}</button><button class="${d.mode === 'arduino' ? 'on' : ''}" data-act="sys-mode" data-mode="arduino">${t('sysModeArduino')}</button></div>
      ${u.desktop.count ? check('skipDesktop', d.skipDesktop, t('sysSkipDesktop'), esc(t('sysSkipDesktopHelp', { size: mb(desk) }))) : ''}
      <div class="sys-will">${esc(t('sysWillInstall', { n: count, size: mb(size) }))}</div>
      ${u.running?.length ? `<div class="callout warn" style="margin:10px 0 0">${icon('alert')}<div class="grow">${esc(t('sysRunning', { apps: u.running.join(', ') }))}</div></div>` : ''}
      ${u.rebootRequired ? `<div class="callout warn" style="margin:10px 0 0">${icon('info')}<div class="grow">${t('sysReboot')}</div></div>` : ''}
      ${tips.length ? `<div class="tips"><div class="lbl">${t('sysFaster')}</div>${tips.join('')}</div>` : ''}
      <p class="sys-note">${t('sysNote')}</p>`;
  }
  return {
    cls: 'wide',
    html: header('package', t('sysModalTitle'), esc(t('sysModalSub', { name: d.board }))) + `<div class="mbody">${body}</div>
      <footer><button class="btn ghost" data-act="sys-recheck" ${s.loading ? 'disabled' : ''}>${icon('refresh', 'sm')} ${t('sysRecheck')}</button><div class="spacer"></div><button class="btn" data-act="modal-close">${t('close')}</button>
      ${count ? `<button class="btn" data-act="sys-download" ${size && !s.loading ? '' : 'disabled'} title="${esc(t('sysDownloadHelp'))}">${icon('download', 'sm')} ${esc(t('sysDownloadOnly', { size: mb(size) }))}</button>` : ''}
      <button class="btn primary" data-act="modal-ok" data-primary ${count && !s.loading ? '' : 'disabled'}>${esc(!count ? t('sysInstallNone') : size ? t('sysInstall', { n: count, size: mb(size) }) : t('sysInstallCached', { n: count }))}</button></footer>`,
  };
};

// --- about
modalViews.about = (d) => ({
  cls: 'wide',
  html: `<header><div class="mi brand-mi">${icon('anchor')}</div><div><h3>Quay ${esc(S.info?.version || '')}</h3><p>${t('aboutSub')}</p></div><button class="btn sm ghost icon x" data-act="modal-close">${icon('x', 'sm')}</button></header>
    <div class="mbody about">
      <p>Copyright 2026 Xiaogang Cao · <a class="link" data-act="open-web" data-url="https://www.apache.org/licenses/LICENSE-2.0">Apache License 2.0</a> · <a class="link" data-act="open-web" data-url="https://github.com/caoxg/quay">github.com/caoxg/quay</a></p>
      <p class="muted">${t('trademark')}</p>
      <details ${d.open ? 'open' : ''}><summary data-act="about-toggle">${t('thirdParty')}</summary>
        ${d.text == null ? `<div class="loading-line">${icon('spinner', 'spin')}</div>` : `<pre class="log selectable">${esc(d.text)}</pre>`}</details>
    </div>
    <footer><div class="spacer"></div><button class="btn primary" data-act="modal-ok" data-primary>${t('close')}</button></footer>`,
});
async function openAbout() {
  const d = { text: null, open: false };
  const p = pushModal('about', d);
  call('Licenses').then((l) => { d.text = l.thirdParty; invalidate('modal'); }).catch((e) => { d.text = String(e.message || e); invalidate('modal'); });
  await p;
}

// --- restore
modalViews.restore = (d) => ({
  html: header('restore', esc(t('restoreTitle', { app: d.app.name })), `<span class="mono">${esc(d.file.split('/').pop())}</span>`, 'warn') + `<div class="mbody"><p style="margin:0 0 10px;color:var(--text-2)">${t('restoreText')}</p>${check('force', d.force, t('restoreForce'))}</div>
    <footer><div class="spacer"></div><button class="btn" data-act="modal-close">${t('cancel')}</button><button class="btn primary" data-act="modal-ok" data-primary>${t('restoreBtn')}</button></footer>`,
});

// --- logs
modalViews.logs = (d) => ({
  cls: 'xwide',
  html: header('terminal', esc(t('logsTitle', { app: d.app.name })), `<span class="mono">~/ArduinoApps/${esc(d.app.folder)}</span>`) + `<div class="mbody">
    <div class="term">${d.lines.length ? d.lines.map(logLine).join('\n') : `<span class="muted">…</span>`}</div></div>
    <footer>${check('follow', d.follow, t('follow'))}<div class="spacer"></div><button class="btn" data-act="logs-clear">${t('clear')}</button><button class="btn primary" data-act="modal-close" data-primary>${t('close')}</button></footer>`,
});

// ---------------------------------------------------------------- flows
function appByFolder(folder) { return S.status[S.cur]?.data?.apps?.find((a) => a.folder === folder); }
function openActivity() { if (!S.activity) { S.activity = true; invalidate('activity', 'main'); } }

// boardOp runs an op on the current board. start is the started op (a promise of its id), or a
// function (force) => started for ops the app's safety probe can refuse.
async function boardOp(kind, start, folder, okMsg) {
  const name = S.cur;
  const what = opTitle(kind, name, folder);
  try {
    const res = await (typeof start === 'function' ? runForceable(start) : runOp(start));
    if (okMsg) toast('ok', okMsg(res));
    return res;
  } catch (e) {
    fail(what, e);
    throw e;
  } finally {
    refresh(name);
  }
}

async function doAutostart(folder) {
  await boardOp('autostart', call('Autostart', S.cur, folder), folder, () => t('t_autostart')).catch(() => {});
}
async function doBackup(a) {
  openActivity();
  const p = await boardOp('backup', call('Backup', S.cur, a.folder), a.folder).catch(() => null);
  if (p) toast('ok', t('t_backup'), { label: t('reveal'), fn: () => call('Reveal', p) });
  else if (p === '') toast('info', t('t_noBackup'));
}
async function doRestore(a) {
  const file = await call('PickBackup').catch(() => '');
  if (!file) return;
  const d = { app: a, file, force: false };
  if (!(await pushModal('restore', d))) return;
  openActivity();
  await boardOp('restore', (f) => call('Restore', S.cur, a.folder, file, f || !!d.force), a.folder, () => t('t_restore')).catch(() => {});
}
async function doExport(a) {
  const path = await call('PickExport', `${a.folder}.zip`).catch(() => '');
  if (!path) return;
  openActivity();
  const p = await boardOp('export', call('Export', S.cur, a.folder, path), a.folder).catch(() => null);
  if (p) toast('ok', t('t_export'), { label: t('reveal'), fn: () => call('Reveal', p) });
}
async function doPrepare(a, revert) {
  const sys = a.profile?.system;
  const ok = await confirm({ icon: 'shield', warn: true, title: revert ? t('revertSetup') : t('systemSetup'), text: esc(sys?.description || ''), ok: revert ? t('revertSetup') : t('applySetup') });
  if (!ok) return;
  openActivity();
  await boardOp(revert ? 'revert' : 'prepare', (f) => call('Prepare', S.cur, a.folder, revert, f), a.folder, () => (revert ? t('t_revert') : t('t_prepare'))).catch(() => {});
}
async function doUpdate(a, ref = '') {
  openActivity();
  try {
    const res = await boardOp('update', (f) => call('Update', S.cur, a.folder, ref, f), a.folder);
    if (res && res.previous === res.commit) toast('info', t('t_upToDate', { app: a.name }));
    else toast('ok', t('t_update', { app: a.name }));
    if (res?.needsSetup) setupToast(a.folder);
  } catch { /* toast shown */ }
}
function setupToast(folder) {
  const a = appByFolder(folder);
  toast('warn', t('needsSetupToast', { app: a?.profile?.name || folder }), { label: t('applySetup'), fn: () => { const x = appByFolder(folder); if (x) doPrepare(x, false); } }, 12000);
}
async function openUpdate(a) {
  const d = { app: a, refs: null, ref: a.deploy.ref };
  const p = pushModal('update', d);
  runOp(call('Refs', a.deploy.repo)).then((r) => { d.refs = r; if (!r.refs.some((x) => x.name === d.ref)) d.ref = ''; invalidate('modal'); }).catch((e) => { d.error = e.message; invalidate('modal'); });
  if (await p) doUpdate(a, d.ref);
}
async function openErase(a) {
  const d = { app: a, backup: true, revert: false, confirm: '' };
  if (!(await pushModal('erase', d))) return;
  openActivity();
  try {
    const backup = await boardOp('erase', (f) => call('Erase', { board: S.cur, folder: a.folder, backup: !!d.backup, force: f, revertSystem: !!d.revert }), a.folder);
    toast('ok', t('t_erase', { app: a.name }), backup ? { label: t('reveal'), fn: () => call('Reveal', backup) } : null);
  } catch { /* toast shown */ }
}
async function openLogs(a) {
  const d = { app: a, lines: [], follow: true, opId: null };
  const p = pushModal('logs', d);
  const m = topModal();
  m.onClose = () => d.opId && call('Cancel', d.opId);
  runOp(call('Logs', S.cur, a.folder, true, 300), (e) => {
    if (e.type === 'log') {
      d.lines.push(e.text);
      if (d.lines.length > 5000) d.lines.splice(0, 1000);
      const term = $('#modal-root .term');
      if (term && topModal() === m) {
        const atBottom = term.scrollHeight - term.scrollTop - term.clientHeight < 40;
        if (term.querySelector('.muted')) term.innerHTML = '';
        term.insertAdjacentHTML('beforeend', (term.childNodes.length ? '\n' : '') + logLine(e.text));
        if (d.follow && atBottom) term.scrollTop = term.scrollHeight;
      }
    }
  }, (id) => { d.opId = id; }).catch((e) => { if (!e.cancelled) { d.lines.push('— ' + e.message); invalidate('modal'); } });
  await p;
}

async function openInstall(opts = {}) {
  const d = { tab: opts.tab || ((S.info?.catalog || []).length ? 'catalog' : 'git'), repo: opts.repo || '', ref: '', subdir: '', dir: '', refs: null, catId: '', start: true, backup: true, boot: false, setup: false, forceFolder: opts.folder || '' };
  const p = pushModal('install', d);
  if (opts.cat) pickCatalog(d, opts.cat);
  else if (opts.repo) inspect(d);
  if (!(await p)) return;
  const insp = d.insp;
  const folder = (d.folder || insp.folder).trim();
  const name = S.cur;
  openActivity();
  try {
    // Deploy applies the system setup itself, while the app is stopped, and only when its check says it is not done.
    const setup = !!(d.setup && insp.profile?.system);
    const res = await runForceable((f) => call('Deploy', { board: name, key: insp.key, folder, start: !!d.start, backup: d.backup !== false, force: f, setup }));
    toast('ok', t('t_deploy', { app: insp.bundle.app.name }));
    if (res?.needsSetup) setupToast(folder);
    if (d.boot) await runOp(call('Autostart', name, folder)).catch((e) => fail(t('runAtBoot'), e));
  } catch (e) {
    fail(t('op_deploy', { app: insp.bundle.app.name }), e);
  } finally {
    refresh(name);
  }
}
function pickCatalog(d, id) {
  const p = (S.info?.catalog || []).find((x) => x.id === id);
  if (!p) return;
  d.catId = id;
  d.repo = p.source.repo; d.ref = p.source.ref || ''; d.subdir = p.source.subdir || '';
  inspect(d);
}
async function loadRefs(d) {
  if (!d.repo?.trim()) return;
  d.refsLoading = true; d.refsError = ''; invalidate('modal');
  try { d.refs = await runOp(call('Refs', d.repo.trim())); } catch (e) { d.refs = null; d.refsError = e.message; }
  d.refsLoading = false; invalidate('modal');
}
async function inspect(d) {
  d.inspecting = true; d.inspectError = ''; d.insp = null; d.inspectLog = []; invalidate('modal');
  const req = d.tab === 'local' ? { dir: d.dir.trim() } : { repo: d.repo.trim(), ref: d.ref || '', subdir: d.subdir?.trim() || '' };
  try {
    d.insp = await runOp(call('Inspect', req), (e) => { if (e.type === 'log') { d.inspectLog.push(e.text); invalidate('modal'); } });
    d.folder = d.forceFolder || d.insp.folder;
    d.setup = !!d.insp.profile?.system && !appByFolder(d.folder)?.systemReady;
  } catch (e) {
    if (!e.cancelled) d.inspectError = e.message;
  }
  d.inspecting = false; invalidate('modal');
}

async function openBoardDialog(existing, mode) {
  const d = existing
    ? { ...existing, oldName: existing.name, makeDefault: existing.name === S.def, port: existing.port || 22, devices: null, more: !!(existing.keyFile || (existing.port && existing.port !== 22) || (existing.user && existing.user !== 'arduino')) }
    : { kind: mode === 'usb' ? 'adb' : 'ssh', host: '', user: 'arduino', port: 22, keyFile: '', serial: '', makeDefault: !S.boards.length, devices: null, found: null, more: false, uploadKey: true };
  const p = pushModal('board', d);
  if (d.kind === 'adb') scanUSB(d);
  else if (!existing) scanNet(d);
  if (!(await p)) return;
  if (existing) {
    S.cur = existing.name;
    await loadBoards();
    delete S.status[S.cur];
    renderAll();
    refresh(S.cur);
    return;
  }
  const res = d.result, name = res.board;
  await loadBoards();
  S.cur = name;
  S.status[name] = { data: res };
  delete S.sys[name];
  renderAll();
  checkUpdates(name, res.apps || []);
  loadSystem(name);
}
async function scanNet(d) {
  d.found = null; d.foundError = ''; invalidate('modal');
  try { d.found = await runOp(call('Scan')); } catch (e) { d.found = []; d.foundError = e.message || String(e); }
  const fresh = d.found.filter((b) => !b.saved);
  if (fresh.length === 1 && !d.host) d.host = fresh[0].host || fresh[0].ips?.[0] || '';
  invalidate('modal');
}
async function openMirror() {
  const st = S.status[S.cur]?.data;
  if (!st) return;
  const on = mirrorOn(st);
  const debOn = !!(st.mirror?.debian && !/deb\.debian\.org/.test(st.mirror.debian));
  const cur = mirrorOf(debOn ? st.mirror.debian : st.mirror?.pip);
  const d = { board: S.cur, on, usePyPI: !on || !!st.mirror?.pip, useDebian: !on || debOn, sel: cur && cur.id !== 'official' ? cur.id : S.info?.mirrors?.[0]?.id,
    results: {}, ranked: null, testing: false, custom: on && !cur, index: on && !cur ? st.mirror?.pip || '' : '', debian: on && !cur ? st.mirror?.debian || '' : '', touched: on };
  const p = pushModal('mirror', d);
  testMirrors(d);
  if (!(await p)) return;
  openActivity();
  const opts = d.off ? { pypi: '', debian: '' } : mirrorOptions(d);
  boardOp('mirror', call('Mirror', S.cur, opts), undefined, () => t('t_mirror')).catch(() => {});
}
async function turnMirrorOff() {
  if (!(await confirm({ icon: 'download', title: t('mirrorOffTitle'), text: t('mirrorOffText'), ok: t('turnOff') }))) return;
  openActivity();
  boardOp('mirror', call('Mirror', S.cur, { pypi: '', debian: '' }), undefined, () => t('t_mirror')).catch(() => {});
}
// mirrorInfoHtml lists what the board uses now: apt's mirrors in the order it tries them, and pip/uv's.
function mirrorInfoHtml(d) {
  const m = d?.mirror || {};
  const name = (u) => { const x = mirrorOf(u); return x ? mirrorName(x) : hostOf(u); };
  const official = (u) => /deb\.debian\.org/.test(u || '');
  const debOn = m.debian && !official(m.debian);
  const apt = debOn ? [m.debian, ...(m.debianFallback || [])] : [];
  const li = (u, i, note) => `<li class="${official(u) ? 'muted' : ''}"><b>${esc(name(u))}</b>${i === 0 ? ` <span class="pill ok">${t('mirrorPrimary')}</span>` : ''}${note ? ` <span class="muted">${esc(note)}</span>` : ''}<div class="url mono">${esc(u)}</div></li>`;
  const sec = (title, items) => `<div class="pop-sec">${title}</div>${items.length ? `<ol class="mirror-list">${items.join('')}</ol>` : `<p class="muted">${t('mirrorNotUsed')}</p>`}`;
  const py = m.pip ? [li(m.pip, 0, m.uv && m.uv !== m.pip ? t('mirrorPipOnly') : ''), ...(m.pipFallback ? [li(m.pipFallback, 1, t('mirrorPipExtra'))] : []), ...(m.uv && m.uv !== m.pip ? [li(m.uv, 1, 'uv')] : [])] : [];
  return `<b>${t('mirror')}</b>
    ${sec(t('mirrorDebianSec'), apt.map((u, i) => li(u, i, official(u) ? t('mirrorLast') : '')))}
    ${sec(t('mirrorPyPISec'), py)}
    <div class="pop-actions"><button class="btn sm" data-act="mirror-change">${icon('activity', 'sm')} ${t('mirrorSpeedTest')}</button><button class="btn sm ghost danger-text" data-act="mirror-off-now">${t('mirrorTurnOff')}</button></div>`;
}
// mirrorOptions: the chosen mirror first; apt also gets the next two working ones (fastest first
// when tested) and pip the next one, to fall back to.
function mirrorOptions(d) {
  const cat = S.info?.mirrors || [];
  const order = (d.ranked || cat.map((m) => m.id)).filter((id) => id !== d.sel && cat.some((m) => m.id === id) && (!d.ranked || works(d.results[id])));
  const rest = order.slice(0, 2).map((id) => cat.find((m) => m.id === id));
  const primary = d.custom ? { pypi: d.index.trim(), debian: d.debian.trim() } : cat.find((m) => m.id === d.sel) || cat[0];
  return {
    pypi: d.usePyPI ? primary.pypi : '', debian: d.useDebian ? primary.debian : '',
    pypiFallback: d.usePyPI && !d.custom ? rest[0]?.pypi || '' : '', debianFallback: d.useDebian ? rest.map((m) => m.debian) : [],
  };
}
async function testMirrors(d) {
  if (d.testing) return;
  d.testing = true; d.results = {}; d.ranked = null; d.testError = '';
  invalidate('modal');
  try {
    const rs = await runOp(call('MirrorTest', d.board), (e) => { if (e.type === 'info' && e.data?.id) { d.results[e.data.id] = e.data; invalidate('modal'); } });
    (rs || []).forEach((r) => { d.results[r.id] = r; });
    d.ranked = (rs || []).map((r) => r.id);
    if (!d.touched) { const best = (rs || []).find((r) => works(r) && r.id !== 'official'); if (best) { d.sel = best.id; d.custom = false; } }
  } catch (e) { if (!e.cancelled) d.testError = e.message || String(e); } finally { d.testing = false; invalidate('modal'); }
}
async function doUploadKey() {
  const generate = !S.info?.publicKey;
  if (generate && !(await confirm({ icon: 'key', title: t('keyNewTitle'), text: t('keyNewText'), ok: t('keyNewOk') }))) return;
  const res = await boardOp('uploadkey', call('UploadKey', S.cur, generate), undefined,
    (r) => (r?.created ? t('t_keyCreated', { path: tildeKey(r.path) }) : r?.added ? t('t_keyUploaded', { path: tildeKey(r.path) }) : t('t_keyPresent'))).catch(() => null);
  if (res?.created) S.info = await call('Info');
}
async function openRename() {
  const old = S.cur;
  const cur = S.status[old]?.data?.hostname || old;
  const d = { name: cur, old: cur };
  if (!(await pushModal('rename', d))) return;
  const nw = d.name.trim().toLowerCase();
  if (!HOSTNAME_RE.test(nw) || nw === cur) return;
  openActivity();
  try {
    const res = await runOp(call('Rename', old, nw));
    renameLocal(old, res.board);
    S.status[res.board] = { ...(S.status[res.board] || {}), data: res, error: null };
    toast('ok', t('t_rename', { name: nw }));
  } catch (e) { fail(t('op_rename'), e); }
  invalidate('main', 'sidebar');
}
async function openSystem() {
  const name = S.cur;
  const d = { board: name, mode: 'all', skipDesktop: false, log: [] };
  const p = pushModal('system', d);
  if (!S.sys[name]?.data && !S.sys[name]?.loading) loadSystem(name);
  if (!(await p)) return;
  const opt = { arduinoOnly: d.mode === 'arduino', skipDesktop: !!d.skipDesktop, downloadOnly: !!d.downloadOnly };
  openActivity();
  await boardOp('sysupdate', (f) => call('SystemUpdate', name, { ...opt, force: f }), undefined, (r) => (r?.downloaded ? t('t_sysdownload') : r?.rebootRequired ? t('t_sysupdateReboot') : t('t_sysupdate'))).catch(() => {});
  loadSystem(name);
}
async function toggleRelay() {
  const d = S.status[S.cur]?.data;
  if (!d) return;
  if (d.relay?.installed) {
    if (!(await confirm({ icon: 'link', title: t('relayOffTitle'), text: t('relayOffText'), ok: t('turnOff') }))) return;
    openActivity();
    boardOp('relay', call('Relay', S.cur, false, ''), undefined, () => t('t_relay')).catch(() => {});
    return;
  }
  const info = await call('RelaySettings').catch(() => ({}));
  const m = { upstream: info.upstream || '', effective: info.effective || '' };
  if (!(await pushModal('relay', m))) return;
  openActivity();
  boardOp('relay', call('Relay', S.cur, true, m.upstream.trim()), undefined, () => t('t_relay')).catch(() => {});
}
async function scanUSB(d) {
  d.devices = null; d.devicesError = ''; invalidate('modal');
  try { d.devices = await call('USBDevices'); } catch (e) { d.devices = []; d.devicesError = e.message || String(e); }
  if (d.devices.length === 1 && !d.serial) d.serial = d.devices[0].serial;
  invalidate('modal');
}

// ---------------------------------------------------------------- policies
const zhOr = (en, zh) => (lang() === 'zh' && zh ? zh : en) || '';
const slotLabel = (s) => zhOr(s?.name || s?.id, s?.nameZh);
const kb = (n) => (n >= 1048576 ? `${(n / 1048576).toFixed(1)} MB` : `${Math.max(1, Math.round((n || 0) / 1024))} KB`);
const shape = (x) => `[${(x?.shape || []).map((d) => (d < 0 ? '?' : d)).join(',')}]`;
const policySlots = (a) => a?.profile?.policies?.slots || [];

modalViews.policies = (d) => {
  const a = d.app, st = d.status, cat = d.cat;
  const active = st ? st.slots.filter((s) => s.active).length : Object.keys(a.policies || {}).length;
  const inSlot = (e) => st?.slots.find((s) => s.id === e.slot);
  let body;
  if (d.tab === 'slots') {
    body = d.statusError ? `<div class="callout warn" style="margin:0">${icon('alert')}<div class="grow">${esc(d.statusError)}</div></div>`
      : !st ? `<div class="loading-line">${icon('spinner', 'spin')}</div>`
      : `<table class="pol-slots"><tbody>${st.slots.map((s) => `<tr title="${esc(s.description || '')}">
          <td><b>${esc(slotLabel(s))}</b><div class="sub mono">${esc(s.file)}</div></td>
          <td>${s.active ? `<b>${esc(s.active.name)}</b><div class="sub">${esc([s.active.author ? t('polBy', { author: s.active.author }) : '', s.active.source ? t('polFile', { file: s.active.source }) : '', when(s.active.installedAt)].filter(Boolean).join(' · '))}</div>`
            : `<span class="muted">${t('polStock')}</span>`}${s.changed ? ` <span class="pill warn">${t('polChanged')}</span>` : ''}${!s.sha256 ? ` <span class="pill warn">${t('polMissing')}</span>` : ''}</td>
          <td class="n">${s.active ? `<button class="btn sm" data-act="pol-restore" data-slot="${esc(s.id)}">${icon('restore', 'sm')} ${t('polRestore')}</button>` : ''}</td></tr>`).join('')}</tbody></table>`;
  } else if (d.catError) {
    body = `<div class="callout warn" style="margin:0">${icon('alert')}<div class="grow">${esc(d.catError)}</div></div>`;
  } else if (!cat) {
    body = `<div class="loading-line">${icon('spinner', 'spin')} ${t('polLoading')}</div>`;
  } else {
    const list = cat.entries.map((e, i) => {
      const used = inSlot(e)?.sha256 === e.sha256;
      const links = [e.video ? `<a class="link" data-act="open-web" data-url="${esc(e.video)}">${icon('play', 'sm')} ${t('polVideo')}</a>` : '', e.homepage ? `<a class="link" data-act="open-web" data-url="${esc(e.homepage)}">${icon('external', 'sm')} ${t('polHome')}</a>` : ''].filter(Boolean).join('');
      return `<div class="pol"><div class="grow">
          <div class="pol-name"><b>${esc(zhOr(e.name, e.nameZh))}</b> <span class="pill">${esc(slotLabel(policySlots(a).find((s) => s.id === e.slot) || { id: e.slot }))}</span></div>
          <div class="sub">${esc([t('polBy', { author: e.author }), kb(e.size), e.license, e.created].filter(Boolean).join(' · '))}</div>
          ${e.description ? `<div class="pol-desc">${esc(zhOr(e.description, e.descriptionZh))}</div>` : ''}${links ? `<div class="pol-links">${links}</div>` : ''}</div>
        <div>${used ? `<span class="pill ok">${icon('check', 'sm')} ${t('polInUse')}</span>` : `<button class="btn sm primary" data-act="pol-install" data-i="${i}" ${st ? '' : 'disabled'}>${t('polInstall')}</button>`}</div></div>`;
    }).join('');
    body = (list || `<div class="empty-pol">${icon('cpu', 'lg')}<p>${esc(t('polEmpty', { app: a.name }))}</p><p class="muted">${esc(t('polPublish', { id: a.profile.id }))}</p></div>`)
      + (cat.problems?.length ? `<details class="pol-problems"><summary>${icon('alert', 'sm')} ${t('polIndexProblems')}</summary>${cat.problems.map((p) => `<div class="mono">${esc(p)}</div>`).join('')}</details>` : '');
  }
  return {
    cls: 'wide',
    html: header('cpu', esc(t('polTitle', { app: a.name })), t('polSub')) + `<div class="mbody">
      <div class="seg"><button class="${d.tab !== 'slots' ? 'on' : ''}" data-act="pol-tab" data-tab="store">${icon('download')} ${t('polStore')}${cat ? ` · ${cat.entries.length}` : ''}</button><button class="${d.tab === 'slots' ? 'on' : ''}" data-act="pol-tab" data-tab="slots">${icon('cpu')} ${t('polSlots')}${active ? ` · ${esc(t('policiesChip', { n: active }))}` : ''}</button></div>
      ${d.tab !== 'slots' && cat?.sources?.length ? `<div class="pol-from">${icon('link', 'sm')}<span>${t('polFrom', { sources: cat.sources.map((x) => x.page
        ? `<a class="link" data-act="open-web" data-url="${esc(x.page)}" title="${esc(x.url)}">${esc(x.name)}</a>` : `<span class="mono" title="${esc(x.url)}">${esc(x.name)}</span>`).join(t('listSep')) })}</span></div>` : ''}
      <div class="pol-body">${body}</div>
      <p class="sys-note">${t('polNote')}</p></div>
      <footer><button class="btn ghost" data-act="pol-file" ${st ? '' : 'disabled'}>${icon('folder', 'sm')} ${t('polFromFile')}</button><button class="btn ghost" data-act="pol-index">${t('polIndexes')}</button>
        ${active ? `<button class="btn ghost" data-act="pol-restore-all">${t('polRestoreAll')}</button>` : ''}<div class="spacer"></div><button class="btn primary" data-act="modal-ok" data-primary>${t('close')}</button></footer>`,
  };
};

modalViews.polcheck = (d) => {
  const c = d.check, slots = policySlots(d.app);
  const title = d.entry ? zhOr(d.entry.name, d.entry.nameZh) : d.name || d.file.split(/[\\/]/).pop();
  const pick = d.entry ? '' : field(t('polPickSlot'), `<div class="pol-pick">${slots.map((s) => `<div class="mirror-custom ${d.slot === s.id ? 'on' : ''}" data-act="pol-slot" data-slot="${esc(s.id)}"><span class="radio ${d.slot === s.id ? 'on' : ''}"></span>${esc(slotLabel(s))} <span class="sub mono">${esc(s.file)}</span></div>`).join('')}</div>`);
  const meta = d.entry ? '' : `<div class="field-row">${field(t('polName'), input('name', d.name, '', 'class="input"'))}${field(t('polAuthor'), input('author', d.author, '', 'class="input"'))}</div>`;
  let res = '';
  if (d.checking) res = `<div class="loading-line">${icon('spinner', 'spin')} ${t('polChecking')}</div>`;
  else if (d.error) res = `<div class="callout warn" style="margin:0">${icon('alert')}<div class="grow">${esc(d.error)}</div></div>`;
  else if (c) {
    const sl = slots.find((s) => s.id === c.slot);
    res = `<div class="pol-facts"><div><span class="lbl">${t('polSlot')}</span>${esc(slotLabel(sl))} <span class="sub mono">${esc(sl?.file || '')}</span></div>
        ${c.model ? `<div><span class="lbl">ONNX</span>${esc(t('polShapes', { in: `${c.model.inputs[0]?.name} ${shape(c.model.inputs[0])}`, out: `${c.model.outputs[0]?.name} ${shape(c.model.outputs[0])}` }))}</div>` : ''}
        <div><span class="lbl">SHA-256</span><span class="mono">${esc(c.sha256.slice(0, 16))}…</span> · ${kb(c.size)}</div></div>
      ${c.problems.length ? `<div class="callout warn" style="margin:10px 0 0">${icon('alert')}<div class="grow">${t('polBad')}<ul>${c.problems.map((p) => `<li>${esc(p)}</li>`).join('')}</ul></div></div>${c.model ? check('force', d.force, t('polForce')) : ''}`
        : c.same ? `<div class="callout info" style="margin:10px 0 0">${icon('info')}<div class="grow">${t('polSame')}</div></div>`
        : `<div class="ok-line">${icon('check')} ${t('polFits')}</div>`}
      <p class="sys-note">${esc(t(d.running ? 'polRestartNote' : 'polNotRunning', { app: d.app.name }))}</p>`;
  }
  const ok = c && !d.checking && !c.same && (!c.problems.length || (d.force && c.model));
  return {
    html: header('cpu', esc(t('polCheckTitle', { name: title })), esc(d.app.name)) + `<div class="mbody">${pick}${meta}${res}</div>
      <footer><div class="spacer"></div><button class="btn" data-act="modal-close">${t('cancel')}</button><button class="btn primary" data-act="modal-ok" data-primary ${ok ? '' : 'disabled'}>${t('polInstall')}</button></footer>`,
  };
};

modalViews.polindex = (d) => ({
  html: header('link', t('polIndexTitle'), t('polIndexSub')) + `<div class="mbody">
    ${d.profileIndex ? field(esc(t('polIndexProfile', { name: d.profileName })), `<div class="input mono ro">${esc(d.profileIndex)}</div>`) : ''}
    ${field(t('polIndexMine'), `<textarea class="input mono" rows="4" data-bind="text" spellcheck="false">${esc(d.text)}</textarea>`, t('polIndexHelp'))}</div>
    <footer><div class="spacer"></div><button class="btn" data-act="modal-close">${t('cancel')}</button><button class="btn primary" data-act="modal-ok">${t('save')}</button></footer>`,
});

async function openPolicies(a, tab) {
  const d = { app: a, tab: tab || 'store', status: null, cat: null, statusError: '', catError: '' };
  const p = pushModal('policies', d);
  loadPolicyStatus(d);
  loadPolicyCatalog(d);
  await p;
}
function loadPolicyStatus(d) {
  d.status = null; d.statusError = ''; invalidate('modal');
  runOp(call('Policies', S.cur, d.app.folder)).then((r) => { d.status = r; }).catch((e) => { d.statusError = e.message || String(e); }).finally(() => invalidate('modal'));
}
function loadPolicyCatalog(d) {
  d.cat = null; d.catError = ''; invalidate('modal');
  runOp(call('PolicyCatalog', d.app.profile.id, d.app.profile.policies?.index || null)).then((r) => { d.cat = r; }).catch((e) => { d.catError = e.message || String(e); }).finally(() => invalidate('modal'));
}
function runPolicyCheck(c) {
  if (!c.slot) { c.check = null; invalidate('modal'); return; }
  c.checking = true; c.check = null; c.error = ''; c.force = false; invalidate('modal');
  runOp(call('CheckPolicy', { board: S.cur, folder: c.app.folder, entry: c.entry, file: c.entry ? '' : c.file, slot: c.slot }))
    .then((r) => { c.check = r; c.file = r.file; c.slot = r.slot; })
    .catch((e) => { c.error = e.cancelled ? '' : e.message || String(e); })
    .finally(() => { c.checking = false; invalidate('modal'); });
}
async function installPolicy(d, src) {
  const base = (src.file || '').split(/[\\/]/).pop();
  const c = { app: d.app, entry: src.entry || null, file: src.file || '', slot: src.entry?.slot || policySlots(d.app).find((s) => s.file === base)?.id || '',
    name: src.file ? base.replace(/\.onnx$/i, '') : '', author: '', check: null, error: '', checking: false, force: false, running: !!d.status?.running };
  const p = pushModal('polcheck', c);
  runPolicyCheck(c);
  if (!(await p)) return;
  closeModal(true); // the policies dialog: progress shows in Activity
  openActivity();
  const name = c.entry ? zhOr(c.entry.name, c.entry.nameZh) : c.name || base;
  const req = { board: S.cur, folder: d.app.folder, entry: c.entry, file: c.entry ? '' : c.file, slot: c.slot, name: c.name, author: c.author };
  await boardOp('policy', (f) => call('InstallPolicy', { ...req, force: f || !!c.force }), d.app.folder, () => t('t_policy', { name })).catch(() => {});
}
async function restorePolicies(d, slots) {
  const names = (slots.length ? slots : (d.status?.slots || []).filter((s) => s.active).map((s) => s.id))
    .map((id) => slotLabel(policySlots(d.app).find((s) => s.id === id) || { id })).join(', ');
  if (!(await confirm({ icon: 'restore', title: t('polRestoreQ'), text: esc(t('polRestoreText', { slots: names, app: d.app.name })), ok: t('polRestore') }))) return;
  closeModal(true);
  openActivity();
  await boardOp('policyrestore', (f) => call('RestorePolicies', S.cur, d.app.folder, slots, f), d.app.folder, () => t('t_polRestore')).catch(() => {});
}
async function editPolicyIndexes(d) {
  const cfg = d.cat?.indexes || [];
  const own = d.app.profile?.policies?.index?.url || '';
  const m = { profileIndex: own, profileName: d.app.profile?.name || '', text: cfg.filter((u) => u !== own).join('\n') };
  if (!(await pushModal('polindex', m))) return;
  try { await call('SetPolicyIndexes', m.text.split('\n').map((s) => s.trim()).filter(Boolean)); loadPolicyCatalog(d); } catch (e) { toast('err', e.message || String(e)); }
}

// ---------------------------------------------------------------- actions (data-act)
const act = {
  'select-board': ({ board: name }) => { if (S.cur === name) return; S.cur = name; renderAll(); if (!S.status[name]?.data) refresh(name); },
  'add-board': ({ mode }) => openBoardDialog(null, mode),
  'edit-board': () => openBoardDialog(board(S.cur)),
  'board-menu': (_, el) => {
    const b = board(S.cur);
    openMenu(el, [
      { icon: 'edit', label: t('editBoard'), fn: () => openBoardDialog(b) },
      { icon: 'sliders', label: t('renameBoard'), disabled: !S.status[S.cur]?.data, fn: () => openRename() },
      { icon: 'package', label: t('systemUpdate'), disabled: !S.status[S.cur]?.data, fn: () => openSystem() },
      { icon: 'key', label: keyOn(S.status[S.cur]?.data) ? t('keyUploaded') : t('uploadKey'), disabled: !S.status[S.cur]?.data || keyOn(S.status[S.cur]?.data), fn: () => doUploadKey() },
      { icon: 'star', label: t('makeDefault'), disabled: S.def === b.name, fn: async () => { await call('SetDefault', b.name); await loadBoards(); invalidate('sidebar'); } },
      { icon: 'boot', label: t('op_autostart_none'), disabled: !S.status[S.cur]?.data?.apps?.some((a) => a.default), fn: () => doAutostart('') },
      '-',
      { icon: 'trash', label: t('removeBoard'), danger: true, fn: async () => {
        if (!(await confirm({ icon: 'trash', danger: true, title: esc(t('removeBoardQ', { name: b.name })), text: t('removeBoardText'), ok: t('removeBoard') }))) return;
        await call('RemoveBoard', b.name); delete S.status[b.name]; S.cur = null; await loadBoards(); renderAll(); if (S.cur) refresh(S.cur);
      } },
    ]);
  },
  refresh: () => refresh(S.cur),
  install: ({ cat, tab }) => openInstall({ cat, tab }),
  'toggle-activity': () => { S.activity = !S.activity; invalidate('activity', 'main'); },
  'clear-ops': () => { S.ops = S.ops.filter((o) => o.state === 'run'); S.opMap = new Map(S.ops.map((o) => [o.id, o])); invalidate('activity'); },
  'op-toggle': ({ op }) => { const o = S.opMap.get(op); if (o) { o.open = !o.open; invalidate('activity'); } },
  'op-log': ({ op }) => { const o = S.opMap.get(op); if (o) { o.showLog = !o.showLog; invalidate('activity'); } },
  'op-cancel': ({ op }) => call('Cancel', op),
  'reveal-backups': () => call('Reveal', ''),
  about: () => openAbout(),
  'about-toggle': () => { const m = topModal(); if (m?.type === 'about') { m.data.open = !m.data.open; invalidate('modal'); } },
  lang: async ({ lang: l }) => { setLang(l); await call('SetLang', l).catch(() => {}); S.ops.forEach((o) => { o.title = opTitle(o.kind, o.board, o.folder); }); renderAll(); },
  'open-web': ({ url }) => call('OpenURL', url),
  start: ({ folder }) => { const a = appByFolder(folder); boardOp('start', call('Start', S.cur, folder), folder, () => t('t_start', { app: a?.name })).catch(() => {}); },
  stop: async ({ folder }) => {
    const a = appByFolder(folder);
    try { await boardOp('stop', (f) => call('Stop', S.cur, folder, f), folder, () => t('t_stop', { app: a?.name })); } catch { /* shown */ }
  },
  restart: ({ folder }) => { const a = appByFolder(folder); boardOp('restart', (f) => call('Restart', S.cur, folder, f), folder, () => t('t_restart', { app: a?.name })).catch(() => {}); },
  update: ({ folder }) => { const a = appByFolder(folder); if (a) doUpdate(a); },
  prepare: ({ folder }) => { const a = appByFolder(folder); if (a) doPrepare(a, false); },
  logs: ({ folder }) => { const a = appByFolder(folder); if (a) openLogs(a); },
  'app-menu': ({ folder }, el) => { const a = appByFolder(folder); if (a) appMenu(el, a); },
  mirror: () => (mirrorOn(S.status[S.cur]?.data) ? turnMirrorOff() : openMirror()),
  'mirror-info': (_, el) => openPopover(el, mirrorInfoHtml(S.status[S.cur]?.data)),
  'mirror-change': () => { closeMenu(); openMirror(); },
  'mirror-test': () => openMirror(),
  'mirror-off-now': () => { closeMenu(); turnMirrorOff(); },
  'pick-mirror': ({ id }) => { const d = topModal().data; d.sel = id; d.custom = false; d.touched = true; invalidate('modal'); },
  'mirror-custom': () => { const d = topModal().data; d.custom = !d.custom; d.touched = true; invalidate('modal'); },
  'mirror-retest': () => testMirrors(topModal().data),
  'mirror-off': async () => {
    const d = topModal().data;
    if (!(await confirm({ icon: 'download', title: t('mirrorOffTitle'), text: t('mirrorOffText'), ok: t('turnOff') }))) return;
    d.off = true; closeModal(true);
  },
  relay: () => toggleRelay(),
  network: () => openNetwork(),
  'net-refresh': () => loadNetwork(topModal().data),
  'wifi-add': ({ phone }) => openWifiAdd(topModal().data, !!phone),
  'wifi-rescan': () => scanWifi(topModal().data, S.cur),
  'wifi-pick': ({ ssid }) => { const d = topModal().data; d.ssid = ssid; invalidate('modal'); },
  'wifi-menu': ({ uuid }, el) => wifiMenu(uuid, el),
  'away-pick': ({ mode }) => pickAway(mode),
  'hotspot-edit': () => editHotspot(),
  'sys-fix': ({ id }) => systemFix(id),
  'hotspot-open': () => hotspotNow(true),
  'hotspot-close': () => hotspotNow(false),
  system: () => openSystem(),
  'sys-mode': ({ mode }) => { const d = topModal().data; d.mode = mode; invalidate('modal'); },
  'sys-download': () => { topModal().data.downloadOnly = true; closeModal(true); },
  'sys-recheck': () => { const d = topModal().data; d.log = []; loadSystem(d.board, true, (e) => { if (e.type === 'log') { d.log.push(e.text); invalidate('modal'); } }); },
  'scan-net': () => scanNet(topModal().data),
  'pick-found': ({ host }) => { const d = topModal().data; d.host = host; d.addError = ''; invalidate('modal'); },
  'board-more': () => { const d = topModal().data; d.more = !d.more; invalidate('modal'); },
  'rename-board': () => { closeModal(false); openRename(); },
  // modal plumbing
  overlay: (_, el, ev) => { if (ev.target === el && !['secret', 'hostkey', 'logs'].includes(topModal()?.type)) closeModal(false); },
  'modal-close': () => { const m = topModal(); if (m?.type === 'secret' || m?.type === 'hostkey') respond(false); else closeModal(false); },
  'modal-ok': () => closeModal(true),
  'prompt-ok': () => respond(true),
  'prompt-cancel': () => respond(false),
  'toggle-show': () => { const d = topModal().data; d.show = !d.show; invalidate('modal'); },
  'board-kind': ({ kind }) => { const d = topModal().data; d.kind = kind; if (kind === 'adb' && d.devices == null) scanUSB(d); invalidate('modal'); },
  'scan-usb': () => scanUSB(topModal().data),
  'pick-device': ({ serial }) => { const d = topModal().data; d.serial = serial; invalidate('modal'); },
  'pick-key': async () => { const p = await call('PickKey').catch(() => ''); if (p) { topModal().data.keyFile = p; invalidate('modal'); } },
  'save-board': async () => {
    const d = topModal().data;
    const t0 = { kind: d.kind, host: (d.host || '').trim(), user: (d.user || 'arduino').trim(), port: parseInt(d.port, 10) || 22, keyFile: (d.keyFile || '').trim(), serial: d.kind === 'adb' ? d.serial || '' : '' };
    if (d.oldName) {
      try { await call('SaveBoard', { ...t0, name: d.oldName }, d.oldName, !!d.makeDefault); closeModal(true); } catch (e) { toast('err', e.message || String(e)); }
      return;
    }
    if (t0.kind === 'ssh' && !t0.host) { d.addError = t('hostRequired'); invalidate('modal'); return; }
    // The board is named after itself: connect first (answering any trust/password prompts), then save.
    d.adding = true; d.addError = ''; invalidate('modal');
    try { d.result = await runOp(call('AddBoard', t0, !!d.makeDefault, !!d.uploadKey)); d.adding = false; closeModal(true); if (d.uploadKey && !S.info?.publicKey) S.info = await call('Info'); } catch (e) { d.adding = false; d.addError = e.cancelled ? '' : e.message || String(e); invalidate('modal'); }
  },
  'install-tab': ({ tab }) => { const d = topModal().data; d.tab = tab; d.insp = null; d.inspectError = ''; invalidate('modal'); },
  'pick-cat': ({ cat }) => pickCatalog(topModal().data, cat),
  'load-refs': () => loadRefs(topModal().data),
  'toggle-adv': () => { const d = topModal().data; d.adv = !d.adv; invalidate('modal'); },
  'pick-folder': async () => { const p = await call('PickFolder').catch(() => ''); if (p) { const d = topModal().data; d.dir = p; inspect(d); } },
  inspect: () => inspect(topModal().data),
  'do-install': () => closeModal(true),
  'do-update-ref': () => closeModal(true),
  'do-erase': () => { const d = topModal().data; if (d.confirm === d.app.folder) closeModal(true); },
  'logs-clear': () => { topModal().data.lines = []; invalidate('modal'); },
  policies: ({ folder, tab }) => { const a = appByFolder(folder); if (a) openPolicies(a, tab); },
  'profile-info': ({ folder }, el) => {
    const a = appByFolder(folder);
    if (a?.profile) openPopover(el, `<b>${esc(t('profileChip', { name: a.profile.name }))}</b>${hooksHtml(a.profile)}`);
  },
  'hooks-toggle': () => { const d = topModal()?.data; if (d) { d.hooksOpen = d.hooksOpen === false; invalidate('modal'); } },
  help: ({ topic }, el) => { if (S.menu?.html) closeMenu(); else openPopover(el, helpHtml(topic)); },
  'relay-upstream': () => editRelayUpstream(),
  takeover: ({ folder }) => {
    const a = appByFolder(folder), upd = S.updates[S.cur]?.[folder];
    const cat = (S.info?.catalog || []).find((p) => p.id === a?.profile?.id);
    openInstall(cat ? { cat: cat.id, folder } : { tab: 'git', repo: upd?.repo, folder });
  },
  'pol-tab': ({ tab }) => { topModal().data.tab = tab; invalidate('modal'); },
  'pol-install': ({ i }) => { const d = topModal().data; const e = d.cat?.entries[+i]; if (e) installPolicy(d, { entry: e }); },
  'pol-file': async () => { const d = topModal().data; const f = await call('PickPolicyFile').catch(() => ''); if (f) installPolicy(d, { file: f }); },
  'pol-slot': ({ slot }) => { const c = topModal().data; if (c.slot !== slot) { c.slot = slot; runPolicyCheck(c); } },
  'pol-restore': ({ slot }) => restorePolicies(topModal().data, [slot]),
  'pol-restore-all': () => restorePolicies(topModal().data, []),
  'pol-index': () => editPolicyIndexes(topModal().data),
};

function respond(ok) {
  const m = topModal();
  if (!m) return;
  call('Respond', m.data.id, ok ? m.data.value || '' : '', ok);
  closeModal(ok);
}
on('quay:prompt', (p) => { pushModal(p.kind === 'hostkey' ? 'hostkey' : 'secret', { ...p, value: '' }); });
on('quay:prompt-cancel', (pid) => {
  const i = S.modals.findIndex((m) => m.data.id === pid);
  if (i >= 0) { S.modals.splice(i, 1); invalidate('modal'); }
});

// ---------------------------------------------------------------- DOM events
document.addEventListener('click', (ev) => {
  const mi = ev.target.closest('[data-menu]');
  if (mi && S.menu) { const it = S.menu.items[+mi.dataset.menu]; closeMenu(); it?.fn?.(); return; }
  if (S.menu && !ev.target.closest('.menu')) closeMenu();
  const el = ev.target.closest('[data-act]');
  if (!el) return;
  const fn = act[el.dataset.act];
  if (fn) { if (el.dataset.act !== 'overlay') ev.preventDefault(); fn({ ...el.dataset }, el, ev); }
});
document.addEventListener('input', (ev) => {
  const el = ev.target.closest('[data-bind]');
  const m = topModal();
  if (!el || !m) return;
  const k = el.dataset.bind;
  m.data[k] = el.type === 'checkbox' ? el.checked : el.value;
  if (m.type === 'install' && (k === 'repo' || k === 'subdir' || k === 'dir')) {
    if (m.data.insp || m.data.inspectError) { m.data.insp = null; m.data.inspectError = ''; invalidate('modal'); }
    if (k === 'repo') { m.data.refs = null; m.data.catId = ''; }
    const btn = $('#modal-root [data-act="inspect"]');
    if (btn) btn.disabled = !(m.data.tab === 'git' ? m.data.repo.trim() : m.data.dir.trim());
  }
  if (m.type === 'install' && k === 'ref' && m.data.insp) { m.data.insp = null; invalidate('modal'); }
  if (m.type === 'install' && k === 'folder') { const h = el.closest('.field')?.querySelector('.help'); if (h) h.textContent = t('folderHelp', { f: el.value }); }
  if (el.type === 'checkbox' && (m.type === 'system' || m.type === 'mirror' || m.type === 'polcheck')) invalidate('modal');
  if (m.type === 'board' && k === 'host' && m.data.found) { const fh = m.data.found.some((b) => (b.host || b.ips?.[0]) === el.value.trim()); if (!fh) $$('#modal-root .device.on').forEach((x) => x.classList.remove('on')); }
  if (m.type === 'hotspot' || m.type === 'wifiadd') {
    const [ok, sMsg, pMsg] = netForm(m);
    const b = $('#modal-root [data-act="modal-ok"]'); if (b) b.disabled = !ok;
    const hs = $('#modal-root .v-ssid'); if (hs) hs.textContent = sMsg;
    const hp = $('#modal-root .v-psk'); if (hp) hp.textContent = pMsg;
    if (k === 'ssid') $$('#modal-root .device.on').forEach((x) => x.classList.remove('on'));
    if (k === 'phone') invalidate('modal');
  }
  if (m.type === 'rename' && k === 'name') {
    const v = el.value.trim().toLowerCase(), ok = HOSTNAME_RE.test(v);
    const b = $('#modal-root [data-act="modal-ok"]'); if (b) b.disabled = !ok;
    const h = $('#modal-root .rename-help'); if (h) h.textContent = ok ? t('renameHelp', { name: v }) : t('renameBad');
  }
  if (m.type === 'erase' && k === 'confirm') { const b = $('#modal-root [data-act="do-erase"]'); if (b) b.disabled = el.value !== m.data.app.folder; }
});
document.addEventListener('change', (ev) => {
  const el = ev.target.closest('[data-bind="repo"]');
  if (el && topModal()?.type === 'install') loadRefs(topModal().data);
});
document.addEventListener('keydown', (ev) => {
  if (ev.key === 'Escape') {
    if (S.menu) { closeMenu(); return; }
    const m = topModal();
    if (m) { if (m.type === 'secret' || m.type === 'hostkey') respond(false); else closeModal(false); return; }
    if (S.activity) { S.activity = false; invalidate('activity', 'main'); }
  }
  if (ev.key === 'Enter' && topModal() && ev.target.tagName !== 'TEXTAREA' && ev.target.tagName !== 'SELECT') {
    if (ev.target.dataset?.bind === 'repo') { loadRefs(topModal().data); return; }
    const b = $('#modal-root [data-primary]');
    if (b && !b.disabled) { ev.preventDefault(); b.click(); }
  }
  if ((ev.metaKey || ev.ctrlKey) && ev.key.toLowerCase() === 'r' && !topModal()) { ev.preventDefault(); refresh(S.cur); }
});
window.addEventListener('resize', closeMenu);
setInterval(() => { if (S.cur && S.status[S.cur]?.data && !document.hidden && !topModal()) refresh(S.cur); }, 30000);

// ---------------------------------------------------------------- boot
async function init() {
  S.info = await call('Info');
  S.relayInfo = await call('RelaySettings').catch(() => ({}));
  document.documentElement.dataset.os = S.info.os; // the window chrome differs: see app.css
  setLang(detectLang(S.info.lang));
  await loadBoards();
  renderAll();
  if (S.cur) refresh(S.cur);
}
init().catch((e) => { document.body.innerHTML = `<pre style="padding:20px">${esc(e.stack || e)}</pre>`; });
