// transcript.js — turns the records and live events of one chat into DOM:
// messages, work groups (the thinking, tool cards and sub-agent lanes between
// them, folded into one line each) and the reply in progress.

import {S, bus, chevron, h, icon, api, changed, fmtDuration, fmtElapsed, fmtBytes, fmtTokens, fileUrl, copyButton, lightbox, liveLabel, modelLabel} from './lib.js';
import {renderMarkdown, LiveMarkdown, plain} from './markdown.js';

const TOOL_ICONS = {
  bash: 'terminal', read: 'file', write: 'edit', edit: 'edit', glob: 'search', grep: 'search',
  web_fetch: 'globe', agent: 'bot', schedule: 'clock', ask_secret: 'key',
};
const STATE_LABELS = {running: 'Running', done: 'Finished', ok: 'Finished', error: 'Failed', stopped: 'Stopped before it finished'};
const STEP_NAMES = {agent: 'sub-agent', web_search: 'web search'}; // what a work group's summary calls a step

/** Reads tool arguments that may still be streaming in as half a JSON object. */
function partialArgs(text) {
  try { return JSON.parse(text); } catch { /* not complete yet */ }
  const out = {};
  for (const m of text.matchAll(/"(\w+)"\s*:\s*"((?:[^"\\]|\\.)*)/g)) {
    try { out[m[1]] = JSON.parse(`"${m[2]}"`); } catch { out[m[1]] = m[2]; }
  }
  return out;
}

/** One line that says what a tool call is about. */
export function toolSummary(name, a = {}) {
  switch (name) {
    case 'bash': return a.command;
    case 'read': case 'write': case 'edit': return a.path;
    case 'glob': case 'grep': return [a.pattern, a.path && 'in ' + a.path].filter(Boolean).join(' ');
    case 'web_fetch': return a.url;
    case 'agent': return a.name || a.prompt;
    case 'schedule': return [a.action, a.when, a.id, a.note && '— ' + a.note].filter(Boolean).join(' ');
    case 'ask_secret': return a.name;
    default: return Object.values(a).find(v => typeof v === 'string');
  }
}

/** The title the server gives a sub-agent chat that has no name: its prompt, shortened. */
function promptTitle(prompt = '') {
  const chars = [...prompt.trim().split(/\s+/).join(' ')];
  return chars.length > 60 ? chars.slice(0, 60).join('').trim() + '…' : chars.join('');
}

/** A line diff of two short texts (longest common subsequence), removals in red and additions in green. */
function diffView(before, after) {
  const a = before ? before.split('\n') : [], b = after.split('\n');
  const lcs = Array.from({length: a.length + 1}, () => new Uint16Array(b.length + 1));
  for (let i = a.length - 1; i >= 0; i--) {
    for (let j = b.length - 1; j >= 0; j--) lcs[i][j] = a[i] === b[j] ? lcs[i + 1][j + 1] + 1 : Math.max(lcs[i + 1][j], lcs[i][j + 1]);
  }
  const rows = [];
  for (let i = 0, j = 0; i < a.length || j < b.length;) {
    if (i < a.length && j < b.length && a[i] === b[j]) { rows.push(h('span', '  ' + a[i++] + '\n')); j++; }
    else if (i < a.length && (j === b.length || lcs[i + 1][j] >= lcs[i][j + 1])) rows.push(h('span.del', '- ' + a[i++] + '\n'));
    else rows.push(h('span.add', '+ ' + b[j++] + '\n'));
  }
  return h('pre.diff', {tabindex: 0}, rows);
}

/** Splits a user-role message into its hidden time stamp, text, attachment list, media and tool results. */
function parseUser(msg) {
  const out = {time: '', text: '', files: [], media: [], results: []};
  const texts = [];
  for (const b of msg.blocks) {
    const time = b.type === 'text' && /^<time>(.*)<\/time>$/.exec(b.text);
    if (time) out.time = time[1];
    else if (b.type === 'text') texts.push(b.text);
    else if (b.type === 'tool_result') out.results.push(b);
    else if (b.type === 'image' || b.type === 'file') out.media.push({path: b.path, name: b.name, type: b.mediaType});
  }
  out.text = texts.join('\n');
  const list = /\n*<attachments>\n([\s\S]*?)\n<\/attachments>\s*$/.exec(out.text);
  if (list) {
    out.text = out.text.slice(0, list.index);
    for (const m of list[1].matchAll(/^- (.+) \(([^,()]+), (\d+) bytes\)$/gm)) {
      out.files.push({path: m[1], name: m[1].split('/').pop(), type: m[2], size: Number(m[3])});
    }
  }
  return out;
}

