// web/src/conversation.js
// The PR Conversation tab (pr://conversation), active only in PR sessions: a
// virtual tab pinned first and drawn in the preview surface (#mdview), like
// GitHub's Conversation tab. It shows the header and merge readiness, the
// description, and the timeline -- comments, reviews with their inline
// threads, commits and events -- from /api/pr/conversation (conversation.go).
// Bodies arrive as goldmark HTML and go through the Markdown sanitizer before
// they reach the page. Replies and new comments use the existing /api/pr
// endpoints, which post right away.
import { S, esc, api, apiPostJson, keyLabel } from './state.js';
import { on } from './bus.js';
import { showToast } from './ui.js';
import { registerVirtualTab } from './virtualtab.js';
import { sanitizeForgeHTML, virtualArticle } from './markdown.js';
import { openFile } from './tabs.js';
import { revealPRLine, nudgeGitHubToken } from './pr.js';

const CV_PATH = 'pr://conversation';
const CV_STALE_MS = 30000;

let cvData = null;     // {conversation, needsToken, fetchedAt}
let cvError = '';
let cvLoadedAt = 0;
let cvPending = null;  // in-flight load
const cvOpened = new Set(); // collapsed-by-default threads the reviewer opened
const cvClosed = new Set(); // open-by-default threads the reviewer closed
let cvPendingReveal = null; // {path, side, line} to scroll to on the next draw

export function initConversation() {
  if (!S.meta?.pr) return;
  registerVirtualTab('pr', { title: () => 'Conversation', pinned: true, render: cvRender });
  on('pr:submitted', () => cvRefresh(true));
  on('pr:refreshed', () => cvRefresh(true));
  // A gutter badge on a posted thread (pr.js) lands here.
  on('conversation:reveal', target => { cvPendingReveal = target; const a = cvArticle(); if (a && cvData) cvDraw(a); });
}

/* ---------- data ---------- */

function cvLoad(force) {
  if (cvPending && !force) return cvPending;
  cvPending = (async () => {
    try {
      cvData = await api('/api/pr/conversation', force ? { refresh: 1 } : undefined);
      cvError = '';
    } catch (e) {
      cvError = e.message || 'Could not load the conversation';
    } finally {
      cvLoadedAt = Date.now();
      cvPending = null;
    }
  })();
  return cvPending;
}

async function cvRender(article) {
  if (!cvData || Date.now() - cvLoadedAt > CV_STALE_MS) {
    if (!cvData) article.innerHTML = '<div class="cv-loading hint">Loading the conversation…</div>';
    else cvDraw(article);
    await cvLoad(false);
    const a = cvArticle();
    if (a) cvDraw(a);
    return;
  }
  cvDraw(article);
}

function cvArticle() {
  return virtualArticle(S.tabs.find(t => t.path === CV_PATH));
}

async function cvRefresh(force) {
  await cvLoad(force);
  const a = cvArticle();
  if (a) cvDraw(a);
}

/* ---------- helpers ---------- */

function cvRel(iso) {
  const t = Date.parse(iso);
  if (!t) return '';
  const s = Math.round((Date.now() - t) / 1000);
  if (s < 60) return 'just now';
  const units = [[60, 'minute'], [3600, 'hour'], [86400, 'day'], [2592000, 'month'], [31536000, 'year']];
  let n = s, name = 'second';
  for (const [secs, label] of units) {
    if (s >= secs) { n = Math.floor(s / secs); name = label; }
  }
  return n + ' ' + name + (n === 1 ? '' : 's') + ' ago';
}

function cvTime(iso) {
  if (!iso) return '';
  let abs = iso;
  try { abs = new Date(iso).toLocaleString(); } catch {}
  return '<time class="cv-time" datetime="' + esc(iso) + '" title="' + esc(abs) + '">' + esc(cvRel(iso)) + '</time>';
}

function cvAvatar(url) {
  return /^https:\/\//.test(url || '')
    ? '<img class="cv-avatar" src="' + esc(url + (url.includes('?') ? '&' : '?') + 's=40') + '" alt="" loading="lazy">'
    : '<span class="cv-avatar cv-avatar-none"></span>';
}

