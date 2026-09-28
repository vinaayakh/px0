# PR Inbox

The inbox lists open pull requests in the sidebar and opens one in a PR review session with a click, replacing pasted URLs as the normal way in. It is the third button of the Explorer toggle (Files, Git changes, Pull Requests) and works in any session that can find a GitHub token.

Source: [`inbox.go`](../../inbox.go) (search, sections, cache, launching), [`web/src/inbox.js`](../../web/src/inbox.js) (panel, opening a PR in its window).

## 1. Sections

| Section | Search qualifiers | Shown when |
| --- | --- | --- |
| Review requested | `is:open is:pr archived:false review-requested:@me sort:created-asc` (oldest first, so nothing waits forever at the bottom) | a token is found |
| Mine | `is:open is:pr archived:false author:@me sort:updated-desc` | a token is found |
| This repo | `is:open is:pr repo:<owner/name> sort:updated-desc` | the workspace has a GitHub repository |

"This repo" is the PR's own repository in a PR session, else the `origin` remote parsed by `githubRepoFromRemote` (HTTPS with or without credentials, `git@github.com:`, `ssh://`, `git://`; anything not on github.com gives none and the section is hidden).

## 2. One Search per Section

`listPRs` runs a GraphQL `search` (type `ISSUE`, 50 per page, at most 2 pages) that returns everything a row needs in one call, instead of one REST call per PR: number, title, URL, draft, created and updated times, author, repository, `reviewDecision`, and the head commit's `statusCheckRollup`. `parseInboxSearch` maps the rollup to `pass` (SUCCESS), `fail` (FAILURE, ERROR), `pending` (anything else) or none, and the review decision to `approved`, `changes_requested` or none. Search can return issues for a loose query; nodes without a number and URL are skipped. The response also carries GitHub's total count so the panel can say "Showing 100 of N".

## 3. Caching and Tokens

GitHub allows 30 searches a minute. Results are cached in memory per query for 60 seconds, so toggling the panel costs nothing; `?refresh=1` (the panel's refresh button) skips the cache. Nothing is written to disk.

The token is the PR session's own when there is one, else the usual four sources (`github.token`, `GITHUB_TOKEN`, `GH_TOKEN`, `gh auth token`), resolved at most once a minute because `gh auth token` is a process spawn. Without a token every section answers `needsToken` and the panel shows one line explaining how to add one, with a button to the setting.

## 4. Refresh

The panel loads when it opens, on the refresh button, and every 5 minutes while it is open and the page is visible. A hidden page stops the timer; coming back after more than 5 minutes loads at once.

## 5. Opening a PR

Opening a PR starts a child px0 (`px0 -no-open <url>`) from the workspace directory, as the palette's **Open Pull Request…** always did. What is new is that the parent remembers it (`launched`, keyed by provider/owner/repo/number, so any spelling of the URL matches):

1. The click opens a browser window named after the PR (`px0-pr-owner-repo-123`) synchronously, so popup blockers allow it, and writes "Preparing…" into it.
2. `POST /api/pr/launch` starts the child unless one started from here is still alive (`already: true`). Launching the session's own PR returns `self` and nothing starts.
3. `watchLaunched` reads the child's output, strips ANSI codes, and records its URL from the `url:` line it prints once it is serving. The client polls `GET /api/pr/launch?target=` until the state is `running` (or `failed`, with the last lines of the child's output as the error) and points the window at the URL.
4. Opening the same PR again finds that window by name. When it already shows the child's page (another port, which this page cannot read), it is just focused; if the reviewer closed it, the new window goes to the running child's URL. Either way no second worktree is created.
5. When the child exits, it is forgotten and the next click starts a new one. A failed launch is retried on the next click.

The launched child opens without its own browser tab (`-no-open`) because the parent's page opens it. If the browser blocks the window anyway, a toast gives the URL.

## 6. Endpoints

| Method and path | Returns |
| --- | --- |
| `GET /api/inbox?section=review\|mine\|repo[&refresh=1]` | `{section, items: []PRSummary, total, fetchedAt, repo, current, hidden, needsToken}` |
| `POST /api/pr/launch {target}` | `{ok, already, launch: {key, target, url, state, error}}` or `{ok, self}` |
| `GET /api/pr/launch?target=` | `{key, target, url, state, error}`; 404 when not launched from here |

## 7. Tests

[`inbox_test.go`](../../inbox_test.go): search parsing (CI and review mapping, ghost authors, skipped nodes), paging stops at 2 pages, remote URL parsing (including look-alike hosts), no-token, hidden repo section, the cache and `refresh=1`, the repo section in a PR session, and the launch registry, which runs the test binary as a stand-in child: the URL is read through ANSI codes, another spelling of the same PR reuses the running child, an exited child is forgotten, a failed one reports its output and is retried, and the session's own PR is refused. [`github_live_test.go`](../../github_live_test.go) checks the inbox and Conversation queries against GitHub itself when run with `PX0_LIVE_GITHUB=1` (read-only).

## 8. Not Yet Done

- Filter box and sort options (INB-4).
- The palette command **Git: Pull Requests** and `px0 inbox` to start straight into the panel without a workspace (INB-6).
