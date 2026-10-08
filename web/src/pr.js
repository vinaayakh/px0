// web/src/pr.js
// GitHub PR review: shown only when this process was launched as `px0 pr ...`
// (S.meta.pr, set by main.go/pr.go). A one-row header above the tabs names
// the PR; the Submit review tab (pr://submit, a pinned virtual tab) holds the
// drafts, the summary and Comment/Approve/Request changes. Selecting a diff line and
// pressing Alt+R (or the footer/context-menu action) drafts an inline review
// comment. Everything here talks to /api/pr/*; nothing is stored client-side
// beyond what's needed to repaint -- a page refresh re-fetches the server's
// in-memory draft list (pr.go's prSession), which is the only source of truth.
import { $, S, doc_, esc, api, apiPostJson, keyLabel, withKeys } from './state.js';
import { showToast } from './ui.js';
import { setReviewHandler, SEL_MENU_ITEMS } from './selbar.js';
import { diffview, setPRSyncHandler } from './diff.js';
import { reloadWorkspace } from './agent.js';
import { openFile, drawTabs } from './tabs.js';
import { layout, render } from './renderer.js';
import { openSettings } from './settings.js';
import { emit } from './bus.js';
import { openPullRequest } from './inbox.js';
import { registerVirtualTab } from './virtualtab.js';

let meta = null;      // this session's PR info: {number, title, base, head, writeAccess, readOnly}
let comments = [];    // draft comments known to the server
let issueComments = [];   // top-level PR conversation comments, already posted (fetched read-only)
let reviewComments = [];  // inline diff-line comments, already posted (fetched read-only) -- may include replies

const prBar = () => $('#pr-bar');
const list = () => $('#pr-comment-list');

const SR_PATH = 'pr://submit';
let srPage = null; // the Submit review page, moved into the tab's article while shown

export function initPR() {
  if (!S.meta || !S.meta.pr) return;
  meta = S.meta.pr;
  document.body.classList.add('pr-mode');

  if (!SEL_MENU_ITEMS.some(item => item.sel === 'review-comment')) {
    SEL_MENU_ITEMS.push({ sel: 'review-comment', label: 'Add Review Comment', keys: 'Alt+R' });
  }
  setReviewHandler(openCommentComposer);
  setPRSyncHandler(renderMarkersForActiveDoc);
  srPage = $('#sr-page');
  if (srPage) { srPage.remove(); srPage.hidden = false; }
  registerVirtualTab(SR_PATH, { title: () => 'Submit review', pinned: true, render: srRenderTab, count: () => comments.length });
  injectFooterButton();
  wireBarButtons();
  wireCommentsPanel();
  renderBar();
  refreshComments();
  refreshExistingComments();
}

// Re-fetches PR metadata and comments after an external change to the
// checkout -- specifically, the sidebar git panel's Pull fast-forwarding
// onto a new PR head -- so the bar, diff-base warning, and comments reflect
// the new state instead of the one captured at session start.
export async function refreshPRMeta() {
  if (!meta) return;
  try {
    const j = await api('/api/pr/meta');
    meta = { ...meta, ...j };
    renderBar();
  } catch {
    // Best-effort.
  }
  await refreshExistingComments();
  await refreshComments();
  emit('pr:refreshed');
}

function fmtTime(iso) {
  if (!iso) return '';
  try {
    return new Date(iso).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
  } catch {
    return iso;
  }
}

// Comments already posted on GitHub -- top-level conversation and inline diff
// comments -- fetched live (never cached) so another reviewer's activity
// shows up on the next open of the panel or diff.
export async function refreshExistingComments() {
  try {
    const j = await api('/api/pr/existing-comments');
    issueComments = j.issueComments || [];
    reviewComments = j.reviewComments || [];
    renderCommentsPanel();
    renderMarkersForActiveDoc();
  } catch {
    // Best-effort: panel/gutter just stay empty on a transient failure.
  }
}

// AI suggestions (review.js) live in the same server-side draft list; only
// the ones the reviewer accepted or edited are drafts here.
function isDraft(c) {
  return c.origin !== 'ai' || c.status === 'accepted' || c.status === 'edited';
}

export async function refreshComments() {
  try {
    const j = await api('/api/pr/comments');
    comments = (j.comments || []).filter(isDraft);
    renderBar();
    renderCommentsPanel();
    renderMarkersForActiveDoc();
  } catch {
    // Read-only or a transient error: the bar still shows PR metadata.
  }
}

