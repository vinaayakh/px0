# Git Graph and Branch Focus

The **Graph** tab (`git://graph`) draws the commit graph of every local and remote branch, every tag and HEAD, with a branch list beside it that sorts each branch into a health category. Clicking a branch, a ref pill or any lane focuses that branch's life: its own commits, the commit it forked from and the merges that brought it in stay bright, and everything else fades to 35%.

Source: [`graph.go`](../../graph.go) (lane layout, streaming, paging, segment focus), [`graph_branches.go`](../../graph_branches.go) (default branch, categories, ref focus), [`web/src/graph.js`](../../web/src/graph.js) (virtualized drawing, branch list, focus).

The graph only reads. It never checks out, rebases or deletes anything.

## 1. Reading History

`git log --topo-order --branches --remotes --tags HEAD` is streamed with `%H %P %an %ct %s` separated by `%x1f`, one commit per line, and laid out as it is read. Stashes are left out on purpose: their internal commits would show as stray merges. HEAD is included because a PR worktree is often detached.

Refs come from one `git for-each-ref` call (`readRefs`), which also yields the checked-out branch (`%(HEAD)`), where `origin/HEAD` points (`%(symref)`) and, peeled, the commit behind every annotated tag. Only a detached HEAD costs a second call. On Windows with a virus scanner every git spawn costs about 70 ms, so the graph avoids them wherever one call can answer.

## 2. Lane Layout

`laneLayout.place` takes commits in topological order (children first) and keeps one slot per column saying which commit that column is waiting for. For each commit:

1. Every column waiting for it converges into it. It takes the column of the default branch's segment if that is among them, else the oldest segment's. The other columns end there.
2. With no column waiting, it is a branch tip and opens a new column in the leftmost free slot.
3. The first parent continues in the commit's column. Each further parent (a merge) joins a column already waiting for it, or opens a new one.
4. Trailing empty columns are trimmed.

Each row is sent with its lane, its segment, and its edges as `[fromLane, toLane, segment, kind]`: kind 0 passes through the row, 1 runs from the top into the dot, 2 from the dot to the bottom. The client never has to reason about topology. The layout is O(commits × active lanes), deterministic, and has golden-file tests (`testdata/graph/*.golden`) for linear history, a branch merge, an octopus merge, a criss-cross merge, orphan roots, a branch merged twice, and two unmerged tips.

### Segments

