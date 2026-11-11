// lib.js — shared state, the API client and the small UI toolkit every view
// builds on. Nothing here knows about a particular page.

/** What the server told us that more than one view needs. */
export const S = {
  version: '', home: '', config: {}, providers: [], efforts: [],
  chats: new Map(),   // id → ChatMeta, every kind (chat, task run, sub-agent)
  running: new Set(), // ids of chats with an active run
  live: new Map(),    // id → Live, for the running chats we have seen
  models: null,       // null until /api/models has answered once
  modelErrors: {},
  tasks: [], triggers: [],
  browser: [],        // open browser sessions
  route: {page: 'chat', id: ''},
};

/**
 * 'server' carries one event of the live stream (detail = the event),
 * 'state' fires after anything in S changed, 'reload' after a reconnect.
 */
export const bus = new EventTarget();
export const changed = () => bus.dispatchEvent(new Event('state'));

/** Runs render now and again whenever key() changes after a state update. */
export function watch(signal, key, render) {
  let last = key();
  render();
  bus.addEventListener('state', () => {
    const next = key();
    if (next !== last) { last = next; render(); }
  }, {signal});
}

// ---------------------------------------------------------------- DOM

/** h('button.btn.primary', {title: 'Send', onclick: fn}, icon('up'), 'Send') */
export function h(spec, props, ...kids) {
  const [tag, ...classes] = spec.split('.');
  const el = document.createElement(tag || 'div');
  if (classes.length) el.className = classes.join(' ');
  if (props == null || typeof props !== 'object' || props instanceof Node || Array.isArray(props)) {
    kids.unshift(props);
    props = null;
  }
  for (const [key, value] of Object.entries(props || {})) {
    if (value == null) continue;
    if (key.startsWith('on')) el.addEventListener(key.slice(2), value);
    else if (key === 'class') { if (value) el.classList.add(...value.split(' ').filter(Boolean)); }
    else if (key in el) el[key] = value;
    else if (value !== false) el.setAttribute(key, value === true ? '' : value);
  }
  el.append(...kids.flat().filter(k => k != null && k !== false));
  return el;
}

const ICONS = {
  plus: 'M12 5v14M5 12h14',
  x: 'M6 6l12 12M18 6L6 18',
  check: 'M5 12.5l4.5 4.5L19 7.5',
  search: 'M11 4a7 7 0 1 0 0 14 7 7 0 0 0 0-14zM20 20l-4-4',
  sidebar: 'M4 5h16v14H4zM9 5v14',
  more: 'M5 12a1 1 0 1 0 2 0 1 1 0 0 0-2 0M11 12a1 1 0 1 0 2 0 1 1 0 0 0-2 0M17 12a1 1 0 1 0 2 0 1 1 0 0 0-2 0',
  pin: 'M9 4h6l-1 6 3 3H7l3-3-1-6zM12 13v7',
  trash: 'M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13M10 11v5M14 11v5',
  edit: 'M4 20h4L19 9l-4-4L4 16v4zM13 7l4 4',
  copy: 'M9 9h10v11H9zM5 15V4h10',
  right: 'M9 6l6 6-6 6',
  down: 'M6 9l6 6 6-6',
  up: 'M12 19V5M6 11l6-6 6 6',
  arrowDown: 'M12 5v14M6 13l6 6 6-6',
  back: 'M19 12H5M11 6l-6 6 6 6',
  stop: 'M7 7h10v10H7z',
  clip: 'M20 11l-8.5 8.5a5 5 0 0 1-7-7L13 4a3.5 3.5 0 0 1 5 5l-8.5 8.5a2 2 0 0 1-3-3L14 7',
  clock: 'M12 4a8 8 0 1 0 0 16 8 8 0 0 0 0-16zM12 8v4l3 2',
  book: 'M5 4h11a3 3 0 0 1 3 3v13H8a3 3 0 0 1-3-3V4zM5 17a3 3 0 0 1 3-3h11',
  spark: 'M12 3l2 6 6 2-6 2-2 6-2-6-6-2 6-2 2-6z',
  sliders: 'M4 8h9M17 8h3M4 16h3M11 16h9M13 8a2 2 0 1 0 4 0 2 2 0 0 0-4 0M7 16a2 2 0 1 0 4 0 2 2 0 0 0-4 0',
  sun: 'M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8zM12 2v2M12 20v2M2 12h2M20 12h2M5 5l1.5 1.5M17.5 17.5L19 19M19 5l-1.5 1.5M6.5 17.5L5 19',
  moon: 'M20 14a8 8 0 1 1-10-10 6.5 6.5 0 0 0 10 10z',
  auto: 'M12 4a8 8 0 1 0 0 16 8 8 0 0 0 0-16zM12 4v16M12 8l5.5-2M12 12l8-1M12 16l6.5 1.5',
  terminal: 'M4 5h16v14H4zM8 10l3 2-3 2M13 15h3',
  file: 'M7 3h7l4 4v14H7zM14 3v4h4',
  globe: 'M12 4a8 8 0 1 0 0 16 8 8 0 0 0 0-16zM4 12h16M12 4c2.5 2.5 2.5 13.5 0 16M12 4c-2.5 2.5-2.5 13.5 0 16',
  bot: 'M6 9h12v10H6zM12 9V5M9.5 13v1.5M14.5 13v1.5M3 13v3M21 13v3',
  key: 'M8 11a4 4 0 1 0 0 8 4 4 0 0 0 0-8zM11 12l8-8M16 7l3 3M14 9l2 2',
  cube: 'M4 8l8-4 8 4v8l-8 4-8-4zM4 8l8 4 8-4M12 12v8',
  window: 'M3 5h18v14H3zM3 9h18',
  fork: 'M7 3v12M7 15a3 3 0 1 0 0 6 3 3 0 0 0 0-6zM17 3a3 3 0 1 0 0 6 3 3 0 0 0 0-6zM17 9c0 5-10 3-10 6',
  retry: 'M20 12a8 8 0 1 1-2.5-5.8M20 4v5h-5',
  play: 'M8 5l11 7-11 7z',
  pause: 'M8 5v14M16 5v14',
  info: 'M12 4a8 8 0 1 0 0 16 8 8 0 0 0 0-16zM12 11v5M12 8v.5',
  alert: 'M12 4l9 16H3zM12 10v4M12 17v.5',
  chat: 'M5 5h14v10H10l-5 4z',
  open: 'M14 5h5v5M19 5l-8 8M11 7H5v12h12v-6',
};

