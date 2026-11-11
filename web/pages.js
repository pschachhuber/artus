// pages.js — Tasks, Topics and Skills, plus the two widgets other views
// share: the model picker and the Markdown file pane.

import {S, bus, watch, h, fill, icon, api, loadTasks, toast, popover, modal, confirmDialog, promptDialog, pageHead, modelName,
  fmtTokens, fmtDate, fmtUntil} from './lib.js';
import {renderMarkdown} from './markdown.js';

const enc = path => path.split('/').map(encodeURIComponent).join('/');
const fsFile = path => '/api/fs/file?path=' + encodeURIComponent(path);

// ---------------------------------------------------------------- shared widgets

/**
 * A searchable list of the available models, grouped by provider. With
 * `multi` it stays open and reports the whole selection on every change.
 */
export function modelPicker(anchor, {selected = [], onpick, multi = false}) {
  const chosen = new Set(selected.filter(Boolean));
  const search = h('input.input', {type: 'search', placeholder: 'Search models', 'aria-label': 'Search models'});
  const list = h('div.model-list', {role: 'listbox', 'aria-label': 'Models'});
  const option = m => h('button.option', {type: 'button', role: 'option', 'aria-selected': String(chosen.has(m.id)), onclick: () => {
    if (!multi) { close(); onpick(m.id); return; }
    if (!chosen.delete(m.id)) chosen.add(m.id);
    onpick([...chosen]);
    render();
  }}, h('span.option-name', m.name || m.id), h('span.muted', m.id.split('/').slice(1).join('/')),
    m.window ? h('span.muted', fmtTokens(m.window)) : null, chosen.has(m.id) ? icon('check', 14) : null);
  const render = () => {
    const q = search.value.trim().toLowerCase();
    const groups = Map.groupBy((S.models || []).filter(m => `${m.id} ${m.name}`.toLowerCase().includes(q)), m => m.id.split('/')[0]);
    const empty = S.models === null ? 'Loading models…' : q ? 'No model matches.' : 'No models yet. Add a provider key in Settings.';
    list.replaceChildren(
      ...groups.size ? [...groups].flatMap(([provider, models]) => [h('div.group-label', provider), ...models.map(option)]) : [h('p.muted', empty)],
      ...Object.entries(S.modelErrors).map(([provider, error]) => h('p.error-text', `${provider}: ${error}`)));
  };
  const options = () => [...list.querySelectorAll('.option')];
  search.oninput = render;
  search.onkeydown = e => {
    if (e.key === 'Enter') { e.preventDefault(); options()[0]?.click(); }
    if (e.key === 'ArrowDown') { e.preventDefault(); options()[0]?.focus(); }
  };
  list.onkeydown = e => {
    const step = {ArrowDown: 1, ArrowUp: -1}[e.key];
    if (!step) return;
    e.preventDefault();
    const all = options();
    (all[all.indexOf(document.activeElement) + step] || search).focus();
  };
  render();
  const close = popover(anchor, h('div.model-picker', search, list));
  if (close) search.focus();
}

/** Markdown as a document; a leading front-matter block is shown as its own small box. */
function markdownDoc(text) {
  const front = /^---\n([\s\S]*?)\n---\n?/.exec(text);
  return h('div.prose', front ? h('pre.frontmatter', front[1]) : null, renderMarkdown(front ? text.slice(front[0].length) : text));
}

/**
 * A text file from the home: rendered, with an Edit toggle that swaps in a
 * textarea. With `always` it is an editor that never leaves edit mode. The
 * buttons go into `bar`, which the caller may place in its own header.
 */
export function docPane(path, {always = false, ondelete, empty = 'This file is empty.', bar} = {}) {
  const body = h('div');
  const el = h('div.doc', bar ? null : bar = h('div.doc-bar'), body);
  let content = '', editing = always, area = null;
  const button = (label, name, onclick, cls = '') => h('button.btn' + cls, {type: 'button', onclick}, name ? icon(name, 15) : null, label);
  const save = async () => {
    if (!(await api('PUT', fsFile(path), area.value))) return;
    content = area.value;
    editing = always;
    toast('Saved');
    render();
  };
  const render = () => {
    if (editing) {
      area = h('textarea.editor', {value: content, spellcheck: false, 'aria-label': 'Content of ' + path});
      bar.replaceChildren(button('Save', '', save, '.primary'), ...always ? [] : [button('Cancel', '', () => { editing = false; render(); })]);
      body.replaceChildren(area);
    } else {
      bar.replaceChildren(button('Edit', 'edit', () => { editing = true; render(); }), ...ondelete ? [button('Delete', 'trash', ondelete)] : []);
      body.replaceChildren(!content.trim() ? h('p.muted', empty) : path.endsWith('.md') ? markdownDoc(content) : h('pre.file-text', {tabindex: 0}, content));
    }
  };
  const load = async () => {
    const res = await api('GET', fsFile(path));
    if (!res) return;
    content = res.content;
    render();
  };
  load();
  // A change on disk is shown unless the user has unsaved edits in the textarea.
  return {el, reload: () => { if (!editing || area.value === content) load(); }};
}