/** A file in a message: images as thumbnails that open large, anything else as a link. */
function attachment(f) {
  const url = fileUrl(f.path);
  if ((f.type || '').startsWith('image/')) {
    return h('button.thumb', {type: 'button', title: f.name, onclick: () => lightbox(url, f.name)}, h('img', {src: url, alt: f.name, loading: 'lazy'}));
  }
  return h('a.chip', {href: url, target: '_blank', rel: 'noopener'}, icon('file', 16), h('span.chip-name', f.name),
    f.size != null && h('span.muted', fmtBytes(f.size)));
}

/** Text the harness wrote into the conversation (wake-ups, compaction prompts, a sub-agent's assignment). */
function harnessNote(text) {
  const tagged = text.includes('<harness>');
  const clean = text.replace(/<\/?harness>/g, '').trim();
  const [first, ...rest] = clean.split('\n');
  const more = rest.join('\n').trim();
  if (tagged && !more) return h('div.harness', icon('info', 14), h('span', plain(first)));
  return h('details.harness',
    h('summary', chevron(), h('span', tagged ? first : 'Assignment')),
    h('div.prose', renderMarkdown(tagged ? more : clean)));
}

/** "bash ×3, read, edit": what was used and how often, in order of first use. */
function tally(names) {
  const counts = new Map();
  for (const n of names) counts.set(n, (counts.get(n) || 0) + 1);
  return [...counts].map(([n, k]) => k > 1 ? `${n} ×${k}` : n).join(', ');
}

// ---------------------------------------------------------------- work groups

const thinkStep = (text, ms) => ({kind: 'think', ms, el: h('details.thinking',
  h('summary', chevron(), ms ? `Thought for ${fmtDuration(ms)}` : 'Thought'),
  h('div.thinking-text', text))});

const searchStep = text => ({kind: 'search', name: 'web_search', summary: text || '',
  el: h('div.searched', icon('globe', 14), text ? 'Searched: ' + text : 'Searched the web')});

/**
 * A run of steps (thinking, tool calls, searches) with no text between them,
 * folded into one line of the chat. It opens only when the user opens it; the
 * line says what is happening while the run is in it and what was done after.
 */
class Work {
  constructor(owner) {
    Object.assign(this, {owner, steps: [], ms: 0, solo: true, shown: ''});
    this.mark = h('span.work-mark');
    this.label = h('span.work-label');
    this.digest = h('span.work-digest');
    this.failed = h('span.work-failed');
    this.time = h('span.work-time');
    this.body = h('div.work-body');
    this.el = h('details.work', {hidden: true},
      h('summary', chevron(), this.mark, this.label, this.digest, this.failed, this.time), this.body);
  }

  /** Appends a step. Sub-agents started together (the same batch) sit side by side. */
  add(step, batch) {
    step.work = this;
    if (step.card) step.card.work = this;
    this.steps.push(step);
    if (step.card instanceof Lane) {
      let lanes = this.body.lastElementChild;
      if (!lanes?.matches('.lanes') || lanes.batch !== batch) lanes = this.body.appendChild(Object.assign(h('div.lanes'), {batch}));
      lanes.append(step.el);
    } else this.body.append(step.el);
    this.refresh();
    return step;
  }

  remove(step) {
    const at = this.steps.indexOf(step);
    if (at < 0) return;
    this.steps.splice(at, 1);
    const parent = step.el.parentNode;
    step.el.remove();
    if (parent?.matches('.lanes') && !parent.firstChild) parent.remove();
    this.refresh();
  }