function cvLabel(l) {
  const hex = /^[0-9a-f]{6}$/i.test(l.color || '') ? l.color : '';
  if (!hex) return '<span class="cv-label">' + esc(l.name) + '</span>';
  const r = parseInt(hex.slice(0, 2), 16), g = parseInt(hex.slice(2, 4), 16), b = parseInt(hex.slice(4, 6), 16);
  const dark = (0.299 * r + 0.587 * g + 0.114 * b) < 150;
  return '<span class="cv-label" style="background:#' + hex + ';color:' + (dark ? '#fff' : '#111') + ';border-color:#' + hex + '">' + esc(l.name) + '</span>';
}

const cvShort = sha => esc((sha || '').slice(0, 7));

/* Bodies are drawn after the markup, into placeholders, so sanitised HTML is
   adopted as nodes and never goes back through innerHTML. */
let cvBodies = new Map();
function cvBody(html, text) {
  const key = 'b' + cvBodies.size;
  cvBodies.set(key, { html, text });
  return '<div class="cv-body" data-cv-body="' + key + '"></div>';
}

/* ---------- drawing ---------- */

function cvDraw(article) {
  cvBodies = new Map();
  if (!cvData) {
    article.innerHTML = '<div class="cv-error">' + esc(cvError || 'No conversation loaded.') +
      ' <button class="opt" type="button" data-cv-refresh>Retry</button></div>';
    cvWire(article);
    return;
  }
  const c = cvData.conversation;
  const threadsByReview = new Map();
  for (const t of c.threads || []) {
    const k = t.reviewId || '';
    if (!threadsByReview.has(k)) threadsByReview.set(k, []);
    threadsByReview.get(k).push(t);
  }
  const shownReviews = new Set((c.items || []).filter(i => i.kind === 'review').map(i => i.id));

  // A thread asked for from the gutter starts open, even if resolved or outdated.
  let revealId = '';
  if (cvPendingReveal) {
    const r = cvPendingReveal;
    const t = (c.threads || []).find(t => t.path === r.path && (t.side || 'RIGHT') === (r.side || 'RIGHT') && (t.line === r.line || t.originalLine === r.line));
    if (t) { revealId = t.id; cvOpened.add(t.id); cvClosed.delete(t.id); }
  }

  let html = cvHeaderHtml(c.header) + cvChecksHtml(c.checks || []);
  if (cvError) html += '<div class="cv-error">Refresh failed: ' + esc(cvError) + '. Showing what was loaded ' + esc(cvRel(new Date(cvData.fetchedAt || Date.now()).toISOString())) + '.</div>';
  if (cvData.needsToken) {
    html += '<div class="cv-notice">Connect a GitHub token to see the timeline, reviews and threads. ' +
      '<button class="opt" type="button" data-cv-token>Connect</button></div>';
  }
  html += '<section class="cv-item cv-desc">' + cvCommentHead(c.header.author, c.header.avatarUrl, 'opened this pull request', c.header.createdAt) +
    (c.header.body ? cvBody(c.header.bodyHtml, c.header.body) : '<div class="cv-body cv-empty">No description provided.</div>') + '</section>';

  html += '<ol class="cv-timeline">';
  for (const it of c.items || []) html += cvItemHtml(it, threadsByReview.get(it.id) || []);
  html += '</ol>';

  const orphans = (c.threads || []).filter(t => !shownReviews.has(t.reviewId));
  if (orphans.length) {
    html += '<h3 class="cv-section">Other review threads</h3>' + orphans.map(cvThreadHtml).join('');
  }
  if (c.truncated) html += '<div class="cv-notice">This conversation is very long; the newest part is not shown. Open it on GitHub for the rest.</div>';

  html += '<section class="cv-compose"><textarea class="cv-compose-input agent-input" rows="3" spellcheck="false" placeholder="Leave a comment on the pull request (posts immediately)"></textarea>' +
    '<div class="cv-compose-foot"><span class="agent-hint">' + esc(keyLabel('Mod+Enter')) + ' to post</span><span class="grow"></span>' +
    '<button class="footer-btn" type="button" data-cv-post>Comment</button></div></section>';

  article.innerHTML = html;
  for (const el of article.querySelectorAll('[data-cv-body]')) {
    const b = cvBodies.get(el.dataset.cvBody);
    if (!b) continue;
    if (b.html) el.replaceChildren(sanitizeForgeHTML(b.html));
    else el.textContent = b.text || '';
  }
  cvWire(article);
  if (cvPendingReveal) {
    cvPendingReveal = null;
    const el = revealId && article.querySelector('[data-cv-thread="' + CSS.escape(revealId) + '"]');
    if (el) {
      el.scrollIntoView({ block: 'center' });
      el.classList.add('cv-flash');
      setTimeout(() => el.classList.remove('cv-flash'), 1400);
    }
  }
}

