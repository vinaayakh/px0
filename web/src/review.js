// web/src/review.js
// AI draft review, active only in PR sessions (S.meta.pr). The AI Review tab
// (pr://review, a pinned virtual tab) runs a review skill through the
// selected harness, read-only (review.go), then lists what came back as
// suggestions. Each one is added to the review, edited or dismissed here or
// on its line in Files changed (prfiles.js, which reads them through
// reviewSuggestions() and acts through triageSuggestions()); added ones
// become ordinary drafts (pr.js), and nothing is posted until the reviewer
// submits the review or posts one on its own. Pending suggestions also get a
// gutter marker in the single-file diff, drawn through pr.js's marker hook.
import { $, S, esc, api, apiPostJson, keyLabel } from './state.js';
import { on, emit } from './bus.js';
import { showToast } from './ui.js';
import { registerAgentPicker } from './agent.js';
import { registerVirtualTab } from './virtualtab.js';
import { openFile } from './tabs.js';
import { refreshComments, setPRMarkerHook, renderPRMarkers, prefillReview } from './pr.js';

const RV_PATH = 'pr://review';
let rvPage = null;        // the page, moved into the tab's article while shown

let rvRun = null;         // latest run: {id, status, summary, verdict, counts, tainted, ...}
let rvItems = [];         // AI suggestions, every status
let rvFilter = 'pending'; // 'pending' or 'all'
let rvSelId = 0;          // selected suggestion id
let rvEditId = 0;         // suggestion being edited
let rvPollTimer = 0;
let rvPrefilled = 0;      // run id whose summary was already offered to the form
let rvStale = 0;          // pending suggestions made against an older head
let rvHeadMoved = false;  // GitHub has a newer head than this checkout
const rvExpanded = new Set(); // low-confidence ids the reviewer opened

const RV_SEV_RANK = { blocker: 0, major: 1, minor: 2, nit: 3 };
const RV_LOW_CONFIDENCE = 0.5;
const RV_ANCHOR_LABEL = { anchored: '', reanchored: 're-anchored', file: 'file-level', summary: 'summary note' };

export function initReview() {
  rvPage = $('#rv-page');
  if (!S.meta?.pr) { rvPage?.remove(); return; }
  if (rvPage) { rvPage.remove(); rvPage.hidden = false; }
  registerVirtualTab(RV_PATH, { title: () => 'AI Review', pinned: true, render: rvRenderTab });
  registerAgentPicker({ el: rvQ('#rv-run'), harnessSelect: rvQ('#rv-harness'), modelSelect: rvQ('#rv-model') });
  on('agent:meta', rvRenderRun);
  $('#pr-ai-review')?.addEventListener('click', () => openFile(RV_PATH));
  rvQ('#rv-start')?.addEventListener('click', () => rvStart());
  rvQ('#rv-cancel')?.addEventListener('click', rvCancel);
  rvQ('#rv-focus')?.addEventListener('keydown', e => {
    if (e.key === 'Enter') { e.preventDefault(); rvStart(); }
  });
  rvQ('#rv-filters')?.addEventListener('click', e => {
    const b = e.target.closest('[data-rv-filter]');
    if (!b) return;
    rvFilter = b.dataset.rvFilter;
    for (const x of rvQ('#rv-filters').children) x.classList.toggle('on', x === b);
    rvRenderList();
  });
  rvQ('#rv-dismiss-nits')?.addEventListener('click', () => {
    const ids = rvItems.filter(s => s.status === 'pending' && s.ai?.severity === 'nit').map(s => s.id);
    if (ids.length) rvTriage(ids, 'dismiss');
  });
  rvQ('#rv-alert')?.addEventListener('click', e => {
    if (e.target.closest('[data-rv-retry]')) rvStart();
    if (e.target.closest('[data-rv-recheck]')) rvRecheck();
  });
  on('pr:refreshed', rvLoad); // a Pull moved the head: suggestions may be stale now
  const list = rvQ('#rv-list');
  list?.addEventListener('click', rvOnClick);
  list?.addEventListener('keydown', rvOnKey);
  setPRMarkerHook(rvDrawMarkers);
  on('pr:submitted', rvLoad);
  on('pr:drafts-changed', rvLoad); // discarded drafts: accepted suggestions are dismissed now
  rvLoad();
}

