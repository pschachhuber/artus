// settings.js — the Settings page and the first-run setup card.

import {S, bus, chevron, watch, h, icon, api, saveConfig, toast, confirmDialog, promptDialog, pageHead, modelName, fmtDate} from './lib.js';
import {modelPicker, docPane} from './pages.js';

const PROVIDERS = {anthropic: 'Anthropic', openai: 'OpenAI', openrouter: 'OpenRouter'};
const PASS_TOKEN = 'PROTON_PASS_PERSONAL_ACCESS_TOKEN';

const putSecret = (name, value) => api('PUT', '/api/secrets/' + encodeURIComponent(name), {value});
const field = (label, control, hint) => h('label.field', h('span.field-label', label), control, hint ? h('span.muted', hint) : null);

/** One provider: whether its key is set, a field to paste a new one, and (in Settings) a way to remove it. */
function providerRow(name, signal, removable) {
  const provider = () => S.providers.find(p => p.name === name) || {};
  const label = PROVIDERS[name] || name;
  const state = h('span.state'), error = h('p.error-text');
  const input = h('input.input', {type: 'password', autocomplete: 'off', placeholder: 'Paste API key', 'aria-label': `${label} API key`});
  const save = h('button.btn', {type: 'submit'});
  const remove = h('button.btn', {type: 'button', onclick: async () => {
    if (await confirmDialog(`Remove the ${label} key?`, 'Chats using its models stop working until you add a key again.', 'Remove key')) {
      api('DELETE', '/api/secrets/' + provider().key);
    }
  }}, 'Remove');
  const row = h('form.key-row', {onsubmit: async e => {
    e.preventDefault();
    const value = input.value.trim();
    input.value = '';
    if (value && await putSecret(provider().key, value)) toast(`${label} key saved`);
  }}, h('span.key-name', label), state, input, save, remove, error);
  watch(signal, () => `${provider().hasKey}|${S.modelErrors[name] || ''}`, () => {
    const has = Boolean(provider().hasKey);
    state.textContent = has ? 'set' : 'not set';
    state.className = 'state ' + (has ? 'active' : 'paused');
    save.textContent = has ? 'Replace' : 'Save';
    remove.hidden = !has || !removable;
    error.textContent = S.modelErrors[name] ? `Could not list its models: ${S.modelErrors[name]}` : '';
  });
  return row;
}

const pickDefaultModel = anchor => modelPicker(anchor, {selected: [S.config.model], onpick: model => saveConfig({model})});

/** Shown in the chat view until a provider has a key and a default model is chosen. */
export function setupCard(signal) {
  const pick = h('button.btn.primary', {type: 'button', onclick: () => pickDefaultModel(pick)}, 'Choose a model', icon('down', 14));
  const keyStep = h('li', h('h3', 'Add an API key'),
    h('p.muted', 'Paste a key from at least one provider. It is stored in your Artus home and never shown to the model.'),
    S.providers.filter(p => PROVIDERS[p.name]).map(p => providerRow(p.name, signal, false)));
  const modelStep = h('li', h('h3', 'Choose a default model'),
    h('p.muted', 'Used for new chats and tasks. You can switch per chat at any time.'), pick);
  watch(signal, () => `${S.providers.some(p => p.hasKey)}|${S.config.model}`, () => {
    const ready = S.providers.some(p => p.hasKey);
    keyStep.classList.toggle('done', ready);
    pick.disabled = !ready;
  });
  return h('section.setup', {'aria-label': 'Set up Artus'},
    h('h2', 'Welcome to Artus'),
    h('p', 'Two steps, then you can start your first chat.'),
    h('ol.steps', keyStep, modelStep));
}

// ---------------------------------------------------------------- sections

