// chat.js — the chat view: header, transcript, status line, asks, composer,
// and the two side panels (context inspector and browser live view).

import {S, bus, chevron, watch, h, icon, api, fileUrl, needsSetup, modelName, liveLabel, menu, pushLayer, pageHead, toast,
  fmtTokens, fmtBytes, fmtElapsed} from './lib.js';
import {Transcript, toolSummary} from './transcript.js';
import {modelPicker} from './pages.js';
import {setupCard} from './settings.js';

let carried = null; // a draft handed from a chat to the fork that continues it

const compactAt = window => {
  const c = S.config.context;
  return Math.min(window * c.compactAt, c.compactMaxTokens || Infinity);
};

/** A track showing used / window with a tick where compaction starts. */
function meterTrack(used, window) {
  const track = h('span.meter-track', h('span.meter-fill'), h('span.meter-tick'));
  track.style.setProperty('--fill', Math.min(used / window, 1) * 100 + '%');
  track.style.setProperty('--tick', Math.min(compactAt(window) / window, 1) * 100 + '%');
  track.classList.toggle('over', used > compactAt(window));
  return track;
}

export function chatView(root, id) { return new ChatView(root, id); }

class ChatView {
  constructor(root, id) {
    this.root = root;
    this.id = id || null;
    this.files = [];   // attachments in the composer: {name, size, type, url, att}
    this.model = S.config.model;   // used until the chat exists
    this.effort = S.config.effort;
    this.stick = true; // follow new content while the user is at the bottom
    this.abort = new AbortController();
    const {signal} = this.abort;
    this.build();
    bus.addEventListener('server', e => this.onEvent(e.detail), {signal});
    bus.addEventListener('reload', () => this.load(), {signal});
    watch(signal, () => JSON.stringify([needsSetup(), S.config, S.models?.length, S.browser, this.meta]), () => this.refresh());
    this.timer = setInterval(() => this.tick(), 250);
    this.load();
  }

  get meta() { return this.id ? S.chats.get(this.id) : null; }
  get live() { return this.transcript?.live || null; }

  destroy() {
    this.abort.abort();
    clearInterval(this.timer);
    this.transcript?.destroy();
    this.closePanel();
    this.files.forEach(f => f.url && URL.revokeObjectURL(f.url));
  }

  // ---------------------------------------------------------------- layout

