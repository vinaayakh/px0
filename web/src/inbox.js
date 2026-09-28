// web/src/inbox.js
// The PR inbox: a sidebar view (the third button of the Explorer toggle)
// listing open pull requests in three sections -- review requested from me,
// mine, and this repository's -- from /api/inbox (inbox.go). Opening one
// starts a child px0 on it through /api/pr/launch and shows it in a browser
// window named after the PR, so opening the same PR again brings that window
// back instead of checking the PR out a second time.
import { $, S, esc, api, apiPostJson } from './state.js';
import { showToast } from './ui.js';
import { openSettings } from './settings.js';

const IB_SECTIONS = [
  { id: 'review', title: 'Review requested' },
  { id: 'mine', title: 'Mine' },
  { id: 'repo', title: 'This repo' },
];
const IB_POLL_MS = 5 * 60 * 1000;

let ibOpen = false;
let ibTimer = 0;
let ibLoadedAt = 0;
let ibLoading = false;
const ibData = {};                 // section -> response
const ibCollapsed = new Set();     // collapsed section ids
const ibOpening = new Set();       // PR URLs being launched
let ibFilter = '';                 // filter box text
let ibSort = '';                   // '', 'updated', 'created' or 'repo'

// The filter and sort are a per-viewer convenience: remembered in this
// browser if it allows, fine to lose.
function ibRemember(key, value) {
  try { localStorage.setItem('px0.inbox.' + key, value); } catch {}
}
function ibRecall(key) {
  try { return localStorage.getItem('px0.inbox.' + key) || ''; } catch { return ''; }
}

export function initInbox() {
  $('#btn-inbox')?.addEventListener('click', () => { if (!ibOpen) showInbox(); });
  for (const sel of ['#btn-files', '#btn-changed']) $(sel)?.addEventListener('click', hideInbox);
  $('#inbox-refresh')?.addEventListener('click', () => ibLoad(true));
  ibSort = ibRecall('sort');
  const filterEl = $('#inbox-filter'), sortEl = $('#inbox-sort');
  if (sortEl) {
    sortEl.value = ibSort;
    sortEl.addEventListener('change', () => { ibSort = sortEl.value; ibRemember('sort', ibSort); ibRender(); });
  }
  filterEl?.addEventListener('input', () => { ibFilter = filterEl.value; ibRender(); });
  filterEl?.addEventListener('keydown', e => {
    if (e.key === 'Escape' && filterEl.value) { e.stopPropagation(); filterEl.value = ''; ibFilter = ''; ibRender(); }
    if (e.key === 'ArrowDown') { e.preventDefault(); $('#inbox-body .ib-row')?.focus(); }
  });
  const body = $('#inbox-body');
  body?.addEventListener('click', e => {
    if (e.target.closest('[data-ib-token]')) { openSettings('ui', 'GitHub', 'github.token'); return; }
    const head = e.target.closest('[data-ib-section]');
    if (head) {
      const id = head.dataset.ibSection;
      if (ibCollapsed.has(id)) ibCollapsed.delete(id); else ibCollapsed.add(id);
      ibRender();
      return;
    }
    const row = e.target.closest('.ib-row');
    if (row) openPullRequest(row.dataset.url);
  });
  body?.addEventListener('keydown', e => {
    const row = e.target.closest('.ib-row');
    if (!row) return;
    if (e.key === 'Enter') { e.preventDefault(); openPullRequest(row.dataset.url); }
    else if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      const rows = [...body.querySelectorAll('.ib-row')];
      rows[rows.indexOf(row) + (e.key === 'ArrowDown' ? 1 : -1)]?.focus();
    }
  });
  document.addEventListener('visibilitychange', () => {
    if (!ibOpen) return;
    if (document.visibilityState === 'visible') {
      if (Date.now() - ibLoadedAt > IB_POLL_MS) ibLoad(false);
      ibSchedule();
    } else clearTimeout(ibTimer); // never poll while the page is hidden
  });
}

export function showInbox() {
  ibOpen = true;
  document.body.classList.remove('side-hidden');
  document.body.classList.add('inbox-mode');
  $('#inbox').hidden = false;
  $('#btn-inbox')?.classList.add('active');
  for (const sel of ['#btn-files', '#btn-changed']) $(sel)?.classList.remove('active');
  ibRender();
  ibLoad(false);
  ibSchedule();
}

