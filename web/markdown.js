// markdown.js — a small, safe Markdown renderer.
//
// Model output, tool results, web pages and files are untrusted. Nothing in
// this file assigns markup: the source is parsed into blocks and every piece
// of text ends up in a text node, so no input can become an element or an
// attribute we did not choose. Links are limited to http, https and mailto;
// images load only from the Artus home, never from the network.

import {S, h, icon, copyButton, fileUrl, lightbox} from './lib.js';

const FENCE = /^ {0,3}(`{3,}|~{3,})\s*([^\s`]*)/;
const HEADING = /^ {0,3}(#{1,6})\s+(.*?)(?:\s+#+)?\s*$/;
const RULE = /^ {0,3}([-*_])(?:\s*\1){2,}\s*$/;
const QUOTE = /^ {0,3}>\s?/;
const ITEM = /^(\s*)([-*+]|\d{1,9}[.)])\s+(.*)$/;
const TABLE_RULE = /^\s*\|?\s*:?-+:?\s*(?:\|\s*:?-+:?\s*)*\|?\s*$/;

const isTableStart = (lines, i) => lines[i].includes('|') && i + 1 < lines.length && TABLE_RULE.test(lines[i + 1]) && lines[i + 1].includes('-');
const startsBlock = (lines, i) => FENCE.test(lines[i]) || HEADING.test(lines[i]) || RULE.test(lines[i]) || QUOTE.test(lines[i]) || ITEM.test(lines[i]) || isTableStart(lines, i);
const indentOf = line => line.length - line.trimStart().length;

/** Splits source into blocks. Each block keeps its source so an unchanged one can be reused while streaming. */
function parseBlocks(src) {
  const lines = src.replace(/\r\n?/g, '\n').split('\n');
  const blocks = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i], start = i;
    let m, block;
    if (!line.trim()) { i++; continue; }
    if ((m = FENCE.exec(line))) {
      const close = new RegExp(`^ {0,3}${m[1][0]}{${m[1].length},}\\s*$`);
      const body = [];
      for (i++; i < lines.length && !close.test(lines[i]); i++) body.push(lines[i]);
      i++; // the closing fence; past the end while the block is still streaming
      block = {type: 'code', lang: m[2], text: body.join('\n')};
    } else if ((m = HEADING.exec(line))) {
      i++;
      block = {type: 'heading', level: m[1].length, text: m[2]};
    } else if (RULE.test(line)) {
      i++;
      block = {type: 'rule'};
    } else if (QUOTE.test(line)) {
      const body = [];
      for (; i < lines.length && lines[i].trim() && (QUOTE.test(lines[i]) || !startsBlock(lines, i)); i++) body.push(lines[i].replace(QUOTE, ''));
      block = {type: 'quote', blocks: parseBlocks(body.join('\n'))};
    } else if (ITEM.test(line)) {
      [block, i] = parseList(lines, i);
    } else if (isTableStart(lines, i)) {
      const rows = [];
      for (; i < lines.length && lines[i].trim() && lines[i].includes('|'); i++) rows.push(splitRow(lines[i]));
      const align = rows[1].map(c => /^:-+:$/.test(c) ? 'center' : /-:$/.test(c) ? 'right' : '');
      block = {type: 'table', head: rows[0], align, rows: rows.slice(2)};
    } else {
      for (i++; i < lines.length && lines[i].trim() && !startsBlock(lines, i); i++);
      block = {type: 'p', text: lines.slice(start, i).join('\n').trim()};
    }
    block.src = lines.slice(start, i).join('\n');
    blocks.push(block);
  }
  return blocks;
}

function splitRow(line) {
  const cells = line.trim().replace(/^\|/, '').replace(/\|$/, '').split(/(?<!\\)\|/);
  return cells.map(c => c.trim().replace(/\\\|/g, '|'));
}

/** Parses a list starting at line i; nested content is parsed as blocks of its own. */
function parseList(lines, i) {
  const first = ITEM.exec(lines[i]);
  const indent = first[1].length, ordered = /\d/.test(first[2]);
  const sibling = m => m && m[1].length >= indent && m[1].length <= indent + 1 && /\d/.test(m[2]) === ordered;
  const items = [];
  while (i < lines.length) {
    const m = ITEM.exec(lines[i]);
    if (!sibling(m)) break;
    const body = [m[3]], column = m[1].length + m[2].length + 1;
    for (i++; i < lines.length; i++) {
      const line = lines[i];
      if (!line.trim()) { // a blank line stays in the item only when indented content follows
        let next = i + 1;
        while (next < lines.length && !lines[next].trim()) next++;
        if (next === lines.length || indentOf(lines[next]) <= indent + 1) break;
        body.push('');
      } else if (indentOf(line) > indent + 1) {
        body.push(line.slice(Math.min(indentOf(line), column)));
      } else if (startsBlock(lines, i)) {
        break;
      } else {
        body.push(line.trim()); // a paragraph line that was not indented
      }
    }
    const task = /^\[([ xX])\]\s+(.*)$/s.exec(body[0]);
    if (task) body[0] = task[2];
    items.push({done: task ? task[1] !== ' ' : null, blocks: parseBlocks(body.join('\n'))});
    let next = i;
    while (next < lines.length && !lines[next].trim()) next++;
    if (next > i && sibling(ITEM.exec(lines[next] || ''))) i = next; // a loose list: skip the gap
  }
  return [{type: 'list', ordered, start: ordered ? parseInt(first[2], 10) : 1, items}, i];
}