  /** Rewrites the summary line from the steps and, for the group the run is in, from the run's state. */
  refresh() {
    const live = this.owner.active === this ? this.owner.live : null;
    const thinks = this.steps.filter(s => s.kind === 'think'), tools = this.steps.filter(s => s.kind !== 'think');
    const cards = tools.filter(s => s.card).map(s => s.card);
    const called = step => STEP_NAMES[step.card?.name || step.name] || step.card.name;
    const about = step => `${called(step)} ${step.card ? step.card.summary : step.summary}`.trim();

    // Thoughts alone show their text at once; beside tools each sits behind its own toggle.
    const solo = !tools.length;
    if (solo) thinks.forEach(s => { s.el.open = true; });
    else if (this.solo) thinks.forEach(s => { s.el.open = Boolean(s.active); });
    this.solo = solo;
    this.el.hidden = !this.steps.length;
    this.el.classList.toggle('solo', solo);

    const running = tools.filter(s => s.card?.state === 'running');
    const thinking = thinks.find(s => s.active);
    const ms = this.ms + thinks.reduce((n, s) => n + (s.ms || 0), 0);
    let state = 'done', label, digest = '', fail = '', time = '';
    let one = running.length === 1 ? running[0] : null; // the step the line is about, when it is about one
    if (live?.state === 'waiting') {
      [state, label, digest] = ['waiting', 'Waiting for you', one ? about(one) : tally(running.map(called))];
    } else if (one) {
      [state, label, digest] = ['running', called(one), one.card.summary];
    } else if (running.length) {
      const agents = running.every(s => s.card.name === 'agent');
      [state, label, digest] = ['running', agents ? `${running.length} sub-agents running` : `Running ${running.length} tools`, agents ? '' : tally(running.map(called))];
    } else if (thinking || live?.state === 'thinking') {
      const since = thinking?.since || Date.parse(live.since);
      [state, label, time] = ['thinking', 'Thinking', since ? fmtElapsed(Date.now() - since) : ''];
    } else if (live) {
      [state, label] = ['running', {retrying: 'Retrying', compacting: 'Compacting'}[live.state] || 'Working'];
    } else if (solo) {
      [state, label] = ['thought', ms ? `Thought for ${fmtDuration(ms)}` : 'Thought'];
    } else {
      one = tools.length === 1 ? tools[0] : null;
      const agents = tools.every(s => s.card?.name === 'agent');
      const stopped = cards.some(c => c.state === 'stopped'), failed = cards.filter(c => c.state === 'error').length;
      state = stopped ? 'stopped' : 'done';
      label = stopped ? 'Stopped' : agents ? (one ? 'Ran a sub-agent' : `Ran ${tools.length} sub-agents`)
        : !one ? `Used ${tools.length} tools` : one.card ? `Used ${called(one)}` : 'Searched the web';
      digest = one ? (stopped ? about(one) : one.card ? one.card.summary : one.summary)
        : agents ? cards.map(c => c.summary).join(', ') : tally(tools.map(called));
      fail = !failed ? '' : one ? 'failed' : `${failed} failed`;
      time = ms ? fmtDuration(ms) : '';
    }

    const cmd = Boolean(one?.card) && one.card.name !== 'agent'; // a command or a path, not names
    const shown = [state, label, digest, fail, time, cmd].join('\n');
    if (shown === this.shown) return;
    this.shown = shown;
    if (this.el.dataset.state !== state) { // a spinner that is replaced starts its turn over
      this.el.dataset.state = state;
      this.mark.replaceChildren(...state === 'running' ? [h('span.spinner')] : state === 'stopped' ? [icon('stop', 12)] : []);
    }
    this.label.textContent = label;
    this.digest.textContent = digest;
    this.digest.classList.toggle('cmd', cmd);
    this.failed.replaceChildren(...fail ? [icon('x', 12), fail] : []);
    this.time.textContent = time;
  }
}

// ---------------------------------------------------------------- tool cards

class ToolCard {
  constructor(owner, call) {
    Object.assign(this, {owner, id: call.id, name: call.name, input: call.input, args: '', output: '', media: []});
    this.head = h('span.tool-args');
    this.status = h('span.tool-status');
    this.body = h('div.tool-body');
    this.el = h('details.tool', {ontoggle: () => this.renderBody()},
      h('summary', icon(TOOL_ICONS[this.name] || 'cube', 16), h('span.tool-name', this.name), this.head, this.status),
      this.body);
    this.setState('running');
    this.renderHead();
  }