A segment is a run of commits sharing one column, from where it opens (a branch tip, or a merge's second parent) to where it joins another column (its fork point). The layout records each segment's opening merge (`segStart`) and fork commit (`segFork`). Because the default branch's segment always wins where lines meet, the default branch keeps one segment through its whole first-parent history, and every branch's segment ends exactly where it forked from it. So a lane click needs no branch name, and still works for a branch that was merged and deleted. A branch merged, continued and merged again stays one segment, and every merge into it is reported (`merges`).

## 3. Paging and Memory

A `graphSession` per repository holds the running `git log`, the layout state and the rows laid out so far. `GET /api/graph?cursor=&limit=` (2,000 by default, 5,000 at most) reads only as far as the page needs, so the lane state carries from page to page and the first page of a huge repository costs only that page. The total for the scrollbar comes from `git rev-list --count` in the background.

The session is keyed by a signature of all refs and HEAD. When a request carries an older signature (`sig=`), the answer is `{reset: true}` and the client starts over, keeping its scroll position and focus. A session unused for 2 minutes is dropped, git process and rows, and the process's idle memory reclamation returns it to the OS.

Measured on a 100k-commit repository built with `git fast-import` (`TestGraphBigRepo`, opt-in with `PX0_GRAPH_BIG=1`) on Windows:

| | commit-graph file | no commit-graph |
| --- | --- | --- |
| First 2,000 rows (server) | 210 ms | 760 ms |
| Of which `git log` | 100 ms | 640 ms |
| Layout of 2,000 rows | 0.3 ms | 0.3 ms |

Without a commit-graph file, git must walk the whole history before it prints the first `--topo-order` line, and that walk dominates. `git gc` writes the file by default, so most long-lived repositories have one. For a repository over 20,000 commits without one, the tab says so and suggests `git commit-graph write --reachable`; px0 does not write it itself, since that would be a write px0 made to the repository on its own. Laying out all 100k commits takes 18 ms and about 11 MB (`BenchmarkGraphLayoutAll100k`).

## 4. Branch Categories

`graphBranches` lists `refs/heads` and `refs/remotes` in one `for-each-ref` call that also returns ahead and behind counts against the default branch (`%(ahead-behind:<default>)`, git 2.41+; older git counts per branch with `rev-list --left-right --count`), plus one `for-each-ref --merged=<default>` call. Each branch gets the first category that applies:

| Category | Rule |
| --- | --- |
| default | the default branch, local or remote |
| merged | its tip is in the default branch (`--merged`) |
| orphan | `merge-base` finds no common ancestor (checked per unmerged branch, in parallel within the `NumCPU × 4` budget) |
| gone | local branch whose upstream is `[gone]` |
| squash merged? | not an ancestor, but GitHub has a merged PR from a branch of that name (one GraphQL query with an alias per branch, up to 50; needs a token and a GitHub `origin`) |
| stale | unmerged, tip older than `graph.staleDays` (30) |
| active | anything else |

The default branch is `graph.defaultBranch` if set, else where `origin/HEAD` points, else `main`, `master`, `origin/main`, `origin/master`. The list is cached for 30 seconds per ref signature.

## 5. Focus

`GET /api/graph/focus` answers three ways:

- `?ref=<branch>` (the branch list and ref pills), computed with git against the default branch D:
  - unmerged: `rev-list T ^D` and the fork point `merge-base T D`;
  - merged: the entry merge M is the oldest commit on D's first-parent chain that contains T (`--ancestry-path T..D` intersected with `--first-parent T..D`); the branch is `rev-list M^2 ^M^1`, with fork point `merge-base M^1 M^2`;
  - merged by fast-forward (no merge commit): just the tip, since nothing records where it began;
  - the default branch itself: its first-parent line.
- `?sha=<commit>` and `?seg=<id>` (a click on a dot or a line): the segment's commits, its opening merge, every merge into it, and its fork point. The session reads further (up to 20,000 more rows) if the segment has not ended yet.

The client keeps the focused commits' own segments at full colour and everything else at 35% opacity, marks the fork point and merges on their rows, and writes the focus into the URL (`#graph=ref:feature`, `#graph=sha:<commit>`), so a reload or a shared link opens on the same focus. Esc or a click on empty space clears it.

## 6. Drawing

`graph.js` draws only the rows in view plus 12 above and below, about 60 DOM rows, each absolutely positioned at `index × 24px` inside a sizer as tall as the whole history. One SVG covers the same window with the edges (straight for pass-throughs, curves where a line changes column) and the dots (hollow for merges). Scrolling redraws on the next animation frame; nearing the end of the loaded rows loads the next page. Lane colours are `--gr-0` to `--gr-7` by segment, chosen to read on light and dark themes.

## 7. Endpoints

| Method and path | Returns |
| --- | --- |
| `GET /api/graph?cursor=&limit=&sig=` | `{rows, cursor, next, total, maxLane, sig, head, notice, hint}` or `{reset, sig}` |
| `GET /api/graph/branches[?refresh=1]` | `{default, branches: [{name, ref, kind, sha, category, ahead, behind, date, author, current, prUrl}], staleDays, squashChecked}` |
| `GET /api/graph/focus?ref=\|sha=\|seg=` | `{label, tip, shas, fork, merge, merges, seg}` |

## 8. Scope

The graph works fully in a normal workspace. In a PR session whose checkout is a worktree of the local clone it shows that clone's refs. A PR checked out as a fresh single-branch clone has little history; the tab says so.

## 9. Commit Inspector, Search, Cleanup and Live Refresh

Source: [`graph_commit.go`](../../graph_commit.go).

**Inspector.** Clicking a row (not a lane or a pill) opens the commit in a pane on the right: subject and body, author and committer, parents (each clickable), refs, and the changed files with their status and added/removed lines. The list is one `git diff-tree -r -M --raw --numstat -z` against the first parent, or against the empty tree for a root commit; a merge therefore shows what it brought in relative to its first parent. (`--name-status` cannot be combined with `--numstat` that way: git prints only one of the two, so `--raw` supplies the status.) Clicking a file expands its diff below it (`GET /api/graph/diff?sha=&path=&old=`, with the old path for a rename, capped at 2 MB).

**Search.** The search box finds commits whose subject or author contains the text (any case) or whose hash starts with it. `GET /api/graph/search?q=` reads the rest of history first (up to 300,000 commits) so a match far below the loaded pages is found, and returns up to 1,000 row indices. Matching rows are highlighted; Enter and Shift+Enter (or the arrows) step through them, loading pages up to the match. Commit bodies are not searched.

**Cleanup.** Merged, gone, stale and squash-merged branches get a **Copy delete** button: `git branch -d <name>` for a merged or gone local branch (git refuses if anything is unmerged), `git branch -D <name>` for a stale or squash-merged one (the title says it force-deletes), and `git push <remote> --delete <branch>` for a remote branch. The checked-out branch never gets one. px0 copies the command; it never runs it.

**Live refresh.** `gitstream.js` emits `git:status` whenever the git watcher's snapshot changes (a commit, checkout, fetch or pull, including in a terminal). The Graph tab then asks `GET /api/graph/sig` (one `for-each-ref`) and, when the refs moved, reloads in place: the scroll position stays, the pages the view needs are read again, and the focus and search are re-applied. A hidden Graph tab only notes that it may be stale and checks when it is shown again.

| Method and path | Returns |
| --- | --- |
| `GET /api/graph/commit?sha=` | `{sha, parents, author, email, date, committer, committerDate, message, refs, files: [{path, oldPath, status, added, deleted}], against}` |
| `GET /api/graph/diff?sha=&path=[&old=]` | `{diff, truncated}` |
| `GET /api/graph/search?q=&sig=` | `{matches, complete, capped}` or `{reset}` |
| `GET /api/graph/sig` | `{sig}` |