// The tab draws by moving the one page into its article, so its controls
// keep their listeners and the list keeps its state between visits.
function rvRenderTab(article) {
  article.classList.add('rv-host');
  if (rvPage) article.append(rvPage);
  rvLoad();
}

/* For Files changed (prfiles.js): every suggestion, and the triage actions. */
export function reviewSuggestions() { return rvItems; }

export async function triageSuggestions(ids, action, body) {
  return rvTriage(ids, action, body);
}

/* ---------- server state ---------- */

async function rvLoad() {
  let j;
  try {
    j = await api('/api/pr/review/suggestions');
  } catch {
    return;
  }
  const wasRunning = rvRun?.status === 'running';
  rvRun = j.run || null;
  rvItems = j.suggestions || [];
  rvStale = j.stale || 0;
  rvHeadMoved = !!(j.remoteHead && j.remoteHead !== j.head);
  rvRender();
  if (rvRun?.status === 'running') {
    rvSchedulePoll();
    return;
  }
  if (wasRunning && rvRun) rvAnnounce();
  if (rvRun?.status === 'done' && rvPrefilled !== rvRun.id) {
    rvPrefilled = rvRun.id;
    prefillReview(rvRun.summary, rvRun.verdict);
  }
  renderPRMarkers();
}

function rvSchedulePoll() {
  clearTimeout(rvPollTimer);
  rvPollTimer = setTimeout(rvLoad, 1000);
}

function rvAnnounce() {
  const r = rvRun;
  if (r.status === 'done') {
    const n = rvItems.filter(s => s.runId === r.id).length;
    showToast('AI', n ? `AI review: ${n} suggestion${n === 1 ? '' : 's'} to triage` : 'AI review found nothing to comment on');
  } else if (r.status === 'failed') {
    showToast('!', 'AI review failed: ' + (r.error || 'unknown error'));
  }
  if (r.tainted) showToast('!', 'The harness changed files during a read-only review', 6000);
}

async function rvStart() {
  if (rvRun?.status === 'running') return;
  const focus = rvQ('#rv-focus')?.value.trim() || '';
  const btn = rvQ('#rv-start');
  if (btn) btn.disabled = true;
  try {
    const j = await apiPostJson('/api/pr/review/run', { focus });
    rvRun = j.run;
    rvRender();
    rvSchedulePoll();
  } catch (e) {
    showToast('!', e.message || 'Could not start the AI review');
    rvRenderRun();
  }
}

async function rvCancel() {
  try { await apiPostJson('/api/pr/review/cancel', {}); } catch (e) { showToast('!', e.message || 'Could not cancel'); }
  rvLoad();
}

async function rvRecheck() {
  try {
    const j = await apiPostJson('/api/pr/review/revalidate', {});
    const c = j.counts || {};
    const n = Object.values(c).reduce((a, b) => a + b, 0);
    showToast('✓', n ? `Re-checked ${n} suggestion${n === 1 ? '' : 's'} against the current diff` : 'Nothing to re-check');
  } catch (e) {
    showToast('!', e.message || 'Could not re-check the suggestions');
  }
  await rvLoad();
  renderPRMarkers();
}

async function rvTriage(ids, action, body) {
  try {
    const j = await apiPostJson('/api/pr/review/triage', { ids, action, body: body || '' });
    const byId = new Map((j.suggestions || []).map(s => [s.id, s]));
    rvItems = rvItems.map(s => byId.get(s.id) || s);
    if (j.staleRefused) showToast('!', j.staleRefused + ' stale suggestion' + (j.staleRefused === 1 ? ' was' : 's were') + ' skipped: re-check them first');
  } catch (e) {
    showToast('!', e.message || 'Could not update the suggestion');
    return false;
  }
  rvEditId = 0;
  await refreshComments(); // accepted ones are drafts now: bar count, bottom panel, gutter
  rvRender();
  renderPRMarkers();
  return true;
}

// Re-reads the suggestions after something else changed them (a post from
// Files changed). Exported for prfiles.js.
export function reloadSuggestions() { return rvLoad(); }

/* ---------- rendering ---------- */

function rvHarnessInfo() {
  const name = S.meta?.agent || '';
  const h = (S.meta?.agents || []).find(a => a.name === name);
  return { name, readOnly: !!h?.readOnly };
}