  get params() { return this.input ?? partialArgs(this.args); }

  setState(state, ms) {
    this.state = this.el.dataset.state = state;
    const mark = {running: h('span.spinner'), error: icon('x', 14), stopped: icon('stop', 12)}[state] || icon('check', 14);
    this.status.replaceChildren(ms != null ? h('span.tool-time', fmtDuration(ms)) : '', mark, h('span.sr-only', STATE_LABELS[state]));
    this.work?.refresh();
  }

  /** summary is the call in one line; its work group shows it too. */
  renderHead() {
    this.head.textContent = this.summary = String(toolSummary(this.name, this.params) ?? '').trim().replace(/\s*\n\s*/g, ' ⏎ ');
    this.work?.refresh();
  }

  renderBody() {
    if (!this.el.open) return;
    const ui = this.ui || {};
    const diff = ui.new !== undefined;
    const shown = Object.entries(this.params).filter(([k]) => !(diff && ['content', 'old_string', 'new_string'].includes(k)));
    this.out = this.output ? h('pre.output', {tabindex: 0}) : null;
    this.body.replaceChildren(...[
      ...shown.map(([k, v]) => h('div.kv', h('span.k', k), h('pre.v', typeof v === 'string' ? v : JSON.stringify(v, null, 2)))),
      diff && diffView(ui.old || '', ui.new),
      ui.secrets?.length && h('div.kv', h('span.k', 'secrets'), h('span', ui.secrets.map(n => plain(`[secret:${n}]`)))),
      ...this.media.map(f => attachment(f)),
      this.out,
    ].filter(Boolean));
    this.showOutput();
  }

  showOutput() {
    if (!this.out) return;
    this.out.replaceChildren(plain(this.output));
    this.out.scrollTop = this.out.scrollHeight;
  }

  addArgs(text) {
    this.args += text;
    this.renderHead();
    this.renderBody();
  }

  /** Live output fills the card if it is open. Only the user opens it. */
  addOutput(text) {
    this.output += text;
    if (this.out) this.showOutput(); else this.renderBody();
  }

  addMedia(file) {
    this.media.push(file);
    this.el.open = true;
    this.renderBody();
  }

  /** Applies the server's view of a running call (after a reload, or when it completes). */
  sync(tool) {
    if (tool.output && tool.output.length > this.output.length && this.state === 'running') {
      this.output = '';
      this.addOutput(tool.output);
    }
    if (tool.done && this.state === 'running') this.setState('done');
  }

  finish(result) {
    const {ms, ui} = result.ui || {};
    this.ui = ui;
    this.output = result.text || '';
    // A call the user stopped comes back as an error: it reads as stopped, not as failed.
    const stopped = result.isError && /^Stopped by the user\.$|\[stopped by the user\]$/.test(this.output);
    this.setState(stopped ? 'stopped' : result.isError ? 'error' : 'ok', ms);
    this.renderHead();
    this.renderBody();
  }
}

/** An agent call: a lane showing the sub-agent's state, expandable to its whole transcript. */
class Lane extends ToolCard {
  renderHead() {
    const p = this.params, child = S.chats.get(this.child), live = S.live.get(this.child);
    const state = live ? liveLabel(live) : {running: 'starting', error: 'failed', stopped: 'stopped'}[this.state] || 'done';
    const parts = [p.name || child?.title || promptTitle(p.prompt) || 'Sub-agent', child?.model || p.model || S.chats.get(this.owner.id)?.model, state];
    if (parts.join('\n') === this.shown) return;
    this.shown = parts.join('\n');
    this.summary = parts[0];
    this.head.replaceChildren(h('span.lane-name', parts[0]), modelLabel(parts[1], 'lane-model'), h('span.lane-state' + (live ? '.busy' : ''), parts[2]));
    this.work?.refresh();
  }

  setState(state, ms) {
    super.setState(state, ms);
    if (this.head) this.renderHead();
  }