function hideInbox() {
  if (!ibOpen) return;
  ibOpen = false;
  document.body.classList.remove('inbox-mode');
  $('#inbox').hidden = true;
  $('#btn-inbox')?.classList.remove('active');
  clearTimeout(ibTimer);
}

function ibSchedule() {
  clearTimeout(ibTimer);
  if (!ibOpen || document.visibilityState !== 'visible') return;
  ibTimer = setTimeout(() => { ibLoad(false); ibSchedule(); }, IB_POLL_MS);
}

async function ibLoad(force) {
  if (ibLoading) return;
  ibLoading = true;
  $('#inbox-refresh')?.classList.add('busy');
  await Promise.all(IB_SECTIONS.map(async s => {
    try {
      ibData[s.id] = await api('/api/inbox', force ? { section: s.id, refresh: 1 } : { section: s.id });
    } catch (e) {
      ibData[s.id] = { error: e.message || 'Could not load' };
    }
  }));
  ibLoading = false;
  ibLoadedAt = Date.now();
  $('#inbox-refresh')?.classList.remove('busy');
  ibRender();
}

/* ---------- rendering ---------- */

function ibAge(iso) {
  const t = Date.parse(iso);
  if (!t) return '';
  const m = Math.max(0, Math.round((Date.now() - t) / 60000));
  if (m < 60) return m + 'm';
  const h = Math.round(m / 60);
  if (h < 48) return h + 'h';
  const d = Math.round(h / 24);
  return d < 60 ? d + 'd' : Math.round(d / 30) + 'mo';
}

const IB_CI = {
  pass: ['ib-ci-pass', '✓', 'Checks passing'],
  fail: ['ib-ci-fail', '✕', 'Checks failing'],
  pending: ['ib-ci-pending', '●', 'Checks running'],
  '': ['ib-ci-none', '○', 'No checks'],
};
const IB_REVIEW = {
  approved: ['ib-rv-ok', 'Approved'],
  changes_requested: ['ib-rv-bad', 'Changes requested'],
};

function ibKey(url) {
  const m = /github\.com\/([^/]+)\/([^/]+)\/pull\/(\d+)/i.exec(url || '');
  return m ? (m[1] + '/' + m[2] + '/' + m[3]).toLowerCase() : '';
}

function ibRowHtml(pr, current) {
  const [ciCls, ciIcon, ciTitle] = IB_CI[pr.ci || ''] || IB_CI[''];
  const rv = IB_REVIEW[pr.review];
  const isCurrent = current && ibKey(current) === ibKey(pr.url);
  const opening = ibOpening.has(pr.url);
  return '<div class="ib-row' + (isCurrent ? ' current' : '') + (opening ? ' opening' : '') + '" tabindex="0" data-url="' + esc(pr.url) + '" title="' + esc(pr.title) + '">' +
    '<div class="ib-top"><span class="ib-ci ' + ciCls + '" title="' + ciTitle + '">' + ciIcon + '</span>' +
    '<span class="ib-title">' + esc(pr.title) + '</span></div>' +
    '<div class="ib-meta"><span class="ib-repo">' + esc(pr.repo) + '#' + pr.number + '</span>' +
    '<span>' + esc(pr.author) + '</span>' +
    '<span title="Updated ' + esc(pr.updatedAt) + '">' + esc(ibAge(pr.updatedAt)) + '</span>' +
    (pr.draft ? '<span class="ib-chip">Draft</span>' : '') +
    (rv ? '<span class="ib-chip ' + rv[0] + '">' + rv[1] + '</span>' : '') +
    (isCurrent ? '<span class="ib-chip">This session</span>' : '') +
    (opening ? '<span class="ib-chip">Opening…</span>' : '') +
    '</div></div>';
}

