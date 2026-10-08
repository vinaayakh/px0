# Pull Request Review & Agent Collaboration

px0 can check out a pull request's full source tree and review it like any local workspace: full codebase navigation, symbol outline, LSP intelligence, and search, alongside a diff scoped to the PR's merge-base, real-time coding agent synchronization, inline comment drafting, AI batch application, and the same git panel a plain workspace has — so you can commit and push fixes back to the PR's own branch, and pull in new commits someone else pushed while you were reviewing.

---

## Overview & Core Purpose

GitHub's web PR view shows you the isolated diff, but not the codebase around it: jumping to a caller three files away, or checking how a changed function is used elsewhere, means either trusting memory or manually cloning and switching branches.

With px0, you simply pass the pull request URL:
```bash
px0 https://github.com/owner/repo/pull/123
```

px0 automatically prepares a temporary worktree or clone, computes the merge-base diff against the target branch, and opens a lightweight, zero-latency code viewer in your browser.

---

## Opening a Pull Request

Pull request review is triggered by passing the full URL directly:

```bash
px0 https://github.com/owner/repo/pull/123
```

> [!NOTE]
> Bare PR numbers (e.g. `px0 123`) and the `px0 pr` subcommand have been deprecated in favor of explicit URL routing (`px0 <url>`). Running `px0 pr` provides a helpful reminder to pass the URL directly.

### The Pull Requests Inbox
From any px0 session, the **Pull Requests** button (third in the Explorer toggle, next to Files and Git changes) lists the open pull requests of one repository. The repository picker at the top starts on the workspace's GitHub repository and offers the repositories you own or collaborate on, most recently pushed first; **Other repository…** takes any `owner/name` or GitHub URL. The choice is remembered in this browser. Three chips narrow the list, each with its count:
- **All**: every open PR of the repository.
- **Review requested**: the ones waiting on your review, oldest first.
- **Mine**: your own.

Each row shows the number, title, author, time since the last update, a draft flag, the CI result (✓ passing, ✕ failing, ● running) and the review state (approved, changes requested). Click a row or press **Enter** to open it: px0 checks the PR out in a new session and shows it in its own browser window. Opening the same PR again brings that window back rather than checking it out twice. The list refreshes when you open it, with its refresh button, and every 5 minutes while it is visible. Without a GitHub token it tells you how to add one.

Type in the filter box to narrow the list by title or author; the sort menu orders it by last update or creation instead of the chip's own order. **Git: Pull Requests** in the Command Palette opens the inbox from anywhere, and `px0 inbox` starts px0 straight into it without opening any workspace.

### Interactive Preparation Spinner
Because fetching metadata and checking out remote references takes a few moments, px0 displays an animated CLI spinner:
```text
⠋ Fetching PR #123 metadata from github...
⠙ Fetching PR #123 head and preparing worktree...
⠸ Computing merge base with main...
✔ PR #123 checked out (Refactor auth token resolution)
```

### Merged PR Handling
If the pull request is already merged:
- px0 detects its merged status from the API and opens it directly without blocking.
- In both the CLI checkout message and the browser review header, a prominent purple **`Merged`** pill badge is displayed.

### Multi-Session Isolation
Each PR review runs as its own isolated process on its own port. Running `px0 https://github.com/owner/repo/pull/456` while another PR review or local workspace is open will not disturb existing sessions.

From inside a running px0 browser session, the Command Palette (`Cmd/Ctrl+K` → **Git: Open Pull Request…**) launches a new review process in a fresh browser tab.

---

## Reviewing & Real-Time Agent Collaboration