  setChild(id) {
    this.child = id;
    bus.addEventListener('server', e => { if (e.detail.chat === id) this.renderHead(); }, {signal: this.owner.abort.signal});
    if (S.running.has(id) && !S.live.has(id)) this.openChild().then(() => this.renderHead()); // learn its state after a reload
    this.renderHead();
    this.renderBody();
  }

  openChild() {
    this.sub = new Transcript(this.child, {nested: true, onChange: this.owner.opts.onChange});
    this.owner.nested.push(this.sub);
    return this.sub.load();
  }

  renderBody() {
    if (!this.el.open) return;
    if (!this.child) return super.renderBody(); // not started, or refused before it started
    if (!this.sub) this.openChild();
    if (!this.body.contains(this.sub.el)) {
      this.body.replaceChildren(h('a.ghost', {href: '#/chat/' + this.child}, icon('open', 14), 'Open as chat'), this.sub.el);
    }
  }

  finish(result) {
    const child = result.ui?.ui?.chat;
    if (child && !this.child) this.setChild(child);
    super.finish(result);
  }
}

// ---------------------------------------------------------------- transcript

export class Transcript {
  /** opts: nested (a lane inside another transcript), onChange(), onLive(live), onFork(index, parsed, resend). */
  constructor(id, opts = {}) {
    this.id = id;
    this.opts = opts;
    this.el = h('div.transcript');
    this.records = [];
    this.cards = new Map(); // tool call id → card
    this.nested = [];       // transcripts of opened sub-agent lanes
    this.work = null;       // the work group that steps join, until text or a message ends it
    this.live = null;
    this.stream = null;     // the reply being written
    this.loading = true;
    this.pending = [];      // events that arrived while loading
    this.frame = 0;
    this.abort = new AbortController();
    bus.addEventListener('server', e => this.onEvent(e.detail), {signal: this.abort.signal});
    // Where nothing can hover, tapping a message shows its actions (see .tapped in the stylesheet).
    this.el.addEventListener('click', e => {
      const msg = e.target.closest('.msg');
      if (!msg || msg.closest('.transcript') !== this.el || e.target.closest('button, a, summary, .tool, .thumb')) return;
      const shown = msg.classList.contains('tapped');
      for (const m of this.el.querySelectorAll(':scope > .msg.tapped')) m.classList.remove('tapped');
      msg.classList.toggle('tapped', !shown);
    });
  }

  destroy() {
    this.abort.abort();
    cancelAnimationFrame(this.frame);
    this.nested.forEach(t => t.destroy());
  }

  /** Fetches the chat and renders it; events newer than the snapshot are applied on top. */
  async load() {
    const data = await api('GET', '/api/chats/' + this.id, undefined, {quiet: this.opts.nested});
    if (!data || this.abort.signal.aborted) return null;
    S.chats.set(this.id, data.meta);
    for (const child of data.children) S.chats.set(child.id, child);
    if (data.live) { S.live.set(this.id, data.live); S.running.add(this.id); }
    else { S.live.delete(this.id); S.running.delete(this.id); }
    changed();
    for (const rec of data.records) this.addRecord(rec, false);
    this.restoreLive(data.live);
    this.linkAgents();
    this.loading = false;
    for (const ev of this.pending.splice(0)) if (ev.seq > data.seq) this.apply(ev);
    return data;
  }

  onEvent(ev) {
    if (ev.type === 'chat' && ev.data.parent === this.id) this.linkAgents();
    if (ev.chat !== this.id) return;
    if (this.loading) this.pending.push(ev); else this.apply(ev);
  }

  apply(ev) {
    switch (ev.type) {
      case 'record': this.addRecord(ev.data, true); this.linkAgents(); break;
      case 'delta': this.onDelta(ev.data); break;
      case 'tool_output': this.cards.get(ev.data.id)?.addOutput(ev.data.text); break;
      case 'live': this.onLive(ev.data); break;
      default: return;
    }
    this.opts.onChange?.();
  }

  tick() {
    const think = this.stream?.think;
    const elapsed = think?.active ? fmtElapsed(Date.now() - think.since) : '';
    if (elapsed && think.timer.textContent !== elapsed) think.timer.textContent = elapsed;
    this.active?.refresh();
    this.nested.forEach(t => t.tick());
  }

  // ---- work groups

