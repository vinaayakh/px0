// web/src/virtualtab.js
/* Tabs that are not files: the PR Conversation (pr://conversation) and the
   git graph (git://graph). A feature registers a scheme once at init; opening
   a path with that scheme then goes through openFile() like any tab, but the
   doc it gets has no lines, gutter, LSP or diff, and its content is drawn by
   the feature into the preview surface (#mdview, see markdown.js). The module
   holds only the registry so tabs.js and markdown.js can both read it without
   importing each other's features. */

const vtabKinds = new Map(); // scheme -> { title(path), render(article, doc), pinned }

/* spec.title(path) names the tab; spec.render(article, doc) fills the
   article while the tab is shown and may be async; spec.pinned keeps the tab
   first in the tab bar. */
export function registerVirtualTab(scheme, spec) {
  vtabKinds.set(scheme, spec);
}

export function isVirtualPath(path) {
  return /^[a-z][a-z0-9+.-]*:\/\//.test(path || '');
}

export function virtualTabSpec(path) {
  const m = /^([a-z][a-z0-9+.-]*):\/\//.exec(path || '');
  return m ? vtabKinds.get(m[1]) || null : null;
}
