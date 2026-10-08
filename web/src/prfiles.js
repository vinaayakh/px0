// web/src/prfiles.js
// The PR "Files changed" tab (pr://files), active only in PR sessions: every
// file the PR changes, stacked, with a file list beside them, like GitHub's
// own Files changed page. The diff is the PR's own change (base..head, what
// a review is posted against) from /api/pr/files (prfiles.go), drawn with
// diff.js's row markup.
//
// Comments live on their lines, as GitHub draws them: comments already
// posted, the reviewer's drafts, pending AI suggestions (review.js) and the
// composer the "+" beside a line number opens. An AI suggestion's card is
// where it is triaged: add it to the review (a draft, posted with the next
// review), post it now on its own, edit it first, or dismiss it. The AI
// Review tab sends a clicked suggestion here through 'files:reveal'.
import { $, S, esc, api, apiPostJson, keyLabel } from './state.js';
import { on, emit } from './bus.js';
import { showToast } from './ui.js';
import { registerVirtualTab } from './virtualtab.js';
import { virtualArticle, sanitizeForgeHTML } from './markdown.js';
import { openFile } from './tabs.js';
import { diffHunksFragment, layoutPref, setLayoutPref } from './diff.js';
import { prDrafts, prPostedReviewComments, prSessionMeta, refreshComments, refreshExistingComments, deleteDraft, nudgeGitHubToken } from './pr.js';
import { reviewSuggestions, triageSuggestions, reloadSuggestions } from './review.js';

const PF_PATH = 'pr://files';
const PF_STATUS = { added: ['A', 'Added'], deleted: ['D', 'Deleted'], renamed: ['R', 'Renamed'], modified: ['M', 'Modified'] };

let pfData = null;        // {files, base, head, truncated} from /api/pr/files
let pfError = '';
let pfLoading = null;     // the load in flight
let pfMode = 'split';     // 'split' or 'unified', shared with the single-file diff
let pfFilter = '';        // file list filter
let pfRoot = null;        // the drawn page
let pfFocusId = 0;        // AI suggestion (or the draft it became) called out
let pfRangeFrom = null;   // last "+" clicked: {path, side, line}, for shift-click ranges
let pfReveal = null;      // {path, side, line, id} waiting for the page to draw
const pfCollapsed = new Set();   // file paths folded shut
const pfFull = new Map();        // path -> hunks of a large file, loaded on request
const pfRows = new Map();        // path -> Map('SIDE:line' -> {box, els})
const pfComposers = new Map();   // 'path|SIDE|line' -> {path, side, line, startLine, el}
const pfEditors = new Map();     // 'ai:id' or 'draft:id' -> editor element

export function initPRFiles() {
  if (!S.meta?.pr) return;
  pfMode = layoutPref() === 'unified' ? 'unified' : 'split';
  registerVirtualTab(PF_PATH, { title: () => 'Files changed', pinned: true, render: pfRender });
  on('pr:comments-changed', pfRedrawThreads);
  on('review:changed', pfOnSuggestions);
  on('pr:refreshed', pfReload); // a Pull moved the head: the diff is different now
  on('files:reveal', t => { pfReveal = t; pfTryReveal(); });
}

// review.js renders (and says so) every second while a run is going; redraw
// only when the suggestions themselves changed, so a box being typed in
// stays put.
let pfAiSig = '';
function pfOnSuggestions() {
  const sig = reviewSuggestions().map(s => s.id + ':' + s.status + ':' + s.line + ':' + s.body.length + ':' + (s.ai?.stale ? 1 : 0)).join(',');
  if (sig === pfAiSig) return;
  pfAiSig = sig;
  pfRedrawThreads();
}

/* ---------- data ---------- */

function pfLoad() {
  if (!pfLoading) {
    pfLoading = api('/api/pr/files')
      .then(j => { pfData = j; pfError = ''; })
      .catch(e => { pfError = e.message || 'Could not load the PR diff'; })
      .finally(() => { pfLoading = null; });
  }
  return pfLoading;
}

async function pfReload() {
  pfData = null;
  pfFull.clear();
  await pfLoad();
  const a = pfArticle();
  if (a) pfDraw(a);
}

function pfArticle() {
  return virtualArticle(S.tabs.find(t => t.path === PF_PATH));
}

async function pfRender(article) {
  article.classList.add('pf-host');
  if (!pfData) {
    article.innerHTML = '<div class="pf-msg">Loading the PR diff…</div>';
    await pfLoad();
    if (pfArticle() !== article) return;
  }
  pfDraw(article);
}

function pfFileHunks(f) {
  return pfFull.get(f.path) || f.hunks || [];
}

/* ---------- drawing the page ---------- */