// The Submit review page is out of the document whenever its tab is not
// shown, so its controls are looked up inside it rather than with $.
function srQ(sel) {
  return srPage?.querySelector(sel) || null;
}

// The tab draws by moving the one page into its article, so the summary
// being written survives switching tabs.
function srRenderTab(article) {
  article.classList.add('rv-host');
  if (srPage) article.append(srPage);
}

// Opens the Submit review tab with the summary focused.
export async function openSubmitReview() {
  await openFile(SR_PATH);
  const body = srQ('#pr-review-body');
  body?.focus();
  return body;
}

function renderBar() {
  const b = prBar();
  if (!b || !meta) return;
  b.hidden = false;
  $('#pr-badge').textContent = '#' + meta.number;
  const link = $('#pr-link');
  if (link) link.href = meta.url || '#';
  const mb = $('#pr-merged-badge');
  if (mb) mb.hidden = !meta.merged;
  $('#pr-title').textContent = meta.title;
  $('#pr-title').title = meta.title;
  $('#pr-refs').textContent = meta.base + ' ← ' + meta.head;
  srQ('#pr-draft-count').textContent = comments.length
    ? (comments.length + (comments.length === 1 ? ' draft comment goes out with this review' : ' draft comments go out with this review'))
    : 'No draft comments: the review is just the summary below.';
  const badge = $('#pr-draft-badge');
  if (badge) {
    badge.hidden = !comments.length;
    badge.textContent = comments.length ? String(comments.length) : '';
  }
  drawTabs(); // the Submit review tab's draft count
  const ro = $('#pr-readonly-note');
  if (ro) ro.hidden = !meta.readOnly;
  const dw = $('#pr-diff-warning');
  if (dw) {
    dw.hidden = !meta.diffBaseWarning;
    if (meta.diffBaseWarning) dw.title = meta.diffBaseWarning;
  }
  const batchBtn = srQ('#pr-batch-apply');
  if (batchBtn) {
    const hasApplicable = comments.some(c => c.path && c.line && c.body?.trim());
    batchBtn.hidden = !hasApplicable;
  }
  const discardBtn = srQ('#pr-discard-drafts');
  if (discardBtn) discardBtn.hidden = !comments.length;
  const reqBtn = srQ('#pr-submit-request-changes');
  const appBtn = srQ('#pr-submit-approve');
  if (reqBtn) reqBtn.hidden = !meta.writeAccess;
  if (appBtn) appBtn.hidden = !meta.writeAccess;
  const cmtBtn = srQ('#pr-submit-comment');
  if (cmtBtn) {
    cmtBtn.disabled = false;
    cmtBtn.title = meta.readOnly
      ? 'No GitHub token configured -- click to connect and submit'
      : 'Submit the review with its drafts, without approving or requesting changes';
  }
}

export function nudgeGitHubToken() {
  showToast('!', 'No GitHub token configured: set GITHUB_TOKEN, set GH_TOKEN, or run `gh auth login` -- or connect in Settings.', 5000);
  openSettings('ui', 'GitHub', 'github.token');
}

function wireBarButtons() {
  $('#pr-open-submit')?.addEventListener('click', openSubmitReview);
  srQ('#pr-batch-apply')?.addEventListener('click', batchApplyComments);
  srQ('#pr-discard-drafts')?.addEventListener('click', discardAllDrafts);
  $('#pr-end-review')?.addEventListener('click', endReview);
  srQ('#pr-submit-comment')?.addEventListener('click', () => submitReview('COMMENT'));
  srQ('#pr-submit-request-changes')?.addEventListener('click', () => submitReview('REQUEST_CHANGES'));
  srQ('#pr-submit-approve')?.addEventListener('click', () => submitReview('APPROVE'));
  $('#pr-readonly-note')?.addEventListener('click', nudgeGitHubToken);
}

const BATCH_LABEL = 'Fix locally with AI';