const statusChip = status => h('span.state.' + status, status);

// ---------------------------------------------------------------- tasks

export function tasksView(root, id) {
  const abort = new AbortController();
  const el = h('section.page');
  root.append(el);
  const page = id ? taskPage(el, id) : taskList(el);
  watch(abort.signal, () => JSON.stringify([S.tasks, S.triggers, [...S.running]]), page);
  bus.addEventListener('server', e => { if (e.detail.type === 'changes') loadTasks(); }, {signal: abort.signal}); // NOTES.md may have changed
  bus.addEventListener('reload', loadTasks, {signal: abort.signal});
  loadTasks();
  document.title = 'Tasks — Artus';
  return {destroy: () => abort.abort()};
}

const removeTrigger = t => api('DELETE', '/api/triggers/' + encodeURIComponent(t.id));

function triggerRow(t, withTarget) {
  const [kind, target] = t.target.split(':');
  const name = kind === 'task' ? S.tasks.find(x => x.id === target)?.name || target : S.chats.get(target)?.title || 'A chat';
  return h('li.row',
    h('span.row-main', h('strong', t.text),
      withTarget ? h('a', {href: kind === 'task' ? '#/tasks/' + enc(target) : '#/chat/' + target}, icon(kind === 'task' ? 'clock' : 'chat', 14), name) : null,
      t.note ? h('span.muted', t.note) : null),
    h('span.muted.nowrap', {title: new Date(t.next).toLocaleString()}, `${fmtDate(t.next)} · ${fmtUntil(t.next)}`),
    h('button.icon-btn', {type: 'button', title: 'Remove this trigger', 'aria-label': `Remove trigger ${t.text}`, onclick: () => removeTrigger(t)}, icon('x', 16)));
}

function taskList(el) {
  const cards = h('div.cards'), upcoming = h('ul.rows');
  fill(el, pageHead('Tasks', h('button.btn.primary', {type: 'button', onclick: newTask}, icon('plus', 16), 'New task')),
    h('div.page-body',
      h('p.lede', 'Standing jobs Artus runs on its own, on a schedule it keeps itself. Each run starts fresh from the task description and its notes.'),
      cards, h('h2', 'Upcoming'), upcoming));
  return () => {
    cards.replaceChildren(...S.tasks.map(t => {
      const next = t.triggers.map(x => x.next).sort()[0];
      return h('a.card', {href: '#/tasks/' + enc(t.id)},
        h('div.card-top', h('h2', t.name), statusChip(t.status),
          t.unread ? h('span.badge', {title: 'Unread runs'}, String(t.unread)) : null),
        h('p.muted.clamp', t.description),
        h('div.card-foot',
          t.running ? h('span.running', h('span.pulse'), 'running') : null,
          h('span', icon('clock', 14), t.triggers.length ? t.triggers.map(x => x.text).join(', ') : 'no schedule'),
          next && t.status === 'active' ? h('span.muted', 'next ' + fmtUntil(next)) : null));
    }));
    if (!S.tasks.length) cards.replaceChildren(h('p.muted', 'No tasks yet.'));
    upcoming.replaceChildren(...S.triggers.map(t => triggerRow(t, true)));
    if (!S.triggers.length) upcoming.replaceChildren(h('li.muted', 'Nothing is scheduled.'));
  };
}

async function newTask() {
  let model = '';
  const name = h('input.input', {placeholder: 'Name (optional)', 'aria-label': 'Name', autocomplete: 'off'});
  const description = h('textarea.input', {rows: 6, required: true, 'aria-label': 'Description',
    placeholder: 'What should Artus do, and when? For example: every weekday at 7:00, summarise what changed in my repositories.'});
  const modelBtn = h('button.pill', {type: 'button', onclick: () => modelPicker(modelBtn, {selected: [model], onpick: picked => {
    model = picked;
    modelBtn.replaceChildren(modelName(picked), icon('down', 14));
  }})}, 'Default model', icon('down', 14));
  const effort = h('select.input', {'aria-label': 'Effort'}, h('option', {value: ''}, 'Default effort'), S.efforts.map(e => h('option', e)));
  const form = h('div.form', name, description, h('div.form-row', modelBtn, effort),
    h('p.muted', 'The first run starts right away: the agent plans the work and sets its own schedule.'));
  if (!(await modal('New task', form, {action: 'Create and run'}))) return;
  const res = await api('POST', '/api/tasks', {name: name.value, description: description.value, model, effort: effort.value});
  if (res) location.hash = '#/chat/' + res.chat;
}