function pfDraw(article) {
  pfRows.clear();
  const root = document.createElement('div');
  root.className = 'pf-root';
  if (pfError || !pfData) {
    root.innerHTML = '<div class="pf-msg pf-err">' + esc(pfError || 'No diff.') + ' <button class="opt" type="button" data-pf-retry>Retry</button></div>';
    root.addEventListener('click', e => { if (e.target.closest('[data-pf-retry]')) pfReload(); });
    article.replaceChildren(root);
    pfRoot = root;
    return;
  }
  const files = pfData.files || [];
  const adds = files.reduce((n, f) => n + f.additions, 0);
  const dels = files.reduce((n, f) => n + f.deletions, 0);
  root.innerHTML =
    '<div class="pf-bar">' +
      '<span class="pf-bar-title">' + files.length + (files.length === 1 ? ' file' : ' files') + ' changed</span>' +
      '<span class="pf-add">+' + adds + '</span><span class="pf-del">−' + dels + '</span>' +
      '<span class="grow"></span>' +
      '<span class="pf-ai-nav" hidden><span class="pf-ai-count"></span>' +
        '<button class="opt" type="button" data-pf-ai="-1" title="Previous AI suggestion">↑</button>' +
        '<button class="opt" type="button" data-pf-ai="1" title="Next AI suggestion">↓</button></span>' +
      '<span class="pf-seg" role="group" aria-label="Diff layout">' +
        '<button class="opt' + (pfMode === 'split' ? ' on' : '') + '" type="button" data-pf-mode="split">Split</button>' +
        '<button class="opt' + (pfMode === 'unified' ? ' on' : '') + '" type="button" data-pf-mode="unified">Unified</button></span>' +
      '<button class="opt" type="button" data-pf-fold-all title="Collapse or expand every file">Collapse all</button>' +
      '<button class="footer-btn pf-finish" type="button" data-pf-finish title="Write the review summary and submit, in the PR bar above">Finish your review</button>' +
    '</div>' +
    (pfData.truncated ? '<div class="pf-msg pf-warn">The diff is too large to show whole; the last files are left out.</div>' : '') +
    '<div class="pf-layout">' +
      '<nav class="pf-tree"><input class="pf-filter" type="text" spellcheck="false" autocomplete="off" placeholder="Filter changed files" value="' + esc(pfFilter) + '">' +
      '<div class="pf-tree-list"></div></nav>' +
      '<div class="pf-files"><div class="pf-notes pf-summary-notes" hidden></div></div>' +
    '</div>';
  const list = root.querySelector('.pf-files');
  if (!files.length) list.insertAdjacentHTML('beforeend', '<div class="pf-msg">This PR changes no files.</div>');
  for (const f of files) list.append(pfFileSection(f));
  pfWire(root);
  article.replaceChildren(root);
  pfRoot = root;
  pfApplyFilter();
  pfRedrawThreads();
  pfTryReveal();
}

function pfFileSection(f) {
  const sec = document.createElement('section');
  sec.className = 'pf-file' + (pfCollapsed.has(f.path) ? ' collapsed' : '');
  sec.dataset.path = f.path;
  const [letter, label] = PF_STATUS[f.status] || PF_STATUS.modified;
  const name = f.oldPath ? esc(f.oldPath) + ' <span class="pf-arrow">→</span> ' + esc(f.path) : esc(f.path);
  sec.innerHTML =
    '<header class="pf-file-head">' +
      '<button class="pf-chev" type="button" data-pf-fold title="Collapse or expand">▾</button>' +
      '<span class="pf-st pf-st-' + esc(f.status) + '" title="' + label + '">' + letter + '</span>' +
      '<span class="pf-path" title="' + esc(f.path) + '">' + name + '</span>' +
      '<span class="pf-counts"><span class="pf-add">+' + f.additions + '</span> <span class="pf-del">−' + f.deletions + '</span></span>' +
      '<span class="pf-file-badges"></span>' +
      '<span class="grow"></span>' +
      (f.status !== 'deleted' ? '<button class="opt" type="button" data-pf-open title="Open the file in an editor tab">Open file</button>' : '') +
    '</header>' +
    '<div class="pf-notes pf-file-notes" hidden></div>' +
    '<div class="pf-file-body pf-diff"></div>';
  pfFillBody(sec, f);
  return sec;
}

function pfFillBody(sec, f) {
  const body = sec.querySelector('.pf-file-body');
  body.replaceChildren();
  const hunks = pfFileHunks(f);
  if (f.binary) body.innerHTML = '<div class="pf-msg">Binary file not shown.</div>';
  else if (f.tooLarge && !pfFull.has(f.path)) {
    body.innerHTML = '<div class="pf-msg">Large diff: ' + (f.additions + f.deletions) + ' changed lines. ' +
      '<button class="opt" type="button" data-pf-load>Load diff</button></div>';
  } else if (!hunks.length) body.innerHTML = '<div class="pf-msg">' + (f.status === 'renamed' ? 'File renamed without changes.' : 'No changes to show.') + '</div>';
  else body.append(diffHunksFragment(hunks, pfMode, true));
  pfIndexRows(f.path, body);
}

// Where each reviewable line sits: 'RIGHT:n' for a context or added line,
// 'LEFT:n' for a deleted one (what GitHub's side/line mean). box is the row
// a thread goes after -- the pair in the split layout.
function pfIndexRows(path, body) {
  const idx = new Map();
  for (const el of body.querySelectorAll('[data-l], [data-old-l]')) {
    if (el.dataset.reviewable === '0') continue;
    const oldOnly = el.dataset.l === undefined;
    const key = oldOnly ? 'LEFT:' + el.dataset.oldL : 'RIGHT:' + el.dataset.l;
    const box = el.closest('.diff-row-pair') || el;
    const at = idx.get(key);
    if (at) at.els.push(el);
    else idx.set(key, { box, els: [el] });
  }
  pfRows.set(path, idx);
}

function pfFile(path) {
  return pfData?.files?.find(f => f.path === path) || null;
}

function pfSection(path) {
  return pfRoot ? [...pfRoot.querySelectorAll('.pf-file')].find(s => s.dataset.path === path) || null : null;
}

/* ---------- events ---------- */