const SVG = 'http://www.w3.org/2000/svg';

/** An inline stroke icon; decorative unless the caller labels its parent. */
export function icon(name, size = 18) {
  const svg = document.createElementNS(SVG, 'svg');
  for (const [k, v] of Object.entries({viewBox: '0 0 24 24', width: size, height: size, class: 'icon', 'aria-hidden': 'true'})) {
    svg.setAttribute(k, v);
  }
  const path = document.createElementNS(SVG, 'path');
  path.setAttribute('d', ICONS[name]);
  svg.append(path);
  return svg;
}

/** The arrow of a collapsible section; it turns when the section opens. */
export function chevron() {
  const svg = icon('right', 14);
  svg.classList.add('chevron');
  return svg;
}

// ---------------------------------------------------------------- API

/**
 * Calls the JSON API. A plain object is sent as JSON, a string or Blob as the
 * raw body. Failures are shown as a toast (unless quiet) and resolve to null,
 * so callers write `if (!res) return` instead of try/catch.
 */
export async function api(method, path, body, {quiet = false} = {}) {
  const opts = {method, headers: {}};
  if (typeof body === 'string' || body instanceof Blob) opts.body = body;
  else if (body !== undefined) {
    opts.body = JSON.stringify(body);
    opts.headers['Content-Type'] = 'application/json';
  }
  let message;
  try {
    const res = await fetch(path, opts);
    const text = await res.text();
    let data = null;
    try { data = JSON.parse(text); } catch { /* plain-text error from the guard */ }
    if (res.ok) return data ?? {};
    message = data?.error || text.trim() || res.statusText;
  } catch {
    message = 'Cannot reach Artus. Is it still running?';
  }
  if (!quiet) toast(message, 'error');
  return null;
}

export const fileUrl = path => '/api/file?path=' + encodeURIComponent(path);

export function applyState(st) {
  Object.assign(S, {version: st.version, home: st.home, config: st.config, providers: st.providers, efforts: st.efforts});
  S.config.allowedModels ||= [];
  changed();
}

/** Saves a partial configuration; the server merges it into the current one. */
export async function saveConfig(patch) {
  const st = await api('PUT', '/api/config', patch);
  if (st) applyState(st);
  return Boolean(st);
}

export async function loadModels(refresh = false) {
  const res = await api('GET', '/api/models' + (refresh ? '?refresh=1' : ''), undefined, {quiet: true});
  S.models = res?.models || S.models || [];
  S.modelErrors = res?.errors || {};
  for (const el of document.querySelectorAll('[data-model]')) el.textContent = modelName(el.dataset.model);
  changed();
}

export async function loadTasks() {
  const res = await api('GET', '/api/tasks', undefined, {quiet: true});
  if (!res) return;
  S.tasks = res.tasks;
  S.triggers = res.triggers;
  changed();
}

export async function loadBrowser() {
  const res = await api('GET', '/api/browser', undefined, {quiet: true});
  if (!res) return;
  S.browser = res.sessions || [];
  changed();
}