// Ends the review (prcleanup.go): the server removes the checkout, its refs
// and its session file, then stops. Anything that would be lost -- drafts,
// uncommitted edits, unpushed commits -- is named and confirmed first.
async function endReview() {
  const btn = $('#pr-end-review');
  const end = force => apiPostJson('/api/pr/end', { force });
  if (btn) btn.disabled = true;
  try {
    let r;
    try {
      r = await end(false);
    } catch (e) {
      const st = e.body?.state;
      if (!e.body?.needsConfirm || !st) throw e;
      const lost = [];
      if (st.drafts) lost.push(st.drafts + (st.drafts === 1 ? ' draft comment (not submitted)' : ' draft comments (not submitted)'));
      if (st.dirty) lost.push('uncommitted edits in the checkout');
      if (st.unpushed) lost.push(st.unpushed + (st.unpushed === 1 ? ' commit' : ' commits') + ' not pushed to the PR');
      if (!window.confirm('Ending the review deletes its checkout. These will be lost:\n\n- ' + lost.join('\n- ') + '\n\nEnd the review anyway?')) return;
      r = await end(true);
    }
    showReviewEnded(r.worktree);
  } catch (e) {
    showToast('!', e.message || 'Could not end the review');
  } finally {
    if (btn && btn.isConnected) btn.disabled = false;
  }
}

// The session is gone with the review: replace the page with a note, and
// stop everything that would keep calling the server.
function showReviewEnded(worktree) {
  emit('session:ended');
  const n = meta?.number ? '#' + meta.number : '';
  document.title = 'Review ended - px0';
  document.body.className = 'pr-ended-page';
  document.body.innerHTML =
    '<div class="pr-ended"><h1>Review of ' + esc(n) + ' ended</h1>' +
    '<p>The checkout' + (worktree ? ' at <code>' + esc(worktree) + '</code>' : '') + ' and the refs px0 fetched for it were removed, and this PR session has stopped.</p>' +
    '<p>You can close this tab.' + (meta?.url ? ' The PR is still on <a href="' + esc(meta.url) + '" target="_blank" rel="noopener">GitHub</a>.' : '') + '</p></div>';
}

// Drops every draft at once (pr.go): the reviewer's own are deleted and
// accepted AI suggestions go back to dismissed, where AI Review can restore
// them.
async function discardAllDrafts() {
  if (!comments.length) return;
  const n = comments.length;
  if (!window.confirm('Discard ' + n + (n === 1 ? ' draft comment' : ' draft comments') + '? Your own are deleted; accepted AI suggestions are dismissed (AI Review can restore them).')) return;
  try {
    await apiPostJson('/api/pr/comments/delete?all=1', {});
    closeAllComposers();
    await refreshComments();
    emit('pr:drafts-changed');
    showToast('✓', 'Discarded ' + n + (n === 1 ? ' draft' : ' drafts'));
  } catch (e) {
    showToast('!', e.message || 'Could not discard drafts');
  }
}

// Only the label swaps text; the icon markup (SVG + .footer-btn-label span,
// see index.html) stays put so the button never reverts to a plain-text look.
function setBatchBtnLabel(btn, text) {
  const label = btn?.querySelector('.footer-btn-label');
  if (label) label.textContent = text; else if (btn) btn.textContent = text;
}

// The instruction a draft becomes for Fix locally with AI. An AI suggestion that
// carries replacement code hands it over, so the harness applies the change
// the reviewer accepted rather than re-deriving it from the comment.
function batchInstruction(c) {
  let text = c.body.trim();
  if (c.ai?.suggestion) {
    const where = c.startLine && c.startLine < c.line ? 'lines ' + c.startLine + '-' + c.line : 'line ' + c.line;
    text += '\n\nSuggested replacement for ' + where + ':\n```\n' + c.ai.suggestion + '\n```';
  }
  return text;
}

async function batchApplyComments() {
  // Summary notes are about files outside the PR: nothing there to edit.
  const applicable = comments.filter(c => c.path && c.line && c.body?.trim() && c.subjectType !== 'summary');
  if (!applicable.length) {
    showToast('!', 'No draft comments on lines to fix');
    return;
  }
  const btn = srQ('#pr-batch-apply');
  if (btn) {
    btn.disabled = true;
    setBatchBtnLabel(btn, 'Fixing...');
  }
  const edits = applicable.map(c => ({
    path: c.path,
    l1: c.startLine && c.startLine < c.line ? c.startLine : c.line,
    l2: c.line,
    instruction: batchInstruction(c),
  }));
  try {
    const job = await apiPostJson('/api/agent/batch', { edits });
    showToast('AI', `${job.harness || 'The agent'} is editing this checkout for ${edits.length} draft comments...`);
    pollPRBatch(job.id, applicable.length);
  } catch (e) {
    if (btn) {
      btn.disabled = false;
      setBatchBtnLabel(btn, BATCH_LABEL);
    }
    showToast('!', e.message || 'Could not start the agent');
  }
}