function taskPage(el, id) {
  const path = '/api/tasks/' + encodeURIComponent(id);
  const head = h('div.task-head'), descBar = h('div.doc-bar'), descBox = h('div.doc'), rest = h('div');
  let editing = false;
  fill(el, pageHead(h('nav.crumb', {'aria-label': 'Breadcrumb'}, h('a', {href: '#/tasks'}, icon('back', 14), 'Tasks'))),
    h('div.page-body', head, h('div.section-head', h('h2', 'What it does'), descBar), descBox, rest));

  const patch = body => api('PATCH', path, body);
  const run = async () => {
    const res = await api('POST', path + '/run');
    if (res) location.hash = '#/chat/' + res.chat;
  };
  const remove = async t => {
    if (!(await confirmDialog(`Delete “${t.name}”?`, 'The task, its notes, its triggers and the transcripts of its runs are removed.', 'Delete task'))) return;
    if (await api('DELETE', path)) location.hash = '#/tasks';
  };
  const rename = async t => {
    const name = await promptDialog('Rename task', 'Name', t.name);
    if (name) patch({name});
  };
  const describe = t => {
    if (editing) return; // keep what the user is typing
    descBar.replaceChildren(h('button.btn', {type: 'button', onclick: () => edit(t)}, icon('edit', 15), 'Edit'));
    descBox.replaceChildren(h('div.prose', renderMarkdown(t.description)));
  };
  const edit = t => {
    editing = true;
    const area = h('textarea.editor', {value: t.description, 'aria-label': 'Task description'});
    const done = () => { editing = false; describe(S.tasks.find(x => x.id === id) || t); };
    descBar.replaceChildren(
      h('button.btn.primary', {type: 'button', onclick: async () => { if (await patch({description: area.value})) done(); }}, 'Save'),
      h('button.btn', {type: 'button', onclick: done}, 'Cancel'));
    descBox.replaceChildren(area);
    area.focus();
  };

  return () => {
    const t = S.tasks.find(x => x.id === id);
    if (!t) { head.replaceChildren(h('p.muted', 'This task does not exist (any more).')); descBar.replaceChildren(); descBox.replaceChildren(); rest.replaceChildren(); return; }
    document.title = t.name + ' — Artus';
    head.replaceChildren(
      h('div.task-title', h('h1', t.name), statusChip(t.status), t.running ? h('span.running', h('span.pulse'), 'running') : null),
      h('div.task-actions',
        t.status === 'active' ? h('button.btn.primary', {type: 'button', disabled: t.running, onclick: run}, icon('play', 15), 'Run now') : null,
        t.status === 'active' ? h('button.btn', {type: 'button', onclick: () => patch({status: 'paused'})}, icon('pause', 15), 'Pause') : null,
        t.status !== 'active' ? h('button.btn', {type: 'button', onclick: () => patch({status: 'active'})}, icon('play', 15), t.status === 'done' ? 'Reactivate' : 'Resume') : null,
        t.status !== 'done' ? h('button.btn', {type: 'button', onclick: () => patch({status: 'done'})}, icon('check', 15), 'Mark done') : null,
        h('button.btn', {type: 'button', onclick: () => rename(t)}, icon('edit', 15), 'Rename'),
        h('button.btn', {type: 'button', onclick: () => remove(t)}, icon('trash', 15), 'Delete')));
    describe(t);
    rest.replaceChildren(
      h('h2', 'Schedule'),
      h('ul.rows', t.triggers.length ? t.triggers.map(x => triggerRow(x, false))
        : h('li.muted', 'No triggers. The agent sets them with its schedule tool; say when it should run in the description.')),
      h('h2', 'Notes'),
      t.notes.trim() ? h('div.prose.boxed', renderMarkdown(t.notes)) : h('p.muted', 'No notes yet. The agent keeps NOTES.md here between runs.'),
      h('h2', 'Runs'),
      h('ul.rows', t.runs.length ? t.runs.map(r => h('li.row',
        h('a.row-main', {href: '#/chat/' + r.id},
          S.running.has(r.id) ? h('span.pulse') : r.unread ? h('span.dot', {title: 'Unread'}) : null, h('strong', r.title)),
        h('span.muted.nowrap', `${fmtTokens(r.tokensIn + r.tokensOut)} tokens`),
        h('span.muted.nowrap', fmtDate(r.updated)))) : h('li.muted', 'It has not run yet.')));
  };
}

// ---------------------------------------------------------------- topics and skills

export const topicsView = (root, id) => filesView(root, id, 'topics');
export const skillsView = (root, id) => filesView(root, id, 'skills');

