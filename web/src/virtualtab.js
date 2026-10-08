// web/src/virtualtab.js
/* Tabs that are not files: the PR's Conversation, Files changed, AI Review
   and Submit review (pr://conversation, pr://files, pr://review,
   pr://submit) and the git graph
   (git://graph). A feature registers a scheme, or one exact path, once at
   init; opening such a path then goes through openFile() like any tab, but
   the doc it gets has no lines, gutter, LSP or diff, and its content is drawn
   by the feature into the preview surface (#mdview, see markdown.js). The
   module holds only the registry so tabs.js and markdown.js can both read it
   without importing each other's features. */

const vtabKinds = new Map(); // scheme, or exact path -> { title(path), render(article, doc), pinned, count }

/* key is a scheme ('git') or an exact path ('pr://files'); an exact path wins
   over its scheme. spec.title(path) names the tab; spec.render(article, doc)
   fills the article while the tab is shown and may be async; spec.pinned
   keeps the tab ahead of file tabs, in the order the pinned tabs opened, and
   makes it impossible to close (tabs.js's isFixedTab); spec.count(), when
   set, is a number drawn as a badge on the tab (0 draws none). */
export function registerVirtualTab(key, spec) {
  vtabKinds.set(key, spec);
}

export function isVirtualPath(path) {
  return /^[a-z][a-z0-9+.-]*:\/\//.test(path || '');
}

export function virtualTabSpec(path) {
  const exact = vtabKinds.get(path || '');
  if (exact) return exact;
  const m = /^([a-z][a-z0-9+.-]*):\/\//.exec(path || '');
  return m ? vtabKinds.get(m[1]) || null : null;
}