function pfWire(root) {
  root.addEventListener('click', pfOnClick);
  root.addEventListener('input', e => {
    if (e.target.classList?.contains('pf-filter')) { pfFilter = e.target.value; pfApplyFilter(); }
  });
}

function pfOnClick(e) {
  const t = e.target;
  // The "+" on a line number opens a composer here, not the editor's line menu.
  const plus = t.closest('.line-btn');
  if (plus) {
    e.preventDefault();
    e.stopPropagation();
    pfOpenComposerAt(plus, e.shiftKey);
    return;
  }
  const lnum = t.closest('.diff-ln-nav');
  if (lnum) {
    const row = lnum.closest('[data-l], [data-at]');
    const sec = lnum.closest('.pf-file');
    if (row && sec && row.dataset.l !== undefined) openFile(sec.dataset.path, { line: +row.dataset.l });
    return;
  }
  const mode = t.closest('[data-pf-mode]');
  if (mode) { pfSetMode(mode.dataset.pfMode); return; }
  if (t.closest('[data-pf-fold-all]')) { pfFoldAll(); return; }
  if (t.closest('[data-pf-finish]')) { pfFinish(); return; }
  const nav = t.closest('[data-pf-ai]');
  if (nav) { pfStepAI(+nav.dataset.pfAi); return; }
  const treeRow = t.closest('[data-pf-goto]');
  if (treeRow) { pfReveal = { path: treeRow.dataset.pfGoto, line: 0 }; pfTryReveal(); return; }
  const sec = t.closest('.pf-file');
  if (sec) {
    if (t.closest('[data-pf-fold]') || (t.closest('.pf-file-head') && !t.closest('button'))) { pfFold(sec); return; }
    if (t.closest('[data-pf-open]')) { openFile(sec.dataset.path); return; }
    if (t.closest('[data-pf-load]')) { pfLoadFull(sec.dataset.path); return; }
  }
  const act = t.closest('[data-pf-act]');
  if (act) pfAct(act);
}

function pfSetMode(mode) {
  if (mode === pfMode) return;
  pfMode = mode;
  setLayoutPref(mode);
  const a = pfArticle();
  if (!a) return;
  // Keep the file at the top of the view at the top after the redraw.
  const scroller = a.parentElement;
  const top = scroller?.getBoundingClientRect().top || 0;
  const first = [...a.querySelectorAll('.pf-file')].find(s => s.getBoundingClientRect().bottom > top + 60);
  pfDraw(a);
  if (first) pfSection(first.dataset.path)?.scrollIntoView({ block: 'start' });
}

function pfFold(sec) {
  const path = sec.dataset.path;
  if (pfCollapsed.has(path)) pfCollapsed.delete(path); else pfCollapsed.add(path);
  sec.classList.toggle('collapsed', pfCollapsed.has(path));
}

function pfFoldAll() {
  const files = pfData?.files || [];
  const allShut = files.length && files.every(f => pfCollapsed.has(f.path));
  for (const f of files) { if (allShut) pfCollapsed.delete(f.path); else pfCollapsed.add(f.path); }
  for (const sec of pfRoot?.querySelectorAll('.pf-file') || []) sec.classList.toggle('collapsed', !allShut);
  const btn = pfRoot?.querySelector('[data-pf-fold-all]');
  if (btn) btn.textContent = allShut ? 'Collapse all' : 'Expand all';
}

function pfFinish() {
  const body = $('#pr-review-body');
  if (!body) return;
  body.focus();
  body.classList.add('pf-flash');
  setTimeout(() => body.classList.remove('pf-flash'), 1200);
}

async function pfLoadFull(path) {
  const sec = pfSection(path);
  const f = pfFile(path);
  if (!sec || !f) return;
  sec.querySelector('[data-pf-load]')?.setAttribute('disabled', '');
  try {
    const j = await api('/api/pr/files', { path });
    pfFull.set(path, j.files?.[0]?.hunks || []);
  } catch (e) {
    showToast('!', e.message || 'Could not load the diff');
    sec.querySelector('[data-pf-load]')?.removeAttribute('disabled');
    return;
  }
  pfFillBody(sec, f);
  pfRedrawThreads();
}

function pfApplyFilter() {
  if (!pfRoot) return;
  const words = pfFilter.toLowerCase().split(/\s+/).filter(Boolean);
  const shown = p => words.every(w => p.toLowerCase().includes(w));
  for (const sec of pfRoot.querySelectorAll('.pf-file')) sec.hidden = !shown(sec.dataset.path);
  for (const row of pfRoot.querySelectorAll('.pf-tree-row')) row.hidden = !shown(row.dataset.pfGoto);
}

/* ---------- threads on lines ---------- */

function pfKey(side, line) { return (side || 'RIGHT') + ':' + line; }

