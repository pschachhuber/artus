// Body of `async function(op, arg)`, run in an isolated world. `this` is the target
// node, or undefined for document-level operations. Results are plain JSON.
const node = this && this.nodeType ? this : document;
const element = node.nodeType === 1 ? node : node.parentElement;
const win = (node.ownerDocument || node).defaultView || window;

function parentOrHost(e) {
  if (e.assignedSlot) return e.assignedSlot;
  if (e.parentElement) return e.parentElement;
  const root = e.parentNode;
  return root && root.nodeType === 11 && root.host ? root.host : null;
}

function closestComposed(e, selector) {
  for (; e; e = parentOrHost(e)) if (e.matches(selector)) return e;
  return null;
}

// retarget maps the node an agent pointed at to the element that carries the
// behaviour: the button around an icon, the control of a label.
function retarget(e, mode) {
  if (!e) return null;
  if (!e.matches('input, textarea, select') && !e.isContentEditable) {
    const selector = mode === 'button' ? 'button, [role=button], a, [role=link]' : 'button, [role=button], [role=checkbox], [role=radio]';
    e = e.closest(selector) || e;
  }
  if (mode === 'label' && !e.matches('a, input, textarea, button, select, option, optgroup, [role=link], [role=button], [role=checkbox], [role=radio]') && !e.isContentEditable) {
    const label = e.closest('label');
    if (label && label.control) e = label.control;
  }
  return e;
}

function isVisible(e) {
  const style = win.getComputedStyle(e);
  if (style.display === 'contents') {
    for (let c = e.firstChild; c; c = c.nextSibling) {
      if (c.nodeType === 1 && isVisible(c)) return true;
      if (c.nodeType === 3 && c.nodeValue.trim()) {
        const range = e.ownerDocument.createRange();
        range.selectNode(c);
        const r = range.getBoundingClientRect();
        if (r.width > 0 && r.height > 0) return true;
      }
    }
    return false;
  }
  if (!e.checkVisibility() || style.visibility !== 'visible') return false;
  const rect = e.getBoundingClientRect();
  return rect.width > 0 && rect.height > 0;
}

function isDisabled(e) {
  return e.matches(':disabled') || !!closestComposed(e, '[aria-disabled=true]');
}

function isReadOnly(e) {
  if (e.matches('input, textarea, select')) return e.hasAttribute('readonly');
  return e.getAttribute('aria-readonly') === 'true';
}

// stable resolves true once the element's box is the same on two consecutive
// animation frames. Hidden tabs do not run animation frames, so a timer
// stands in for them.
function stable(e) {
  return new Promise(resolve => {
    let last, done = false, fallback;
    const sample = () => {
      if (done) return;
      clearTimeout(fallback);
      if (!e.isConnected) { done = true; return resolve(false); }
      const r = e.getBoundingClientRect();
      const rect = [r.top, r.left, r.width, r.height].join();
      if (last !== undefined) { done = true; return resolve(rect === last); }
      last = rect;
      win.requestAnimationFrame(sample);
      fallback = setTimeout(sample, 100);
    };
    win.requestAnimationFrame(sample);
    fallback = setTimeout(sample, 100);
  });
}

function describe(e) {
  let out = '<' + e.localName;
  for (const name of ['id', 'class', 'role', 'name', 'type', 'aria-label']) {
    const value = e.getAttribute(name);
    if (value) out += ' ' + name + '="' + (value.length > 40 ? value.slice(0, 40) + '…' : value) + '"';
  }
  const text = (e.textContent || '').replace(/\s+/g, ' ').trim();
  return out + '>' + (text.length > 40 ? text.slice(0, 40) + '…' : text) + (text ? '</' + e.localName + '>' : '');
}

// hitTarget checks that a click at the point lands on the target or inside
// it, looking through shadow roots, and names what would get it otherwise.
function hitTarget(target, x, y) {
  const roots = [];
  for (let e = target; e;) {
    const root = e.getRootNode();
    roots.push(root);
    if (root.nodeType === 9 || !root.host) break;
    e = root.host;
  }
  let hit;
  for (let i = roots.length - 1; i >= 0; i--) {
    const inner = roots[i].elementsFromPoint(x, y)[0];
    if (!inner) break;
    hit = inner;
    if (i && inner !== roots[i - 1].host) break;
  }
  const first = hit;
  for (; hit && hit !== target; hit = parentOrHost(hit)) {
    // A click on a label is forwarded to its control.
    if (hit.localName === 'label' && hit.control === target) return {};
  }
  if (hit === target) return {};
  return { blocker: first ? describe(first) : 'nothing (the point is outside of the frame)' };
}

function checkedState(e) {
  if (!e) return { error: 'notconnected' };
  if (e.localName === 'input' && (e.type === 'checkbox' || e.type === 'radio'))
    return { checked: e.indeterminate ? 'mixed' : e.checked, radio: e.type === 'radio' };
  if (['checkbox', 'menuitemcheckbox', 'option', 'radio', 'switch', 'menuitemradio', 'treeitem'].includes(e.getAttribute('role'))) {
    const value = e.getAttribute('aria-checked');
    return { checked: value === 'mixed' ? 'mixed' : value === 'true', radio: e.getAttribute('role') === 'radio' };
  }
  return { error: 'Not a checkbox or radio button' };
}

function selectText(e) {
  if (e.localName === 'input') { e.select(); e.focus(); return; }
  if (e.localName === 'textarea') { e.selectionStart = 0; e.selectionEnd = e.value.length; e.focus(); return; }
  let host = e;
  while (host.isContentEditable && host.parentElement && host.parentElement.isContentEditable) host = host.parentElement;
  host.focus();
  const range = e.ownerDocument.createRange();
  range.selectNodeContents(e);
  const selection = win.getSelection();
  if (selection) { selection.removeAllRanges(); selection.addRange(range); }
}