  build() {
    this.titleBtn = h('button.title-btn', {type: 'button', title: 'Rename', onclick: () => this.rename()});
    this.title = h('h1', this.titleBtn);
    this.crumb = h('nav.crumb', {'aria-label': 'Breadcrumb'});
    this.meter = h('button.meter', {type: 'button', onclick: () => this.togglePanel('context')});
    this.browserBtn = h('button.btn', {type: 'button', onclick: () => this.togglePanel('browser')}, icon('window', 16), h('span.only-wide', 'Browser'));

    this.asks = h('div.asks');
    this.column = h('div.column');
    this.scroller = h('div.scroller', {onscroll: () => this.onScroll()}, this.column);
    this.jump = h('button.jump', {type: 'button', hidden: true, onclick: () => this.toBottom()}, icon('arrowDown', 14), 'Jump to latest');

    this.banners = h('div.banners');
    this.queue = h('div.queue');
    this.statusLabel = h('span.status-label', {role: 'status', 'aria-live': 'polite'});
    this.statusMeta = h('span.muted', {'aria-hidden': 'true'});
    this.stopBtn = h('button.btn.stop', {type: 'button', onclick: () => this.stop()}, icon('stop', 14), 'Stop');
    this.status = h('div.status', {hidden: true}, h('span.pulse'), this.statusLabel, this.statusMeta, this.stopBtn);

    this.input = h('textarea', {rows: 1, 'aria-label': 'Message', oninput: () => this.grow(), onkeydown: e => this.onKey(e), onpaste: e => {
      if (!e.clipboardData.files.length) return;
      e.preventDefault();
      this.attach([...e.clipboardData.files]);
    }});
    this.chips = h('div.chips');
    this.picker = h('input', {type: 'file', multiple: true, hidden: true, onchange: () => {
      this.attach([...this.picker.files]);
      this.picker.value = '';
    }});
    this.modelBtn = h('button.pill', {type: 'button', title: 'Model', onclick: () => modelPicker(this.modelBtn, {
      selected: [this.meta?.model || this.model], onpick: model => this.patch({model}),
    })});
    this.effortBtn = h('button.pill', {type: 'button', title: 'Reasoning effort', onclick: () => menu(this.effortBtn, S.efforts.map(effort => ({
      label: effort, checked: effort === (this.meta?.effort || this.effort), onclick: () => this.patch({effort}),
    })))});
    this.sendBtn = h('button.send', {type: 'submit', title: 'Send', 'aria-label': 'Send'}, icon('up'));
    this.composer = h('form.composer', {onsubmit: e => { e.preventDefault(); this.send(); }},
      this.chips, this.input,
      h('div.composer-row',
        h('button.icon-btn', {type: 'button', title: 'Attach files', 'aria-label': 'Attach files', onclick: () => this.picker.click()}, icon('clip')),
        this.picker, this.modelBtn, this.effortBtn, h('span.spacer'), this.sendBtn));
    this.readonly = h('p.muted.readonly', {hidden: true}, 'This is the transcript of a sub-agent. Talk to it through the chat that started it.');

    this.panel = h('aside.panel', {hidden: true});
    const section = h('section.chat',
      pageHead(h('div.titles', this.crumb, this.title), this.browserBtn, this.meter),
      h('div.chat-body', this.scroller, this.jump),
      h('footer.dock', this.banners, this.queue, this.status, this.composer, this.readonly));
    // Dropping files anywhere on the chat attaches them.
    section.addEventListener('dragover', e => { if (e.dataTransfer.types.includes('Files')) { e.preventDefault(); section.classList.add('dropping'); } });
    section.addEventListener('dragleave', e => { if (!section.contains(e.relatedTarget)) section.classList.remove('dropping'); });
    section.addEventListener('drop', e => {
      e.preventDefault();
      section.classList.remove('dropping');
      this.attach([...e.dataTransfer.files]);
    });
    this.root.append(section, this.panel);
  }

  async load() {
    this.transcript?.destroy();
    this.transcript = null;
    this.missing = false;
    if (this.id) {
      const t = this.transcript = new Transcript(this.id, {
        onChange: () => this.follow(), onLive: () => this.renderLive(), onFork: (index, msg, resend) => this.fork(index, msg, resend),
      });
      const data = await t.load();
      if (this.transcript !== t) return; // superseded by a newer load
      if (!data) { this.transcript = null; this.missing = true; }
    }
    this.refresh();
    this.renderLive();
    this.toBottom();
    await this.takeCarried();
    if (matchMedia('(pointer: fine)').matches) this.input.focus();
  }

  /** Everything derived from shared state: titles, meter, pickers, and what fills the column. */
  refresh() {
    const meta = this.meta, setup = needsSetup();
    document.title = `${meta?.title || 'New chat'} — Artus`;
    if (!this.title.querySelector('input')) {
      this.titleBtn.textContent = meta?.title || 'New chat';
      this.titleBtn.disabled = !meta;
    }
    const task = meta?.task && S.tasks.find(t => t.id === meta.task);
    this.crumb.replaceChildren(...meta?.kind === 'task' ? [h('a', {href: '#/tasks'}, 'Tasks'), icon('right', 12), h('a', {href: '#/tasks/' + encodeURIComponent(meta.task)}, task?.name || meta.task)]
      : meta?.parent ? [h('a', {href: '#/chat/' + meta.parent}, icon('back', 12), S.chats.get(meta.parent)?.title || 'Parent chat')] : []);

    const window = meta?.window || S.config.context?.defaultWindow || 200000;
    this.meter.hidden = !meta;
    this.meter.replaceChildren(meterTrack(meta?.context || 0, window));
    this.meter.title = `${fmtTokens(meta?.context || 0)} of ${fmtTokens(window)} tokens in context · compacts at ${fmtTokens(compactAt(window))}. Open the context inspector.`;
    this.meter.setAttribute('aria-label', this.meter.title);
    this.browserBtn.hidden = !S.browser.length;
    this.renderBanners();

    const model = meta?.model || this.model;
    this.modelBtn.replaceChildren(modelName(model), icon('down', 14));
    this.effortBtn.replaceChildren(meta?.effort || this.effort, icon('down', 14));
    this.effortBtn.hidden = S.models?.find(m => m.id === model)?.reasoning === false;
    const agent = meta?.kind === 'agent';
    this.composer.hidden = agent;
    this.readonly.hidden = !agent;
    this.input.disabled = setup;
    this.input.placeholder = setup ? 'Finish the setup above to start' : this.live ? 'Add a message: it is queued for the next step' : 'Message Artus';

    if (setup && !this.setup) this.setup = setupCard(this.abort.signal);
    if (!setup) this.setup = null;
    this.empty ||= h('div.empty', h('h2', 'What should we work on?'),
      h('p.muted', 'Ask a question, hand over a job, or drop in files. Enter sends, Shift+Enter starts a new line.'));
    const gone = this.missing ? h('div.empty', h('p', 'This chat does not exist (any more).'), h('a.btn', {href: '#/'}, 'Start a new chat')) : null;
    const want = [this.setup, gone || this.transcript?.el || (this.setup ? null : this.empty), this.asks].filter(Boolean);
    if (want.length !== this.column.children.length || want.some((node, i) => node !== this.column.children[i])) this.column.replaceChildren(...want);
  }