// Lays every comment, draft, AI suggestion and open composer on its line
// (or in its file's notes when it has no line in the diff). Cheap enough to
// redo whole on any change; open composers and editors are moved, not
// rebuilt, so what is typed in them survives.
function pfRedrawThreads() {
  const root = pfRoot;
  if (!root || !root.isConnected || !pfData) return;
  const active = document.activeElement;
  const caret = active && 'selectionStart' in active ? [active.selectionStart, active.selectionEnd] : null;
  for (const el of root.querySelectorAll('.pf-thread')) el.remove();
  for (const el of root.querySelectorAll('.pf-notes')) { el.replaceChildren(); el.hidden = true; }
  for (const el of root.querySelectorAll('.pf-range, .pf-ai-row, .pf-focus-row')) el.classList.remove('pf-range', 'pf-ai-row', 'pf-focus-row');

  const paths = new Set((pfData.files || []).map(f => f.path));
  const groups = new Map(); // path -> Map(key -> {posted, drafts, ai, composer})
  const notes = new Map();  // path -> [html] for things without a line here
  const summary = [];
  const group = (path, key) => {
    if (!groups.has(path)) groups.set(path, new Map());
    const g = groups.get(path);
    if (!g.has(key)) g.set(key, { posted: [], drafts: [], ai: [], composer: null });
    return g.get(key);
  };

  for (const c of prPostedReviewComments()) {
    if (paths.has(c.path) && c.line) group(c.path, pfKey(c.side, c.line)).posted.push(c);
  }
  const drafts = prDrafts();
  for (const c of drafts) {
    if (!paths.has(c.path)) { if (c.subjectType === 'summary') summary.push(c); continue; }
    if (c.subjectType) { pfNote(notes, c.path, c); continue; }
    group(c.path, pfKey(c.side, c.line)).drafts.push(c);
  }
  const ai = reviewSuggestions().filter(s => s.status === 'pending');
  for (const s of ai) {
    if (s.subjectType === 'summary' || !paths.has(s.path)) { summary.push(s); continue; }
    if (s.subjectType) { pfNote(notes, s.path, s); continue; }
    group(s.path, pfKey(s.side, s.line)).ai.push(s);
  }
  for (const [key, c] of pfComposers) {
    if (!paths.has(c.path)) { pfComposers.delete(key); continue; }
    group(c.path, pfKey(c.side, c.line)).composer = c;
  }

  for (const [path, byKey] of groups) {
    const idx = pfRows.get(path);
    for (const [key, g] of byKey) {
      const at = idx?.get(key);
      if (!at) {
        // Not on a line the diff shows (or the file's diff is not loaded):
        // keep it reachable in the file's notes.
        for (const c of [...g.drafts, ...g.ai]) pfNote(notes, path, c);
        if (g.composer) pfComposers.delete(pfComposerKey(g.composer));
        continue;
      }
      const thread = document.createElement('div');
      thread.className = 'pf-thread';
      thread.dataset.key = key;
      thread.innerHTML = g.posted.map(pfPostedHtml).join('') + (g.posted.length ? pfPostedFoot(path, key) : '');
      for (const c of g.drafts) thread.append(pfEditors.get('draft:' + c.id) || pfCard(pfDraftHtml(c)));
      for (const s of g.ai) thread.append(pfEditors.get('ai:' + s.id) || pfCard(pfAiHtml(s)));
      if (g.composer) thread.append(g.composer.el);
      let after = at.box;
      while (after.nextElementSibling?.classList.contains('pf-thread')) after = after.nextElementSibling;
      after.after(thread);
      pfMarkRange(idx, g, key);
    }
  }
  for (const [path, list] of notes) {
    const box = pfSection(path)?.querySelector('.pf-file-notes');
    if (!box) continue;
    box.hidden = false;
    for (const c of list) box.append(c.origin === 'ai' && c.status === 'pending'
      ? (pfEditors.get('ai:' + c.id) || pfCard(pfAiHtml(c)))
      : (pfEditors.get('draft:' + c.id) || pfCard(pfDraftHtml(c))));
  }
  const sum = root.querySelector('.pf-summary-notes');
  if (sum && summary.length) {
    sum.hidden = false;
    sum.insertAdjacentHTML('beforeend', '<div class="pf-notes-head">About files outside this PR: these go in the review body</div>');
    for (const c of summary) sum.append(c.origin === 'ai' && c.status === 'pending'
      ? (pfEditors.get('ai:' + c.id) || pfCard(pfAiHtml(c)))
      : (pfEditors.get('draft:' + c.id) || pfCard(pfDraftHtml(c))));
  }
  for (const el of root.querySelectorAll('.pf-text:not(.pf-text-md)')) pfMarkdown(el);
  pfDrawTree(groups, notes);
  pfDrawBar(ai.length, drafts.length);
  if (active && root.contains(active) && document.activeElement !== active) {
    active.focus();
    if (caret) try { active.setSelectionRange(caret[0], caret[1]); } catch {}
  }
}

// Comment bodies are Markdown, as on GitHub: rendered by the server once per
// text and kept, so a redraw paints them straight away. Until then (or if
// rendering fails) the plain text stays.
const pfMdCache = new Map(); // text -> html
function pfMarkdown(el) {
  const text = el.textContent;
  const show = html => {
    if (!el.isConnected || el.textContent !== text) return;
    el.replaceChildren(sanitizeForgeHTML(html));
    el.classList.add('pf-text-md');
  };
  if (pfMdCache.has(text)) { show(pfMdCache.get(text)); return; }
  apiPostJson('/api/markdown/render', { text })
    .then(j => { pfMdCache.set(text, j.html || ''); show(j.html || ''); })
    .catch(() => {});
}

function pfNote(notes, path, c) {
  if (!notes.has(path)) notes.set(path, []);
  notes.get(path).push(c);
}

function pfCard(html) {
  const t = document.createElement('template');
  t.innerHTML = html;
  return t.content.firstElementChild;
}