async function pollPRBatch(id, count) {
  const btn = srQ('#pr-batch-apply');
  const poll = async () => {
    try {
      const j = await api('/api/agent/job?id=' + id);
      if (j.running) {
        const sec = Math.round((j.ms || 0) / 1000);
        if (btn) setBatchBtnLabel(btn, `Fixing... (${sec}s)`);
        setTimeout(poll, 600);
        return;
      }
      if (btn) {
        btn.disabled = false;
        setBatchBtnLabel(btn, BATCH_LABEL);
      }
      if (j.error) {
        showToast('!', `Agent error: ${j.error}`);
        if (j.changed?.length) await reloadWorkspace(null);
        return;
      }
      // The agent only edits files here: the drafts stay drafts, and nothing
      // reaches the PR until the reviewer pushes.
      const n = j.changed?.length || 0;
      showToast('✓', n
        ? `Edited ${n} ${n === 1 ? 'file' : 'files'} for ${count} draft comments. Nothing is on GitHub yet: check Git changes, then Push.`
        : 'The agent finished without changing any file.', 8000);
      await reloadWorkspace(null);
      await refreshComments();
    } catch (e) {
      if (btn) {
        btn.disabled = false;
        setBatchBtnLabel(btn, BATCH_LABEL);
      }
      showToast('!', e.message || 'The fix failed');
    }
  };
  setTimeout(poll, 400);
}

async function submitReview(event) {
  if (meta?.readOnly) {
    nudgeGitHubToken();
    return;
  }
  const bodyEl = srQ('#pr-review-body');
  const body = bodyEl ? bodyEl.value.trim() : '';
  if (event === 'REQUEST_CHANGES' && !body && !comments.length) {
    showToast('!', 'Add a comment or review body before requesting changes');
    return;
  }
  try {
    const res = await apiPostJson('/api/pr/submit', { event, body });
    if (res?.headMoved) {
      showToast('!', 'The PR has new commits on GitHub since this checkout. Your review was posted on the commit you reviewed; Pull to see the new ones.', 8000);
    }
    comments = [];
    emit('pr:submitted');
    for (const b of (srPage?.querySelectorAll('.pr-suggested') || [])) b.classList.remove('pr-suggested');
    if (bodyEl) bodyEl.value = '';
    closeAllComposers();
    renderBar();
    renderCommentsPanel();
    renderMarkersForActiveDoc();
    showToast('✓', event === 'APPROVE' ? 'Review approved'
      : event === 'REQUEST_CHANGES' ? 'Changes requested'
      : 'Review comment submitted');
  } catch (e) {
    showToast('!', e.message || 'Could not submit review');
  }
}

/* ---------- locating a diff row from a path/side/line ---------- */

// Shared by the composer (to pin itself beside its line) and the gutter
// markers below. Only searches the visible diff, and only when it's showing
// the same file -- a composer for a file that isn't on screen has nothing to
// find, which callers treat as "dock it instead" rather than an error.
function findDiffRowEl(path, side, line) {
  if (!diffview || diffview.hidden) return null;
  const d = doc_();
  if (!d || d.path !== path) return null;
  // Review comments are only ever posted against the PR's own diff (see
  // diff.js's reviewable flag), so a "Your changes" row sharing the same
  // line number must never be matched here.
  for (const el of diffview.querySelectorAll('[data-l]:not([data-reviewable="0"]), [data-old-l]:not([data-reviewable="0"])')) {
    const isOldOnly = el.dataset.oldL !== undefined && el.dataset.l === undefined;
    const elSide = isOldOnly ? 'LEFT' : 'RIGHT';
    const elLine = isOldOnly ? +el.dataset.oldL : +el.dataset.l;
    if (elSide === side && elLine === line) return el;
  }
  return null;
}

function flashDiffRow(el) {
  el.classList.add('pr-line-flash');
  setTimeout(() => el.classList.remove('pr-line-flash'), 1100);
}