function rvRender() {
  rvRenderRun();
  rvRenderList();
  const pending = rvItems.filter(s => s.status === 'pending').length;
  const el = $('#pr-ai-count');
  if (el) {
    el.hidden = !pending;
    el.textContent = pending ? String(pending) : '';
  }
  emit('review:changed');
}

function rvElapsed(r) {
  const t = Date.parse(r.startedAt);
  if (!t) return '';
  const s = Math.max(0, Math.round((Date.now() - t) / 1000));
  return s < 60 ? s + 's' : Math.floor(s / 60) + 'm ' + (s % 60) + 's';
}

function rvRenderRun() {
  const r = rvRun;
  const running = r?.status === 'running';
  const h = rvHarnessInfo();
  const start = rvQ('#rv-start');
  if (start) {
    start.hidden = running;
    start.disabled = !h.readOnly;
    start.textContent = r && r.status !== 'running' ? 'Run Again' : 'Run AI Review';
    start.title = h.readOnly ? 'Review this PR with ' + h.name + ', read-only'
      : 'AI review needs a harness px0 can run read-only (claude or codex)';
  }
  const cancel = rvQ('#rv-cancel');
  if (cancel) cancel.hidden = !running;
  rvQ('#rv-run')?.classList.toggle('busy', running);

  const status = rvQ('#rv-status');
  if (status) {
    if (!h.name) status.textContent = 'Choose a coding harness first.';
    else if (!h.readOnly && !running) status.textContent = h.name + ' has no read-only mode px0 can enforce; pick claude or codex.';
    else if (running) status.textContent = 'Reviewing with ' + (r.harness || h.name) + '… ' + rvElapsed(r);
    else if (r?.status === 'done') status.textContent = rvCountsText(r);
    else if (r?.status === 'cancelled') status.textContent = 'Cancelled.';
    else status.textContent = '';
  }

  const alert = rvQ('#rv-alert');
  if (alert) {
    let html = '';
    if (rvStale) {
      html += '<div class="rv-alert-item rv-alert-taint"><b>' + rvStale + ' suggestion' + (rvStale === 1 ? ' was' : 's were') + ' made against an older head.</b> ' +
        (rvHeadMoved ? 'The PR has new commits on GitHub: Pull them in the git panel, then re-check. '
          : 'Their lines may have moved, so they cannot be accepted until they are checked against the current diff. ') +
        '<button class="opt" type="button" data-rv-recheck' + (rvHeadMoved ? ' disabled' : '') + '>Re-check</button></div>';
    }
    if (r?.tainted) {
      html += '<div class="rv-alert-item rv-alert-taint"><b>The harness changed files during a read-only review:</b> ' +
        esc((r.changed || []).join(', ')) + '. px0 did not revert them. Check the working tree before trusting this run.</div>';
    }
    if (r?.status === 'failed') {
      html += '<div class="rv-alert-item"><b>AI review failed:</b> ' + esc(r.error || 'unknown error') +
        ' <button class="opt" type="button" data-rv-retry>Retry</button>' +
        (r.raw ? '<details><summary>Harness output</summary><pre class="rv-raw">' + esc(r.raw) + '</pre></details>' : '') + '</div>';
    }
    if (r?.diffOmitted && running) {
      html += '<div class="rv-alert-item rv-alert-note">The diff is large, so the harness gets the file list and reads what it needs.</div>';
    }
    alert.innerHTML = html;
    alert.hidden = !html;
  }

  const sum = rvQ('#rv-summary');
  if (sum) {
    const show = r?.status === 'done' && (r.summary || r.suggestedApprove);
    sum.hidden = !show;
    if (show) {
      sum.innerHTML = (r.summary ? '<div class="rv-summary-text">' + esc(r.summary) + '</div>' : '') +
        '<div class="rv-summary-foot">' +
        (r.verdict === 'request_changes' ? '<span class="rv-verdict">Suggested verdict: request changes</span>' : '') +
        (r.suggestedApprove ? '<span class="rv-verdict">The AI suggested approving. px0 never selects Approve for you.</span>' : '') +
        '<span class="grow"></span>' +
        '<button class="opt" type="button" data-rv-use-summary>Use as review body</button></div>';
      sum.querySelector('[data-rv-use-summary]')?.addEventListener('click', () => {
        const b = $('#pr-review-body');
        if (b) b.value = '';
        prefillReview(r.summary, r.verdict);
        showToast('✓', 'Summary copied into the review form');
      });
    }
  }
}