// Tints the lines a multi-line comment covers, the lines with a pending AI
// suggestion, and the ones of the suggestion called out.
function pfMarkRange(idx, g, key) {
  const [side, lineStr] = key.split(':');
  const line = +lineStr;
  const mark = (from, cls) => {
    for (let n = from; n <= line; n++) for (const el of idx.get(side + ':' + n)?.els || []) el.classList.add(cls);
  };
  for (const c of [...g.drafts, ...g.ai]) {
    const from = c.startLine && c.startLine < line ? c.startLine : line;
    if (from < line) mark(from, 'pf-range');
    if (g.ai.includes(c)) mark(from, 'pf-ai-row');
    if (c.id === pfFocusId) mark(from, 'pf-focus-row');
  }
  if (g.composer?.startLine) mark(g.composer.startLine, 'pf-range');
}

function pfDrawTree(groups, notes) {
  const list = pfRoot?.querySelector('.pf-tree-list');
  if (!list) return;
  const counts = path => {
    let drafts = 0, ai = 0, posted = 0;
    for (const g of groups.get(path)?.values() || []) { drafts += g.drafts.length; ai += g.ai.length; posted += g.posted.length; }
    for (const c of notes.get(path) || []) { if (c.origin === 'ai' && c.status === 'pending') ai++; else drafts++; }
    return { drafts, ai, posted };
  };
  list.innerHTML = (pfData.files || []).map(f => {
    const [letter, label] = PF_STATUS[f.status] || PF_STATUS.modified;
    const c = counts(f.path);
    const slash = f.path.lastIndexOf('/');
    return '<div class="pf-tree-row" data-pf-goto="' + esc(f.path) + '" title="' + esc(f.path) + '" role="button" tabindex="0">' +
      '<span class="pf-st pf-st-' + esc(f.status) + '" title="' + label + '">' + letter + '</span>' +
      '<span class="pf-tree-name">' + esc(f.path.slice(slash + 1)) + (slash > 0 ? '<span class="pf-tree-dir">' + esc(f.path.slice(0, slash)) + '</span>' : '') + '</span>' +
      (c.ai ? '<span class="pf-badge pf-badge-ai" title="Pending AI suggestions">' + c.ai + '</span>' : '') +
      (c.drafts ? '<span class="pf-badge pf-badge-draft" title="Your draft comments">' + c.drafts + '</span>' : '') +
      (c.posted ? '<span class="pf-badge" title="Posted comments">' + c.posted + '</span>' : '') +
      '</div>';
  }).join('');
  for (const sec of pfRoot.querySelectorAll('.pf-file')) {
    const c = counts(sec.dataset.path);
    const badges = sec.querySelector('.pf-file-badges');
    if (badges) badges.innerHTML =
      (c.ai ? '<span class="pf-badge pf-badge-ai" title="Pending AI suggestions">' + c.ai + ' AI</span>' : '') +
      (c.drafts ? '<span class="pf-badge pf-badge-draft" title="Your draft comments">' + c.drafts + ' pending</span>' : '') +
      (c.posted ? '<span class="pf-badge" title="Posted comments">' + c.posted + ' posted</span>' : '');
  }
  pfApplyFilter();
}

function pfDrawBar(aiCount, draftCount) {
  const nav = pfRoot?.querySelector('.pf-ai-nav');
  if (nav) {
    nav.hidden = !aiCount;
    nav.querySelector('.pf-ai-count').textContent = aiCount + (aiCount === 1 ? ' AI suggestion' : ' AI suggestions');
  }
  const fin = pfRoot?.querySelector('[data-pf-finish]');
  if (fin) fin.textContent = draftCount ? 'Finish your review (' + draftCount + ')' : 'Finish your review';
}

/* ---------- cards ---------- */

function pfWhen(iso) {
  const t = Date.parse(iso || '');
  if (!t) return '';
  const m = Math.max(0, Math.round((Date.now() - t) / 60000));
  if (m < 1) return 'just now';
  if (m < 60) return m + 'm ago';
  const h = Math.round(m / 60);
  if (h < 48) return h + 'h ago';
  return Math.round(h / 24) + 'd ago';
}

function pfLines(c) {
  const p = c.side === 'LEFT' ? 'L' : 'R';
  return c.startLine && c.startLine < c.line ? 'lines ' + p + c.startLine + '–' + p + c.line : 'line ' + p + c.line;
}

function pfSuggestionHtml(code) {
  if (!code) return '';
  return '<div class="pf-sugg"><div class="pf-sugg-head">Suggested change</div><pre>' +
    code.split('\n').map(l => '<span class="pf-sugg-add">+ ' + esc(l) + '</span>').join('\n') + '</pre></div>';
}

function pfPostedHtml(c) {
  return '<div class="pf-box pf-posted">' +
    '<div class="pf-box-head">' + (c.avatarUrl ? '<img class="pf-avatar" src="' + esc(c.avatarUrl) + '" alt="">' : '<span class="pf-avatar"></span>') +
      '<b>' + esc(c.author || 'someone') + '</b><span class="pf-when" title="' + esc(c.createdAt || '') + '">' + esc(pfWhen(c.createdAt)) + '</span>' +
      '<span class="grow"></span>' + (c.url ? '<a class="pf-link" href="' + esc(c.url) + '" target="_blank" rel="noopener">GitHub</a>' : '') +
    '</div><div class="pf-text">' + esc(c.body || '') + '</div></div>';
}

function pfPostedFoot(path, key) {
  return '<div class="pf-thread-foot"><button class="opt" type="button" data-pf-act="conversation" data-path="' + esc(path) + '" data-key="' + esc(key) + '">Reply in Conversation</button></div>';
}

