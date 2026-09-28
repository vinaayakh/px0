// web/src/graph.js
// The Graph tab (git://graph): every branch, remote branch and tag as a
// commit graph, with a branch list sorted into health categories beside it.
// Lanes are laid out on the server (graph.go) and arrive with each 2,000-row
// page; this module only draws the rows in view -- about 60 DOM rows and one
// SVG for their lanes -- and loads the next page as the view nears it.
// Focusing a branch (from the list, a ref pill, or any lane) keeps its
// commits, fork point and merges bright and fades everything else; the focus
// is kept in the URL hash.
import { $, S, esc, api } from './state.js';
import { on } from './bus.js';
import { showToast, copyToClipboard } from './ui.js';
import { registerVirtualTab } from './virtualtab.js';
import { virtualArticle } from './markdown.js';
import { openFile } from './tabs.js';

const GR_PATH = 'git://graph';
const GR_ROW = 24;       // row height, px
const GR_LANE = 14;      // lane width, px
const GR_PAD = 6;        // left padding of the lane area
const GR_OVERSCAN = 12;  // rows drawn above and below the view
const GR_PAGE = 2000;
const GR_COLORS = 8;     // lane colours, --gr-0 .. --gr-7 in style.css

let grRows = [];          // laid-out rows loaded so far
let grNext = 0;           // cursor of the next page, -1 at the end
let grTotal = -1;         // commits in the graph, -1 until known
let grMaxLane = 0;
let grSig = '';
let grHead = '';          // HEAD commit
let grLoading = null;
let grNotice = '', grHint = '', grError = '';
let grIndex = new Map();  // sha -> row index
let grBranches = null;    // /api/graph/branches
let grCat = 'all';        // branch list filter
let grFocus = null;       // {label, shas:Set, fork, merges:Set, segs:Set, key}
let grScrollTop = 0;
let grDrawQueued = false;
let grBranchError = '';
let grSel = '';           // commit shown in the inspector
let grQuery = '';         // commit search
let grMatches = [];       // row indices matching grQuery
let grMatchSet = new Set();
let grMatchAt = -1;       // current match in grMatches
let grSearchTimer = 0;
let grStale = false;      // refs may have moved while the tab was hidden
const grOpenFiles = new Set(); // inspector files whose diff is expanded

const GR_CAT_LABEL = {
  default: 'default', active: 'active', stale: 'stale', gone: 'upstream gone',
  squash_merged: 'squash merged?', merged: 'merged', orphan: 'orphan',
};
const GR_CAT_TITLE = {
  default: 'The branch everything else is compared against',
  active: 'Unmerged, with recent commits',
  stale: 'Unmerged, and no commits for a while',
  gone: 'Local branch whose remote branch was deleted',
  squash_merged: 'GitHub shows a merged PR from this branch, so it was probably squash- or rebase-merged',
  merged: 'Already contained in the default branch: safe to delete',
  orphan: 'Shares no history with the default branch',
};

export function initGraph() {
  if (!S.meta?.git) return;
  registerVirtualTab('git', { title: () => 'Graph', render: grRender });
  $('#git-graph-open')?.addEventListener('click', e => { e.stopPropagation(); openGraph(); });
  document.addEventListener('keydown', e => {
    if (e.key === 'Escape' && grFocus && grEl() && !e.target.closest?.('input, textarea')) {
      e.stopPropagation();
      grSetFocus(null);
    }
  }, true);
  on('tab:activated', ({ doc }) => {
    if (doc?.path !== GR_PATH) grSaveScroll();
  });
  // Refs move under the graph (a commit, fetch, checkout or pull in a
  // terminal): the git watcher's snapshot changes, and the graph checks its
  // ref signature. Hidden, it only notes that it may be stale.
  on('git:status', () => {
    if (!grRows.length) return;
    if (grEl()) grCheckRefs(); else grStale = true;
  });
}

async function grCheckRefs() {
  grStale = false;
  let j;
  try { j = await api('/api/graph/sig'); } catch { return; }
  if (!j.sig || j.sig === grSig || !grEl()) return;
  // Reload in place: scroll position and focus stay, the pages the view
  // needs are read again.
  const sc = grEl()?.querySelector('.gr-scroll');
  const want = sc ? Math.ceil((sc.scrollTop + sc.clientHeight) / GR_ROW) + GR_OVERSCAN : 0;
  await grLoadPage(true);
  await grEnsureLoaded(want);
  grBranches = null;
  grLoadBranches();
  if (grFocus) grApplyFocusKey(grFocus.key, { scroll: false });
  if (grQuery) grRunSearch(grQuery, { keepAt: true });
}

