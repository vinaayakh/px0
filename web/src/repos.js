// web/src/repos.js
// The Local repositories dialog: the clones pull requests are reviewed from,
// saved by the server (repos.go). Opened from the inbox's repository picker,
// and by the inbox when a PR's repository has no saved clone yet -- then it
// says which repository it needs and resolves once a clone of it is added.
import { $, esc, api, apiPostJson } from './state.js';
import { showToast } from './ui.js';

let rpNeed = '';          // owner/name the caller is waiting for, '' if none
let rpSettle = null;      // resolves the open() promise
let rpRepos = [];
let rpOnChange = null;    // the inbox, to refresh its picker

export function initRepos(opts = {}) {
  rpOnChange = opts.onChange || null;
  const modal = $('#repos-modal');
  if (!modal) return;
  $('#repos-close')?.addEventListener('click', () => rpClose(null));
  modal.addEventListener('mousedown', e => { if (e.target === modal) rpClose(null); });
  modal.addEventListener('keydown', e => { if (e.key === 'Escape') { e.stopPropagation(); rpClose(null); } });
  $('#repos-add')?.addEventListener('submit', e => { e.preventDefault(); rpAdd($('#repos-path').value); });
  $('#repos-browse')?.addEventListener('click', rpBrowse);
  $('#repos-list')?.addEventListener('click', e => {
    const rm = e.target.closest('[data-repo-remove]');
    if (rm) rpRemove(rm.dataset.repoRemove);
  });
}

/* Opens the dialog. With needRepo (owner/name), it asks for a clone of that
   repository and resolves with it once added; otherwise, or when closed
   first, it resolves with null. */
export function openRepos(needRepo = '') {
  if (rpSettle) rpSettle(null);
  rpNeed = needRepo;
  const needEl = $('#repos-need');
  if (needEl) {
    needEl.hidden = !rpNeed;
    needEl.innerHTML = rpNeed
      ? 'To review this pull request, add your local clone of <strong>' + esc(rpNeed) + '</strong>. ' +
        'px0 checks the PR out of it into a temporary worktree and removes that when the review ends.'
      : '';
  }
  rpSetError('');
  $('#repos-modal').hidden = false;
  rpLoad();
  setTimeout(() => $('#repos-path')?.focus(), 0);
  return new Promise(res => { rpSettle = res; });
}

function rpClose(result) {
  $('#repos-modal').hidden = true;
  const s = rpSettle;
  rpSettle = null;
  rpNeed = '';
  s?.(result);
}

function rpSetError(msg) {
  const el = $('#repos-error');
  if (!el) return;
  el.hidden = !msg;
  el.textContent = msg;
}

async function rpLoad() {
  try {
    rpRepos = (await api('/api/repos')).repos || [];
  } catch (e) {
    rpRepos = [];
    rpSetError(e.message || 'Could not load the repositories');
  }
  rpRender();
}

function rpRender() {
  const list = $('#repos-list');
  if (!list) return;
  if (!rpRepos.length) {
    list.innerHTML = '<div class="repos-empty">No repositories yet.</div>';
    return;
  }
  const sorted = [...rpRepos].sort((a, b) => a.repo.localeCompare(b.repo, undefined, { sensitivity: 'base' }));
  list.innerHTML = sorted.map(r =>
    '<div class="repos-row' + (r.missing ? ' missing' : '') + '">' +
      '<div class="repos-main">' +
        '<div class="repos-name">' + esc(r.repo) +
          (r.remote && r.remote !== 'origin' ? ' <span class="ib-chip" title="Matched through this remote">' + esc(r.remote) + '</span>' : '') +
          (r.current ? ' <span class="ib-chip">This workspace</span>' : '') +
          (r.missing ? ' <span class="ib-chip ib-rv-bad" title="The folder is gone or is no longer a clone of this repository">Not found</span>' : '') +
        '</div>' +
        '<div class="repos-path" title="' + esc(r.path) + '">' + esc(r.path) + '</div>' +
      '</div>' +
      '<button class="mini" type="button" data-repo-remove="' + esc(r.path) + '" title="Remove from px0. The clone on disk is not touched." aria-label="Remove ' + esc(r.repo) + '">&#10005;</button>' +
    '</div>').join('');
}

async function rpAdd(path) {
  path = (path || '').trim();
  if (!path) { rpSetError('Type the path to a local clone, or choose it with Browse.'); return; }
  const btn = $('#repos-add-btn');
  if (btn) btn.disabled = true;
  rpSetError('');
  try {
    const r = await apiPostJson('/api/repos', { path });
    rpRepos = r.repos || [];
    rpRender();
    $('#repos-path').value = '';
    rpOnChange?.();
    const added = r.added;
    if (rpNeed) {
      const matches = rpRepos.find(x => x.path === added.path && !x.missing && x.repo.toLowerCase() === rpNeed.toLowerCase());
      if (matches || added.repo.toLowerCase() === rpNeed.toLowerCase()) { rpClose(added); return; }
      rpSetError('Added ' + added.repo + ', but this pull request needs a clone of ' + rpNeed + '.');
      return;
    }
    showToast('✓', 'Added ' + added.repo);
  } catch (e) {
    rpSetError(e.message || 'Could not add that folder');
  } finally {
    if (btn) btn.disabled = false;
  }
}

async function rpBrowse() {
  const btn = $('#repos-browse');
  if (btn) { btn.disabled = true; btn.textContent = 'Choose in the dialog…'; }
  rpSetError('');
  try {
    const r = await apiPostJson('/api/repos/browse', { prompt: rpNeed ? 'Choose your local clone of ' + rpNeed : 'Choose a local clone' });
    if (r.path) {
      $('#repos-path').value = r.path;
      await rpAdd(r.path);
    }
  } catch (e) {
    rpSetError(e.message || 'Could not open the folder dialog; type the path instead');
    $('#repos-path')?.focus();
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = 'Browse…'; }
  }
}

async function rpRemove(path) {
  try {
    const r = await apiPostJson('/api/repos/remove', { path });
    rpRepos = r.repos || [];
    rpRender();
    rpOnChange?.();
  } catch (e) {
    rpSetError(e.message || 'Could not remove it');
  }
}

/* The saved repositories, for the inbox's picker. */
export async function savedRepos() {
  try { return (await api('/api/repos')).repos || []; } catch { return []; }
}