function pfDraftHtml(c) {
  const ai = c.origin === 'ai';
  const where = c.subjectType === 'file' ? 'whole file' : c.subjectType === 'summary' ? 'review body' : pfLines(c);
  return '<div class="pf-box pf-draft' + (ai ? ' pf-from-ai' : '') + (c.id === pfFocusId ? ' pf-focus' : '') + '" data-draft-id="' + c.id + '">' +
    '<div class="pf-box-head"><span class="pf-avatar pf-avatar-you"></span><b>' + (ai ? 'AI suggestion you added' : 'You') + '</b>' +
      '<span class="pf-when">' + esc(where) + '</span><span class="pf-pending" title="Goes out with your review">Pending</span><span class="grow"></span></div>' +
    '<div class="pf-text">' + esc(c.body) + '</div>' + pfSuggestionHtml(c.ai?.suggestion) +
    '<div class="pf-box-foot"><span class="grow"></span>' +
      '<button class="opt" type="button" data-pf-act="draft-delete" data-id="' + c.id + '">Delete</button>' +
      '<button class="opt" type="button" data-pf-act="draft-edit" data-id="' + c.id + '">Edit</button>' +
      '<button class="opt" type="button" data-pf-act="draft-post" data-id="' + c.id + '" title="Post this comment to the PR now, on its own">Comment now</button>' +
    '</div></div>';
}

function pfAiHtml(s) {
  const ai = s.ai || {};
  const sev = ['critical', 'high', 'medium', 'low'].includes(ai.severity) ? ai.severity : 'medium';
  const where = s.subjectType === 'file' ? 'whole file' : s.subjectType === 'summary' ? s.path : pfLines(s);
  const stale = !!ai.stale;
  const dis = stale ? ' disabled title="Made against an older PR head: re-check it in AI Review first"' : '';
  return '<div class="pf-box pf-ai' + (s.id === pfFocusId ? ' pf-focus' : '') + '" data-ai-id="' + s.id + '">' +
    '<div class="pf-box-head"><span class="pf-ai-badge">AI</span><b>Suggestion</b><span class="pf-when">' + esc(where) + '</span>' +
      '<span class="rv-sev rv-sev-' + esc(sev) + '">' + esc(sev[0].toUpperCase() + sev.slice(1)) + '</span>' +
      (ai.category ? '<span class="rv-cat">' + esc(ai.category) + '</span>' : '') +
      (stale ? '<span class="rv-anchor rv-anchor-file">stale</span>' : '') +
      '<span class="grow"></span><span class="pf-conf" title="Model confidence">' + Math.round((ai.confidence ?? 0) * 100) + '% confident</span></div>' +
    '<div class="pf-text">' + esc(s.body) + '</div>' + pfSuggestionHtml(ai.suggestion) +
    '<div class="pf-box-foot"><span class="pf-hint">Not posted yet</span><span class="grow"></span>' +
      '<button class="opt" type="button" data-pf-act="ai-dismiss" data-id="' + s.id + '">Dismiss</button>' +
      '<button class="opt" type="button" data-pf-act="ai-edit" data-id="' + s.id + '"' + dis + '>Edit</button>' +
      '<button class="opt" type="button" data-pf-act="ai-post" data-id="' + s.id + '"' + dis + ' title="Post this comment to the PR now, on its own">Comment now</button>' +
      '<button class="footer-btn pf-primary" type="button" data-pf-act="ai-add" data-id="' + s.id + '"' + dis + ' title="Add to your review: posted when you submit it">Add to review</button>' +
    '</div></div>';
}

/* ---------- the editor box: new comments and edits ---------- */

// GitHub's comment box: Write and Preview tabs, then Cancel, a post-now
// button and the add-to-review button. opts: {title, text, suggestLines,
// onCancel, onPost, onAdd, addLabel}. Each handler gets the trimmed text.
function pfEditorEl(opts) {
  const el = document.createElement('div');
  el.className = 'pf-box pf-editor';
  const modEnter = keyLabel('Mod+Enter');
  el.innerHTML =
    '<div class="pf-box-head"><span class="pf-avatar pf-avatar-you"></span><b>' + esc(opts.title) + '</b></div>' +
    '<div class="pf-tabs"><button class="pf-tab on" type="button" data-tab="write">Write</button><button class="pf-tab" type="button" data-tab="preview">Preview</button>' +
      '<span class="grow"></span>' +
      (opts.suggestLines ? '<button class="opt pf-insert" type="button" data-insert-suggestion title="Insert a suggested change for these lines">± Suggestion</button>' : '') +
    '</div>' +
    '<textarea class="pf-input" rows="4" spellcheck="true" placeholder="Leave a comment"></textarea>' +
    '<div class="pf-preview pf-text-md" hidden></div>' +
    '<div class="pf-err" hidden></div>' +
    '<div class="pf-box-foot"><span class="pf-hint">' + esc(modEnter) + ' ' + esc(opts.addLabel.toLowerCase()) + ' · Esc cancels</span><span class="grow"></span>' +
      '<button class="opt" type="button" data-ed="cancel">Cancel</button>' +
      '<button class="opt" type="button" data-ed="post" title="Post this comment to the PR now, on its own">Comment now</button>' +
      '<button class="footer-btn pf-primary" type="button" data-ed="add" title="Add to your review: posted when you submit it">' + esc(opts.addLabel) + '</button>' +
    '</div>';
  const ta = el.querySelector('textarea');
  const preview = el.querySelector('.pf-preview');
  const err = el.querySelector('.pf-err');
  ta.value = opts.text || '';
  const busy = on => { for (const b of el.querySelectorAll('[data-ed]')) b.disabled = on; };
  const run = async fn => {
    const text = ta.value.trim();
    if (!text) { ta.focus(); return; }
    err.hidden = true;
    busy(true);
    try { await fn(text); }
    catch (e) { err.hidden = false; err.textContent = e.message || 'That did not work'; }
    finally { busy(false); }
  };
  el.addEventListener('click', async e => {
    const tab = e.target.closest('[data-tab]');
    if (tab) {
      for (const b of el.querySelectorAll('[data-tab]')) b.classList.toggle('on', b === tab);
      const pv = tab.dataset.tab === 'preview';
      ta.hidden = pv;
      preview.hidden = !pv;
      if (pv) {
        preview.textContent = 'Rendering…';
        try {
          const j = await apiPostJson('/api/markdown/render', { text: ta.value || 'Nothing to preview' });
          preview.replaceChildren(sanitizeForgeHTML(j.html));
        } catch (er) { preview.textContent = er.message || 'Could not render'; }
      } else ta.focus();
      return;
    }
    if (e.target.closest('[data-insert-suggestion]')) {
      const block = '```suggestion\n' + opts.suggestLines().join('\n') + '\n```\n';
      const at = ta.selectionStart;
      ta.value = ta.value.slice(0, at) + (at && ta.value[at - 1] !== '\n' ? '\n' : '') + block + ta.value.slice(ta.selectionEnd);
      ta.focus();
      return;
    }
    const b = e.target.closest('[data-ed]');
    if (!b) return;
    if (b.dataset.ed === 'cancel') opts.onCancel();
    else if (b.dataset.ed === 'post') run(opts.onPost);
    else run(opts.onAdd);
  });
  ta.addEventListener('keydown', e => {
    if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); opts.onCancel(); }
    else if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); run(opts.onAdd); }
  });
  return el;
}