function modelsSection(el, signal) {
  const model = h('button.pill', {type: 'button', onclick: () => pickDefaultModel(model)});
  const effort = h('select.input', {'aria-label': 'Default effort', onchange: () => saveConfig({effort: effort.value})},
    S.efforts.map(e => h('option', e)));
  const allowed = h('div.chips');
  const add = h('button.btn', {type: 'button', onclick: () => modelPicker(add, {
    multi: true, selected: S.config.allowedModels, onpick: allowedModels => saveConfig({allowedModels}),
  })}, icon('plus', 15), 'Add models');
  watch(signal, () => JSON.stringify([S.config.model, S.config.effort, S.config.allowedModels, S.models?.length]), () => {
    model.replaceChildren(modelName(S.config.model), icon('down', 14));
    effort.value = S.config.effort;
    allowed.replaceChildren(...S.config.allowedModels.map(id => h('span.chip', h('span.chip-name', {title: id}, modelName(id)),
      h('button.chip-x', {type: 'button', 'aria-label': `Remove ${id}`, onclick: () => saveConfig({allowedModels: S.config.allowedModels.filter(m => m !== id)})}, icon('x', 12)))));
  });
  el.append(
    h('h2', 'Provider keys'),
    h('p.muted', 'Keys are stored in your Artus home. Artus only ever shows whether one is set.'),
    S.providers.filter(p => PROVIDERS[p.name]).map(p => providerRow(p.name, signal, true)),
    h('h2', 'Defaults'),
    h('div.field-grid', field('Default model', model, 'For new chats and tasks.'), field('Default effort', effort, 'How hard reasoning models think.')),
    h('h2', 'Models sub-agents may use'),
    h('p.muted', 'A sub-agent runs on the model of the chat that starts it. The agent may also pick one of these, for example a cheaper model for simple lookups.'),
    allowed, add);
}

function behaviourSection(el) {
  const c = S.config;
  const saved = ok => { if (ok) toast('Saved'); };
  const number = (label, hint, get, patch, attrs) => {
    const input = h('input.input.num', {type: 'number', value: get(S.config), ...attrs, onchange: async () => {
      if (input.value !== '') saved(await saveConfig(patch(Number(input.value))));
      input.value = get(S.config); // the server may have clamped it
    }});
    return field(label, input, hint);
  };
  const choice = (value, title, text) => h('label.choice',
    h('input', {type: 'radio', name: 'autonomy', value, checked: c.autonomy === value, onchange: async () => saved(await saveConfig({autonomy: value}))}),
    h('span', h('strong', title), h('span.muted', text)));
  const search = h('input', {type: 'checkbox', checked: c.webSearch, onchange: async () => saved(await saveConfig({webSearch: search.checked}))});
  el.append(
    h('h2', 'Autonomy'),
    h('div.choices', {role: 'radiogroup', 'aria-label': 'Autonomy'},
      choice('auto', 'Act on its own', 'Runs commands and edits files without asking.'),
      choice('confirm', 'Ask first in chats', 'Asks before commands and edits. Unattended task runs never ask.')),
    h('h2', 'Web search'),
    h('label.choice', search, h('span', h('strong', 'Let the model search the web'), h('span.muted', 'Uses the provider\'s built-in search where the model has one.'))),
    h('h2', 'Limits'),
    h('div.field-grid',
      number('Sub-agents at once', '1 to 64.', c => c.limits.subagents, v => ({limits: {subagents: v}}), {min: 1, max: 64}),
      number('Sub-agent depth', '0 turns sub-agents off; 4 at most.', c => c.limits.depth, v => ({limits: {depth: v}}), {min: 0, max: 4}),
      number('Model requests per run', 'A run stops after this many.', c => c.limits.turns, v => ({limits: {turns: v}}), {min: 1, max: 10000}),
      number('Token budget of a task run', '0 for no budget.', c => c.limits.taskTokens, v => ({limits: {taskTokens: v}}), {min: 0, step: 100000})),
    h('h2', 'Context'),
    h('div.field-grid',
      number('Compact at (% of the window)', 'The chat is summarised when its context grows past this.',
        c => Math.round(c.context.compactAt * 100), v => ({context: {compactAt: v / 100}}), {min: 6, max: 95}),
      number('Compact at the latest at (tokens)', 'A cap for models with very large windows. 0 for none.',
        c => c.context.compactMaxTokens, v => ({context: {compactMaxTokens: v}}), {min: 0, step: 10000}),
      number('Tool output kept (characters)', 'Longer results are cut and saved to a file the agent can page through.',
        c => c.context.toolOutputChars, v => ({context: {toolOutputChars: v}}), {min: 2000, max: 2000000, step: 1000})));
}