function rvCountsText(r) {
  const c = r.counts || {};
  const total = Object.values(c).reduce((a, b) => a + b, 0);
  if (!total) return 'No suggestions.';
  const parts = [];
  if (c.anchored) parts.push(c.anchored + ' on their line');
  if (c.reanchored) parts.push(c.reanchored + ' re-anchored');
  if (c.file) parts.push(c.file + ' file-level');
  if (c.summary) parts.push(c.summary + ' for the summary');
  if (c.repeat) parts.push(c.repeat + ' already decided, hidden');
  return total + ' suggestion' + (total === 1 ? '' : 's') + ': ' + parts.join(', ') + '.';
}

function rvVisible() {
  const items = rvFilter === 'pending' ? rvItems.filter(s => s.status === 'pending') : rvItems.slice();
  items.sort((a, b) => a.path.localeCompare(b.path) ||
    (RV_SEV_RANK[a.ai?.severity] ?? 2) - (RV_SEV_RANK[b.ai?.severity] ?? 2) || a.line - b.line);
  return items;
}

function rvLocLabel(s) {
  if (s.subjectType === 'summary') return 'not in this PR';
  if (s.subjectType === 'file') return 'whole file' + (s.ai?.reportedLine ? ' (L' + s.ai.reportedLine + ')' : '');
  const range = s.startLine ? 'L' + s.startLine + '–' + s.line : 'L' + s.line;
  return range + (s.side === 'LEFT' ? ' (base)' : '');
}

function rvItemHtml(s) {
  const ai = s.ai || {};
  const sev = ai.severity || 'minor';
  const low = (ai.confidence ?? 1) < RV_LOW_CONFIDENCE;
  const collapsed = low && !rvExpanded.has(s.id) && s.id !== rvEditId;
  const anchor = RV_ANCHOR_LABEL[ai.anchor] || '';
  const anchorTitle = ai.anchor === 'reanchored'
    ? 'The model said line ' + ai.reportedLine + '; px0 found the quoted text on this line'
    : ai.anchor === 'file' ? 'The line could not be matched to the diff, so this comments on the whole file'
      : ai.anchor === 'summary' ? 'This file is not part of the PR, so the comment goes in the review body' : '';
  const cls = ['rv-item', 'rv-st-' + s.status];
  if (s.id === rvSelId) cls.push('sel');
  if (collapsed) cls.push('collapsed');
  let html = '<div class="' + cls.join(' ') + '" data-id="' + s.id + '">' +
    '<div class="rv-head">' +
      '<span class="rv-sev rv-sev-' + esc(sev) + '">' + esc(sev) + '</span>' +
      (ai.category ? '<span class="rv-cat">' + esc(ai.category) + '</span>' : '') +
      '<span class="rv-loc" data-rv-jump title="Open this line in the diff">' + esc(rvLocLabel(s)) + '</span>' +
      (anchor ? '<span class="rv-anchor rv-anchor-' + esc(ai.anchor) + '" title="' + esc(anchorTitle) + '">' + esc(anchor) + '</span>' : '') +
      (ai.stale ? '<span class="rv-anchor rv-anchor-file" title="Made against an older PR head: re-check before accepting">stale</span>' : '') +
      '<span class="grow"></span>' +
      '<span class="rv-conf" title="Model confidence">' + Math.round((ai.confidence ?? 0) * 100) + '%</span>' +
      (s.status !== 'pending' ? '<span class="rv-status-chip">' + esc(s.status) + '</span>' : '') +
    '</div>';
  if (collapsed) {
    html += '<div class="rv-preview" data-rv-expand title="Low confidence: click to read">' + esc(s.body) + '</div></div>';
    return html;
  }
  if (s.id === rvEditId) {
    html += '<textarea class="rv-edit agent-input" rows="5" spellcheck="false">' + esc(s.body) + '</textarea>' +
      '<div class="rv-actions"><span class="agent-hint">' + esc(keyLabel('Mod+Enter')) + ' to save, Esc to cancel</span><span class="grow"></span>' +
      '<button class="opt" type="button" data-rv-act="cancel-edit">Cancel</button>' +
      '<button class="opt on" type="button" data-rv-act="save">Save &amp; add to review</button></div></div>';
    return html;
  }
  html += '<div class="rv-body">' + esc(s.body) + '</div>';
  if (ai.suggestion) {
    html += '<div class="rv-sugg-head">Suggested change</div><pre class="rv-sugg">' +
      ai.suggestion.split('\n').map(l => '<span class="rv-add">+ ' + esc(l) + '</span>').join('\n') + '</pre>';
  }
  html += '<div class="rv-actions">';
  if (s.status === 'pending') {
    html += '<button class="opt rv-accept" type="button" data-rv-act="accept" title="Add to your review as a draft comment (A)">Add to review</button>' +
      '<button class="opt" type="button" data-rv-act="edit" title="Edit, then add to your review (E)">Edit</button>' +
      '<button class="opt" type="button" data-rv-act="dismiss" title="Dismiss (D)">Dismiss</button>';
  } else if (s.status !== 'posted') {
    html += '<button class="opt" type="button" data-rv-act="restore" title="Back to pending">Undo</button>';
    if (s.status !== 'dismissed') html += '<button class="opt" type="button" data-rv-act="edit" title="Edit">Edit</button>';
  }
  html += '</div></div>';
  return html;
}