  rename() {
    const input = h('input.input', {value: this.meta.title, 'aria-label': 'Chat title'});
    let closed = false;
    const done = save => {
      if (closed) return;
      closed = true;
      const title = input.value.trim();
      if (save && title && title !== this.meta.title) api('PATCH', '/api/chats/' + this.id, {title});
      this.title.replaceChildren(this.titleBtn);
      this.refresh();
    };
    input.onblur = () => done(true);
    input.onkeydown = e => {
      if (e.key === 'Enter') done(true);
      if (e.key === 'Escape') { e.stopPropagation(); done(false); }
    };
    this.title.replaceChildren(input);
    input.select();
  }

  // ---------------------------------------------------------------- live state

  onEvent(ev) {
    if (ev.chat !== this.id || ev.type !== 'chat') return;
    if (ev.data.unread) api('PATCH', '/api/chats/' + this.id, {unread: false}, {quiet: true}); // it is being read right now
  }

  renderLive() {
    const live = this.live;
    this.status.hidden = !live;
    this.stopBtn.disabled = false;
    this.syncStatus();
    this.queue.replaceChildren(...(live?.queue || []).map(text => h('span.queued', h('b', 'queued'), h('span', text))));
    this.renderAsk(live?.ask);
    this.refresh();
  }

  /** The label is rewritten only when it changes, so screen readers hear each state once. */
  syncStatus() {
    const live = this.live;
    if (!live) return;
    const label = liveLabel(live);
    const meta = `${fmtElapsed(Date.now() - Date.parse(live.started))} · turn ${live.turn || 1}`;
    if (this.statusLabel.textContent !== label) this.statusLabel.textContent = label;
    if (this.statusMeta.textContent !== meta) this.statusMeta.textContent = meta;
  }

  tick() {
    this.syncStatus();
    this.transcript?.tick();
  }

  async stop() {
    this.stopBtn.disabled = true;
    await api('POST', `/api/chats/${this.id}/stop`);
  }

  renderAsk(ask) {
    if ((ask?.id || '') === (this.askId || '')) return; // keep what is being typed into the open form
    this.askId = ask?.id;
    this.asks.replaceChildren(...ask ? [ask.kind === 'secret' ? this.secretAsk(ask) : this.approvalAsk(ask)] : []);
    if (ask) this.toBottom();
  }

  answer(ask, answer) {
    this.asks.replaceChildren();
    api('POST', '/api/asks/' + ask.id, {answer});
  }

  approvalAsk(ask) {
    const button = (label, answer, cls = '') => h('button.btn' + cls, {type: 'button', onclick: () => this.answer(ask, answer)}, label);
    return h('section.ask', {'aria-label': 'Approval needed'},
      h('h2', icon('alert', 16), 'Allow ', h('code', ask.tool), '?'),
      h('pre.ask-summary', String(toolSummary(ask.tool, ask.input || {}) ?? '')),
      h('details', h('summary', chevron(), 'All arguments'), h('pre', JSON.stringify(ask.input ?? {}, null, 2))),
      h('div.ask-actions', button('Allow once', 'allow', '.primary'), button('Always allow in this chat', 'always'), button('Deny', 'deny')));
  }