// Opens path in diff view and brings the row for side:line into view. The
// diff may still be drawing right after the open, so one late retry.
export async function revealPRLine(path, side, line) {
  if (!findDiffRowEl(path, side, line)) {
    await openFile(path, { line: side === 'LEFT' ? undefined : line, view: 'diff' });
  }
  const show = () => {
    const el = findDiffRowEl(path, side, line);
    if (el) { el.scrollIntoView({ block: 'center', behavior: 'smooth' }); flashDiffRow(el); }
    return !!el;
  };
  if (!show()) setTimeout(show, 300);
}

/* ---------- inline draft comment composer ----------
   Each open composer is pinned beside the diff row it's on (recomputed as
   the diff scrolls) instead of sitting in one fixed spot -- so it never
   drifts from the line it's actually commenting on, and opening one never
   shifts the tabs or code beneath it (the list is an absolute overlay; see
   .pr-comment-list in style.css). A composer for a file that isn't the one
   on screen has no row to pin to, so it docks in the bottom-right corner
   until you jump back to it (click its ref, or the file's own tab). */

let seq = 0;
const openBoxes = new Map(); // id -> { box, path, side, line }
let trackingBound = false;

function ensureTracking() {
  if (trackingBound) return;
  trackingBound = true;
  let scheduled = false;
  const schedule = () => {
    if (scheduled) return;
    scheduled = true;
    requestAnimationFrame(() => { scheduled = false; repositionAll(); });
  };
  diffview?.addEventListener('scroll', schedule);
  addEventListener('resize', schedule);
}

function positionBox(entry) {
  const { box, path, side, line } = entry;
  const host = $('#editor');
  const target = findDiffRowEl(path, side, line);
  if (!host) return;
  if (!target) {
    box.classList.add('docked');
    box.style.top = '';
    box.style.left = '';
    // Stack docked boxes bottom-up so several at once don't overlap.
    const docked = [...openBoxes.values()].filter(e => e.box.classList.contains('docked'));
    const i = Math.max(0, docked.indexOf(entry));
    box.style.right = '16px';
    box.style.bottom = (16 + i * (box.offsetHeight + 8)) + 'px';
    return;
  }
  box.classList.remove('docked');
  box.style.right = '';
  box.style.bottom = '';
  const hostRect = host.getBoundingClientRect();
  const tRect = target.getBoundingClientRect();
  const left = Math.min(hostRect.width - box.offsetWidth - 16, Math.max(16, tRect.left - hostRect.left));
  let top = tRect.bottom - hostRect.top + 4;
  top = Math.max(4, Math.min(top, hostRect.height - box.offsetHeight - 4));
  box.style.left = left + 'px';
  box.style.top = top + 'px';
}

function repositionAll() {
  for (const entry of openBoxes.values()) positionBox(entry);
}

// Scrolls/switches back to the composer's line and calls it out, whether
// it's pinned on screen already or you've since navigated elsewhere.
async function revealComposer(entry) {
  const { path, side, line } = entry;
  if (!findDiffRowEl(path, side, line)) {
    await openFile(path, { line });
  }
  positionBox(entry);
  const target = findDiffRowEl(path, side, line);
  if (target) { target.scrollIntoView({ block: 'center', behavior: 'smooth' }); flashDiffRow(target); }
}