  /** The group the run is working in right now: the open one, unless the reply has moved on to its text. */
  get active() {
    const s = this.stream;
    return this.live ? s?.tail || (s?.el ? null : this.work) : null;
  }

  /** The open group. The first step after text, a message or a note starts a new one. */
  group() {
    if (!this.work) this.el.append((this.work = new Work(this)).el);
    return this.work;
  }

  /** Ends the open group: whatever shows in the chat next comes after it. */
  closeWork() {
    const work = this.work;
    this.work = null;
    if (work && !work.steps.length) work.el.remove(); else work?.refresh();
  }

  // ---- records

  addRecord(rec, fresh) {
    const index = this.records.push(rec) - 1;
    const msg = rec.t === 'msg' ? rec.msg : null;
    let node = null;
    if (rec.t === 'note') {
      const error = rec.level === 'error';
      node = h('div.note' + (error ? '.error' : ''), icon(error ? 'alert' : 'info', 15), h('span', plain(rec.note || '')));
    } else if (rec.t === 'compact') {
      node = this.compacted(rec);
    } else if (msg?.role === 'assistant') {
      this.assistantMessage(rec, this.endStream());
    } else if (msg) {
      node = this.userMessage(rec, index);
      if (node && fresh) node.classList.add('enter');
    }
    if (!node) return;
    this.closeWork();
    this.el.append(node);
  }

  /** The divider that marks a compaction. The summary the model just wrote moves inside it. */
  compacted(rec) {
    const said = this.lastReply;
    if (said && said.text.trim() === (rec.summary || '').trim()) said.drop();
    return h('details.compact',
      h('summary', h('span', 'Context compacted', rec.tokens ? ` · was ${fmtTokens(rec.tokens)} tokens` : '', chevron())),
      h('div.prose', renderMarkdown(rec.summary || '')));
  }

  /**
   * An assistant message. Its thinking, tool calls and searches join the open
   * work group; its text goes into the chat and ends that group, so that steps
   * after the text start the next one. was is what endStream() reported.
   */
  assistantMessage(rec, was) {
    const texts = [], articles = [], steps = [];
    let article = null;
    const step = st => {
      const reopen = !this.work && articles.length && was.open; // the group the user opened while it streamed
      steps.push(this.group().add(st, rec));
      if (reopen) this.work.el.open = true;
      article = null;
    };
    for (const b of rec.msg.blocks) {
      if (b.type === 'thinking' && b.text) step(thinkStep(b.text, was.ms || b.ui?.ms));
      else if (b.type === 'opaque' && b.name === 'web_search') step(searchStep(b.text));
      else if (b.type === 'tool_call') { const card = this.addCard(b); step({kind: 'tool', card, el: card.el}); }
      else if (b.type === 'text' && b.text.trim()) {
        this.closeWork();
        if (!article) this.el.append(articles[articles.length] = article = h('article.msg.assistant'));
        texts.push(b.text);
        article.append(h('div.prose', renderMarkdown(b.text)));
      }
    }
    if (!texts.length) return;
    const u = rec.usage;
    articles.at(-1).append(h('div.msg-actions', copyButton(() => texts.join('\n\n')),
      h('span', {title: u ? `${u.input.toLocaleString()} tokens in · ${u.output.toLocaleString()} out` : null}, modelLabel(rec.msg.model))));
    this.lastSaid?.classList.remove('last'); // a touch screen shows the actions of the newest reply only, the others when tapped
    this.lastSaid = articles.at(-1);
    this.lastSaid.classList.add('last');
    this.lastReply = {text: texts.join('\n'), drop: () => {
      articles.forEach(a => a.remove());
      for (const st of steps) {
        st.work.remove(st);
        if (!st.work.steps.length && st.work !== this.work) st.work.el.remove();
      }
    }};
  }