/* Check runs on the head commit: a summary line that opens into one row per
   check, with its result, duration and a link to its logs on GitHub. Opens by
   itself when something failed. */
function cvChecksHtml(checks) {
  if (!checks.length) return '';
  const failed = checks.filter(c => ['failure', 'timed_out', 'cancelled', 'action_required'].includes(c.conclusion)).length;
  const running = checks.filter(c => c.status !== 'completed').length;
  const passed = checks.filter(c => c.conclusion === 'success').length;
  const other = checks.length - failed - running - passed;
  const parts = [];
  if (failed) parts.push('<span class="cv-bad-t">' + failed + ' failed</span>');
  if (running) parts.push('<span class="cv-wait-t">' + running + ' running</span>');
  if (passed) parts.push('<span class="cv-ok-t">' + passed + ' passed</span>');
  if (other) parts.push(other + ' skipped or neutral');
  const dur = c => {
    const a = Date.parse(c.startedAt), b = Date.parse(c.completedAt);
    if (!a || !b || b < a) return '';
    const s = Math.round((b - a) / 1000);
    return s < 60 ? s + 's' : Math.floor(s / 60) + 'm ' + (s % 60) + 's';
  };
  const icon = c => c.status !== 'completed' ? ['cv-wait-t', '●']
    : c.conclusion === 'success' ? ['cv-ok-t', '✓']
      : ['failure', 'timed_out', 'cancelled', 'action_required'].includes(c.conclusion) ? ['cv-bad-t', '✕'] : ['cv-none', '–'];
  return '<details class="cv-checks"' + (failed ? ' open' : '') + '><summary>Checks: ' + parts.join(', ') + '</summary><ul class="cv-check-list">' +
    checks.map(c => {
      const [cls, ch] = icon(c);
      return '<li><span class="' + cls + '">' + ch + '</span><span class="cv-check-name">' + esc(c.name) + '</span>' +
        '<span class="cv-check-state">' + esc(c.status === 'completed' ? (c.conclusion || 'done').replace('_', ' ') : c.status.replace('_', ' ')) + '</span>' +
        '<span class="cv-check-dur">' + esc(dur(c)) + '</span>' +
        (/^https:\/\//.test(c.url || '') ? '<a href="' + esc(c.url) + '" target="_blank" rel="noopener noreferrer">Details</a>' : '') + '</li>';
    }).join('') + '</ul></details>';
}

function cvHeaderHtml(h) {
  const stateLabel = { open: 'Open', draft: 'Draft', merged: 'Merged', closed: 'Closed' }[h.state] || h.state;
  let html = '<header class="cv-head">' +
    '<h1 class="cv-title">' + esc(h.title) + ' <span class="cv-num">#' + h.number + '</span></h1>' +
    '<div class="cv-meta"><span class="cv-state cv-state-' + esc(h.state) + '">' + esc(stateLabel) + '</span>' +
    '<span><b>' + esc(h.author) + '</b> wants to merge into <code>' + esc(h.baseRef) + '</code> from <code>' + esc(h.headRef) + '</code></span></div>';
  const people = [];
  if (h.labels?.length) people.push('<span class="cv-k">Labels</span>' + h.labels.map(cvLabel).join(''));
  if (h.assignees?.length) people.push('<span class="cv-k">Assignees</span>' + h.assignees.map(a => '<span class="cv-person">' + esc(a) + '</span>').join(''));
  if (h.requestedReviewers?.length) people.push('<span class="cv-k">Awaiting review</span>' + h.requestedReviewers.map(a => '<span class="cv-person">' + esc(a) + '</span>').join(''));
  if (people.length) html += '<div class="cv-chips">' + people.map(p => '<span class="cv-chip-row">' + p + '</span>').join('') + '</div>';

  if (h.state === 'open' || h.state === 'draft') {
    const rd = {
      approved: ['ok', 'Approved'], changes_requested: ['bad', 'Changes requested'], review_required: ['wait', 'Review required'],
    }[h.reviewDecision] || ['none', 'No review decision'];
    const ck = { success: ['ok', 'Checks passing'], failure: ['bad', 'Checks failing'], pending: ['wait', 'Checks running'] }[h.checksState] || ['none', 'No checks'];
    const mg = { mergeable: ['ok', 'No conflicts'], conflicting: ['bad', 'Has conflicts'] }[h.mergeable] || ['wait', 'Conflicts unknown'];
    html += '<div class="cv-ready">' + [rd, ck, mg].map(([cls, text]) => '<span class="cv-ready-item cv-' + cls + '">' + esc(text) + '</span>').join('') + '</div>';
  }
  html += '<div class="cv-actions">' +
    (/^https:\/\//.test(h.url || '') ? '<a href="' + esc(h.url) + '" target="_blank" rel="noopener noreferrer">Open on GitHub</a>' : '') +
    '<span class="grow"></span>' +
    (cvData?.fetchedAt ? '<span class="cv-fetched">Updated ' + esc(cvRel(cvData.fetchedAt)) + '</span>' : '') +
    '<button class="opt" type="button" data-cv-refresh title="Fetch the latest from GitHub">Refresh</button></div></header>';
  return html;
}

function cvCommentHead(author, avatar, verb, when, url) {
  return '<div class="cv-comment-head">' + cvAvatar(avatar) + '<b>' + esc(author || 'ghost') + '</b> <span class="cv-verb">' + esc(verb) + '</span> ' + cvTime(when) +
    (/^https:\/\//.test(url || '') ? ' <a class="cv-link" href="' + esc(url) + '" target="_blank" rel="noopener noreferrer" title="Open on GitHub">↗</a>' : '') + '</div>';
}

function cvEvent(icon, html, when) {
  return '<li class="cv-event"><span class="cv-event-icon">' + icon + '</span><span class="cv-event-text">' + html + ' ' + cvTime(when) + '</span></li>';
}

function cvItemHtml(it, threads) {
  const who = '<b>' + esc(it.author || 'ghost') + '</b>';
  switch (it.kind) {
    case 'comment':
      return '<li class="cv-item cv-comment" data-cv-item="' + esc(it.id) + '">' + cvCommentHead(it.author, it.avatarUrl, 'commented', it.createdAt, it.url) +
        cvBody(it.bodyHtml, it.body) +
        '<div class="cv-item-actions"><button class="opt" type="button" data-cv-quote="' + esc(it.id) + '">Quote reply</button></div></li>';
    case 'review': {
      const verb = { approved: 'approved these changes', changes_requested: 'requested changes', commented: 'reviewed', dismissed: 'left a review that was dismissed' }[it.state] || 'reviewed';
      return '<li class="cv-item cv-review cv-review-' + esc(it.state) + '">' + cvCommentHead(it.author, it.avatarUrl, verb, it.createdAt, it.url) +
        (it.body ? cvBody(it.bodyHtml, it.body) : '') + threads.map(cvThreadHtml).join('') + '</li>';
    }
    case 'commits':
      return '<li class="cv-event cv-commits"><span class="cv-event-icon">⎇</span><div class="cv-event-text">' + who + ' added ' + it.commits.length +
        ' commit' + (it.commits.length === 1 ? '' : 's') + ' ' + cvTime(it.createdAt) + '<ul class="cv-commit-list">' +
        it.commits.map(cm => '<li><code class="cv-sha">' + cvShort(cm.sha) + '</code> ' + esc(cm.subject) + '</li>').join('') + '</ul></div></li>';
    case 'force_push':
      return cvEvent('⤴', who + ' force-pushed the branch from <code class="cv-sha">' + cvShort(it.beforeSha) + '</code> to <code class="cv-sha">' + cvShort(it.sha) + '</code>', it.createdAt);
    case 'review_requested':
      return cvEvent('👁', who + ' requested a review from <b>' + esc(it.subject) + '</b>', it.createdAt);
    case 'labeled':
    case 'unlabeled':
      return cvEvent('🏷', who + (it.kind === 'labeled' ? ' added ' : ' removed ') + cvLabel({ name: it.subject, color: it.color }), it.createdAt);
    case 'merged':
      return cvEvent('⛙', who + ' merged commit <code class="cv-sha">' + cvShort(it.sha) + '</code>', it.createdAt);
    case 'closed':
      return cvEvent('✕', who + ' closed this', it.createdAt);
    case 'reopened':
      return cvEvent('↺', who + ' reopened this', it.createdAt);
    case 'ready_for_review':
      return cvEvent('✓', who + ' marked this ready for review', it.createdAt);
    case 'convert_to_draft':
      return cvEvent('✎', who + ' marked this as a draft', it.createdAt);
  }
  return '';
}

/* The last few lines of the thread's diff hunk, as GitHub shows above it. */
function cvHunkHtml(hunk) {
  if (!hunk) return '';
  const lines = hunk.split('\n').filter(l => !l.startsWith('@@')).slice(-6);
  return '<pre class="cv-hunk">' + lines.map(l => {
    const cls = l.startsWith('+') ? 'cv-add' : l.startsWith('-') ? 'cv-del' : '';
    return '<span class="' + cls + '">' + esc(l) + '</span>';
  }).join('\n') + '</pre>';
}

function cvThreadHtml(t) {
  const collapsedByDefault = t.resolved || t.outdated;
  const open = collapsedByDefault ? cvOpened.has(t.id) : !cvClosed.has(t.id);
  const line = t.line || t.originalLine;
  const loc = t.path + (line ? ':' + (t.startLine && t.startLine < line ? t.startLine + '-' : '') + line : '') + (t.side === 'LEFT' ? ' (base)' : '');
  return '<details class="cv-thread" data-cv-thread="' + esc(t.id) + '"' + (open ? ' open' : '') + '>' +
    '<summary><span class="cv-path" data-cv-open="' + esc(t.id) + '" title="Open in the diff">' + esc(loc) + '</span>' +
    (t.outdated ? '<span class="cv-badge cv-badge-outdated">Outdated</span>' : '') +
    (t.resolved ? '<span class="cv-badge cv-badge-resolved">Resolved</span>' : '') +
    '<span class="cv-thread-count">' + t.comments.length + ' comment' + (t.comments.length === 1 ? '' : 's') + '</span>' +
    (!t.resolved && t.canResolve ? '<button class="opt" type="button" data-cv-resolve="' + esc(t.id) + '" title="Resolve this conversation on GitHub">Resolve</button>' : '') +
    (t.resolved && t.canUnresolve ? '<button class="opt" type="button" data-cv-unresolve="' + esc(t.id) + '" title="Reopen this conversation on GitHub">Unresolve</button>' : '') +
    '</summary>' +
    cvHunkHtml(t.diffHunk) +
    t.comments.map(cm => '<div class="cv-thread-comment">' + cvCommentHead(cm.author, cm.avatarUrl, '', cm.createdAt, cm.url) + cvBody(cm.bodyHtml, cm.body) + '</div>').join('') +
    (t.comments[0]?.databaseId
      ? '<div class="cv-reply"><textarea class="cv-reply-input agent-input" rows="1" spellcheck="false" placeholder="Reply (posts immediately)"></textarea>' +
        '<button class="opt" type="button" data-cv-reply="' + esc(t.id) + '">Reply</button></div>'
      : '') +
    '</details>';
}

/* ---------- interaction ---------- */

function cvFindThread(id) {
  return (cvData?.conversation?.threads || []).find(t => t.id === id);
}

async function cvOpenThread(t) {
  if (!t) return;
  if (t.outdated || !t.line) {
    await openFile(t.path, { view: 'diff' });
    showToast('!', 'This thread is outdated: the code it was on' + (t.originalLine ? ' (line ' + t.originalLine + ')' : '') + ' has changed since. Showing the current diff.', 5000);
    return;
  }
  revealPRLine(t.path, t.side || 'RIGHT', t.line);
}

async function cvPost(url, payload, btn) {
  if (S.meta?.pr?.readOnly || cvData?.needsToken) { nudgeGitHubToken(); return false; }
  if (btn) btn.disabled = true;
  try {
    await apiPostJson(url, payload);
    await cvRefresh(true);
    return true;
  } catch (e) {
    showToast('!', e.message || 'Could not post');
    return false;
  } finally {
    if (btn) btn.disabled = false;
  }
}

function cvWire(article) {
  if (article.dataset.cvWired) return;
  article.dataset.cvWired = '1';
  article.addEventListener('click', e => {
    if (!cvArticle()) return; // the article is shared with Markdown tabs
    if (e.target.closest('[data-cv-refresh]')) { cvRefresh(true); return; }
    if (e.target.closest('[data-cv-token]')) { nudgeGitHubToken(); return; }
    const res = e.target.closest('[data-cv-resolve], [data-cv-unresolve]');
    if (res) {
      e.preventDefault(); // inside <summary>: do not toggle the thread
      const resolved = 'cvResolve' in res.dataset;
      const id = resolved ? res.dataset.cvResolve : res.dataset.cvUnresolve;
      cvPost('/api/pr/threads/resolve', { threadId: id, resolved }, res)
        .then(ok => { if (ok) showToast('✓', resolved ? 'Conversation resolved' : 'Conversation reopened'); });
      return;
    }
    const open = e.target.closest('[data-cv-open]');
    if (open) { e.preventDefault(); cvOpenThread(cvFindThread(open.dataset.cvOpen)); return; }
    const reply = e.target.closest('[data-cv-reply]');
    if (reply) {
      const t = cvFindThread(reply.dataset.cvReply);
      const ta = reply.parentElement.querySelector('textarea');
      const body = ta?.value.trim();
      if (t && body) cvPost('/api/pr/comments/review-reply', { commentId: t.comments[0].databaseId, body }, reply).then(ok => { if (ok) showToast('✓', 'Reply posted'); });
      return;
    }
    const post = e.target.closest('[data-cv-post]');
    if (post) {
      const ta = article.querySelector('.cv-compose-input');
      const body = ta?.value.trim();
      if (body) cvPost('/api/pr/comments/issue', { body }, post).then(ok => { if (ok) showToast('✓', 'Comment posted'); });
      return;
    }
    const quote = e.target.closest('[data-cv-quote]');
    if (quote) {
      const it = (cvData?.conversation?.items || []).find(i => i.id === quote.dataset.cvQuote);
      const ta = article.querySelector('.cv-compose-input');
      if (it && ta) {
        ta.value = (it.body || '').split('\n').map(l => '> ' + l).join('\n') + '\n\n' + ta.value;
        ta.focus();
        ta.setSelectionRange(ta.value.length, ta.value.length);
        ta.scrollIntoView({ block: 'center' });
      }
    }
  });
  article.addEventListener('toggle', e => {
    const d = e.target.closest?.('[data-cv-thread]');
    if (!d || !cvArticle()) return;
    const t = cvFindThread(d.dataset.cvThread);
    if (!t) return;
    const collapsedByDefault = t.resolved || t.outdated;
    if (collapsedByDefault) { if (d.open) cvOpened.add(t.id); else cvOpened.delete(t.id); }
    else if (d.open) cvClosed.delete(t.id); else cvClosed.add(t.id);
  }, true);
  article.addEventListener('keydown', e => {
    if (!cvArticle() || !(e.key === 'Enter' && (e.metaKey || e.ctrlKey))) return;
    const ta = e.target.closest('textarea');
    if (!ta) return;
    e.preventDefault();
    e.stopPropagation();
    (ta.classList.contains('cv-compose-input') ? article.querySelector('[data-cv-post]') : ta.parentElement.querySelector('[data-cv-reply]'))?.click();
  });
}