/** True until a provider has a key and a default model is chosen. */
export const needsSetup = () => !S.config.model || !S.providers.some(p => p.hasKey);

export const modelName = id => S.models?.find(m => m.id === id)?.name || (id || '').split('/').slice(1).join('/') || 'No model';

/** The run state as the status line words it. */
export function liveLabel(live) {
  switch (live?.state) {
    case 'tool': return 'tool: ' + live.detail;
    case 'retrying': return 'retrying: ' + live.detail;
    case 'waiting': return 'waiting for you';
    default: return live?.state || 'starting';
  }
}

// ---------------------------------------------------------------- formatting

const short = x => x >= 100 ? String(Math.round(x)) : x.toFixed(1).replace(/\.0$/, '');
export const fmtTokens = n => n >= 1e6 ? short(n / 1e6) + 'M' : n >= 1e3 ? short(n / 1e3) + 'k' : String(Math.round(n || 0));
export const fmtBytes = n => n >= 1 << 20 ? short(n / (1 << 20)) + ' MB' : n >= 1024 ? short(n / 1024) + ' kB' : n + ' B';

const pad = n => String(Math.floor(n)).padStart(2, '0');

export function fmtDuration(ms) {
  const s = Math.max(ms, 0) / 1000;
  if (s < 1) return Math.round(s * 1000) + 'ms';
  if (s < 10 && ms % 1000) return s.toFixed(1) + 's';
  if (s < 60) return Math.round(s) + 's';
  if (s < 3600) return `${Math.floor(s / 60)}m ${pad(s % 60)}s`;
  return `${Math.floor(s / 3600)}h ${pad(s % 3600 / 60)}m`;
}

/** A running clock in whole seconds, so it does not jitter between ticks. */
export const fmtElapsed = ms => ms < 1000 ? '0s' : fmtDuration(Math.floor(ms / 1000) * 1000);

/** A model's name that corrects itself once the model list has loaded. */
export const modelLabel = (id, cls = 'muted') => h('span.' + cls, {'data-model': id || ''}, modelName(id));

const sameDay = (a, b) => a.toDateString() === b.toDateString();
const clock = new Intl.DateTimeFormat(undefined, {hour: '2-digit', minute: '2-digit'});
const dayMonth = new Intl.DateTimeFormat(undefined, {day: 'numeric', month: 'short'});
const fullDate = new Intl.DateTimeFormat(undefined, {day: 'numeric', month: 'short', year: 'numeric'});

/** A compact timestamp: the time today, otherwise the date (and time this year). */
export function fmtDate(value) {
  const d = new Date(value), now = new Date();
  if (isNaN(d) || d.getFullYear() < 2000) return '';
  if (sameDay(d, now)) return clock.format(d);
  if (d.getFullYear() === now.getFullYear()) return `${dayMonth.format(d)}, ${clock.format(d)}`;
  return fullDate.format(d);
}

/** How far away a future moment is: "in 5m", "in 3h", "in 2d". */
export function fmtUntil(value) {
  const min = (new Date(value) - Date.now()) / 60000;
  if (isNaN(min)) return '';
  if (min < 1) return 'due now';
  if (min < 60) return `in ${Math.round(min)}m`;
  if (min < 48 * 60) return `in ${Math.round(min / 60)}h`;
  return `in ${Math.round(min / 1440)}d`;
}

// ---------------------------------------------------------------- feedback

export function toast(text, kind = 'info') {
  const box = document.getElementById('toasts');
  if ([...box.children].some(t => t.textContent === text)) return;
  const el = h('button.toast.' + kind, {type: 'button', title: 'Dismiss', onclick: () => el.remove()}, text);
  box.append(el);
  setTimeout(() => el.remove(), kind === 'error' ? 9000 : 4000);
}

export async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
  } catch { // no clipboard API on a plain-HTTP origin: fall back to a selection
    const ta = h('textarea.offscreen', {value: text});
    document.body.append(ta);
    ta.select();
    document.execCommand('copy');
    ta.remove();
  }
}

/** A small button that copies text() and confirms it briefly. */
export function copyButton(text, label = 'Copy') {
  const btn = h('button.ghost', {type: 'button', onclick: async () => {
    await copyText(text());
    btn.replaceChildren(icon('check', 14), 'Copied');
    setTimeout(() => btn.replaceChildren(icon('copy', 14), label), 1400);
  }}, icon('copy', 14), label);
  return btn;
}

// ---------------------------------------------------------------- overlays

const layers = []; // open overlays, innermost last: Esc closes the top one

export function pushLayer(close) {
  layers.push(close);
  return () => { const i = layers.indexOf(close); if (i >= 0) layers.splice(i, 1); };
}

