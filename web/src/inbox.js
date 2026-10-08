// web/src/inbox.js
// The PR inbox: a sidebar view (the third button of the Explorer toggle)
// listing the open pull requests of one repository -- the workspace's, or one
// picked from /api/inbox/repos -- from /api/inbox (inbox.go). Chips narrow the
// list to those waiting on the user's review or the user's own. Opening one
// starts a child px0 on it through /api/pr/launch and shows it in a browser
// window named after the PR, so opening the same PR again brings that window
// back instead of checking the PR out a second time.
import { $, S, esc, api, apiPostJson } from './state.js';
import { showToast } from './ui.js';
import { openSettings } from './settings.js';
import { initRepos, openRepos, savedRepos } from './repos.js';

const IB_SECTIONS = [
  { id: 'all', title: 'All' },
  { id: 'review', title: 'Review requested' },
  { id: 'mine', title: 'Mine' },
];
const IB_POLL_MS = 5 * 60 * 1000;
const IB_OTHER = '\u0000other'; // the picker's "Other repository…" entry
const IB_MANAGE = '\u0000manage'; // the picker's "Local repositories…" entry

let ibOpen = false;
let ibTimer = 0;
let ibLoadedAt = 0;
let ibLoading = false;
let ibData = {};                   // section -> response, for ibRepo
const ibOpening = new Set();       // PR URLs being launched
let ibFilter = '';                 // filter box text
let ibSort = '';                   // '', 'updated' or 'created'
let ibSection = 'all';             // chip shown
let ibRepo = '';                   // owner/name shown; '' is the workspace's
let ibRepos = null;                // picker choices, once loaded
let ibDefaultRepo = '';            // the workspace's repository
let ibLocal = [];                  // owner/name of each saved local clone (repos.js)

// The filter and sort are a per-viewer convenience: remembered in this
// browser if it allows, fine to lose.
function ibRemember(key, value) {
  try { localStorage.setItem('px0.inbox.' + key, value); } catch {}
}
function ibRecall(key) {
  try { return localStorage.getItem('px0.inbox.' + key) || ''; } catch { return ''; }
}