  /** The value goes from this field to the server and nowhere else: the field is cleared before the request. */
  secretAsk(ask) {
    const input = h('input.input', {type: 'password', autocomplete: 'new-password', required: true, placeholder: 'Paste the value', 'aria-label': `Value of ${ask.name}`});
    return h('form.ask', {'aria-label': 'Secret requested', onsubmit: e => {
      e.preventDefault();
      const value = input.value;
      input.value = '';
      if (value) this.answer(ask, value);
    }},
      h('h2', icon('key', 16), 'The agent asks for a secret: ', h('code', ask.name)),
      ask.description ? h('p', ask.description) : null,
      input,
      h('p.muted', 'The value goes straight into your secret store. It is never shown to the model and does not appear in this conversation.'),
      h('div.ask-actions', h('button.btn.primary', {type: 'submit'}, 'Save'),
        h('button.btn', {type: 'button', onclick: () => this.answer(ask, '')}, 'Decline')));
  }

  renderBanners() {
    const waiting = S.browser.filter(s => s.handoff);
    const key = waiting.map(s => s.handoff.id).join();
    if (key === this.bannerKey) return;
    this.bannerKey = key;
    this.banners.replaceChildren(...waiting.map(s => h('div.handoff', {role: 'alert'},
      icon('window', 20),
      h('div.handoff-text', h('strong', 'The agent needs you in the browser'), h('span', s.handoff.reason || 'It is waiting for you to take over.')),
      h('button.btn', {type: 'button', onclick: () => this.openPanel('browser', s.name)}, 'Open live view'),
      h('button.btn.primary', {type: 'button', onclick: () => api('POST', '/api/browser/handoff/' + s.handoff.id)}, 'I\'m done'))));
  }

  // ---------------------------------------------------------------- scrolling

  onScroll() {
    const s = this.scroller;
    this.stick = s.scrollHeight - s.scrollTop - s.clientHeight < 60;
    this.jump.hidden = this.stick;
  }

  follow() {
    this.syncStatus();
    if (this.stick) this.scroller.scrollTop = this.scroller.scrollHeight;
  }

  toBottom() {
    this.stick = true;
    this.jump.hidden = true;
    this.scroller.scrollTop = this.scroller.scrollHeight;
  }

  // ---------------------------------------------------------------- composer

  grow() {
    this.input.style.setProperty('height', 'auto');
    this.input.style.setProperty('height', Math.min(this.input.scrollHeight, 260) + 'px');
  }

  onKey(e) {
    // Enter sends. On touch keyboards, which have no Shift+Enter, it starts a new line instead.
    if (e.key !== 'Enter' || e.shiftKey || e.isComposing || matchMedia('(pointer: coarse)').matches) return;
    e.preventDefault();
    this.send();
  }

  /** Changes the model or effort: of the chat if it exists, of the one about to be created otherwise. */
  async patch(change) {
    if (this.id) await api('PATCH', '/api/chats/' + this.id, change);
    else Object.assign(this, change);
    this.refresh();
  }

  /** Creates the chat on first use and binds this view to it without a navigation. */
  async ensureChat() {
    if (this.id) return true;
    const meta = await api('POST', '/api/chats', {model: this.model, effort: this.effort});
    if (!meta) return false;
    S.chats.set(meta.id, meta);
    this.id = meta.id;
    history.replaceState(null, '', '#/chat/' + meta.id);
    S.route = {page: 'chat', id: meta.id};
    await this.load();
    return true;
  }

  async attach(files) {
    if (!files.length || needsSetup() || !(await this.ensureChat())) return;
    await Promise.all(files.map(async file => {
      const chip = {name: file.name, size: file.size, type: file.type, url: file.type.startsWith('image/') ? URL.createObjectURL(file) : ''};
      this.files.push(chip);
      this.renderChips();
      chip.att = await api('POST', `/api/chats/${this.id}/uploads?name=${encodeURIComponent(file.name)}`, file);
      if (!chip.att) this.removeFile(chip);
      this.renderChips();
    }));
  }