const INTRO = {
  topics: 'Notes the agent keeps about subjects that come up again. It reads their one-line index in every chat and opens a note when it is relevant.',
  skills: 'Procedures the agent has written down for itself: a SKILL.md with instructions, sometimes with scripts next to it.',
};

function filesView(root, selected, kind) {
  const abort = new AbortController();
  const topics = kind === 'topics', title = topics ? 'Topics' : 'Skills';
  let entries = [], pane = null;
  // A topic is addressed by its name, a skill by its folder.
  const keyOf = e => topics ? e.name : e.path.split('/')[1];
  const search = h('input.input', {type: 'search', placeholder: `Search ${kind}`, 'aria-label': `Search ${kind}`, oninput: () => renderList()});
  const list = h('nav.file-list', {'aria-label': title});
  const detail = h('div.split-main');
  const el = h('section.page.split',
    pageHead(title, topics ? h('button.btn.primary', {type: 'button', onclick: () => newTopic()}, icon('plus', 16), 'New topic') : null),
    h('div.split-body', h('div.split-side', search, list), detail));
  root.append(el);

  function renderList() {
    const q = search.value.trim().toLowerCase();
    const shown = entries.filter(e => `${e.name} ${e.description}`.toLowerCase().includes(q));
    list.replaceChildren(...shown.map(e => h('a.file-row', {href: `#/${kind}/${enc(keyOf(e))}`, 'aria-current': keyOf(e) === selected ? 'page' : null},
      h('strong', e.name), h('span.muted.clamp', e.description))));
    if (!shown.length) list.replaceChildren(h('p.muted', entries.length ? 'Nothing matches.' : `No ${kind} yet.`));
  }

  function renderDetail() {
    const entry = entries.find(e => keyOf(e) === selected);
    el.classList.toggle('has-selection', Boolean(selected));
    document.title = `${entry?.name || title} — Artus`;
    pane = null;
    if (!entry) {
      detail.replaceChildren(h('div.empty', icon(topics ? 'book' : 'spark', 28), h('p', selected ? `There is no such entry in ${kind}.` : INTRO[kind])));
      return;
    }
    const remove = async () => {
      const what = topics ? entry.path : 'skills/' + selected;
      if (!(await confirmDialog(`Delete “${entry.name}”?`, `${what} is removed. You can bring it back from Settings › Changes.`, 'Delete'))) return;
      if (await api('DELETE', fsFile(what))) location.hash = '#/' + kind;
    };
    const bar = h('div.doc-bar'), where = h('code.muted'), files = h('div.file-strip'), holder = h('div');
    const open = path => {
      where.textContent = path;
      pane = docPane(path, {bar, ondelete: path === entry.path ? remove : null});
      holder.replaceChildren(pane.el);
      for (const pill of files.children) pill.setAttribute('aria-pressed', String(pill.title === path));
    };
    // A skill is a folder: list what is next to its SKILL.md.
    const browse = async dir => {
      const res = await api('GET', '/api/fs?path=' + encodeURIComponent(dir));
      if (!res) return;
      const up = dir === 'skills/' + selected ? [] : [h('button.pill', {type: 'button', onclick: () => browse(dir.slice(0, dir.lastIndexOf('/')))}, icon('back', 14), 'Up')];
      files.replaceChildren(...up, ...res.entries.map(f => h('button.pill', {type: 'button', title: f.path, 'aria-pressed': String(f.path === where.textContent),
        onclick: () => f.dir ? browse(f.path) : open(f.path)}, icon(f.dir ? 'cube' : 'file', 14), f.name + (f.dir ? '/' : ''))));
    };
    detail.replaceChildren(
      h('div.doc-head', h('a.ghost.only-narrow', {href: '#/' + kind}, icon('back', 14), title), h('h2', entry.name), where, bar),
      files, holder);
    if (!topics) browse('skills/' + selected);
    open(entry.path);
  }

  async function newTopic() {
    const name = (await promptDialog('New topic', 'Name, for example travel/japan', '', 'Create') || '').replace(/\.md$/, '').replace(/^\/+|\/+$/g, '');
    if (!name || name.split('/').includes('..')) return;
    if (!(await api('PUT', fsFile(`topics/${name}.md`), `# ${name.split('/').pop()}\n`))) return;
    await load();
    location.hash = '#/topics/' + enc(name);
  }

  async function load() {
    const res = await api('GET', '/api/fs');
    if (!res) return;
    const first = !entries.length;
    entries = res[kind] || [];
    renderList();
    if (first || !pane) renderDetail(); else pane.reload();
  }

  bus.addEventListener('server', e => { if (e.detail.type === 'changes') load(); }, {signal: abort.signal});
  bus.addEventListener('reload', load, {signal: abort.signal});
  renderDetail();
  load();
  return {
    destroy: () => abort.abort(),
    update(next) { selected = next; renderList(); renderDetail(); return true; },
  };
}