// ---------------------------------------------------------------- inline

const INLINE = new RegExp([
  /\\(?<esc>[\\`*_{}[\]()#+\-.!|~<>])/,
  /(?<!`)(?<ticks>`+)(?!`)(?<code>[\s\S]*?[^`])\k<ticks>(?!`)/,
  /!\[(?<alt>[^\]]*)\]\(\s*<?(?<img>(?:\([^()\s]*\)|[^()\s<>])+)>?(?:\s+"[^"]*")?\s*\)/,
  /\[(?<label>(?:\\.|[^[\]\\]|\[[^\]]*\])+)\]\(\s*<?(?<href>(?:\([^()\s]*\)|[^()\s<>])+)>?(?:\s+"[^"]*")?\s*\)/,
  /\[(?:secret|totp):(?<secret>[A-Za-z_][A-Za-z0-9_]*)\]/,
  /\*\*(?=\S)(?<bold>[\s\S]+?)(?<=\S)\*\*(?!\*)/,
  /(?<![A-Za-z0-9_])__(?=\S)(?<bold2>[\s\S]+?)(?<=\S)__(?![A-Za-z0-9_])/,
  /\*(?=[^\s*])(?<em>[^*]+?)(?<=\S)\*(?!\*)/,
  /(?<![A-Za-z0-9_])_(?=[^\s_])(?<em2>[^_]+?)(?<=\S)_(?![A-Za-z0-9_])/,
  /~~(?=\S)(?<strike>[\s\S]+?)(?<=\S)~~/,
  /<(?<auto>(?:https?:\/\/|mailto:)[^\s<>]+)>/,
  /(?<![A-Za-z0-9])(?<url>https?:\/\/(?:\([^\s<>()]*\)|[^\s<>"'`()])*(?:\([^\s<>()]*\)|[^\s<>"'`().,;:!?\]}*_~]))/,
].map(r => r.source).join('|'), 'g');

const SECRET = /\[(?:secret|totp):([A-Za-z_][A-Za-z0-9_]*)\]/g;

const secretPill = name => h('span.secret', {title: 'A secret. Its value is never shown.'}, icon('key', 12), name);

/** Plain text as nodes, with [secret:NAME] placeholders shown as pills. */
export function plain(text) {
  const out = document.createDocumentFragment();
  let last = 0;
  for (const m of text.matchAll(SECRET)) {
    out.append(text.slice(last, m.index), secretPill(m[1]));
    last = m.index + m[0].length;
  }
  out.append(text.slice(last));
  return out;
}

function appendText(into, text) {
  text.split('\n').forEach((part, i) => {
    if (i) into.append(h('br'));
    if (part) into.append(part);
  });
}

/** A link to an allowed scheme; anything else keeps its text and loses the target. */
function link(href, content) {
  const url = href.trim();
  const a = /^(https?:\/\/|mailto:)/i.test(url) ? h('a', {href: url, target: '_blank', rel: 'noopener noreferrer'}) : h('span');
  a.append(content);
  return a;
}

/** An image is shown only when it is a file inside the Artus home. */
function image(src, alt) {
  const remote = /^[a-z][a-z0-9+.-]+:/i.test(src) || src.startsWith('//');
  const outside = src.startsWith('/') && !src.startsWith(S.home + '/');
  if (remote || outside) return link(src, alt || src);
  const img = h('img.md-img', {src: fileUrl(src), alt, loading: 'lazy', onclick: () => lightbox(img.src, alt),
    onerror: () => img.replaceWith(h('span.muted', alt || src))});
  return img;
}

/** Appends the inline rendering of text to `into` and returns it. Inside a link, nested links stay text. */
export function inline(text, into = document.createDocumentFragment(), inLink = false) {
  const nest = (inner, el, linked = inLink) => inline(inner, el, linked);
  let last = 0;
  for (const m of text.matchAll(INLINE)) {
    const g = m.groups, url = g.auto ?? g.url;
    appendText(into, text.slice(last, m.index));
    last = m.index + m[0].length;
    if (g.esc) into.append(g.esc);
    else if (g.code !== undefined) into.append(h('code', plain(g.code.replace(/^ (.+) $/s, '$1'))));
    else if (g.img !== undefined) into.append(image(g.img, g.alt));
    else if (g.secret) into.append(secretPill(g.secret));
    else if (g.bold ?? g.bold2) into.append(nest(g.bold ?? g.bold2, h('strong')));
    else if (g.em ?? g.em2) into.append(nest(g.em ?? g.em2, h('em')));
    else if (g.strike) into.append(nest(g.strike, h('del')));
    else if (inLink) into.append(g.href !== undefined ? nest(g.label, h('span')) : url);
    else if (g.href !== undefined) into.append(link(g.href, nest(g.label, document.createDocumentFragment(), true)));
    else into.append(link(url, url));
  }
  appendText(into, text.slice(last));
  return into;
}

// ---------------------------------------------------------------- blocks

function renderBlock(b) {
  switch (b.type) {
    case 'heading': return inline(b.text, h('h' + Math.min(b.level + 1, 6))); // the page owns h1
    case 'rule': return h('hr');
    case 'quote': return h('blockquote', b.blocks.map(renderBlock));
    case 'p': return inline(b.text, h('p'));
    case 'code': return h('div.code',
      h('div.code-head', h('span', b.lang || 'text'), copyButton(() => b.text)),
      h('pre', {tabindex: 0}, h('code', plain(b.text))));
    case 'table': {
      const cell = (tag, text, i) => inline(text, h(tag + (b.align[i] ? '.' + b.align[i] : '')));
      return h('div.table-wrap', {tabindex: 0}, h('table',
        h('thead', h('tr', b.head.map((c, i) => cell('th', c, i)))),
        h('tbody', b.rows.map(row => h('tr', b.head.map((_, i) => cell('td', row[i] || '', i)))))));
    }
    case 'list': {
      const list = h(b.ordered ? 'ol' : 'ul', {start: b.ordered && b.start !== 1 ? b.start : null});
      for (const item of b.items) {
        const li = h('li' + (item.done === null ? '' : '.task'));
        if (item.done !== null) li.append(h('input', {type: 'checkbox', checked: item.done, disabled: true, 'aria-label': item.done ? 'Done' : 'Open'}));
        item.blocks.forEach((child, i) => {
          if (i === 0 && child.type === 'p') inline(child.text, li); // tight: no paragraph box
          else li.append(renderBlock(child));
        });
        list.append(li);
      }
      return list;
    }
  }
}

/** Renders complete Markdown into a fragment. */
export function renderMarkdown(src) {
  const out = document.createDocumentFragment();
  out.append(...parseBlocks(src).map(renderBlock));
  return out;
}

const FADE_MS = 320;

/**
 * Markdown that is still arriving. Blocks whose source did not change keep
 * their nodes (so selection and scroll positions survive); the block being
 * written is re-rendered, with the newest characters fading in and a caret
 * at the end.
 */
export class LiveMarkdown {
  constructor(el) {
    this.el = el;
    this.blocks = [];
    this.fresh = []; // [{from, at}]: text of the last block that arrived recently
    this.length = 0; // rendered characters of the last block
  }

  update(src) {
    const blocks = parseBlocks(src), now = performance.now();
    blocks.forEach((b, i) => {
      const old = this.blocks[i];
      if (old && old.type === b.type && old.src === b.src) { b.node = old.node; return; }
      b.node = renderBlock(b);
      if (old) old.node.replaceWith(b.node);
      else this.el.append(b.node);
    });
    this.blocks.slice(blocks.length).forEach(old => old.node.remove());
    const last = blocks.at(-1);
    if (last && last.node !== this.blocks.at(-1)?.node) { // the tail was rendered anew
      if (blocks.length !== this.blocks.length) { this.fresh = []; this.length = 0; }
      const length = last.node.textContent.length;
      if (length > this.length) this.fresh.push({from: this.length, at: now});
      this.length = length;
      this.fresh = this.fresh.filter(c => now - c.at < FADE_MS);
      this.el.querySelector('.caret')?.remove();
      let tip = last.node;
      while (tip.lastChild?.nodeType === 1 && !tip.lastChild.matches('br, img, input, .code-head, .secret')) tip = tip.lastChild;
      if (!tip.matches('hr, table, thead, tbody, tr, ul, ol')) tip.append(h('span.caret'));
      fadeTail(last.node, this.fresh, now);
    }
    this.blocks = blocks;
  }
}

/** Wraps the recently arrived ranges of root's text so each continues its own fade. */
function fadeTail(root, fresh, now) {
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  const nodes = [];
  while (walker.nextNode()) nodes.push(walker.currentNode);
  let offset = 0;
  for (const node of nodes) {
    const start = offset;
    offset += node.data.length;
    for (let i = fresh.length - 1; i >= 0; i--) {
      const from = Math.max(fresh[i].from - start, 0);
      if (from >= node.data.length) continue;
      const tail = from > 0 ? node.splitText(from) : node;
      const span = h('span.fresh');
      span.style.setProperty('animation-delay', `${Math.round(fresh[i].at - now)}ms`);
      tail.replaceWith(span);
      span.append(tail);
      if (from === 0) break;
    }
  }
}