// Reads pages until row index i is loaded (or history ends).
async function grEnsureLoaded(i) {
  for (let guard = 0; grRows.length <= i && grNext >= 0 && guard < 100; guard++) {
    await grLoadPage(false, 5000);
  }
}

export function openGraph() {
  if (!S.meta?.git) { showToast('!', 'The graph needs a git repository'); return; }
  return openFile(GR_PATH);
}

/* ---------- data ---------- */

async function grLoadPage(reset, limit = GR_PAGE) {
  if (grLoading) { await grLoading; if (!reset) return; }
  grLoading = (async () => {
    try {
      if (reset) { grRows = []; grNext = 0; grIndex = new Map(); grTotal = -1; grMaxLane = 0; }
      if (grNext < 0) return;
      const j = await api('/api/graph', { cursor: grNext, limit, sig: grRows.length ? grSig : '' });
      if (j.reset) { // the refs moved: start over, keeping the scroll position and focus
        grLoading = null;
        grRows = []; grNext = 0; grIndex = new Map(); grSig = '';
        await grLoadPage(false);
        grBranches = null;
        grLoadBranches();
        if (grFocus) grApplyFocusKey(grFocus.key, { scroll: false });
        return;
      }
      grSig = j.sig;
      grHead = j.head || '';
      grError = j.error || '';
      grNotice = j.notice || '';
      grHint = j.hint || '';
      for (const r of j.rows || []) { grIndex.set(r.h, grRows.length); grRows.push(r); }
      grNext = j.next;
      grTotal = j.total >= 0 ? j.total : grTotal;
      grMaxLane = Math.max(grMaxLane, j.maxLane || 0);
      if (grFocus) grFocus.segs = grSegsOf(grFocus.shas);
    } catch (e) {
      grError = e.message || 'Could not load the graph';
    } finally {
      grLoading = null;
    }
  })();
  await grLoading;
  grQueueDraw();
}

async function grLoadBranches(refresh) {
  try {
    grBranches = await api('/api/graph/branches', refresh ? { refresh: 1 } : undefined);
    grBranchError = '';
  } catch (e) {
    grBranchError = e.message || 'Could not list branches';
  }
  grDrawBranches();
}

/* ---------- layout of the tab ---------- */

function grEl() {
  const a = virtualArticle(S.tabs.find(t => t.path === GR_PATH));
  return a ? a.querySelector('.gr') : null;
}