export function initInbox() {
  initRepos({ onChange: () => ibLoadRepos(false) });
  $('#btn-inbox')?.addEventListener('click', () => { if (!ibOpen) showInbox(); });
  for (const sel of ['#btn-files', '#btn-changed']) $(sel)?.addEventListener('click', hideInbox);
  $('#inbox-refresh')?.addEventListener('click', () => { ibLoadRepos(true); ibLoad(true); });
  ibSort = ibRecall('sort');
  if (ibSort === 'repo') ibSort = ''; // every row is the same repository now
  ibRepo = ibRecall('repo');
  if (IB_SECTIONS.some(s => s.id === ibRecall('section'))) ibSection = ibRecall('section');
  const filterEl = $('#inbox-filter'), sortEl = $('#inbox-sort');
  if (sortEl) {
    sortEl.value = ibSort;
    sortEl.addEventListener('change', () => { ibSort = sortEl.value; ibRemember('sort', ibSort); ibRender(); });
  }
  $('#inbox-repo')?.addEventListener('change', e => ibPickRepo(e.target.value));
  $('#inbox-chips')?.addEventListener('click', e => {
    const chip = e.target.closest('[data-ib-chip]');
    if (!chip) return;
    ibSection = chip.dataset.ibChip;
    ibRemember('section', ibSection);
    ibRender();
  });
  filterEl?.addEventListener('input', () => { ibFilter = filterEl.value; ibRender(); });
  filterEl?.addEventListener('keydown', e => {
    if (e.key === 'Escape' && filterEl.value) { e.stopPropagation(); filterEl.value = ''; ibFilter = ''; ibRender(); }
    if (e.key === 'ArrowDown') { e.preventDefault(); $('#inbox-body .ib-row')?.focus(); }
  });
  const body = $('#inbox-body');
  body?.addEventListener('click', e => {
    if (e.target.closest('[data-ib-token]')) { openSettings('ui', 'GitHub', 'github.token'); return; }
    if (e.target.closest('[data-ib-pick]')) { ibPickRepo(IB_OTHER); return; }
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
  if (!ibRepos) ibLoadRepos(false);
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

// Loads every chip's list for the shown repository at once, so switching
// chips is instant and each chip can show its count. A repository picked
// while a load is running loads again once it lands.
async function ibLoad(force) {
  if (ibLoading) return;
  ibLoading = true;
  const repo = ibRepo;
  $('#inbox-refresh')?.classList.add('busy');
  const data = {};
  await Promise.all(IB_SECTIONS.map(async s => {
    const q = { section: s.id };
    if (repo) q.repo = repo;
    if (force) q.refresh = 1;
    try {
      data[s.id] = await api('/api/inbox', q);
    } catch (e) {
      data[s.id] = { error: e.message || 'Could not load' };
    }
  }));
  ibLoading = false;
  $('#inbox-refresh')?.classList.remove('busy');
  if (repo !== ibRepo) { ibLoad(force); return; }
  ibData = data;
  ibLoadedAt = Date.now();
  const d = data.all;
  if (d?.defaultRepo !== undefined) ibDefaultRepo = d.defaultRepo;
  ibRenderRepos();
  ibRender();
}

async function ibLoadRepos(force) {
  const local = savedRepos();
  try {
    const r = await api('/api/inbox/repos', force ? { refresh: 1 } : {});
    ibRepos = r.repos || [];
    ibDefaultRepo = r.defaultRepo || '';
  } catch {
    ibRepos = ibRepos || []; // the picker still offers the shown one and Other
  }
  const seen = new Set();
  ibLocal = (await local).filter(r => !r.missing).map(r => r.repo)
    .filter(r => !seen.has(r.toLowerCase()) && seen.add(r.toLowerCase()))
    .sort((a, b) => a.localeCompare(b, undefined, { sensitivity: 'base' }));
  ibRenderRepos();
}

// The repository shown: the one picked, else the workspace's.
function ibShownRepo() {
  return ibRepo || ibDefaultRepo;
}

function ibRenderRepos() {
  const sel = $('#inbox-repo');
  if (!sel) return;
  const shown = ibShownRepo();
  const lc = r => r.toLowerCase();
  const isLocal = new Set(ibLocal.map(lc));
  // Local clones first: their PRs can be reviewed. Then the rest of GitHub.
  const local = [...ibLocal];
  const remote = (ibRepos || []).filter(r => !isLocal.has(lc(r)));
  if (shown && !local.some(r => lc(r) === lc(shown)) && !remote.some(r => lc(r) === lc(shown))) remote.unshift(shown);
  const opt = r => '<option value="' + esc(r) + '"' + (lc(r) === lc(shown) ? ' selected' : '') + '>' +
    esc(r) + (r === ibDefaultRepo ? ' (this workspace)' : '') + '</option>';
  let html = shown ? '' : '<option value="" selected>Pick a repository…</option>';
  if (local.length) html += '<optgroup label="Local repositories">' + local.map(opt).join('') + '</optgroup>';
  if (remote.length) html += '<optgroup label="' + (local.length ? 'Other repositories on GitHub' : 'GitHub') + '">' + remote.map(opt).join('') + '</optgroup>';
  html += '<option value="' + IB_OTHER + '">Other repository…</option>';
  html += '<option value="' + IB_MANAGE + '">Manage local repositories…</option>';
  sel.innerHTML = html;
  sel.title = shown ? 'Repository: ' + shown : 'Pick a repository';
}

function ibPickRepo(value) {
  if (value === IB_MANAGE) {
    ibRenderRepos(); // put the picker back on the shown repository
    openRepos();
    return;
  }
  if (value === IB_OTHER) {
    const typed = (window.prompt('Show pull requests of which GitHub repository? (owner/name or URL)', ibShownRepo()) || '').trim();
    const m = /^(?:https?:\/\/github\.com\/)?([\w.-]+\/[\w.-]+?)(?:\.git)?\/?$/i.exec(typed);
    if (!m) {
      if (typed) showToast('!', 'Expected owner/name, like octocat/hello-world');
      ibRenderRepos(); // put the picker back on the shown repository
      return;
    }
    value = m[1];
  }
  if (!value) { ibRenderRepos(); return; }
  ibRepo = value === ibDefaultRepo ? '' : value;
  ibRemember('repo', ibRepo);
  ibData = {};
  ibRenderRepos();
  ibRender();
  ibLoad(false);
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
    '<div class="ib-meta"><span class="ib-repo">#' + pr.number + '</span>' +
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
  const words = ibFilter.toLowerCase().split(/\s+/).filter(Boolean);
  const matches = pr => {
    const hay = (pr.title + ' ' + pr.author + ' #' + pr.number).toLowerCase();
    return words.every(w => hay.includes(w));
  };
  const chips = $('#inbox-chips');
  if (chips) {
    chips.innerHTML = IB_SECTIONS.map(s => {
      const d = ibData[s.id];
      const n = d?.error ? '!' : d?.items && !d.needsToken && !d.needsRepo ? d.items.filter(matches).length : '';
      return '<button class="opt' + (s.id === ibSection ? ' on' : '') + '" type="button" data-ib-chip="' + s.id + '">' +
        esc(s.title) + (n !== '' ? ' <span class="ib-count">' + n + '</span>' : '') + '</button>';
    }).join('');
  }
  const d = ibData[ibSection];
  if (!d) { body.innerHTML = '<div class="hint">Loading pull requests…</div>'; return; }
  if (d.needsRepo) {
    body.innerHTML = '<div class="ib-token">This workspace has no GitHub remote. Pick a repository above to see its pull requests. ' +
      '<button class="opt" type="button" data-ib-pick>Choose repository…</button></div>';
    return;
  }
  if (d.needsToken) {
    body.innerHTML = '<div class="ib-token">The inbox needs a GitHub token. Add one in Settings, set <code>GITHUB_TOKEN</code> or <code>GH_TOKEN</code>, or run <code>gh auth login</code>. ' +
      '<button class="opt" type="button" data-ib-token>Open Settings</button></div>';
    return;
  }
  const sorters = {
    updated: (a, b) => (b.updatedAt || '').localeCompare(a.updatedAt || ''),
    created: (a, b) => (b.createdAt || '').localeCompare(a.createdAt || ''),
  };
  let items = (d.items || []).filter(matches);
  if (sorters[ibSort]) items = [...items].sort(sorters[ibSort]);
  let html;
  if (d.error) html = '<div class="ib-error">' + esc(d.error) + '</div>';
  else if (!items.length) html = '<div class="ib-empty">' + (words.length && d.items?.length ? 'No match.' : 'No open pull requests here.') + '</div>';
  else {
    html = items.map(pr => ibRowHtml(pr, d.current)).join('');
    if (!words.length && d.total > items.length) html += '<div class="ib-empty">Showing ' + items.length + ' of ' + d.total + '.</div>';
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
    let r;
    for (;;) {
      try {
        r = await apiPostJson('/api/pr/launch', { target: url });
        break;
      } catch (e) {
        // No local clone of the PR's repository yet: ask for one, then retry.
        const needRepo = e.body?.needsRepo;
        if (!needRepo) throw e;
        if (w) try { w.document.body.textContent = 'Waiting for a local clone of ' + needRepo + ' (add it in px0)…'; } catch {}
        const added = await openRepos(needRepo);
        if (!added) { w?.close(); return; }
        if (w) try { w.document.body.textContent = 'Preparing ' + key + ' in px0…'; } catch {}
      }
    }
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