export function openCommentComposer(info) {
  if (!meta) return;
  const id = 'prc' + (++seq);
  const box = document.createElement('div');
  box.className = 'agent-box entering';
  box.dataset.id = id;
  const side = info.side || 'RIGHT';
  const line = side === 'LEFT' ? (info.delL1 || info.l1) : info.l1;
  const lineEnd = side === 'LEFT' ? (info.delL2 || info.l2) : info.l2;
  const ref = info.path + ':' + (line === lineEnd ? line : line + '-' + lineEnd) + (side === 'LEFT' ? ' (base)' : '');
  const modEnter = keyLabel('Mod+Enter');
  box.innerHTML =
    '<div class="agent-head"><span class="sel-chip">Review Comment</span>' +
    '<span class="agent-ref" role="button" tabindex="0" title="Jump to this line">' + esc(ref) + '</span>' +
    '<span class="grow"></span><button class="agent-close" title="Close (Esc)">✕</button></div>' +
    '<div class="agent-compose">' +
    '<textarea class="agent-input" rows="3" spellcheck="false" autocomplete="off" placeholder="Leave a comment on this line... (' + esc(modEnter) + ' to add)"></textarea>' +
    '<div class="agent-err" hidden></div>' +
    '<div class="agent-foot"><span class="agent-hint">' + esc(modEnter) + ' to add, Esc to cancel</span>' +
    '<button class="agent-send" title="Add comment (' + esc(modEnter) + ')">Add Comment</button></div></div>';

  const entry = { box, path: info.path, side, line };
  openBoxes.set(id, entry);
  ensureTracking();

  list().hidden = false;
  list().append(box);
  positionBox(entry);
  const target = findDiffRowEl(info.path, side, line);
  if (target) { target.scrollIntoView({ block: 'center', behavior: 'smooth' }); flashDiffRow(target); }
  const ta = /** @type {HTMLTextAreaElement|null} */ (box.querySelector('.agent-input'));
  if (ta) ta.focus();

  box.querySelector('.agent-ref')?.addEventListener('click', () => revealComposer(entry));

  const close = () => {
    openBoxes.delete(id);
    box.remove();
    if (!list().children.length) list().hidden = true;
  };
  box.querySelector('.agent-close')?.addEventListener('click', close);

  const send = async () => {
    if (!ta) return;
    const body = ta.value.trim();
    if (!body) return;
    const errEl = /** @type {HTMLElement|null} */ (box.querySelector('.agent-err'));
    if (errEl) errEl.hidden = true;
    try {
      const c = await apiPostJson('/api/pr/comments', { path: info.path, line, side, body });
      comments.push(c);
      close();
      renderBar();
      renderCommentsPanel();
      renderMarkersForActiveDoc();
    } catch (e) {
      if (errEl) {
        errEl.hidden = false;
        errEl.textContent = e.message || 'Could not add comment';
      }
    }
  };
  box.querySelector('.agent-send')?.addEventListener('click', send);
  ta?.addEventListener('keydown', (/** @type {KeyboardEvent} */ e) => {
    if (e.key === 'Escape') { e.preventDefault(); close(); }
    else if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); send(); }
  });
}

function closeAllComposers() {
  const l = list();
  openBoxes.clear();
  if (!l) return;
  l.replaceChildren();
  l.hidden = true;
}

/* ---------- gutter markers on the active diff ---------- */

const COMMENT_ICON = '<svg viewBox="0 0 24 24" width="12" height="12" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/></svg>';

// review.js draws its own markers for pending AI suggestions on the same rows.
let extraMarkers = null;
export function setPRMarkerHook(fn) { extraMarkers = fn; }
export function renderPRMarkers() { renderMarkersForActiveDoc(); }

// Pre-fills the review form from an AI review: the summary goes in the body
// only when it is empty (or always, with replace, for "Use as review body"),
// and the suggested verdict is highlighted, never chosen. An approve verdict
// is never highlighted either.
export function prefillReview(summary, verdict, replace = false) {
  const bodyEl = srQ('#pr-review-body');
  if (bodyEl && summary && (replace || !bodyEl.value.trim())) {
    bodyEl.value = summary;
    bodyEl.dispatchEvent(new Event('input'));
  }
  for (const b of (srPage?.querySelectorAll('.pr-suggested') || [])) b.classList.remove('pr-suggested');
  const target = verdict === 'request_changes' ? srQ('#pr-submit-request-changes') : srQ('#pr-submit-comment');
  if (target && !target.hidden) target.classList.add('pr-suggested');
}