function pfComposerKey(c) { return c.path + '|' + c.side + '|' + c.line; }

// The text of the new-side lines from..to, for a suggestion block.
function pfLineText(path, from, to) {
  const idx = pfRows.get(path);
  const out = [];
  for (let n = from; n <= to; n++) {
    const el = idx?.get('RIGHT:' + n)?.els.find(x => !x.classList.contains('diff-side-left')) || idx?.get('RIGHT:' + n)?.els[0];
    out.push(el?.querySelector('.diff-code')?.textContent.replace(/ $/, '') || '');
  }
  return out;
}

function pfAddLabel() {
  return prDrafts().length ? 'Add review comment' : 'Start a review';
}

function pfOpenComposerAt(btn, extend) {
  const sec = btn.closest('.pf-file');
  const row = btn.closest('.diff-row, .diff-side');
  if (!sec || !row || row.dataset.reviewable === '0') return;
  const path = sec.dataset.path;
  const oldOnly = row.dataset.l === undefined;
  if (oldOnly && row.dataset.oldL === undefined) return;
  const side = oldOnly ? 'LEFT' : 'RIGHT';
  const line = oldOnly ? +row.dataset.oldL : +row.dataset.l;
  let startLine = 0;
  let end = line;
  // Shift-click a second "+" on the same side of the same file for a range.
  if (extend && pfRangeFrom && pfRangeFrom.path === path && pfRangeFrom.side === side && pfRangeFrom.line !== line) {
    startLine = Math.min(pfRangeFrom.line, line);
    end = Math.max(pfRangeFrom.line, line);
    pfComposers.delete(pfComposerKey(pfRangeFrom));
  }
  pfRangeFrom = { path, side, line: end };
  pfOpenComposer({ path, side, line: end, startLine });
}

function pfOpenComposer({ path, side, line, startLine }) {
  const c = { path, side, line, startLine };
  const key = pfComposerKey(c);
  if (pfComposers.has(key)) { pfComposers.get(key).el.querySelector('textarea')?.focus(); return; }
  const p = side === 'LEFT' ? 'L' : 'R';
  const title = startLine ? 'Add a comment on lines ' + p + startLine + ' to ' + p + line : 'Add a comment on line ' + p + line;
  const close = () => { pfComposers.delete(key); c.el.remove(); pfRedrawThreads(); };
  c.el = pfEditorEl({
    title, addLabel: pfAddLabel(),
    suggestLines: side === 'RIGHT' ? () => pfLineText(path, startLine || line, line) : null,
    onCancel: close,
    onAdd: async text => {
      await apiPostJson('/api/pr/comments', { path, line, startLine, side, body: text });
      pfComposers.delete(key);
      c.el.remove();
      await refreshComments(); // redraws through pr:comments-changed
    },
    onPost: async text => {
      if (!pfCanPost()) return;
      await apiPostJson('/api/pr/comments/post', { path, line, startLine, side, body: text });
      pfComposers.delete(key);
      c.el.remove();
      showToast('✓', 'Comment posted to the PR');
      await refreshExistingComments();
      pfRedrawThreads();
    },
  });
  pfComposers.set(key, c);
  pfRedrawThreads();
  c.el.querySelector('textarea')?.focus();
}

function pfCanPost() {
  if (prSessionMeta()?.readOnly) { nudgeGitHubToken(); return false; }
  return true;
}

/* ---------- card actions ---------- */