### The Conversation Tab
A PR session opens on its **Conversation** tab, pinned first in the tab bar, as on GitHub:
- **Header**: title and number, state (open, draft, merged, closed), author, base and head branches, labels, assignees and requested reviewers. For an open PR, a readiness row shows the review decision, whether checks pass, and whether the branch has conflicts.
- **Checks**: a summary of the check runs on the head commit that opens into each check's result, duration and a link to its logs; it opens by itself when something failed.
- **Description**, rendered as GitHub Flavored Markdown through the same sanitizer as Markdown files: images load from GitHub, and scripts and unsafe HTML are stripped.
- **Timeline** in GitHub's order: comments, reviews (with their state), commits (consecutive ones grouped), force-pushes, review requests, label changes, and merge, close and reopen events.
- **Review threads** appear under the review that started them, with the last lines of the diff they are on. Resolved and outdated threads start collapsed. Click a thread's path to open the diff at that line; an outdated thread opens the file's current diff with a note that the line has moved.
- **Reply** to a thread, **Quote reply** to a comment, or write a new comment at the bottom. **Resolve** and **Unresolve** appear on threads when GitHub lets you use them. These act on GitHub immediately, as on GitHub.
- Clicking the comment marker of a posted thread in the diff gutter opens this tab at that thread.
- The tab refreshes when you come back to it after 30 seconds, after you submit a review, after a Pull, and on **Refresh**. Without a GitHub token it shows the header and description, with a button to connect one.

The file tree and the diff are the "Files changed" view.

### 1. Merge-Base Diff View
- The **Changes** toggle in the sidebar defaults to all files modified by the PR.
- Press **`Cmd/Ctrl+D`** on any file to open side-by-side or unified diffs.
- The diff is computed against the merge-base between the PR head and its target branch, exactly mirroring the diff shown on GitHub.

### 2. Live Agent Synchronization
When you or your background AI coding agents (Claude Code, Gemini CLI, Cursor Agent, Antigravity, Aider, etc.) make edits to the PR checkout from other terminals:
- px0's real-time file watcher immediately picks up modifications without full re-indexing.
- Gutter diffs, status badges, and open tabs reload live in the browser.

### 3. Line Comments & Hover Actions
When hovering over code lines or diff lines:
- A thread icon appears next to the line number.
- Clicking it opens a context menu for that line. On a diff line GitHub knows about it includes:
  - **Add Review Comment**: Drafts an inline review comment.
  - **Edit Inline**: Prompts your local AI coding agent to edit those lines directly.
  - **Start Thread**: Opens a conversation with the agent about the line.
- Alternatively, select any range of lines and press **`Alt+R`** (or click **Comment** on the selection bar) to open the review comment composer.

### 4. Files Changed
The **Files changed** tab, pinned next to Conversation, is GitHub's page of the same name: every file the PR changes, stacked, in split or unified layout, with a filterable file list beside them. Files fold shut from their header, a large diff loads on request, and **Open file** opens one in an editor tab. The diff is the PR's own change, the one a review is posted against.

Comments sit on their lines, as on GitHub:
- **The blue + on a line number** opens a comment box under the line: **Write** and **Preview** tabs, **± Suggestion** to insert a GitHub suggestion block for the line, then **Cancel**, **Comment now** (posts this one comment to the PR right away, on its own) and **Start a review** / **Add review comment** (keeps it as a draft for your review). **Shift-click** a second + on the same side for a multi-line comment.
- **Drafts** are marked *Pending* and can be edited, deleted or posted now.
- **Comments already posted** show with their author; **Reply in Conversation** opens their thread there.
- **Pending AI suggestions** show as cards with severity, category and confidence: **Dismiss**, **Edit**, **Comment now**, or **Add to review**. Their lines are marked in the gutter, and the arrows in the bar step from one suggestion to the next.

**Finish your review** jumps to the review summary in the PR bar.

### 5. Drafts, Discarding, and Fixing Locally
Drafts are not on GitHub until you submit the review (or post one with **Comment now**). The **Drafts** panel at the bottom lists exactly what your next review will send, by line, and the PR bar counts them:
- **Discard drafts** removes every draft at once, after asking: your own are deleted, and AI suggestions you had added go back to dismissed, where the AI Review tab's **All** filter can restore them.
- **⚡ Fix locally with AI** hands every draft to your coding harness as an instruction, and the harness edits the files of the PR checkout to address them. It posts nothing and leaves the drafts as they are: review the edits in the git panel, then **Push** to put them on the PR. An added AI suggestion that proposes code hands that code to the harness too.