function renderMarkersForActiveDoc() {
  if (!meta) return;
  // Runs whether or not a diff is on screen right now, so a composer whose
  // file just scrolled out of (or into) view re-docks or re-pins itself.
  repositionAll();
  if (!diffview || diffview.hidden) return;
  const d = doc_();
  if (!d) return;
  const draftsByKey = new Map();
  for (const c of comments) {
    if (c.path !== d.path) continue;
    const key = (c.side || 'RIGHT') + ':' + c.line;
    if (!draftsByKey.has(key)) draftsByKey.set(key, []);
    draftsByKey.get(key).push(c);
  }
  // Existing (already-posted) review comments: replies carry the same
  // path/line/side as their thread root, so grouping by key alone already
  // gathers a whole thread together.
  const existingByKey = new Map();
  for (const c of reviewComments) {
    if (c.path !== d.path) continue;
    const key = (c.side || 'RIGHT') + ':' + c.line;
    if (!existingByKey.has(key)) existingByKey.set(key, []);
    existingByKey.get(key).push(c);
  }
  for (const el of diffview.querySelectorAll('.pr-comment-mark')) el.remove();
  const rows = [];
  for (const el of diffview.querySelectorAll('[data-l]:not([data-reviewable="0"]), [data-old-l]:not([data-reviewable="0"])')) {
    const isOldOnly = el.dataset.oldL !== undefined && el.dataset.l === undefined;
    const side = isOldOnly ? 'LEFT' : 'RIGHT';
    const line = isOldOnly ? +el.dataset.oldL : +el.dataset.l;
    rows.push({ el, side, line });
    const key = side + ':' + line;
    const drafts = draftsByKey.get(key);
    const existing = existingByKey.get(key);
    el.classList.toggle('pr-has-comment', !!drafts || !!existing);
    if (!drafts && !existing) continue;
    const badge = document.createElement('span');
    badge.className = 'pr-comment-mark' + (drafts?.some(c => c.origin === 'ai') ? ' ai' : '');
    const count = (existing?.length || 0) + (drafts?.length || 0);
    badge.title = 'View ' + count + ' comment' + (count === 1 ? '' : 's');
    badge.innerHTML = COMMENT_ICON;
    badge.addEventListener('click', e => {
      e.stopPropagation();
      if (drafts) { revealThreadInPanel(threadKey(d.path, side, line)); return; }
      // A posted thread: its replies are in the Conversation tab.
      openFile('pr://conversation').then(() => emit('conversation:reveal', { path: d.path, side, line }));
    });
    el.querySelector('.diff-code')?.before(badge);
  }
  extraMarkers?.(d.path, rows);
}

/* ---------- bottom panel: your drafts ----------
   What the next review will send: your own drafts and the AI suggestions you
   accepted, grouped by line. Comments already posted on the PR, with their
   replies, live in the Conversation tab (conversation.js). */

function threadKey(path, side, line) { return path + '|' + (side || 'RIGHT') + ':' + line; }

function wireCommentsPanel() {
  const panel = $('#pr-comments-panel');
  if (panel) {
    panel.hidden = false;
    panel.classList.add('collapsed');
  }

  const toggle = () => {
    panel?.classList.toggle('collapsed');
    layout(); render();
  };

  $('#pr-comments-collapse')?.addEventListener('click', e => {
    e.stopPropagation();
    toggle();
  });

  $('.pr-comments-panel-head')?.addEventListener('click', e => {
    if (e.target.closest('#pr-comments-collapse')) return;
    toggle();
  });

  const rz = $('#pr-comments-resizer');
  if (rz && panel) {
    let dragging = false;
    rz.addEventListener('mousedown', e => {
      dragging = true;
      rz.classList.add('drag');
      panel.classList.remove('collapsed');
      e.preventDefault();
    });
    addEventListener('mousemove', e => {
      if (!dragging) return;
      const bottom = panel.getBoundingClientRect().bottom;
      const h = Math.max(80, Math.min(window.innerHeight * 0.8, bottom - e.clientY));
      panel.style.height = h + 'px';
      layout(); render();
    });
    addEventListener('mouseup', () => {
      if (!dragging) return;
      dragging = false;
      rz.classList.remove('drag');
      layout(); render();
    });
  }

  const onListClick = e => {
    const loc = e.target.closest('.pr-comment-loc');
    if (loc) {
      revealPRLine(loc.dataset.path, loc.dataset.side || 'RIGHT', +loc.dataset.line);
      return;
    }
    const conv = e.target.closest('[data-open-conversation]');
    if (conv) {
      openFile('pr://conversation');
      return;
    }
    const delBtn = e.target.closest('.pr-comment-delete-btn');
    if (delBtn) deleteDraft(+delBtn.dataset.draftId);
  };
  $('#pr-comments-list')?.addEventListener('click', onListClick);
  srQ('#sr-draft-list')?.addEventListener('click', onListClick);
}

// The session's drafts and the comments already posted inline, for the Files
// changed tab (prfiles.js), which draws both on their lines.
export function prDrafts() { return comments; }
export function prPostedReviewComments() { return reviewComments; }
export function prSessionMeta() { return meta; }

export async function deleteDraft(id) {
  try {
    await apiPostJson('/api/pr/comments/delete?id=' + id, {});
    comments = comments.filter(c => c.id !== id);
    renderBar();
    renderCommentsPanel();
    renderMarkersForActiveDoc();
    emit('pr:drafts-changed');
  } catch (e) {
    showToast('!', e.message || 'Could not delete draft');
  }
}