function rvRenderList() {
  const list = rvQ('#rv-list');
  if (!list) return;
  const items = rvVisible();
  if (!items.length) {
    const any = rvItems.length;
    list.innerHTML = '<div class="hint">' + (rvRun?.status === 'running' ? 'Waiting for the review…'
      : any ? 'Nothing pending. Suggestions you added are in your drafts; switch to All to see everything.'
        : 'Run an AI review to get suggested comments. Nothing is posted until you add a suggestion to your review and submit it, or post it on its own.') + '</div>';
    return;
  }
  if (!items.some(s => s.id === rvSelId)) rvSelId = items[0].id;
  let html = '';
  let path = null;
  for (const s of items) {
    if (s.path !== path) {
      path = s.path;
      const pendingHere = items.filter(x => x.path === path && x.status === 'pending').length;
      html += '<div class="rv-file"><span class="rv-file-path" data-rv-file="' + esc(path) + '" title="Show ' + esc(path) + ' in Files changed">' + esc(path) + '</span><span class="grow"></span>' +
        (pendingHere > 1 ? '<button class="opt" type="button" data-rv-accept-file="' + esc(path) + '" title="Add every pending suggestion in this file to your review">Add all</button>' : '') +
        '</div>';
    }
    html += rvItemHtml(s);
  }
  list.innerHTML = html;
  const ta = list.querySelector('.rv-edit');
  if (ta) { ta.focus(); ta.setSelectionRange(ta.value.length, ta.value.length); }
}

/* ---------- interaction ---------- */

function rvItemById(id) { return rvItems.find(s => s.id === id); }

function rvSelect(id, { scroll = true } = {}) {
  rvSelId = id;
  const list = rvQ('#rv-list');
  for (const el of list.querySelectorAll('.rv-item')) el.classList.toggle('sel', +el.dataset.id === id);
  if (scroll) list.querySelector('.rv-item.sel')?.scrollIntoView({ block: 'nearest' });
}

// Shows the suggestion on its line in Files changed, with its card open.
function rvJump(s) {
  if (!s || s.subjectType === 'summary') return;
  const target = { path: s.path, side: s.side || 'RIGHT', line: s.subjectType ? 0 : s.line, id: s.id };
  openFile('pr://files').then(() => emit('files:reveal', target));
}

function rvAct(s, act) {
  if (!s) return;
  switch (act) {
    case 'accept': rvTriage([s.id], 'accept'); break;
    case 'dismiss': rvTriage([s.id], 'dismiss'); break;
    case 'restore': rvTriage([s.id], 'restore'); break;
    case 'edit': rvEditId = s.id; rvRenderList(); break;
    case 'cancel-edit': rvEditId = 0; rvRenderList(); rvQ('#rv-list')?.focus(); break;
    case 'save': {
      const ta = rvQ('#rv-list .rv-edit');
      const body = ta?.value.trim();
      if (body) rvTriage([s.id], 'edit', body);
      break;
    }
  }
}