function instructionsSection(el) {
  el.append(
    h('h2', 'Instructions'),
    h('p.muted', 'Your standing rules. The agent reads them at the start of every conversation and cannot change them.'),
    docPane('instructions.md', {always: true}).el,
    h('h2', 'Memory'),
    h('p.muted', 'What the agent has chosen to remember about you and your setup. It edits memory.md itself; you can correct it here.'),
    docPane('memory.md', {empty: 'Nothing remembered yet.'}).el);
}

function secretsSection(el, signal) {
  const list = h('ul.rows'), passState = h('p.muted');
  const name = h('input.input', {placeholder: 'NAME', required: true, pattern: '[A-Za-z_][A-Za-z0-9_]*', autocomplete: 'off', 'aria-label': 'Secret name'});
  const value = h('input.input', {type: 'password', placeholder: 'Value, or pass://Vault/Item/field', required: true, autocomplete: 'off', 'aria-label': 'Secret value'});
  const token = h('input.input', {type: 'password', placeholder: 'Paste the agent token', required: true, autocomplete: 'off', 'aria-label': 'Proton Pass agent token'});
  const vault = h('input.input', {placeholder: 'Vault name', required: true, autocomplete: 'off', 'aria-label': 'Proton Pass vault'});

  const replace = async s => {
    const next = await promptDialog(`Replace ${s.name}`, 'New value, or a pass:// reference', '', 'Replace');
    if (next) putSecret(s.name, next);
  };
  const remove = async s => {
    if (await confirmDialog(`Delete ${s.name}?`, 'Commands and skills that use this secret will stop working.', 'Delete secret')) {
      api('DELETE', '/api/secrets/' + encodeURIComponent(s.name));
    }
  };
  const load = async () => {
    const res = await api('GET', '/api/secrets');
    if (!res) return;
    const secrets = res.secrets || [];
    list.replaceChildren(...secrets.map(s => h('li.row',
      h('span.row-main', h('code', s.name),
        s.reserved ? h('span.state', {title: 'A credential of Artus itself: never passed to commands the agent runs.'}, 'reserved') : null,
        s.reference ? h('span.state', 'reference') : null,
        s.reference ? h('span.muted', s.reference) : null),
      h('button.btn', {type: 'button', onclick: () => replace(s)}, 'Replace'),
      h('button.icon-btn', {type: 'button', title: 'Delete', 'aria-label': `Delete ${s.name}`, onclick: () => remove(s)}, icon('trash', 16)))));
    if (!secrets.length) list.replaceChildren(h('li.muted', 'No secrets yet.'));
    const hasToken = secrets.some(s => s.name === PASS_TOKEN);
    passState.textContent = res.backend === 'protonpass'
      ? `On: references resolve from the vault “${res.vault}”${hasToken ? '' : ', but the agent token is missing'}.`
      : 'Off: secrets are plain values in your Artus home.';
    if (document.activeElement !== vault) vault.value = res.vault || '';
  };
  bus.addEventListener('server', e => { if (['secrets', 'config'].includes(e.detail.type)) load(); }, {signal});
  load();

  el.append(
    h('h2', 'Secrets'),
    h('p.muted', 'The agent uses secrets by name, such as $GITHUB_TOKEN in a command. Values are injected when a command runs and scrubbed from everything the model reads.'),
    list,
    h('form.inline-form', {onsubmit: async e => {
      e.preventDefault();
      if (await putSecret(name.value.trim(), value.value)) name.value = '';
      value.value = '';
    }}, name, value, h('button.btn.primary', {type: 'submit'}, 'Add secret')),
    h('h2', 'Proton Pass'),
    h('p.muted', 'Keep values in Proton Pass and store only pass:// references here. Create a scoped agent token with ',
      h('code', 'pass-cli agent create artus --expiration 3m --vault <Vault>'), ', then paste it and name the vault.'),
    passState,
    h('form.inline-form', {onsubmit: async e => {
      e.preventDefault();
      const pasted = token.value.trim();
      token.value = '';
      if (pasted && await putSecret(PASS_TOKEN, pasted)) toast('Agent token saved');
    }}, token, h('button.btn', {type: 'submit'}, 'Save token')),
    h('form.inline-form', {onsubmit: async e => {
      e.preventDefault();
      if (await saveConfig({secrets: {backend: 'protonpass', vault: vault.value.trim()}})) toast('Proton Pass is on');
    }}, vault, h('button.btn', {type: 'submit'}, 'Use this vault'),
      h('button.btn', {type: 'button', onclick: () => saveConfig({secrets: {backend: 'env', vault: ''}})}, 'Turn off')));
}

