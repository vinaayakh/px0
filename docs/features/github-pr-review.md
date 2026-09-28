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
From any px0 session, the **Pull Requests** button (third in the Explorer toggle, next to Files and Git changes) lists open pull requests in three sections:
- **Review requested**: every PR waiting on your review, across repositories, oldest first.
- **Mine**: your own open PRs.
- **This repo**: all open PRs of the workspace's GitHub repository (hidden when it has none).

Each row shows the repository and number, title, author, time since the last update, a draft flag, the CI result (✓ passing, ✕ failing, ● running) and the review state (approved, changes requested). Click a row or press **Enter** to open it: px0 checks the PR out in a new session and shows it in its own browser window. Opening the same PR again brings that window back rather than checking it out twice. The list refreshes when you open it, with its refresh button, and every 5 minutes while it is visible. Without a GitHub token it tells you how to add one.

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
- **Description**, rendered as GitHub Flavored Markdown through the same sanitizer as Markdown files: images load from GitHub, and scripts and unsafe HTML are stripped.
- **Timeline** in GitHub's order: comments, reviews (with their state), commits (consecutive ones grouped), force-pushes, review requests, label changes, and merge, close and reopen events.
- **Review threads** appear under the review that started them, with the last lines of the diff they are on. Resolved and outdated threads start collapsed. Click a thread's path to open the diff at that line; an outdated thread opens the file's current diff with a note that the line has moved.
- **Reply** to a thread, **Quote reply** to a comment, or write a new comment at the bottom. These post to GitHub immediately, as on GitHub.
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

### 4. Batch Applying Comments Locally (`⚡ Batch Apply`)
Drafted review comments appear in the PR top bar and inline across files:
- **⚡ Batch Apply**: Lets you apply all drafted review comments across the entire pull request in one go using your configured coding agent harness. The agent reads your comments as instructions and modifies the code directly in the PR worktree.
- Comments remain drafts in memory until either batch-applied or formally submitted.

### 5. AI Review: Suggestions You Curate
**AI Review** in the PR bar opens the AI Review pane in the right inspector. Pick a harness and model, optionally type a focus ("security only"), and press **Run AI Review**. The harness reviews the PR read-only in the checkout: it can open any file and read `CLAUDE.md`, `AGENTS.md` or `CONTRIBUTING.md`, but its file-writing tools are turned off. Only harnesses px0 can run read-only are offered: Claude Code and Codex.

What comes back are *suggestions*, not comments. Nothing is posted until you act:
- Each suggestion shows its severity, category, confidence, the line it is on and, when it proposes code, the change. Low-confidence ones are collapsed.
- **A** accepts, **E** edits then accepts, **D** dismisses, **J**/**K** move, **Enter** opens the line in the diff. **Accept all** per file and **Dismiss nits** work on many at once.
- Accepted and edited suggestions become ordinary drafts. They are submitted with your review, under your name, and **⚡ Batch Apply** can hand them to a harness like any other draft. A proposed change is posted as a GitHub suggestion block.
- Pending suggestions have their own star marker in the diff gutter, distinct from your drafts and posted comments.
- The AI's summary pre-fills an empty review body, and its verdict is highlighted on the Comment or Request Changes button. px0 never selects Approve for you.

px0 checks every suggestion against the real diff before showing it, because models misreport line numbers. A suggestion whose line and quoted text agree stays put; one whose quoted text is found on a single nearby line is moved there and marked **re-anchored**; anything else becomes a **file-level** comment (posted in the review body under the file's name), and a comment on a file outside the PR becomes a **summary note**. None are dropped. If the harness changes a file anyway, the run is flagged and px0 leaves the change for you to inspect.

The review instructions are a markdown skill: `review.skillPath` in Settings, else `~/.px0/skills/review.md`, else the built-in skill. px0 always appends its output format, so a custom skill cannot break parsing. Running again replaces the pending suggestions; accepted, edited and dismissed ones stay.

### 6. Submitting Formal Reviews
The PR review bar above the editor tabs hosts the overall review summary and verdict actions:
- **Comment**: Submit feedback without an approval status (available to all reviewers).
- **Approve** / **Request Changes**: Available when your authenticated token has repository push access.
- Submitting posts a single review payload containing all draft line comments and the review body. Pending and dismissed AI suggestions are never included.

### 7. Editing, Committing, and Pushing Back
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
- You can draft review comments in memory and use **⚡ Batch Apply** with local AI agents.
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
| **`⚡ Batch Apply`** | PR header bar | Dispatch all drafted comments to local AI coding harness |
| **AI Review** | PR header bar | Open the AI Review pane (run a read-only review, triage suggestions) |
| `J` / `K` | AI Review pane | Next / previous suggestion |
| `A` / `E` / `D` | AI Review pane | Accept / edit then accept / dismiss the selected suggestion |
| `Enter` | AI Review pane | Open the suggestion's line in the diff |
| **Submit Review** | PR header bar | Submit Approve / Request Changes / Comment to remote forge |
| **Pull** | Sidebar git panel | Fast-forward the checkout onto the PR's current head; refuses on divergence |
| **Push** | Sidebar git panel | Push the checkout's `HEAD` to the PR's actual head branch |
| **Commit with AI** | Sidebar git panel | Write and commit a message for the staged diff with your coding harness |