function rvOnClick(e) {
  const acceptFile = e.target.closest('[data-rv-accept-file]');
  if (acceptFile) {
    const path = acceptFile.dataset.rvAcceptFile;
    rvTriage(rvItems.filter(s => s.path === path && s.status === 'pending').map(s => s.id), 'accept');
    return;
  }
  const file = e.target.closest('[data-rv-file]');
  if (file) {
    openFile('pr://files').then(() => emit('files:reveal', { path: file.dataset.rvFile, line: 0 }));
    return;
  }
  const item = e.target.closest('.rv-item');
  if (!item) return;
  const s = rvItemById(+item.dataset.id);
  rvSelect(s.id, { scroll: false });
  const act = e.target.closest('[data-rv-act]');
  if (act) { rvAct(s, act.dataset.rvAct); return; }
  if (e.target.closest('[data-rv-expand]')) { rvExpanded.add(s.id); rvRenderList(); return; }
  if (e.target.closest('[data-rv-jump]') || !e.target.closest('textarea, button')) rvJump(s);
}

function rvOnKey(e) {
  if (e.target.closest('textarea')) {
    const s = rvItemById(rvEditId);
    if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); rvAct(s, 'cancel-edit'); }
    else if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); rvAct(s, 'save'); }
    return;
  }
  if (e.metaKey || e.ctrlKey || e.altKey) return;
  const items = rvVisible();
  const i = items.findIndex(s => s.id === rvSelId);
  const s = items[i];
  const key = e.key.toLowerCase();
  let handled = true;
  if (key === 'j' || e.key === 'ArrowDown') { if (items[i + 1]) rvSelect(items[i + 1].id); }
  else if (key === 'k' || e.key === 'ArrowUp') { if (i > 0) rvSelect(items[i - 1].id); }
  else if (key === 'a' && s?.status === 'pending') rvAct(s, 'accept');
  else if (key === 'd' && s?.status === 'pending') rvAct(s, 'dismiss');
  else if (key === 'e' && s && s.status !== 'dismissed' && s.status !== 'posted') rvAct(s, 'edit');
  else if (e.key === 'Enter' && s) {
    if (!rvExpanded.has(s.id)) { rvExpanded.add(s.id); rvRenderList(); } // opens a collapsed low-confidence item
    rvJump(s);
  } else handled = false;
  if (handled) { e.preventDefault(); e.stopPropagation(); }
}

/* ---------- gutter markers for pending suggestions ---------- */

const RV_MARK_ICON = '<svg viewBox="0 0 24 24" width="12" height="12" fill="currentColor" aria-hidden="true"><path d="M12 2l2.2 6.3L20.5 10l-6.3 2.2L12 18.5l-2.2-6.3L3.5 10l6.3-1.7z"/></svg>';

function rvDrawMarkers(path, rows) {
  for (const { el } of rows) el.querySelector('.pr-ai-mark')?.remove();
  const byKey = new Map();
  for (const s of rvItems) {
    if (s.status !== 'pending' || s.path !== path || s.subjectType) continue;
    const key = (s.side || 'RIGHT') + ':' + s.line;
    if (!byKey.has(key)) byKey.set(key, []);
    byKey.get(key).push(s);
  }
  if (!byKey.size) return;
  for (const { el, side, line } of rows) {
    const here = byKey.get(side + ':' + line);
    if (!here) continue;
    here.sort((a, b) => (RV_SEV_RANK[a.ai?.severity] ?? 2) - (RV_SEV_RANK[b.ai?.severity] ?? 2));
    const top = here[0];
    const badge = document.createElement('span');
    badge.className = 'pr-ai-mark rv-sev-' + (top.ai?.severity || 'minor');
    badge.title = 'AI suggestion (' + (top.ai?.severity || 'minor') + ', not posted): ' + top.body.slice(0, 160) +
      (here.length > 1 ? ' (+' + (here.length - 1) + ' more)' : '');
    badge.innerHTML = RV_MARK_ICON;
    badge.addEventListener('click', ev => {
      ev.stopPropagation();
      openFile(RV_PATH).then(() => {
        rvSelect(top.id);
        rvQ('#rv-list')?.focus();
      });
    });
    el.querySelector('.diff-code')?.before(badge);
  }
}

// The page's own elements, found whether or not the tab is showing it.
function rvQ(sel) {
  return rvPage?.querySelector(sel) || null;
}