  removeFile(chip) {
    if (chip.url) URL.revokeObjectURL(chip.url);
    this.files = this.files.filter(f => f !== chip);
    this.renderChips();
  }

  renderChips() {
    this.chips.replaceChildren(...this.files.map(f => h('span.chip' + (f.att ? '' : '.busy'),
      f.url ? h('img', {src: f.url, alt: ''}) : icon('file', 16),
      h('span.chip-name', f.name), h('span.muted', f.att ? fmtBytes(f.size) : 'uploading…'),
      h('button.chip-x', {type: 'button', title: 'Remove', 'aria-label': `Remove ${f.name}`, onclick: () => this.removeFile(f)}, icon('x', 12)))));
    this.sendBtn.disabled = this.files.some(f => !f.att);
  }

  async send() {
    const text = this.input.value.trim(), files = this.files;
    if ((!text && !files.length) || files.some(f => !f.att) || needsSetup()) return;
    if (!(await this.ensureChat())) return;
    this.input.value = '';
    this.files = [];
    this.grow();
    this.renderChips();
    this.toBottom();
    if (await api('POST', `/api/chats/${this.id}/messages`, {text, files: files.map(f => f.att)})) {
      files.forEach(f => f.url && URL.revokeObjectURL(f.url));
    } else { // give the draft back
      this.input.value = text;
      this.files = files;
      this.grow();
      this.renderChips();
    }
  }

  /** Copies the conversation up to a user message into a new chat and opens it with that message as a draft. */
  async fork(index, msg, resend) {
    const meta = await api('POST', `/api/chats/${this.id}/fork`, {records: index});
    if (!meta) return;
    carried = {chat: meta.id, text: msg.text, files: msg.files, send: resend};
    location.hash = '#/chat/' + meta.id;
  }

  /** Picks up the draft a fork left for this chat. Its files are uploaded again: uploads belong to one chat. */
  async takeCarried() {
    if (carried?.chat !== this.id) return;
    const {text, files, send} = carried;
    carried = null;
    this.input.value = text;
    this.grow();
    const copies = await Promise.all(files.map(async f => {
      const res = await fetch(fileUrl(f.path)).catch(() => null);
      return res?.ok ? new File([await res.blob()], f.name, {type: f.type}) : null;
    }));
    if (copies.includes(null)) toast('Some attachments could not be carried over.', 'error');
    await this.attach(copies.filter(Boolean));
    if (send) this.send();
  }

  // ---------------------------------------------------------------- panels

  openPanel(kind, arg) {
    this.closePanel();
    const close = () => this.closePanel();
    this.panelKind = kind;
    this.panelView = kind === 'browser' ? browserPanel(this.panel, arg, close) : contextPanel(this.panel, this.id, close);
    this.panel.hidden = false;
    this.dropLayer = pushLayer(close);
  }

  closePanel() {
    if (!this.panelView) return;
    this.panelView.destroy();
    this.panelView = null;
    this.panel.hidden = true;
    this.panel.replaceChildren();
    this.dropLayer();
  }

  togglePanel(kind) {
    if (this.panelView && this.panelKind === kind) this.closePanel();
    else this.openPanel(kind);
  }
}

const panelHead = (title, close, ...extra) => h('header.panel-head', h('h2', title), ...extra,
  h('button.icon-btn', {type: 'button', title: 'Close', 'aria-label': `Close ${title}`, onclick: close}, icon('x')));

// ---------------------------------------------------------------- context inspector

const PARTS = [
  ['prompt', 'Base prompt'], ['instructions', 'Your instructions'], ['memory', 'Memory'], ['indexes', 'Topic and skill indexes'],
  ['tools', 'Tool definitions'], ['messages', 'Messages'], ['toolResults', 'Tool results'], ['attachments', 'Attachments'],
];