export function closeTopLayer() {
  const close = layers.pop();
  if (close) close();
  return Boolean(close);
}

const openPopovers = new WeakMap(); // anchor → close, so a second click toggles

/** Shows content next to anchor until the user clicks elsewhere or presses Esc. */
export function popover(anchor, content, {align = 'start'} = {}) {
  if (openPopovers.has(anchor)) { openPopovers.get(anchor)(); return null; }
  const el = h('div.popover', content);
  (anchor.closest('dialog') || document.body).append(el); // a modal makes everything outside it inert
  const r = anchor.getBoundingClientRect();
  const left = Math.max(8, Math.min(align === 'end' ? r.right - el.offsetWidth : r.left, innerWidth - el.offsetWidth - 8));
  el.style.setProperty('left', left + 'px');
  if (r.bottom + el.offsetHeight + 12 <= innerHeight || r.top < innerHeight / 2) {
    el.style.setProperty('top', r.bottom + 6 + 'px');
    el.style.setProperty('max-height', innerHeight - r.bottom - 14 + 'px');
  } else {
    el.style.setProperty('bottom', innerHeight - r.top + 6 + 'px');
    el.style.setProperty('max-height', r.top - 14 + 'px');
  }
  const outside = e => { if (!el.contains(e.target) && !anchor.contains(e.target)) close(); };
  const drop = pushLayer(() => close());
  function close() {
    el.remove();
    drop();
    openPopovers.delete(anchor);
    removeEventListener('pointerdown', outside, true);
    if (document.activeElement === document.body) anchor.focus();
  }
  addEventListener('pointerdown', outside, true);
  openPopovers.set(anchor, close);
  return close;
}

/** A menu of {label, icon, onclick, danger, checked} items. */
export function menu(anchor, items, opts) {
  const list = h('div.menu', {role: 'menu', onkeydown: e => {
    const step = {ArrowDown: 1, ArrowUp: -1}[e.key];
    if (!step) return;
    e.preventDefault();
    const all = [...list.children];
    all[(all.indexOf(document.activeElement) + step + all.length) % all.length].focus();
  }}, items.filter(Boolean).map(item => h('button' + (item.danger ? '.danger' : ''), {
    type: 'button', role: 'menuitem', onclick: () => { close(); item.onclick(); },
  }, item.icon && icon(item.icon, 16), h('span', item.label), item.checked && icon('check', 14))));
  const close = popover(anchor, list, opts);
  if (close) list.firstElementChild?.focus();
}

/** A modal with Cancel and one action; resolves true when the action was chosen. */
export function modal(title, body, {action = 'OK', danger = false} = {}) {
  return new Promise(resolve => {
    const done = ok => { dlg.close(); dlg.remove(); resolve(ok); };
    const dlg = h('dialog.dialog', {'aria-label': title},
      h('form', {onsubmit: e => { e.preventDefault(); done(true); }},
        h('h2', title), body,
        h('div.dialog-actions',
          h('button.btn', {type: 'button', onclick: () => done(false)}, 'Cancel'),
          h('button.btn' + (danger ? '.danger' : '.primary'), {type: 'submit'}, action))));
    dlg.addEventListener('cancel', e => { e.preventDefault(); done(false); });
    dlg.addEventListener('pointerdown', e => { if (e.target === dlg) done(false); });
    document.body.append(dlg);
    dlg.showModal();
  });
}

export const confirmDialog = (title, text, action) => modal(title, h('p', text), {action, danger: true});

export async function promptDialog(title, label, value = '', action = 'Save') {
  const input = h('input.input', {value, required: true, 'aria-label': label, placeholder: label, autocomplete: 'off'});
  return (await modal(title, input, {action})) ? input.value.trim() : null;
}

export function lightbox(src, alt) {
  const dlg = h('dialog.lightbox', {'aria-label': alt || 'Image', onclick: () => dlg.close(), onclose: () => dlg.remove()},
    h('img', {src, alt: alt || ''}));
  document.body.append(dlg);
  dlg.showModal();
}

/** Collapses the sidebar on a wide screen, opens it as a drawer on a narrow one. */
export function toggleSidebar() {
  const narrow = matchMedia('(max-width: 760px)').matches;
  document.body.classList.toggle(narrow ? 'drawer-open' : 'side-collapsed');
}

/** The header every page starts with: sidebar toggle, title, then actions. */
export function pageHead(title, ...actions) {
  return h('header.page-head',
    h('button.icon-btn', {type: 'button', title: 'Toggle sidebar', 'aria-label': 'Toggle sidebar', onclick: toggleSidebar}, icon('sidebar')),
    typeof title === 'string' ? h('h1', title) : title,
    h('div.head-actions', actions));
}
