// app.js — start-up, the live event stream, routing, the sidebar and the
// global shortcuts. All state lives on the server: this file fetches a
// snapshot, then keeps it current from the event stream.

import {S, bus, changed, h, icon, api, applyState, loadModels, loadTasks, loadBrowser, toast, menu, confirmDialog, promptDialog,
  closeTopLayer, toggleSidebar} from './lib.js';
import {chatView} from './chat.js';
import {tasksView, topicsView, skillsView} from './pages.js';
import {settingsView} from './settings.js';

const VIEWS = {chat: chatView, tasks: tasksView, topics: topicsView, skills: skillsView, settings: settingsView};
const main = document.getElementById('main');
let view = null;   // the mounted page: {destroy(), update?(id)}
let stream = null; // the EventSource
let online = true;

// ---------------------------------------------------------------- server state

/**
 * Takes a fresh snapshot and opens the stream right behind it. Views load
 * their own data afterwards and apply only events newer than what they got.
 */
async function connect() {
  const state = await api('GET', '/api/state', undefined, {quiet: true});
  const chats = state && await api('GET', '/api/chats', undefined, {quiet: true});
  if (!chats) { setOnline(false); setTimeout(reconnect, 2000); return; }
  applyState(state);
  S.running = new Set(state.running);
  S.live = new Map([...S.live].filter(([id]) => S.running.has(id)));
  S.chats = new Map(chats.chats.map(c => [c.id, c]));
  loadTasks();
  loadBrowser();
  loadModels();
  stream = new EventSource('/api/events?since=' + state.seq);
  stream.onmessage = e => onServerEvent(JSON.parse(e.data));
  stream.addEventListener('ready', () => setOnline(true));
  stream.addEventListener('resync', reconnect); // we fell too far behind: start over
  stream.onerror = () => { setOnline(false); stream.close(); setTimeout(reconnect, 1500); };
  changed();
}

async function reconnect() {
  stream?.close();
  await connect();
  if (stream?.readyState !== EventSource.CLOSED) bus.dispatchEvent(new Event('reload'));
}

function setOnline(now) {
  if (online !== now) { online = now; changed(); }
}

let stateTimer = 0, taskTimer = 0;
const soon = (timer, fn) => { clearTimeout(timer); return setTimeout(fn, 150); };
const refreshState = () => { stateTimer = soon(stateTimer, async () => { const st = await api('GET', '/api/state', undefined, {quiet: true}); if (st) applyState(st); }); };
const refreshTasks = () => { taskTimer = soon(taskTimer, loadTasks); };

/** Folds one event into the shared state, then hands it to the views. */
function onServerEvent(ev) {
  switch (ev.type) {
    case 'chat':
      S.chats.set(ev.data.id, ev.data);
      if (ev.data.kind === 'task') refreshTasks();
      break;
    case 'chat_deleted':
      S.chats.delete(ev.chat);
      S.running.delete(ev.chat);
      S.live.delete(ev.chat);
      if (S.route.page === 'chat' && S.route.id === ev.chat) location.hash = '#/';
      break;
    case 'live':
      if (ev.data) { S.live.set(ev.chat, ev.data); S.running.add(ev.chat); }
      else { S.live.delete(ev.chat); S.running.delete(ev.chat); }
      if (S.chats.get(ev.chat)?.kind === 'task') refreshTasks();
      break;
    case 'delta': { // the server does not announce "writing": the first text of a reply does
      const live = S.live.get(ev.chat);
      if (live && ev.data.kind === 'text' && live.state === 'thinking') live.state = 'writing';
      break;
    }
    case 'alert': toast(String(ev.data), 'error'); break;
    case 'tasks': case 'schedule': refreshTasks(); break;
    case 'config': refreshState(); break;
    case 'secrets': refreshState(); loadModels(true); break;
    case 'browser':
      loadBrowser();
      if (ev.data?.type === 'handoff' && S.route.page !== 'chat') toast('The agent is waiting for you in the browser. Open a chat to help.');
      break;
  }
  if (['chat', 'chat_deleted', 'live'].includes(ev.type)) changed();
  bus.dispatchEvent(new CustomEvent('server', {detail: ev}));
}