/** Shows exactly what the next request of a chat would send, and how the tokens divide up. */
function contextPanel(el, chatId, close) {
  const abort = new AbortController();
  const body = h('div.panel-body');
  let info = null, tab = 'system', timer = 0;
  el.replaceChildren(panelHead('Context', close), body);

  const tabBody = () => {
    if (tab === 'system') return h('pre.raw', {tabindex: 0}, info.system || '(The prompt is written when the first message is sent.)');
    if (tab === 'tools') {
      return h('div', (info.tools || []).map(t => h('details.raw-item',
        h('summary', chevron(), h('code', t.name), h('span.muted.clamp', t.description)),
        h('p', t.description), h('pre.raw', {tabindex: 0}, JSON.stringify(t.schema, null, 2)))));
    }
    return h('div', (info.messages || []).map((m, i) => h('details.raw-item',
      h('summary', chevron(), h('code', `${i + 1} ${m.role}`), h('span.muted.clamp', m.blocks.map(b => b.type).join(', '))),
      h('pre.raw', {tabindex: 0}, JSON.stringify(m, null, 2)))));
  };

  const render = () => {
    const meta = S.chats.get(chatId) || {};
    const parts = PARTS.map(([key, label], i) => ({label, tokens: info.breakdown[key] || 0, slot: i + 1})).filter(p => p.tokens > 0);
    const total = parts.reduce((n, p) => n + p.tokens, 0) || 1;
    const share = p => `${(p.tokens / total * 100).toFixed(p.tokens / total < 0.1 ? 1 : 0)}%`;
    const rows = parts.map(p => h('li', h('span.swatch.s' + p.slot), h('span', p.label), h('span.num', fmtTokens(p.tokens)), h('span.num.muted', share(p))));
    const bar = h('div.stack', {role: 'img', 'aria-label': 'Context by part: ' + parts.map(p => `${p.label} ${share(p)}`).join(', ')},
      parts.map((p, i) => {
        const seg = h('span.seg.s' + p.slot, {title: `${p.label}: ${fmtTokens(p.tokens)} tokens (${share(p)})`,
          onmouseenter: () => rows[i].classList.add('hot'), onmouseleave: () => rows[i].classList.remove('hot')});
        seg.style.setProperty('flex-grow', p.tokens);
        return seg;
      }));
    const tabs = [['system', 'System prompt'], ['tools', `Tools (${(info.tools || []).length})`], ['messages', `Messages (${(info.messages || []).length})`]];
    const cached = meta.tokensIn ? Math.round(meta.cacheRead / meta.tokensIn * 100) : 0;
    body.replaceChildren(
      h('section',
        h('p.figure', h('strong', fmtTokens(info.tokens)), ` of ${fmtTokens(info.window)} tokens`),
        meterTrack(info.tokens, info.window),
        h('p.muted', `Compacts at ${fmtTokens(info.threshold)}: the conversation is then replaced by a summary.`),
        h('button.btn', {type: 'button', disabled: S.running.has(chatId) || (info.messages || []).length < 2,
          onclick: async () => { if (await api('POST', `/api/chats/${chatId}/compact`)) toast('Compacting'); }}, 'Compact now')),
      h('section', h('h3', 'What fills it'), bar, h('ul.legend', rows)),
      h('section', h('h3', 'Files loaded'),
        info.loaded?.length ? h('ul.plain', info.loaded.map(p => h('li', h('code', p)))) : h('p.muted', 'No files were read into this context.')),
      h('section', h('h3', 'This chat so far'),
        h('dl.facts', h('dt', 'Tokens in'), h('dd', fmtTokens(meta.tokensIn || 0)), h('dt', 'Tokens out'), h('dd', fmtTokens(meta.tokensOut || 0)),
          h('dt', 'Read from cache'), h('dd', `${cached}% of input`),
          ...meta.cost ? [h('dt', 'Cost'), h('dd', '$' + meta.cost.toFixed(meta.cost < 1 ? 4 : 2))] : [])),
      h('section', h('h3', 'The next request'),
        h('div.tabs', {role: 'tablist'}, tabs.map(([key, label]) => h('button', {type: 'button', role: 'tab', 'aria-selected': String(tab === key),
          onclick: () => { tab = key; render(); }}, label))),
        tabBody()));
  };

  const load = async () => {
    const res = await api('GET', `/api/chats/${chatId}/context`);
    if (!res || abort.signal.aborted) return;
    info = res;
    render();
  };
  // The numbers move with every request of the chat: refresh shortly after it changes.
  bus.addEventListener('server', e => {
    if (e.detail.chat !== chatId || !['chat', 'live'].includes(e.detail.type)) return;
    clearTimeout(timer);
    timer = setTimeout(load, 500);
  }, {signal: abort.signal});
  load();
  return {destroy() { abort.abort(); clearTimeout(timer); }};
}