// Expands the panel if collapsed, scrolls to the drafts on that line, and
// briefly flashes them -- where a draft's gutter badge lands.
function revealThreadInPanel(key) {
  const panel = $('#pr-comments-panel');
  panel?.classList.remove('collapsed');
  layout(); render();
  const el = $('#pr-comments-list')?.querySelector('[data-thread-key="' + CSS.escape(key) + '"]');
  if (!el) return;
  el.scrollIntoView({ block: 'center', behavior: 'smooth' });
  el.classList.add('flash');
  setTimeout(() => el.classList.remove('flash'), 1200);
}

function draftCardHtml(c) {
  const who = c.origin === 'ai' ? 'AI suggestion you ' + (c.status === 'edited' ? 'edited' : 'accepted') : 'You';
  return '<div class="pr-comment-card draft' + (c.origin === 'ai' ? ' ai' : '') + '">' +
    '<div class="pr-issue-comment-head"><span class="pr-issue-comment-author">' + who + '</span></div>' +
    '<div class="pr-issue-comment-body">' + esc(c.body) + '</div>' +
    (c.ai?.suggestion ? '<pre class="pr-draft-sugg">' + esc(c.ai.suggestion) + '</pre>' : '') +
    '<div class="pr-comment-card-actions">' +
      '<button class="pr-comment-delete-btn" data-draft-id="' + c.id + '">Delete draft</button>' +
    '</div>' +
  '</div>';
}

function renderCommentsPanel() {
  const listEl = $('#pr-comments-list');
  const countEl = $('#pr-comments-count');
  if (!listEl) return;
  const groups = new Map();
  for (const c of comments) {
    if (!c.path) continue;
    const key = threadKey(c.path, c.side, c.line);
    if (!groups.has(key)) groups.set(key, { path: c.path, side: c.side || 'RIGHT', line: c.line, subject: c.subjectType, drafts: [] });
    groups.get(key).drafts.push(c);
  }
  const entries = [...groups.entries()].sort((a, b) => a[1].path.localeCompare(b[1].path) || a[1].line - b[1].line);
  let html = '';
  if (!entries.length) {
    html = '<div class="pr-comments-empty">No drafts yet. Comment on a diff line (' + esc(keyLabel('Alt+R')) + ') or accept an AI suggestion; ' +
      'everything here goes out with your next review. Comments already on the PR are in the ' +
      '<a href="#" data-open-conversation>Conversation</a> tab.</div>';
  } else {
    html = entries.map(([key, g]) => {
      const loc = g.path + (g.subject === 'file' ? ' (whole file)' : g.subject === 'summary' ? ' (review body)' : ':' + g.line + (g.side === 'LEFT' ? ' (base)' : ''));
      return '<div class="pr-comment-thread" data-thread-key="' + esc(key) + '">' +
        '<div class="pr-draft-head"><span class="pr-comment-loc" data-path="' + esc(g.path) + '" data-side="' + esc(g.side) + '" data-line="' + g.line + '">' + esc(loc) + '</span></div>' +
        g.drafts.map(draftCardHtml).join('') + '</div>';
    }).join('');
  }
  listEl.innerHTML = html;
  const srList = srQ('#sr-draft-list');
  if (srList) srList.innerHTML = entries.length ? html : '';
  if (countEl) countEl.textContent = comments.length ? String(comments.length) : '';
  emit('pr:comments-changed'); // drafts or posted comments moved: Files changed redraws its threads
}
/* ---------- launching another PR from a running session ---------- */

// Called from palette.js's "Git: Open Pull Request..." command. The PR opens
// in a child px0 started by this server (inbox.go's handleLaunchPR), shown in
// a window named after the PR, so opening it again reuses that session.
export function launchPR(target) {
  return openPullRequest(target);
}

function injectFooterButton() {
  const sel = $('#footer-sel');
  if (!sel || sel.querySelector('[data-sel="review-comment"]')) return;
  const btn = document.createElement('button');
  btn.className = 'footer-btn';
  btn.dataset.sel = 'review-comment';
  btn.title = withKeys('Add a review comment on this selection ({Alt+R})');
  btn.innerHTML = '<span class="footer-btn-label">Comment</span><kbd class="footer-kbd">' + esc(keyLabel('Alt+R')) + '</kbd>';
  sel.append(btn);
}