### 6. AI Review: Suggestions You Curate
**AI Review** in the PR bar opens the **AI Review** tab, pinned after Files changed. Pick a harness and model, optionally type a focus ("security only"), and press **Run AI Review**. The harness reviews the PR read-only in the checkout: it can open any file and read `CLAUDE.md`, `AGENTS.md` or `CONTRIBUTING.md`, but its file-writing tools are turned off. Only harnesses px0 can run read-only are offered: Claude Code and Codex.

What comes back are *suggestions*, not comments: one per finding, never two issues in one. Nothing is posted until you act:
- The list is sorted by priority across all files: **Critical**, **High**, **Medium**, **Low**, each under its own heading, and within a severity the more confident first.
- Each suggestion shows its severity, category, confidence, its file and line and, when it proposes code, the change. Low-confidence ones are collapsed.
- Each one is added on its own: **A** adds it to your review as a separate draft comment, **P** posts it now as its own comment (**Comment now**), **E** edits then adds, **D** dismisses, **J**/**K** move. **Dismiss Low** clears every pending Low at once.
- Clicking a suggestion (or **Enter**) opens **Files changed** at its line, its lines highlighted and its card outlined, so you can read it against the code and act on it there. Clicking a file name opens that file.
- Added and edited suggestions become ordinary drafts. They are submitted with your review, under your name, and **⚡ Fix locally with AI** can hand them to a harness like any other draft. **Comment now** posts one straight away instead, and it is marked *posted*. A proposed change is posted as a GitHub suggestion block.
- In the single-file diff, pending suggestions have their own star marker in the gutter, distinct from your drafts and posted comments; clicking it opens the AI Review tab.
- The AI's summary pre-fills an empty review body, and its verdict is highlighted on the Comment or Request Changes button. px0 never selects Approve for you.

px0 checks every suggestion against the real diff before showing it, because models misreport line numbers. A suggestion whose line and quoted text agree stays put; one whose quoted text is found on a single nearby line is moved there and marked **re-anchored**; anything else becomes a **file-level** comment (posted in the review body under the file's name), and a comment on a file outside the PR becomes a **summary note**. None are dropped. If the harness changes a file anyway, the run is flagged and px0 leaves the change for you to inspect.

The review instructions are a markdown skill: `review.skillPath` in Settings, else `~/.px0/skills/review.md`, else the built-in skill, a senior-architect review of only what the PR changes and the code it affects, which checks each changed function's callers and tests and reports only issues it can show failing. px0 always appends its output format, so a custom skill cannot break parsing. Running again replaces the pending suggestions; accepted, edited and dismissed ones stay, and a suggestion you already accepted or dismissed is not shown again.

If the PR moves after a review run (you Pull new commits, or GitHub reports new ones when you submit), the pending suggestions are marked **stale**: their lines may have moved, so they cannot be accepted until you press **Re-check**, which places them again against the current diff. Dismissing still works.

### 7. Submitting Formal Reviews
The PR review bar above the editor tabs hosts the overall review summary and verdict actions:
- **Comment**: Submit feedback without an approval status (available to all reviewers).
- **Approve** / **Request Changes**: Available when your authenticated token has repository push access.
- Submitting posts a single review payload containing all draft line comments and the review body. Pending and dismissed AI suggestions are never included.

### 8. Editing, Committing, and Pushing Back
A PR checkout is a real git worktree, and the git panel works inside it exactly as it does in a plain workspace:
- **Edit and commit**: Make a change (by hand, or by dispatching a coding agent on the checkout), stage it, and either write a commit message yourself or click **Commit with AI**.
- **Push**: Sends the checkout's `HEAD` to the pull request's *actual* head branch on its actual repository — a fork included — not wherever the checkout happens to live locally.
- **Pull**: Re-fetches the PR's current head. If new commits landed on it since you opened the review and your checkout can fast-forward onto them cleanly, px0 updates the worktree and refreshes the diff, the PR bar, and the comments panel automatically. If your checkout has diverged — you made local commits that aren't on the PR head yet, or the head was force-pushed — px0 refuses with a clear message rather than merging; commit and push first, or resolve it in a terminal.

> [!IMPORTANT]
> A PR checkout lives in a temporary directory for the life of the px0 process (see [Multi-Session Isolation](#multi-session-isolation) below). Any edits you make there — committed or not — are gone once the session closes, unless you've pushed them back to the PR's branch first.

---

## Authentication & Read-Only Review

px0 discovers forge credentials in the following order:

1. **`github.token`** in px0 Settings (`Cmd/Ctrl+,` → GitHub, or `~/.px0/settings.json`).
2. **`GITHUB_TOKEN`** environment variable.
3. **`GH_TOKEN`** environment variable.
4. **`gh auth token`** via the GitHub CLI if installed and authenticated.

### Unauthenticated & Read-Only Access
If no token is configured:
- Public repositories still check out and diff seamlessly.
- You can draft review comments in memory and use **⚡ Fix locally with AI** with local AI agents. **Comment now** asks for a token.
- Formal review submission back to the remote forge requires an auth token. The CLI banner displays:
  ```text
  access: read-only (no github token: set GITHUB_TOKEN or gh auth login to submit reviews)
  ```

---

## Extensible Forge Architecture (`GitProvider`)

px0 abstracts forge interactions through a clean, minimal `GitProvider` interface in [`provider.go`](file:///home/arpit/workspace/px0/px0/provider.go):
- **Provider Detection**: Matches input URLs against registered providers (GitHub, and in the future GitLab, Bitbucket, etc.).
- **Normalized Metadata**: Maps forge-specific PR/MR objects to standard `PRMeta` structures.
- **Push Access & Token Discovery**: Isolates provider-specific authentication mechanisms.

---

## Keyboard Shortcuts & Controls

| Shortcut / Control | Context | Action |
| :--- | :--- | :--- |
| `Alt+R` | Selection in editor or diff view | Open review comment composer |
| Line Hover (`✏`) | Hovering on editor line number | Choose between GitHub review comment or inline agent edit |
| `Cmd/Ctrl+D` | Active tab | Toggle side-by-side / unified diff against merge-base |
| Command Palette (`Cmd/Ctrl+K`) | Command Palette → **Git: Open Pull Request…** | Launch a new PR review tab |
| **`⚡ Fix locally with AI`** | PR header bar | Have the coding harness edit the checkout to address every draft (posts nothing) |
| **Discard drafts** | PR header bar | Delete every draft; added AI suggestions go back to dismissed |
| **AI Review** | PR header bar | Open the AI Review tab (run a read-only review, triage suggestions) |
| `J` / `K` | AI Review tab | Next / previous suggestion |
| `A` / `E` / `D` | AI Review tab | Add to review / edit then add / dismiss the selected suggestion |
| `Enter` | AI Review tab | Show the suggestion on its line in Files changed |
| **+** on a line number | Files changed tab | Comment on the line; **Shift**-click a second + for a range |
| `Ctrl/Cmd+Enter` / `Esc` | Comment box | Add to review / cancel |
| **Submit Review** | PR header bar | Submit Approve / Request Changes / Comment to remote forge |
| **Pull** | Sidebar git panel | Fast-forward the checkout onto the PR's current head; refuses on divergence |
| **Push** | Sidebar git panel | Push the checkout's `HEAD` to the PR's actual head branch |
| **Commit with AI** | Sidebar git panel | Write and commit a message for the staged diff with your coding harness |