async function grRender(article) {
  article.classList.add('gr-host');
  article.innerHTML =
    '<div class="gr">' +
      '<aside class="gr-side">' +
        '<div class="gr-side-head"><span class="gr-side-title">Branches</span><span class="gr-default" title="Everything is compared against this branch (setting: graph.defaultBranch)"></span>' +
        '<span class="grow"></span><button class="mini" type="button" data-gr-refresh title="Refresh branches and graph">⟳</button></div>' +
        '<div class="gr-filters"></div>' +
        '<div class="gr-branches"><div class="hint">Loading branches…</div></div>' +
      '</aside>' +
      '<section class="gr-main">' +
        '<div class="gr-bar"><span class="gr-focus-label"></span><span class="grow"></span>' +
          '<span class="gr-searchbox"><input class="gr-search" type="text" spellcheck="false" autocomplete="off" placeholder="Search message, author, hash">' +
          '<span class="gr-search-n"></span>' +
          '<button class="mini" type="button" data-gr-prev title="Previous match (Shift+Enter)">↑</button>' +
          '<button class="mini" type="button" data-gr-next title="Next match (Enter)">↓</button></span>' +
          '<span class="gr-count"></span></div>' +
        '<div class="gr-notes"></div>' +
        '<div class="gr-scroll" tabindex="0"><div class="gr-sizer"><svg class="gr-svg" xmlns="http://www.w3.org/2000/svg"></svg><div class="gr-rows"></div></div></div>' +
      '</section>' +
      '<aside class="gr-detail" hidden></aside>' +
    '</div>';
  const root = article.querySelector('.gr');
  grWire(root);
  const scroller = root.querySelector('.gr-scroll');
  scroller.scrollTop = grScrollTop;
  const search = root.querySelector('.gr-search');
  search.value = grQuery;
  if (!grRows.length) await grLoadPage(true);
  else grQueueDraw();
  if (grStale) grCheckRefs();
  if (grSel) grOpenCommit(grSel, { scroll: false });
  if (!grBranches) grLoadBranches(); else grDrawBranches();
  const hash = decodeURIComponent((location.hash.match(/^#graph=(.+)$/) || [])[1] || '');
  if (hash && (!grFocus || grFocus.key !== hash)) grApplyFocusKey(hash, { scroll: true });
  else grDrawFocusBar();
}

function grSaveScroll() {
  const sc = grEl()?.querySelector('.gr-scroll');
  if (sc) grScrollTop = sc.scrollTop;
}

function grWire(root) {
  const scroller = root.querySelector('.gr-scroll');
  scroller.addEventListener('scroll', () => {
    grScrollTop = scroller.scrollTop;
    grQueueDraw();
    const last = Math.ceil((scroller.scrollTop + scroller.clientHeight) / GR_ROW);
    if (grNext >= 0 && last + 400 > grRows.length) grLoadPage(false);
  }, { passive: true });
  const search = root.querySelector('.gr-search');
  search.addEventListener('input', () => {
    clearTimeout(grSearchTimer);
    grSearchTimer = setTimeout(() => grRunSearch(search.value.trim()), 250);
  });
  search.addEventListener('keydown', e => {
    if (e.key === 'Enter') { e.preventDefault(); clearTimeout(grSearchTimer); grStepMatch(e.shiftKey ? -1 : 1, search.value.trim()); }
    if (e.key === 'Escape' && search.value) { e.stopPropagation(); search.value = ''; grRunSearch(''); }
  });
  root.addEventListener('click', e => {
    if (e.target.closest('[data-gr-refresh]')) { grLoadBranches(true); grLoadPage(true); return; }
    const copy = e.target.closest('[data-gr-copy]');
    if (copy) { e.stopPropagation(); copyToClipboard(copy.dataset.grCopy, 'Copied: ' + copy.dataset.grCopy); return; }
    if (e.target.closest('[data-gr-next]')) { grStepMatch(1, search.value.trim()); return; }
    if (e.target.closest('[data-gr-prev]')) { grStepMatch(-1, search.value.trim()); return; }
    if (e.target.closest('[data-gr-close-detail]')) { grSel = ''; grOpenFiles.clear(); root.querySelector('.gr-detail').hidden = true; grQueueDraw(); return; }
    const parent = e.target.closest('[data-gr-commit]');
    if (parent) { grOpenCommit(parent.dataset.grCommit, { scroll: true }); return; }
    const file = e.target.closest('[data-gr-file]');
    if (file && !e.target.closest('.gr-diff')) { grToggleFileDiff(file); return; }
    if (e.target.closest('.gr-detail') && !e.target.closest('[data-gr-ref]')) return;
    if (e.target.closest('[data-gr-clear]')) { grSetFocus(null); return; }
    const chip = e.target.closest('[data-gr-cat]');
    if (chip) { grCat = chip.dataset.grCat; grDrawBranches(); return; }
    const br = e.target.closest('[data-gr-branch]');
    if (br) { grApplyFocusKey('ref:' + br.dataset.grBranch, { scroll: true }); return; }
    const pill = e.target.closest('[data-gr-ref]');
    if (pill) { e.stopPropagation(); grApplyFocusKey('ref:' + pill.dataset.grRef, { scroll: true }); return; }
    const svg = e.target.closest('.gr-svg');
    if (svg) { grLaneClick(e, root); return; }
    const row = e.target.closest('[data-gr-row]');
    if (row) { grOpenCommit(grRows[+row.dataset.grRow]?.h, { scroll: false }); return; }
    if (e.target.closest('.gr-scroll')) grSetFocus(null); // empty space
  });
}

/* ---------- drawing ---------- */

function grQueueDraw() {
  if (grDrawQueued) return;
  grDrawQueued = true;
  requestAnimationFrame(() => { grDrawQueued = false; grDraw(); });
}

const grX = lane => GR_PAD + lane * GR_LANE + GR_LANE / 2;
const grColor = seg => 'var(--gr-' + (((seg % GR_COLORS) + GR_COLORS) % GR_COLORS) + ')';

function grRelTime(t) {
  const s = Math.max(0, Date.now() / 1000 - t);
  if (s < 3600) return Math.max(1, Math.round(s / 60)) + 'm';
  if (s < 172800) return Math.round(s / 3600) + 'h';
  if (s < 5184000) return Math.round(s / 86400) + 'd';
  if (s < 63072000) return Math.round(s / 2592000) + 'mo';
  return Math.round(s / 31536000) + 'y';
}

function grDraw() {
  const root = grEl();
  if (!root) return;
  const scroller = root.querySelector('.gr-scroll');
  const sizer = root.querySelector('.gr-sizer');
  const rowsEl = root.querySelector('.gr-rows');
  const svg = root.querySelector('.gr-svg');
  const notes = root.querySelector('.gr-notes');
  let noteHtml = '';
  if (grError) noteHtml += '<div class="gr-note gr-note-err">' + esc(grError) + '</div>';
  if (grNotice) noteHtml += '<div class="gr-note">' + esc(grNotice) + '</div>';
  if (grHint) noteHtml += '<div class="gr-note">' + esc(grHint) + '</div>';
  notes.innerHTML = noteHtml;
  const count = root.querySelector('.gr-count');
  count.textContent = grTotal >= 0 ? grTotal.toLocaleString() + ' commits' : grRows.length.toLocaleString() + '+ commits';

  const known = grTotal >= 0 ? grTotal : grRows.length + (grNext >= 0 ? GR_PAGE : 0);
  const graphW = GR_PAD * 2 + (grMaxLane + 1) * GR_LANE;
  sizer.style.height = known * GR_ROW + 'px';
  sizer.style.setProperty('--gr-graph-w', graphW + 'px');

  const first = Math.max(0, Math.floor(scroller.scrollTop / GR_ROW) - GR_OVERSCAN);
  const last = Math.min(grRows.length, Math.ceil((scroller.scrollTop + scroller.clientHeight) / GR_ROW) + GR_OVERSCAN);
  const f = grFocus;
  let html = '';
  let paths = '';
  for (let i = first; i < last; i++) {
    const r = grRows[i];
    const inFocus = !f || f.shas.has(r.h) || f.fork === r.h || f.merges.has(r.h);
    const role = !f ? '' : f.fork === r.h ? 'fork point' : f.merges.has(r.h) ? 'merged here' : '';
    const refs = (r.r || []).map(ref => '<span class="gr-ref gr-ref-' + ref.k + (ref.h ? ' gr-ref-head' : '') + '" data-gr-ref="' + esc(ref.n) + '" title="Focus ' + esc(ref.n) + '">' + esc(ref.n) + '</span>').join('');
    const cls = (inFocus ? '' : ' dim') + (r.h === grHead ? ' head' : '') + (r.h === grSel ? ' sel' : '') +
      (grMatchSet.has(i) ? ' match' : '') + (grMatches[grMatchAt] === i ? ' match-cur' : '');
    html += '<div class="gr-row' + cls + '" data-gr-row="' + i + '" style="top:' + i * GR_ROW + 'px">' +
      '<span class="gr-subject">' + refs + (role ? '<span class="gr-role">' + role + '</span>' : '') + esc(r.s) + '</span>' +
      '<span class="gr-author">' + esc(r.a) + '</span>' +
      '<span class="gr-date" title="' + esc(new Date(r.t * 1000).toLocaleString()) + '">' + grRelTime(r.t) + '</span>' +
      '<span class="gr-sha">' + esc(r.h.slice(0, 7)) + '</span></div>';

    const y0 = (i - first) * GR_ROW, ym = y0 + GR_ROW / 2, y1 = y0 + GR_ROW;
    for (const [from, to, seg, kind] of r.e) {
      const bright = !f || f.segs.has(seg);
      const op = bright ? '' : ' opacity=".35"';
      let d;
      if (kind === 0) d = 'M' + grX(from) + ' ' + y0 + ' L' + grX(to) + ' ' + y1;
      else if (kind === 1) d = from === to ? 'M' + grX(from) + ' ' + y0 + ' L' + grX(to) + ' ' + ym
        : 'M' + grX(from) + ' ' + y0 + ' C' + grX(from) + ' ' + ym + ' ' + grX(to) + ' ' + y0 + ' ' + grX(to) + ' ' + ym;
      else d = from === to ? 'M' + grX(from) + ' ' + ym + ' L' + grX(to) + ' ' + y1
        : 'M' + grX(from) + ' ' + ym + ' C' + grX(to) + ' ' + y1 + ' ' + grX(to) + ' ' + ym + ' ' + grX(to) + ' ' + y1;
      paths += '<path d="' + d + '" stroke="' + grColor(seg) + '"' + op + '/>';
    }
    const merge = (r.p || []).length > 1;
    const c = grColor(r.g);
    const dop = inFocus ? '' : ' opacity=".35"';
    paths += merge
      ? '<circle cx="' + grX(r.l) + '" cy="' + ym + '" r="4.5" fill="var(--bg)" stroke="' + c + '" stroke-width="2"' + dop + '/>'
      : '<circle cx="' + grX(r.l) + '" cy="' + ym + '" r="4" fill="' + c + '"' + dop + '/>';
  }
  rowsEl.innerHTML = html;
  svg.setAttribute('width', graphW);
  svg.setAttribute('height', Math.max(0, last - first) * GR_ROW);
  svg.style.top = first * GR_ROW + 'px';
  svg.innerHTML = paths;
  if (!grRows.length && !grLoading && !grError) rowsEl.innerHTML = '<div class="hint gr-empty">No commits.</div>';
}

function grDrawBranches() {
  const root = grEl();
  if (!root) return;
  const list = root.querySelector('.gr-branches');
  const chips = root.querySelector('.gr-filters');
  if (grBranchError) { list.innerHTML = '<div class="gr-note gr-note-err">' + esc(grBranchError) + '</div>'; return; }
  if (!grBranches) return;
  root.querySelector('.gr-default').textContent = grBranches.default ? 'vs ' + grBranches.default : 'no default branch';
  const all = grBranches.branches || [];
  const counts = {};
  for (const b of all) counts[b.category] = (counts[b.category] || 0) + 1;
  chips.innerHTML = ['all', 'active', 'stale', 'gone', 'squash_merged', 'merged', 'orphan'].filter(c => c === 'all' || counts[c])
    .map(c => '<button class="opt' + (grCat === c ? ' on' : '') + '" type="button" data-gr-cat="' + c + '" title="' + esc(GR_CAT_TITLE[c] || 'Every branch') + '">' +
      esc(c === 'all' ? 'all' : GR_CAT_LABEL[c]) + ' <span class="gr-chip-n">' + (c === 'all' ? all.length : counts[c]) + '</span></button>').join('');
  const shown = all.filter(b => grCat === 'all' || b.category === grCat);
  const focusRef = grFocus?.key?.startsWith('ref:') ? grFocus.key.slice(4) : '';
  list.innerHTML = shown.length ? shown.map(b =>
    '<div class="gr-branch' + (b.name === focusRef ? ' on' : '') + '" data-gr-branch="' + esc(b.name) + '" title="Focus ' + esc(b.name) + '">' +
      '<div class="gr-branch-top"><span class="gr-cat gr-cat-' + b.category + '" title="' + esc(GR_CAT_TITLE[b.category] || '') + '">' + esc(GR_CAT_LABEL[b.category] || b.category) + '</span>' +
      '<span class="gr-bname">' + esc(b.name) + '</span>' + (b.current ? '<span class="gr-current" title="Checked out">●</span>' : '') + '</div>' +
      '<div class="gr-bmeta"><span>' + esc(b.author) + '</span><span>' + grRelTime(b.date) + '</span>' +
      (b.category !== 'default' && (b.ahead || b.behind) ? '<span class="gr-ab" title="' + b.ahead + ' commits ahead of, ' + b.behind + ' behind the default branch">↑' + b.ahead + ' ↓' + b.behind + '</span>' : '') +
      (b.prUrl ? '<a href="' + esc(b.prUrl) + '" target="_blank" rel="noopener noreferrer">PR</a>' : '') +
      grCleanupHtml(b) +
      '</div></div>').join('') : '<div class="hint">No branches in this category.</div>';
}

/* The command that deletes a branch worth cleaning up, for the reviewer to
   copy and run. px0 never runs it. A merged or gone local branch can go with
   `git branch -d`, which refuses anything unmerged; a stale or squash-merged
   one needs -D, which the button's title says. */
function grCleanupHtml(b) {
  if (!['merged', 'gone', 'stale', 'squash_merged'].includes(b.category) || b.current) return '';
  let cmd, note;
  if (b.kind === 'remote') {
    const slash = b.name.indexOf('/');
    if (slash < 0) return '';
    cmd = 'git push ' + b.name.slice(0, slash) + ' --delete ' + b.name.slice(slash + 1);
    note = 'Deletes the branch on the remote';
  } else if (b.category === 'merged' || b.category === 'gone') {
    cmd = 'git branch -d ' + b.name;
    note = b.category === 'gone' ? 'git refuses if it has commits that were never merged' : 'Its commits are all in the default branch';
  } else {
    cmd = 'git branch -D ' + b.name;
    note = 'Force-deletes: its commits are not in the default branch';
  }
  return '<button class="opt gr-copy" type="button" data-gr-copy="' + esc(cmd) + '" title="Copy: ' + esc(cmd) + ' (' + esc(note) + ')">Copy delete</button>';
}

/* ---------- commit inspector ---------- */

async function grOpenCommit(sha, { scroll }) {
  if (!sha) return;
  const root = grEl();
  if (!root) return;
  if (grSel !== sha) grOpenFiles.clear();
  grSel = sha;
  grQueueDraw();
  const pane = root.querySelector('.gr-detail');
  pane.hidden = false;
  let d;
  try {
    d = await api('/api/graph/commit', { sha });
  } catch (e) {
    pane.innerHTML = '<div class="gr-note gr-note-err">' + esc(e.message || 'Could not read the commit') + '</div>';
    return;
  }
  if (grSel !== sha) return;
  const [subject, ...rest] = d.message.split('\n');
  const body = rest.join('\n').trim();
  const refs = (d.refs || []).map(ref => '<span class="gr-ref gr-ref-' + ref.k + (ref.h ? ' gr-ref-head' : '') + '" data-gr-ref="' + esc(ref.n) + '">' + esc(ref.n) + '</span>').join('');
  const parents = d.parents.map((p, i) => '<a href="#" data-gr-commit="' + esc(p) + '"><code>' + esc(p.slice(0, 7)) + '</code></a>' + (d.parents.length > 1 ? (i === 0 ? ' (first)' : '') : '')).join(', ');
  const files = d.files.map(f => {
    const stat = f.added < 0 ? 'binary' : '<span class="gr-add">+' + f.added + '</span> <span class="gr-del">−' + f.deleted + '</span>';
    return '<div class="gr-file' + (grOpenFiles.has(f.path) ? ' open' : '') + '" data-gr-file="' + esc(f.path) + '" data-gr-old="' + esc(f.oldPath || '') + '">' +
      '<div class="gr-file-head"><span class="gr-fstat gr-fstat-' + esc(f.status) + '">' + esc(f.status) + '</span>' +
      '<span class="gr-fpath" title="' + esc(f.oldPath ? f.oldPath + ' → ' + f.path : f.path) + '">' + esc(f.oldPath ? f.oldPath + ' → ' + f.path : f.path) + '</span>' +
      '<span class="gr-fnum">' + stat + '</span></div><div class="gr-diff"></div></div>';
  }).join('');
  pane.innerHTML =
    '<div class="gr-detail-head"><code class="gr-detail-sha" title="' + esc(d.sha) + '">' + esc(d.sha.slice(0, 10)) + '</code>' +
      '<button class="mini" type="button" data-gr-copy="' + esc(d.sha) + '" title="Copy the full hash">copy</button><span class="grow"></span>' +
      '<button class="mini" type="button" data-gr-close-detail title="Close">✕</button></div>' +
    '<div class="gr-detail-subject">' + esc(subject) + '</div>' +
    (refs ? '<div class="gr-detail-refs">' + refs + '</div>' : '') +
    (body ? '<pre class="gr-detail-body">' + esc(body) + '</pre>' : '') +
    '<div class="gr-detail-meta"><div><b>' + esc(d.author) + '</b> &lt;' + esc(d.email) + '&gt;</div>' +
      '<div>' + esc(new Date(d.date * 1000).toLocaleString()) + (d.committer !== d.author ? ' · committed by ' + esc(d.committer) : '') + '</div>' +
      (parents ? '<div>Parents: ' + parents + '</div>' : '<div>Root commit</div>') + '</div>' +
    '<div class="gr-detail-files-head">' + d.files.length + ' file' + (d.files.length === 1 ? '' : 's') + ' changed' +
      (d.parents.length > 1 ? ' against the first parent' : '') + '</div>' +
    '<div class="gr-detail-files">' + files + '</div>';
  for (const el of pane.querySelectorAll('.gr-file.open')) grLoadFileDiff(el);
  if (scroll) {
    let i = grIndex.get(d.sha);
    if (i === undefined) {
      for (let pages = 0; i === undefined && grNext >= 0 && pages < 20; pages++) { await grLoadPage(false, 5000); i = grIndex.get(d.sha); }
    }
    if (i !== undefined) grScrollToRow(i);
  }
}

function grScrollToRow(i) {
  const sc = grEl()?.querySelector('.gr-scroll');
  if (!sc) return;
  const top = i * GR_ROW;
  if (top < sc.scrollTop || top + GR_ROW > sc.scrollTop + sc.clientHeight) sc.scrollTop = Math.max(0, top - sc.clientHeight / 3);
  grQueueDraw();
}

function grToggleFileDiff(el) {
  if (!el.closest('.gr-detail')) return;
  const path = el.dataset.grFile;
  if (grOpenFiles.has(path)) { grOpenFiles.delete(path); el.classList.remove('open'); return; }
  grOpenFiles.add(path);
  el.classList.add('open');
  grLoadFileDiff(el);
}

async function grLoadFileDiff(el) {
  const box = el.querySelector('.gr-diff');
  if (!box || box.dataset.loaded) return;
  box.dataset.loaded = '1';
  box.innerHTML = '<div class="hint">Loading…</div>';
  try {
    const j = await api('/api/graph/diff', { sha: grSel, path: el.dataset.grFile, old: el.dataset.grOld || undefined });
    const lines = (j.diff || '').split('\n').filter(l => !/^(diff --git|index |--- |\+\+\+ |similarity |rename )/.test(l));
    box.innerHTML = lines.length && j.diff ? '<pre>' + lines.map(l => {
      const c = l.startsWith('+') ? 'gr-add' : l.startsWith('-') ? 'gr-del' : l.startsWith('@@') ? 'gr-hunk' : '';
      return '<span class="' + c + '">' + esc(l) + '</span>';
    }).join('\n') + (j.truncated ? '\n<span class="gr-hunk">… diff truncated</span>' : '') + '</pre>' : '<div class="hint">No textual changes.</div>';
  } catch (e) {
    box.innerHTML = '<div class="gr-note gr-note-err">' + esc(e.message || 'Could not load the diff') + '</div>';
  }
}

/* ---------- commit search ---------- */

async function grRunSearch(q, { keepAt = false } = {}) {
  grQuery = q;
  const n = grEl()?.querySelector('.gr-search-n');
  if (!q) {
    grMatches = []; grMatchSet = new Set(); grMatchAt = -1;
    if (n) n.textContent = '';
    grQueueDraw();
    return;
  }
  if (n) n.textContent = '…';
  let j;
  try {
    j = await api('/api/graph/search', { q, sig: grSig });
  } catch (e) {
    if (n) n.textContent = '!';
    return;
  }
  if (j.reset) { await grCheckRefs(); return; }
  if (q !== grQuery) return; // typed on
  grMatches = j.matches || [];
  grMatchSet = new Set(grMatches);
  if (!keepAt || grMatchAt >= grMatches.length) grMatchAt = grMatches.length ? 0 : -1;
  if (n) n.textContent = grMatches.length ? (grMatchAt + 1) + '/' + grMatches.length + (j.capped ? '+' : '') : 'no match';
  if (grMatchAt >= 0 && !keepAt) await grShowMatch();
  grQueueDraw();
}

async function grStepMatch(dir, q) {
  if (q !== grQuery) { await grRunSearch(q); return; }
  if (!grMatches.length) return;
  grMatchAt = (grMatchAt + dir + grMatches.length) % grMatches.length;
  const n = grEl()?.querySelector('.gr-search-n');
  if (n) n.textContent = (grMatchAt + 1) + '/' + grMatches.length;
  await grShowMatch();
}

async function grShowMatch() {
  const i = grMatches[grMatchAt];
  if (i === undefined) return;
  await grEnsureLoaded(i);
  grScrollToRow(i);
}

function grDrawFocusBar() {
  const root = grEl();
  if (!root) return;
  const el = root.querySelector('.gr-focus-label');
  if (!grFocus) {
    el.innerHTML = '<span class="gr-hintline">Click a branch, a ref, or any lane to follow one branch. Esc clears.</span>';
    return;
  }
  const n = grFocus.shas.size;
  el.innerHTML = 'Focused: <b>' + esc(grFocus.label) + '</b> · ' + n + ' commit' + (n === 1 ? '' : 's') +
    (grFocus.fork ? ' · forked from <code>' + esc(grFocus.fork.slice(0, 7)) + '</code>' : '') +
    (grFocus.merges.size ? ' · merged in <code>' + [...grFocus.merges].map(m => esc(m.slice(0, 7))).join('</code>, <code>') + '</code>' : '') +
    ' <button class="opt" type="button" data-gr-clear>Clear</button>';
}

/* ---------- focus ---------- */

function grSegsOf(shas) {
  const segs = new Set();
  for (const sha of shas) {
    const i = grIndex.get(sha);
    if (i !== undefined) segs.add(grRows[i].g);
  }
  return segs;
}

// key is "ref:<branch>", "sha:<commit>" or "seg:<id>".
async function grApplyFocusKey(key, { scroll }) {
  const [kind, ...rest] = key.split(':');
  const val = rest.join(':');
  if (!val || !['ref', 'sha', 'seg'].includes(kind)) return;
  let f;
  try {
    f = await api('/api/graph/focus', { [kind]: val });
  } catch (e) {
    showToast('!', e.message || 'Could not focus');
    return;
  }
  const label = kind === 'ref' ? val : kind === 'sha' ? 'lane of ' + val.slice(0, 7) : 'lane';
  const shas = new Set(f.shas || []);
  grSetFocus({
    key: kind === 'seg' ? 'sha:' + (f.tip || '') : key, label, shas, fork: f.fork || '',
    merges: new Set([...(f.merges || []), ...(f.merge ? [f.merge] : [])]), segs: grSegsOf(shas),
  });
  if (scroll) await grScrollToFocus();
}

function grSetFocus(f) {
  grFocus = f;
  try {
    const url = new URL(location.href);
    url.hash = f && f.key ? 'graph=' + encodeURIComponent(f.key) : '';
    history.replaceState(history.state, '', url.href.replace(/#$/, ''));
  } catch {}
  grDrawFocusBar();
  grDrawBranches();
  grQueueDraw();
}

/* Scrolls to the newest focused commit, reading more pages (up to ten) if
   it is not loaded yet. */
async function grScrollToFocus() {
  const f = grFocus;
  if (!f) return;
  const find = () => {
    let best = -1;
    for (const sha of f.shas) {
      const i = grIndex.get(sha);
      if (i !== undefined && (best < 0 || i < best)) best = i;
    }
    return best;
  };
  let i = find();
  for (let pages = 0; i < 0 && grNext >= 0 && pages < 10; pages++) {
    await grLoadPage(false);
    i = find();
  }
  if (grFocus === f) f.segs = grSegsOf(f.shas);
  const sc = grEl()?.querySelector('.gr-scroll');
  if (!sc || i < 0) { grQueueDraw(); return; }
  const top = i * GR_ROW, bottom = top + GR_ROW;
  if (top < sc.scrollTop || bottom > sc.scrollTop + sc.clientHeight) sc.scrollTop = Math.max(0, top - sc.clientHeight / 3);
  grQueueDraw();
}

/* A click in the lane area: on a commit's dot focuses its lane; on a line
   focuses the lane segment that line belongs to; anywhere else clears. */
function grLaneClick(e, root) {
  const sizer = root.querySelector('.gr-sizer');
  const rect = sizer.getBoundingClientRect();
  const y = e.clientY - rect.top, x = e.clientX - rect.left;
  const i = Math.floor(y / GR_ROW);
  const r = grRows[i];
  if (!r) { grSetFocus(null); return; }
  const laneF = (x - GR_PAD - GR_LANE / 2) / GR_LANE; // click position in lanes
  const inRow = (y - i * GR_ROW) / GR_ROW;            // 0 at the top of the row, 1 at the bottom
  if (Math.abs(laneF - r.l) < 0.5 && Math.abs(inRow - 0.5) < 0.35) {
    grApplyFocusKey('sha:' + r.h, { scroll: false });
    return;
  }
  // Where each edge is at this height, as a lane position; the nearest wins.
  let seg = -1, best = 0.5;
  for (const [from, to, s, kind] of r.e) {
    let t;
    if (kind === 0) t = inRow;
    else if (kind === 1) { if (inRow > 0.5) continue; t = inRow * 2; }
    else { if (inRow < 0.5) continue; t = (inRow - 0.5) * 2; }
    const d = Math.abs(from + (to - from) * t - laneF);
    if (d < best) { best = d; seg = s; }
  }
  if (seg < 0) { grSetFocus(null); return; }
  grApplyFocusKey('seg:' + seg, { scroll: false });
}