  userMessage(rec, index) {
    const u = parseUser(rec.msg);
    if (u.results.length) return this.toolResults(u, index);
    if (rec.msg.synthetic) return u.text.startsWith('<summary>') ? null : harnessNote(u.text);
    const actions = h('div.msg-actions', u.time && h('span.muted', {title: u.time}, u.time.split(' ')[1]), copyButton(() => u.text));
    if (this.opts.onFork) {
      actions.append(
        h('button.ghost', {type: 'button', onclick: () => this.opts.onFork(index, u, false)}, icon('fork', 14), 'Fork from here'),
        h('button.ghost.retry', {type: 'button', onclick: () => this.opts.onFork(index, u, true)}, icon('retry', 14), 'Retry'));
    }
    const el = h('article.msg.user',
      u.files.length ? h('div.attachments', u.files.map(attachment)) : null,
      u.text ? h('div.bubble', plain(u.text)) : null,
      actions);
    this.lastUser?.classList.remove('last'); // Retry is offered on the newest message only
    this.lastUser = el;
    el.classList.add('last');
    return el;
  }

  /**
   * Hands each result to its call, which leaves the work group open. Only
   * harness remarks and unclaimed media need a node of their own.
   */
  toolResults(u, index) {
    const cards = u.results.map(result => {
      const card = this.cards.get(result.id);
      card?.finish(result);
      return card;
    });
    // The group's time counts this batch once: calls that ran side by side took no longer than the records are apart.
    const work = cards.find(Boolean)?.work;
    const wall = Date.parse(this.records[index].time) - Date.parse(this.records[index - 1]?.time);
    const each = u.results.reduce((n, r) => n + (r.ui?.ms || 0), 0);
    if (work) { work.ms += wall >= 0 ? Math.min(wall, each) : each; work.refresh(); }

    const stray = u.media.filter(f => {
      const reader = cards.find(c => c?.name === 'read' && String(c.params.path || '').split(/[\\/]/).pop() === f.name);
      reader?.addMedia(f);
      return !reader;
    });
    if (!stray.length && !u.text.trim()) return null;
    return h('div.msg', stray.length ? h('div.attachments', stray.map(attachment)) : null, u.text.trim() ? harnessNote(u.text) : null);
  }

  makeCard(call) { return call.name === 'agent' ? new Lane(this, call) : new ToolCard(this, call); }

  addCard(call) {
    const card = this.makeCard(call);
    this.cards.set(call.id, card);
    return card;
  }

  /**
   * Pairs running agent calls with the child chats they started. Children
   * appear in any order, so the pairing goes by title; a single call and a
   * single child left over belong together.
   */
  linkAgents() {
    const waiting = [...this.cards.values()].filter(c => c.name === 'agent' && !c.child && ['running', 'done'].includes(c.state));
    if (!waiting.length) return;
    const claimed = new Set([...this.cards.values()].map(c => c.child));
    const free = [...S.chats.values()].filter(c => c.parent === this.id && !claimed.has(c.id));
    const unmatched = waiting.filter(card => {
      const at = free.findIndex(c => c.title === (card.params.name || promptTitle(card.params.prompt)));
      if (at >= 0) card.setChild(free.splice(at, 1)[0].id);
      return at < 0;
    });
    if (unmatched.length === 1 && free.length === 1) unmatched[0].setChild(free[0].id);
  }

  // ---- the reply in progress

  /** steps are what the reply has put into work groups so far; its record replaces them. */
  startStream() {
    return this.stream ||= {text: '', thinking: '', cards: new Map(), steps: []};
  }

  /** A step of the reply in progress joins the open group, or a group of its own once the reply has text. */
  liveStep(s, step) {
    if (s.el && !s.tail) this.el.append((s.tail = new Work(this)).el);
    s.steps.push((s.tail || this.group()).add(step, s));
    return step;
  }

  startThinking(s) {
    const body = h('div.thinking-text'), label = h('span', 'Thinking'), timer = h('span.timer');
    const since = this.live?.state === 'thinking' ? Date.parse(this.live.since) : Date.now();
    s.think = this.liveStep(s, {kind: 'think', active: true, since, body, label, timer,
      el: h('details.thinking.active', {open: true}, h('summary', chevron(), label, timer), body)});
    this.tick();
  }

  /** Closes the thinking step. The duration is unknown when the page loaded after thinking ended. */
  endThinking(s, known = true) {
    const think = s.think;
    if (!think?.active) return;
    think.active = false;
    think.ms = known ? Date.now() - think.since : 0;
    think.el.classList.remove('active');
    think.el.open = false;
    think.label.textContent = known ? `Thought for ${fmtDuration(think.ms)}` : 'Thought';
    think.timer.textContent = '';
    think.work.refresh();
  }