async function pfAct(btn) {
  const act = btn.dataset.pfAct;
  const id = +btn.dataset.id;
  if (act === 'conversation') {
    const [side, line] = btn.dataset.key.split(':');
    openFile('pr://conversation').then(() => emit('conversation:reveal', { path: btn.dataset.path, side, line: +line }));
    return;
  }
  const s = reviewSuggestions().find(x => x.id === id);
  const d = prDrafts().find(x => x.id === id);
  btn.disabled = true;
  try {
    switch (act) {
      case 'ai-add': pfFocusId = id; await triageSuggestions([id], 'accept'); break;
      case 'ai-dismiss': if (pfFocusId === id) pfFocusId = 0; await triageSuggestions([id], 'dismiss'); break;
      case 'ai-post': await pfPostExisting(id, ''); break;
      case 'ai-edit': if (s) pfOpenEditor('ai:' + id, s); break;
      case 'draft-delete': if (pfFocusId === id) pfFocusId = 0; await deleteDraft(id); break;
      case 'draft-edit': if (d) pfOpenEditor('draft:' + id, d); break;
      case 'draft-post': await pfPostExisting(id, ''); break;
    }
  } finally {
    if (btn.isConnected) btn.disabled = false;
  }
}

// Posts a draft or AI suggestion now, on its own; body, when set, is the
// edited text.
async function pfPostExisting(id, body) {
  if (!pfCanPost()) return false;
  try {
    await apiPostJson('/api/pr/comments/post', { id, body });
  } catch (e) {
    showToast('!', e.message || 'Could not post the comment');
    return false;
  }
  if (pfFocusId === id) pfFocusId = 0;
  showToast('✓', 'Comment posted to the PR');
  await Promise.all([refreshComments(), refreshExistingComments(), reloadSuggestions()]);
  return true;
}

// Edits a draft or an AI suggestion in place, in the same box as a new
// comment. Saving an AI suggestion adds it to the review, as GitHub's
// "Add review comment" does.
function pfOpenEditor(key, c) {
  const isAI = key.startsWith('ai:');
  const close = () => { pfEditors.delete(key); pfRedrawThreads(); };
  const el = pfEditorEl({
    title: (isAI ? 'Edit the AI suggestion on ' : 'Edit your comment on ') + (c.subjectType ? 'the whole file' : pfLines(c)),
    text: c.body,
    addLabel: isAI ? pfAddLabel() : 'Save',
    suggestLines: !c.subjectType && c.side !== 'LEFT' ? () => pfLineText(c.path, c.startLine || c.line, c.line) : null,
    onCancel: close,
    onAdd: async text => {
      if (isAI || c.origin === 'ai') {
        if (!(await triageSuggestions([c.id], 'edit', text))) throw new Error('Could not save the edit');
      } else {
        await apiPostJson('/api/pr/comments/edit?id=' + c.id, { body: text });
        await refreshComments();
      }
      pfEditors.delete(key);
      pfRedrawThreads();
    },
    onPost: async text => {
      if (await pfPostExisting(c.id, text)) { pfEditors.delete(key); pfRedrawThreads(); }
    },
  });
  el.dataset[isAI ? 'aiId' : 'draftId'] = String(c.id);
  pfEditors.set(key, el);
  pfRedrawThreads();
  const ta = el.querySelector('textarea');
  if (ta) { ta.focus(); ta.setSelectionRange(ta.value.length, ta.value.length); }
}

/* ---------- moving around ---------- */

// Steps to the previous or next pending AI suggestion in page order.
function pfStepAI(dir) {
  const cards = [...(pfRoot?.querySelectorAll('.pf-ai[data-ai-id]') || [])].filter(c => c.offsetParent);
  if (!cards.length) return;
  let i = cards.findIndex(c => +c.dataset.aiId === pfFocusId);
  i = i < 0 ? (dir > 0 ? 0 : cards.length - 1) : (i + dir + cards.length) % cards.length;
  const s = reviewSuggestions().find(x => x.id === +cards[i].dataset.aiId);
  if (s) { pfReveal = { path: s.path, side: s.side, line: s.subjectType ? 0 : s.line, id: s.id }; pfTryReveal(); }
}

// Brings a file, a line, or a suggestion's card into view, opening up
// whatever hides it: a filter, a folded file, a large diff not loaded yet.
async function pfTryReveal() {
  const t = pfReveal;
  if (!t || !pfRoot?.isConnected || !pfData) return;
  pfReveal = null;
  const sec = pfSection(t.path);
  if (!sec) { showToast('!', t.path + ' is not part of this PR'); return; }
  if (sec.hidden) {
    pfFilter = '';
    const box = pfRoot.querySelector('.pf-filter');
    if (box) box.value = '';
    pfApplyFilter();
  }
  if (pfCollapsed.has(t.path)) pfFold(sec);
  const f = pfFile(t.path);
  if (f?.tooLarge && !pfFull.has(t.path) && t.line) await pfLoadFull(t.path);
  if (t.id) {
    pfFocusId = t.id;
    pfRedrawThreads();
  }
  const card = t.id ? pfRoot.querySelector('[data-ai-id="' + t.id + '"], [data-draft-id="' + t.id + '"]') : null;
  const row = t.line ? pfRows.get(t.path)?.get(pfKey(t.side, t.line)) : null;
  const target = card || row?.box || sec;
  target.scrollIntoView({ block: card || row ? 'center' : 'start', behavior: 'smooth' });
  for (const el of row?.els || []) {
    el.classList.add('pr-line-flash');
    setTimeout(() => el.classList.remove('pr-line-flash'), 1200);
  }
  if (card) {
    card.classList.add('pf-flash');
    setTimeout(() => card.classList.remove('pf-flash'), 1400);
  }
}