/** A git patch with additions, removals and hunk headers tinted. */
function patchView(text) {
  const kind = line => /^(diff |index |--- |\+\+\+ |new file|deleted file|similarity|rename )/.test(line) ? '.meta'
    : line.startsWith('@@') ? '.hunk' : line[0] === '+' ? '.add' : line[0] === '-' ? '.del' : '';
  return h('pre.diff', {tabindex: 0}, text.replace(/\n$/, '').split('\n').map(line => h('span' + kind(line), line + '\n')));
}

function changesSection(el, signal) {
  const list = h('div.changes');
  const revert = async c => {
    if (!(await confirmDialog('Revert this change?', `“${c.message}” is undone by a new change. Later edits to the same lines can block it.`, 'Revert'))) return;
    if (await api('POST', `/api/changes/${c.hash}/revert`)) toast('Reverted');
  };
  const load = async () => {
    const res = await api('GET', '/api/changes');
    if (!res) return;
    list.replaceChildren(...res.changes.map(c => {
      const body = h('div.change-body');
      return h('details.change', {ontoggle: async e => {
        if (!e.target.open || body.firstChild) return;
        const diff = await api('GET', '/api/changes/' + c.hash);
        if (diff) body.replaceChildren(patchView(diff.diff), h('button.btn', {type: 'button', onclick: () => revert(c)}, icon('retry', 15), 'Revert this change'));
      }}, h('summary', chevron(), h('span.row-main', h('strong', c.message), h('span.muted', (c.files || []).join(', '))),
        h('span.muted.nowrap', fmtDate(c.time))), body);
    }));
    if (!res.changes.length) list.replaceChildren(h('p.muted', 'Nothing recorded yet. (The change log needs git to be installed.)'));
  };
  bus.addEventListener('server', e => { if (e.detail.type === 'changes') load(); }, {signal});
  load();
  el.append(h('h2', 'Changes'),
    h('p.muted', 'Every edit to memory, topics, skills, tools and tasks is recorded, whether the agent made it or you did. Open one to see what changed, and revert it if it was wrong.'),
    list);
}

function aboutSection(el) {
  const keys = [['Ctrl/⌘ K', 'New chat'], ['/', 'Search chats'], ['Enter', 'Send'], ['Shift Enter', 'New line'], ['Esc', 'Close a panel or menu']];
  el.append(h('h2', 'About'),
    h('dl.facts', h('dt', 'Version'), h('dd', S.version), h('dt', 'Home'), h('dd', h('code', S.home))),
    h('p.muted', 'Everything Artus knows lives in its home folder: chats, memory, topics, skills, tasks and secrets. This interface stores nothing but your theme choice.'),
    h('h2', 'Keyboard'),
    h('dl.facts', keys.flatMap(([key, what]) => [h('dt', h('kbd', key)), h('dd', what)])));
}

const SECTIONS = [
  ['models', 'Models', modelsSection], ['behaviour', 'Behaviour', behaviourSection], ['instructions', 'Instructions', instructionsSection],
  ['secrets', 'Secrets', secretsSection], ['changes', 'Changes', changesSection], ['about', 'About', aboutSection],
];

export function settingsView(root, id) {
  let abort = null;
  const tabs = h('nav.tabs', {'aria-label': 'Settings sections'});
  const body = h('div.page-body.settings');
  root.append(h('section.page', pageHead('Settings'), tabs, body));
  const show = current => {
    abort?.abort();
    abort = new AbortController();
    const [key, title, render] = SECTIONS.find(s => s[0] === current) || SECTIONS[0];
    tabs.replaceChildren(...SECTIONS.map(s => h('a', {href: '#/settings/' + s[0], 'aria-current': s[0] === key ? 'page' : null}, s[1])));
    body.replaceChildren();
    render(body, abort.signal);
    document.title = `${title} — Settings — Artus`;
    return true;
  };
  show(id);
  return {destroy: () => abort.abort(), update: show};
}