  setThinking(s, text) {
    if (!s.think) this.startThinking(s);
    s.thinking = text;
    s.think.body.textContent = text;
    s.think.body.scrollTop = s.think.body.scrollHeight;
  }

  /** The reply's text takes its place in the chat; the steps before it are complete. */
  startText(s) {
    if (s.el) return;
    const prose = h('div.prose.streaming');
    s.markdown = new LiveMarkdown(prose);
    this.el.append(s.el = h('article.msg.assistant.live', prose));
    this.work?.refresh();
  }

  streamCard(s, tool) {
    const card = this.makeCard(tool);
    s.cards.set(tool.id, card);
    this.liveStep(s, {kind: 'tool', card, el: card.el});
    return card;
  }

  onDelta(d) {
    const s = this.startStream();
    if (d.kind === 'thinking') return this.setThinking(s, s.thinking + d.text);
    this.endThinking(s);
    if (d.kind === 'text') {
      s.text += d.text;
      this.paint();
    } else if (d.kind === 'tool') { // a name starts a call; text is more of its arguments
      const card = s.cards.get(d.id) || (d.name && this.streamCard(s, d));
      if (card && d.text) card.addArgs(d.text);
    }
  }

  /** Renders the streamed text at most once per frame. Blank text, which some models put before a call, is no reply yet. */
  paint() {
    const s = this.stream;
    if (!s.text.trim()) return;
    this.startText(s);
    this.frame ||= requestAnimationFrame(() => {
      this.frame = 0;
      if (!this.stream?.markdown) return;
      this.stream.markdown.update(this.stream.text);
      this.opts.onChange?.();
    });
  }

  /**
   * Removes the reply in progress, which its record replaces. Reports how
   * long it thought, and whether the user had opened the group that followed
   * its text, so that the record's own group opens the same way.
   */
  endStream() {
    const s = this.stream;
    if (!s) return {ms: 0, open: false};
    this.endThinking(s);
    cancelAnimationFrame(this.frame);
    this.frame = 0;
    this.stream = null;
    s.el?.remove();
    for (const step of s.steps) {
      step.work.remove(step);
      if (!step.work.steps.length && step.work !== this.work) step.work.el.remove(); // the open group stays for the record
    }
    return {ms: s.think?.ms || 0, open: Boolean(s.tail?.el.open)};
  }

  onLive(live) {
    this.live = live;
    if (!live) {
      this.endStream();
      for (const card of this.cards.values()) if (card.state === 'running') card.setState('stopped');
      this.closeWork();
    } else {
      // A request that starts over (first attempt or a retry) discards what the last one streamed.
      const fresh = live.state === 'thinking' && !live.text && !live.thinking && !live.tools?.length;
      if (fresh || live.state === 'retrying') this.endStream();
      for (const tool of live.tools || []) this.cards.get(tool.id)?.sync(tool);
      this.work?.refresh();
    }
    this.opts.onLive?.(live);
  }

  /**
   * Rebuilds the in-flight state after a (re)load. While tools run, the last
   * record is the assistant message that called them and live.text repeats it;
   * otherwise live.text and live.thinking are a reply that is not recorded yet.
   */
  restoreLive(live) {
    this.live = live;
    if (!live) {
      for (const card of this.cards.values()) if (card.state === 'running') card.setState('stopped');
      this.closeWork();
      return;
    }
    const tools = live.tools || [];
    const last = this.records.findLast(r => r.t === 'msg')?.msg;
    if (last?.role !== 'assistant' && (live.text || live.thinking || tools.some(t => !this.cards.has(t.id)))) {
      const s = this.startStream();
      if (live.thinking) this.setThinking(s, live.thinking);
      if (live.text || tools.length) this.endThinking(s, false);
      s.text = live.text || '';
      this.paint();
      for (const tool of tools) if (!this.cards.has(tool.id)) this.streamCard(s, tool);
    }
    for (const tool of tools) this.cards.get(tool.id)?.sync(tool);
    this.work?.refresh();
    this.opts.onLive?.(live);
  }
}