function ibRender() {
  const body = $('#inbox-body');
  if (!body || !ibOpen) return;
  const any = IB_SECTIONS.some(s => ibData[s.id]);
  if (!any) { body.innerHTML = '<div class="hint">Loading pull requests…</div>'; return; }
  if (IB_SECTIONS.some(s => ibData[s.id]?.needsToken)) {
    body.innerHTML = '<div class="ib-token">The inbox needs a GitHub token. Add one in Settings, set <code>GITHUB_TOKEN</code> or <code>GH_TOKEN</code>, or run <code>gh auth login</code>. ' +
      '<button class="opt" type="button" data-ib-token>Open Settings</button></div>';
    return;
  }
  let html = '';
  const words = ibFilter.toLowerCase().split(/\s+/).filter(Boolean);
  const matches = pr => {
    const hay = (pr.title + ' ' + pr.author + ' ' + pr.repo + '#' + pr.number).toLowerCase();
    return words.every(w => hay.includes(w));
  };
  const sorters = {
    updated: (a, b) => (b.updatedAt || '').localeCompare(a.updatedAt || ''),
    created: (a, b) => (b.createdAt || '').localeCompare(a.createdAt || ''),
    repo: (a, b) => a.repo.localeCompare(b.repo) || a.number - b.number,
  };
  for (const s of IB_SECTIONS) {
    const d = ibData[s.id];
    if (!d || d.hidden) continue;
    let items = (d.items || []).filter(matches);
    if (sorters[ibSort]) items = [...items].sort(sorters[ibSort]);
    const collapsed = ibCollapsed.has(s.id);
    const title = s.id === 'repo' && d.repo ? s.title + ' · ' + d.repo : s.title;
    html += '<div class="ib-section' + (collapsed ? ' collapsed' : '') + '">' +
      '<div class="ib-head" data-ib-section="' + s.id + '" role="button" tabindex="0"><span class="ib-chev">' + (collapsed ? '▸' : '▾') + '</span>' +
      '<span class="ib-head-title">' + esc(title) + '</span><span class="ib-count">' + (d.error ? '!' : items.length) + '</span></div>';
    if (!collapsed) {
      if (d.error) html += '<div class="ib-error">' + esc(d.error) + '</div>';
      else if (!items.length) html += '<div class="ib-empty">' + (words.length && d.items?.length ? 'No match.' : 'Nothing here.') + '</div>';
      else {
        html += items.map(pr => ibRowHtml(pr, d.current)).join('');
        if (!words.length && d.total > items.length) html += '<div class="ib-empty">Showing ' + items.length + ' of ' + d.total + '.</div>';
      }
    }
    html += '</div>';
  }
  body.innerHTML = html;
  const stamp = $('#inbox-updated');
  if (stamp) stamp.textContent = ibLoadedAt ? 'updated ' + ibAge(new Date(ibLoadedAt).toISOString()) : '';
}

/* ---------- opening a PR ---------- */

/* Opens url in a child px0. The window is opened synchronously, inside the
   click, so popup blockers allow it; it is named after the PR, so a second
   open returns the same window. If that window already shows the child (a
   page on another port, which this page cannot read), it is just focused. */
export async function openPullRequest(url) {
  const key = ibKey(url);
  if (!key) { showToast('!', 'Not a GitHub pull request URL'); return; }
  if (S.meta?.pr && ibKey(S.meta.pr.url) === key) { showToast('✓', 'This session is already reviewing that pull request'); return; }

  const w = window.open('', 'px0-pr-' + key.replace(/[^a-z0-9]+/g, '-'));
  let showing = false;
  try { showing = !!w && w.location.href !== 'about:blank'; } catch { showing = true; }
  if (w && showing) { w.focus(); return; }
  if (w) {
    try {
      w.document.title = 'Opening ' + key;
      w.document.body.style.cssText = 'font:14px system-ui,sans-serif;padding:24px;color:#888;background:#111';
      w.document.body.textContent = 'Preparing ' + key + ' in px0…';
    } catch {}
  }

  ibOpening.add(url);
  ibRender();
  try {
    const r = await apiPostJson('/api/pr/launch', { target: url });
    if (r.self) { w?.close(); showToast('✓', r.error); return; }
    let lp = r.launch;
    const deadline = Date.now() + 120000;
    while (lp.state === 'starting' && Date.now() < deadline) {
      await new Promise(res => setTimeout(res, 600));
      lp = await api('/api/pr/launch', { target: url });
    }
    if (lp.state === 'failed' || !lp.url) {
      const msg = lp.error || 'px0 did not start in time';
      if (w) try { w.document.body.textContent = 'Could not open ' + key + ':\n\n' + msg; w.document.body.style.whiteSpace = 'pre-wrap'; } catch {}
      showToast('!', 'Could not open the pull request: ' + msg.split('\n').pop(), 6000);
      return;
    }
    if (w && !w.closed) { w.location.href = lp.url; w.focus(); }
    else if (!window.open(lp.url, 'px0-pr-' + key.replace(/[^a-z0-9]+/g, '-'))) {
      showToast('!', 'The browser blocked the new window. ' + key + ' is at ' + lp.url, 10000);
    }
  } catch (e) {
    w?.close();
    showToast('!', e.message || 'Could not open the pull request');
  } finally {
    ibOpening.delete(url);
    ibRender();
  }
}