// ---------------------------------------------------------------- routing

/** The page and id a hash names: "#/tasks/tea" is the tasks page with id "tea". */
function parseHash() {
  const parts = location.hash.replace(/^#\/?/, '').split('/').filter(Boolean).map(p => { try { return decodeURIComponent(p); } catch { return p; } });
  return {page: VIEWS[parts[0]] ? parts[0] : 'chat', id: parts.slice(1).join('/')};
}

/** Mounts the view for the current hash, or lets the mounted one switch to the new id. */
function route() {
  while (closeTopLayer());
  const {page, id} = parseHash();
  const same = view && S.route.page === page;
  S.route = {page, id};
  if (!(same && view.update?.(id))) {
    view?.destroy();
    main.replaceChildren();
    view = VIEWS[page](main, id);
  }
  document.body.classList.remove('drawer-open');
  changed();
}

/** Navigates at once, without waiting for the hashchange event; the same hash starts the view afresh. */
function go(hash) {
  if (location.hash !== hash) location.hash = hash;
  route();
}

/** A hashchange that go() or a view (binding itself to a new chat) already acted on needs nothing more. */
function onHashChange() {
  const {page, id} = parseHash();
  if (page !== S.route.page || id !== S.route.id) route();
}

// ---------------------------------------------------------------- sidebar

const THEMES = {auto: 'Theme: follows the system', light: 'Theme: light', dark: 'Theme: dark'};

function theme() {
  const t = document.documentElement.dataset.theme;
  return THEMES[t] ? t : 'auto';
}

function cycleTheme() {
  const next = {auto: 'light', light: 'dark', dark: 'auto'}[theme()];
  if (next === 'auto') delete document.documentElement.dataset.theme;
  else document.documentElement.dataset.theme = next;
  try { next === 'auto' ? localStorage.removeItem('artus-theme') : localStorage.setItem('artus-theme', next); } catch { /* not remembered */ }
  changed();
}

/** Chats under "Today", "Yesterday", "Earlier this week", then by month. */
function dateGroup(value) {
  const day = 86400000, d = new Date(value);
  const today = new Date().setHours(0, 0, 0, 0);
  if (d >= today) return 'Today';
  if (d >= today - day) return 'Yesterday';
  if (d >= today - 6 * day) return 'This week';
  return d.toLocaleDateString(undefined, {month: 'long', year: 'numeric'});
}

function chatMenu(anchor, chat) {
  menu(anchor, [
    {label: 'Rename', icon: 'edit', onclick: async () => {
      const title = await promptDialog('Rename chat', 'Title', chat.title);
      if (title) api('PATCH', '/api/chats/' + chat.id, {title});
    }},
    {label: chat.pinned ? 'Unpin' : 'Pin', icon: 'pin', onclick: () => api('PATCH', '/api/chats/' + chat.id, {pinned: !chat.pinned})},
    {label: 'Delete', icon: 'trash', danger: true, onclick: async () => {
      if (await confirmDialog('Delete this chat?', `“${chat.title || 'New chat'}” and its attachments are removed for good.`, 'Delete chat')) {
        api('DELETE', '/api/chats/' + chat.id);
      }
    }},
  ], {align: 'end'});
}

function buildSidebar() {
  const search = h('input.input', {id: 'search', type: 'search', placeholder: 'Search chats', 'aria-label': 'Search chats', oninput: () => renderChats()});
  const list = h('nav.chat-list', {'aria-label': 'Chats'});
  const nav = h('nav.side-nav', {'aria-label': 'Sections'});
  const themeBtn = h('button.icon-btn', {type: 'button', onclick: cycleTheme});
  const offline = h('span.offline', {role: 'status'});

  function chatRow(chat) {
    const active = S.route.page === 'chat' && S.route.id === chat.id;
    const more = h('button.icon-btn.more', {type: 'button', title: 'Chat options', 'aria-label': `Options for ${chat.title || 'New chat'}`,
      onclick: e => { e.preventDefault(); chatMenu(more, chat); }}, icon('more', 16));
    return h('div.chat-row' + (active ? '.active' : ''),
      h('a', {href: '#/chat/' + chat.id, 'aria-current': active ? 'page' : null},
        S.running.has(chat.id) ? h('span.pulse', {title: 'Running'}) : chat.unread ? h('span.dot', {title: 'Unread'}) : null,
        h('span.chat-name', chat.title || 'New chat')),
      more);
  }

  function renderChats() {
    const q = search.value.trim().toLowerCase();
    const chats = [...S.chats.values()].filter(c => c.kind === 'chat' && (c.title || '').toLowerCase().includes(q))
      .sort((a, b) => b.updated.localeCompare(a.updated));
    const groups = Map.groupBy(chats, c => c.pinned ? 'Pinned' : dateGroup(c.updated));
    const order = [...groups.keys()].sort((a, b) => (b === 'Pinned') - (a === 'Pinned')); // pinned first, the rest stay newest first
    list.replaceChildren(...order.flatMap(name => [h('h2.group-label', name), ...groups.get(name).map(chatRow)]));
    if (!chats.length) list.replaceChildren(h('p.muted', q ? 'No chat matches.' : 'No chats yet.'));
  }

  function render() {
    renderChats();
    const unread = S.tasks.reduce((n, t) => n + t.unread, 0);
    const item = (page, name, label, badge) => h('a', {href: '#/' + page, 'aria-current': S.route.page === page ? 'page' : null},
      icon(name), h('span', label), badge ? h('span.badge', {title: `${badge} unread task runs`}, String(badge)) : null);
    nav.replaceChildren(item('tasks', 'clock', 'Tasks', unread), item('topics', 'book', 'Topics'), item('skills', 'spark', 'Skills'), item('settings', 'sliders', 'Settings'));
    const t = theme();
    themeBtn.replaceChildren(icon({auto: 'auto', light: 'sun', dark: 'moon'}[t]));
    themeBtn.title = `${THEMES[t]}. Click to change.`;
    themeBtn.setAttribute('aria-label', themeBtn.title);
    offline.textContent = online ? '' : 'Reconnecting…';
  }

  let frame = 0;
  bus.addEventListener('state', () => { frame ||= requestAnimationFrame(() => { frame = 0; render(); }); });
  document.getElementById('sidebar').append(
    h('div.side-head', h('span.brand', 'Artus'),
      h('button.icon-btn', {type: 'button', title: 'Hide sidebar', 'aria-label': 'Hide sidebar', onclick: toggleSidebar}, icon('sidebar'))),
    h('a.btn.new-chat', {href: '#/', onclick: e => { e.preventDefault(); go('#/'); }}, icon('plus', 16), 'New chat', h('kbd', 'Ctrl K')),
    h('div.side-search', icon('search', 15), search), list, nav,
    h('div.side-foot', themeBtn, offline, h('span.muted', S.version)));
  render();
  return search;
}

// ---------------------------------------------------------------- start

async function start() {
  await connect();
  const search = buildSidebar();
  const scrim = document.getElementById('scrim');
  scrim.addEventListener('click', () => document.body.classList.remove('drawer-open'));

  addEventListener('hashchange', onHashChange);
  addEventListener('keydown', e => {
    const typing = e.target.matches?.('input, textarea, select');
    if ((e.ctrlKey || e.metaKey) && !e.shiftKey && !e.altKey && e.key.toLowerCase() === 'k') {
      e.preventDefault();
      go('#/');
    } else if (e.key === 'Escape' && !document.querySelector('dialog[open]')) {
      if (closeTopLayer()) e.preventDefault();
      else document.body.classList.remove('drawer-open');
    } else if (e.key === '/' && !typing && !e.ctrlKey && !e.metaKey && !e.altKey) {
      e.preventDefault();
      if (matchMedia('(max-width: 760px)').matches) document.body.classList.add('drawer-open');
      else document.body.classList.remove('side-collapsed');
      search.focus();
    }
  });
  route();
}

start();
