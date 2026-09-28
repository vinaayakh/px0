# PR Conversation Tab

Every PR session opens a **Conversation** tab, pinned first in the tab bar, showing what GitHub's Conversation tab shows: the header and merge readiness, the description, and one timeline of comments, reviews with their inline threads, commits and events. The file tree and diff remain the "Files changed" view.

Source: [`conversation.go`](../../conversation.go) (GraphQL, normalisation, cache, handler), [`web/src/conversation.js`](../../web/src/conversation.js) (drawing, replies), the virtual-tab plumbing in [`web/src/virtualtab.js`](../../web/src/virtualtab.js), [`web/src/tabs.js`](../../web/src/tabs.js) and [`web/src/markdown.js`](../../web/src/markdown.js) ([markdown.md §13](markdown.md#13-virtual-tabs)).

## 1. Fetching

REST cannot say whether a review thread is resolved, so the tab uses GraphQL (`githubGraphQL`, plain `net/http`). `fetchConversation` runs two paginated queries:

- **Timeline** (`convTimelineQuery`), 100 items a page, filtered to the event types the tab draws. The first page also carries the header: title, state, draft, mergeable, review decision, refs, labels with colours, assignees, requested reviewers (users and `org/team`), and the head commit's check rollup.
- **Review threads** (`convThreadsQuery`), 50 a page, each with up to 50 comments, its path, current and original line, side, resolved and outdated flags, and the first comment's diff hunk and review id.

Both stop after 10 pages and mark the result `truncated`, so a huge PR costs a bounded number of calls. `MergedEvent.commit` is aliased `mergeCommit`: it is nullable while `PullRequestCommit.commit` is not, and GraphQL refuses two same-named fields of different nullability in one selection.

## 2. Normalising

`buildConversation` keeps GitHub's timeline order, which already matches what GitHub shows, and maps every node to a `TimelineItem` with a `kind`:

| GraphQL node | kind | Notes |
| --- | --- | --- |
| `IssueComment` | `comment` | keeps `databaseId` |
| `PullRequestReview` | `review` | state lower-cased; time is `submittedAt`. `PENDING` (the viewer's unsubmitted review) is dropped. A `COMMENTED` review with no body that starts no thread is dropped too: it is a reply posted on its own, and the reply already shows inside its thread. |
| `PullRequestCommit` | `commits` | consecutive commits collapse into one item; author is the GitHub login, else the git name |
| `HeadRefForcePushedEvent` | `force_push` | before and after SHAs |
| `ReviewRequestedEvent` | `review_requested` | user login or `org/team` |
| `LabeledEvent`, `UnlabeledEvent` | `labeled`, `unlabeled` | label name and colour |
| `MergedEvent`, `ClosedEvent`, `ReopenedEvent`, `ReadyForReviewEvent`, `ConvertToDraftEvent` | `merged`, `closed`, `reopened`, `ready_for_review`, `convert_to_draft` | |

Unknown node types are skipped. A deleted account shows as `ghost`, as on GitHub. Each `ReviewThread` carries the id of the review its first comment belongs to (`reviewId`), which is where GitHub draws it.

## 3. Rendering and Safety

`renderConversationHTML` renders the description and every comment body with the same goldmark pipeline as Markdown files (`renderMarkdown`). As for a README, that HTML is not trusted: the client adopts it only through `sanitizeForgeHTML` (the Markdown allowlist sanitizer in `markdown.js`), which drops `<script>`, event-handler attributes, `javascript:` links and every element and attribute outside the allowlist, and keeps `http(s)` images and links. Everything else the tab draws (names, titles, labels, SHAs) is escaped text. Bodies are inserted as sanitised nodes into placeholders after the surrounding markup is built, so they never go back through `innerHTML`.

## 4. Cache and Refresh

The handler keeps the last result on the session for 30 seconds (`convCache`), so switching tabs does not spend rate limit. `GET /api/pr/conversation?refresh=1` skips the cache. The client redraws from its own copy when the tab is shown, and refetches when that copy is older than 30 seconds, when **Refresh** is pressed, after a review is submitted (`pr:submitted`), and after a Pull moves the head (`pr:refreshed`, from `refreshPRMeta`).

Without a token GraphQL is unavailable. The handler then returns `needsToken` and a header built from the REST metadata fetched at checkout (`headerFromMeta`), and the tab shows the description with a Connect button.

## 5. The Tab

`initConversation` registers the `pr` scheme as a pinned virtual tab before tabs are restored. `boot()` opens `pr://conversation` in a PR session: in the foreground for a fresh session, in the background when earlier tabs were restored, so a reload stays on the file the reviewer was reading.

- **Header**: title and number, state pill (open, draft, merged, closed), author, base and head, labels in their colours, assignees, requested reviewers, and for an open PR a readiness row: review decision, check rollup, conflicts.
- **Timeline**: comments with **Quote reply**, reviews with a state accent and their threads nested inside, grouped commits, and one-line events.
- **Threads**: a `<details>` with the path and line, Outdated and Resolved badges, the last lines of the diff hunk, the comments and a reply box. Resolved and outdated threads start collapsed; the reviewer's open and close choices survive a redraw. Clicking the path opens the diff at that line (`revealPRLine`); an outdated thread opens the file's current diff with a notice that the line has moved. Threads whose review is not in the timeline are listed at the end.
- **Posting**: replies go to `/api/pr/comments/review-reply` (the thread's first comment's REST id) and new comments to `/api/pr/comments/issue`. Both post immediately and refresh the tab. Without a token, posting opens the token setting instead.

## 6. Tests

Beyond what follows, [`m5_test.go`](../../m5_test.go) covers check-run parsing and ordering and the resolve and unresolve mutations (and the no-token refusal).


[`conversation_test.go`](../../conversation_test.go) serves fixture GraphQL pages through a fake transport: two timeline pages and a thread page (paging, commit grouping, the dropped reply-only and pending reviews, submitted time, force push, ghost authors, label colours, the aliased merge commit, unknown types skipped, thread fields and review links), the page limit and `truncated`, GraphQL errors, GFM rendering, the no-token fallback, and the cache with `refresh=1`. The sanitizer is the Markdown preview's, covered by the hostile-input cases in [markdown.md §4](markdown.md#4-sanitization); there is no browser test in the repository.

## 7. Checks, Resolving Threads, and the Drafts Panel

**Checks.** The timeline query also asks for the head commit (`headRefOid`); `fetchCheckRuns` then lists that commit's check runs through REST (`/commits/{sha}/check-runs`, up to 100), sorted failures first, then running, then the rest. A failure here only leaves the panel empty. The tab shows a summary line ("2 failed, 1 running, 9 passed") that opens into one row per check with its result, duration and a link to its logs; it opens by itself when something failed. Legacy commit statuses (the pre-Checks API) are not listed; they still count in the readiness row's rollup.

**Resolving.** The thread query asks for `viewerCanResolve` and `viewerCanUnresolve`, so a thread shows **Resolve** or **Unresolve** only when GitHub would allow it. `POST /api/pr/threads/resolve {threadId, resolved}` runs the `resolveReviewThread` or `unresolveReviewThread` mutation right away, as the button on GitHub does, and drops the cached conversation so the next draw shows the new state.

**Drafts only below.** The bottom panel in `pr.js` now lists only what the next review sends: human drafts and accepted AI suggestions, grouped by line, with a jump to the line and delete. Comments already on the PR, their replies and the top-level comment box live in this tab. A gutter badge on a line with a posted thread opens this tab at that thread (`conversation:reveal`), expanding it even when resolved or outdated; a badge on a line with drafts opens the drafts panel.