// ---------------------------------------------------------------- browser live view

const MOUSE_BUTTONS = ['left', 'middle', 'right'];
const modifiers = e => (e.altKey ? 1 : 0) | (e.ctrlKey ? 2 : 0) | (e.metaKey ? 4 : 0) | (e.shiftKey ? 8 : 0);

/** A live picture of a browser session that forwards the user's mouse and keyboard to it. */
function browserPanel(el, name, close) {
  const abort = new AbortController();
  const {signal} = abort;
  let session = name || S.browser[0]?.name || 'default';
  let chain = Promise.resolve(), backlog = 0, lastMove = 0;
  const img = h('img', {alt: 'Live view of the browser page', draggable: false});
  const view = h('div.browser-view', {tabindex: 0, 'aria-label': 'Browser page. Click it to type into the page; Escape releases the keyboard.'}, img);
  const where = h('p.browser-url'), tabs = h('div.tabs');
  const closeBtn = h('button.icon-btn', {type: 'button', title: 'Close', 'aria-label': 'Close the live view', onclick: close}, icon('x'));
  el.replaceChildren(h('header.panel-head', h('h2', 'Browser'), tabs, closeBtn),
    h('div.panel-body', where, view,
      h('p.muted', 'This is the page the agent sees. Click it to take over the mouse and keyboard; Esc gives the keyboard back.')));

  // Events are sent one after another so a click never overtakes the move before it.
  const send = event => {
    backlog++;
    chain = chain.then(() => api('POST', '/api/browser/input', {session, event})).then(() => { backlog--; });
  };
  const at = e => {
    const r = img.getBoundingClientRect();
    return {x: (e.clientX - r.left) * (img.naturalWidth || r.width) / r.width, y: (e.clientY - r.top) * (img.naturalHeight || r.height) / r.height};
  };
  const mouse = (type, e) => send({type, ...at(e), button: MOUSE_BUTTONS[e.button] || 'left', modifiers: modifiers(e)});
  img.addEventListener('pointerdown', e => { e.preventDefault(); view.focus(); img.setPointerCapture(e.pointerId); mouse('mousedown', e); });
  img.addEventListener('pointerup', e => mouse('mouseup', e));
  img.addEventListener('pointermove', e => {
    if (backlog > 2 || e.timeStamp - lastMove < 40) return; // moves are droppable
    lastMove = e.timeStamp;
    mouse('mousemove', e);
  });
  img.addEventListener('wheel', e => { e.preventDefault(); send({type: 'wheel', ...at(e), deltaX: e.deltaX, deltaY: e.deltaY, modifiers: modifiers(e)}); }, {passive: false});
  img.addEventListener('contextmenu', e => e.preventDefault());
  const key = type => e => {
    e.stopPropagation(); // the page gets the key, not the interface's shortcuts
    if (e.key === 'Escape') { closeBtn.focus(); return; }
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'v') return; // arrives as a paste event
    e.preventDefault();
    send({type, key: e.key, code: e.code, modifiers: modifiers(e)});
  };
  view.addEventListener('keydown', key('keydown'));
  view.addEventListener('keyup', key('keyup'));
  view.addEventListener('paste', e => {
    e.preventDefault();
    const text = e.clipboardData.getData('text');
    if (text) send({type: 'text', text});
  });

  const show = next => {
    session = next;
    img.src = '/api/browser/view?session=' + encodeURIComponent(session);
    sync();
  };
  const sync = () => {
    const current = S.browser.find(s => s.name === session);
    tabs.replaceChildren(...S.browser.length > 1 ? S.browser.map(s => h('button', {type: 'button', role: 'tab',
      'aria-selected': String(s.name === session), onclick: () => show(s.name)}, s.name)) : []);
    where.textContent = current ? `${current.title || 'Untitled'} — ${current.url}` : 'This browser session has closed.';
    view.classList.toggle('closed', !current);
  };
  watch(signal, () => JSON.stringify(S.browser), sync);
  show(session);
  return {destroy() { abort.abort(); img.removeAttribute('src'); }};
}