function fill(e, value) {
  if (e.localName === 'input') {
    const type = e.type.toLowerCase();
    const settable = ['color', 'date', 'time', 'datetime-local', 'month', 'range', 'week'];
    const typable = ['', 'email', 'number', 'password', 'search', 'tel', 'text', 'url'];
    if (!settable.includes(type) && !typable.includes(type)) return { error: `Input of type "${type}" cannot be filled` };
    if (type === 'number' && isNaN(Number(value.trim()))) return { error: 'Cannot type text into input[type=number]' };
    if (settable.includes(type)) {
      value = value.trim();
      e.focus();
      e.value = value;
      if (e.value !== value) return { error: `Malformed value for input of type "${type}"` };
      e.dispatchEvent(new Event('input', { bubbles: true, composed: true }));
      e.dispatchEvent(new Event('change', { bubbles: true }));
      return { result: 'done' };
    }
  } else if (e.localName !== 'textarea' && !e.isContentEditable) {
    return { error: 'Element is not an <input>, <textarea> or [contenteditable] element' };
  }
  selectText(e);
  return { result: 'needsinput' };
}

function selectOptions(e, wanted) {
  if (e.localName !== 'select') return { error: 'Element is not a <select> element' };
  const norm = s => s.replace(/\s+/g, ' ').trim();
  let remaining = wanted.slice();
  const chosen = [];
  for (const option of e.options) {
    const matches = w => w === option.value || w === option.label || norm(w) === norm(option.label);
    if (!remaining.some(matches)) continue;
    if (isDisabled(option)) return { retry: 'the option to select is disabled' };
    chosen.push(option);
    if (!e.multiple) { remaining = []; break; }
    remaining = remaining.filter(w => !matches(w));
  }
  if (remaining.length) return { retry: 'no option matches ' + JSON.stringify(remaining[0]), options: [...e.options].slice(0, 30).map(o => o.label) };
  e.value = undefined;
  for (const option of chosen) option.selected = true;
  e.dispatchEvent(new Event('input', { bubbles: true, composed: true }));
  e.dispatchEvent(new Event('change', { bubbles: true }));
  return { values: chosen.map(o => o.label) };
}

const inlineTags = new Set(['a', 'abbr', 'b', 'bdi', 'bdo', 'cite', 'code', 'em', 'font', 'i', 'kbd', 'label', 'mark', 'q', 's', 'samp', 'small', 'span', 'strong', 'sub', 'sup', 'time', 'u', 'var']);

// visibleText collects the rendered text of a document, shadow roots included.
function visibleText(root) {
  let out = '';
  const walk = n => {
    if (n.nodeType === 3) { out += n.nodeValue; return; }
    if (n.nodeType !== 1 && n.nodeType !== 11) return;
    if (n.nodeType === 1) {
      if (['script', 'style', 'noscript', 'template'].includes(n.localName)) return;
      if (!n.checkVisibility({ visibilityProperty: true })) return;
    }
    const block = n.nodeType === 1 && !inlineTags.has(n.localName);
    if (block) out += ' ';
    if (n.shadowRoot) walk(n.shadowRoot);
    for (let c = n.firstChild; c; c = c.nextSibling) walk(c);
    if (block) out += ' ';
  };
  walk(root);
  return out.replace(/\s+/g, ' ');
}

switch (op) {
  case 'state': {
    if (!element || !element.isConnected) return { error: 'notconnected' };
    const hidden = element.ownerDocument.visibilityState === 'hidden';
    const control = retarget(element, 'label');
    for (const state of arg.states) {
      if (state === 'visible' && !isVisible(element)) return { missing: 'visible', hidden };
      if (state === 'enabled' && isDisabled(control)) return { missing: 'enabled' };
      if (state === 'editable' && (isDisabled(control) || isReadOnly(control))) return { missing: 'editable' };
      if (state === 'stable' && !(await stable(retarget(element, 'nolabel')))) return { missing: 'stable', hidden };
    }
    if (closestComposed(control, '[inert]')) return { missing: 'interactive' };
    return { hidden, dropdown: control.localName === 'select' && !control.multiple && control.size <= 1 };
  }
  case 'scroll':
    element.scrollIntoView({ block: arg.block, inline: arg.block, behavior: 'instant' });
    return {};
  case 'hit':
    return hitTarget(retarget(element, 'button'), arg.x, arg.y);
  case 'describe':
    return { text: describe(element) };
  case 'checked':
    return checkedState(retarget(element, 'label'));
  case 'fill':
    return fill(retarget(element, 'label'), arg.value);
  case 'select':
    return selectOptions(retarget(element, 'label'), arg.values);
  case 'focus':
    element.focus();
    return {};
  case 'hasText':
    return { found: visibleText(node.body || node).toLowerCase().includes(arg.text.replace(/\s+/g, ' ').trim().toLowerCase()) };
  case 'localStorage':
    return { items: Object.keys(localStorage).map(name => ({ name, value: localStorage.getItem(name) })) };
  case 'armDrag':
    // Resolves after the next mouse move with whether it started a drag.
    globalThis.__artusDrag = new Promise(resolve => {
      let started = null;
      win.addEventListener('dragstart', e => { started = e; }, { once: true, capture: true });
      win.addEventListener('mousemove', () => setTimeout(() => resolve({ drag: !!started && !started.defaultPrevented }), 0), { once: true, capture: true });
      setTimeout(() => resolve({ drag: false }), 2000);
    });
    return {};
  case 'dragStarted':
    return globalThis.__artusDrag;
}
return { error: 'unknown operation ' + op };
