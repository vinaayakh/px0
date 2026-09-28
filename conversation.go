package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// conversation.go feeds the PR Conversation tab (web/src/conversation.js):
// the header, description, and one timeline of comments, reviews with their
// inline threads, commits and events, in GitHub's own order. REST cannot say
// whether a thread is resolved, so this is GraphQL: one query pages through
// the timeline (with the header on the first page), another through the
// review threads. Bodies are rendered to HTML with the same goldmark pipeline
// as Markdown files; the browser sanitises that HTML before it reaches the
// page, exactly as it does for a README.

const (
	convCacheTTL      = 30 * time.Second
	convTimelinePages = 10 // x100 items
	convThreadPages   = 10 // x50 threads
)

// convTimelineTypes are the timeline events the tab shows.
const convTimelineTypes = `[ISSUE_COMMENT, PULL_REQUEST_REVIEW, PULL_REQUEST_COMMIT, HEAD_REF_FORCE_PUSHED_EVENT,
  REVIEW_REQUESTED_EVENT, LABELED_EVENT, UNLABELED_EVENT, MERGED_EVENT, CLOSED_EVENT, REOPENED_EVENT,
  READY_FOR_REVIEW_EVENT, CONVERT_TO_DRAFT_EVENT]`

// MergedEvent.commit is nullable while PullRequestCommit.commit is not, and
// GraphQL refuses to merge fields of different nullability under one
// response name, hence the mergeCommit alias.
const convTimelineQuery = `query($owner: String!, $repo: String!, $number: Int!, $cursor: String) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      number title url body createdAt isDraft state merged mergeable reviewDecision baseRefName headRefName headRefOid
      author { login avatarUrl }
      labels(first: 30) { nodes { name color } }
      assignees(first: 20) { nodes { login } }
      reviewRequests(first: 20) { nodes { requestedReviewer { __typename ... on User { login } ... on Team { combinedSlug } } } }
      headCommit: commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }
      timelineItems(first: 100, after: $cursor, itemTypes: ` + convTimelineTypes + `) {
        pageInfo { hasNextPage endCursor }
        nodes {
          __typename
          ... on IssueComment { id databaseId author { login avatarUrl } body createdAt url }
          ... on PullRequestReview { id databaseId author { login avatarUrl } body state createdAt submittedAt url }
          ... on PullRequestCommit { id url commit { oid messageHeadline committedDate author { name user { login } } } }
          ... on HeadRefForcePushedEvent { id createdAt actor { login } beforeCommit { oid } afterCommit { oid } }
          ... on ReviewRequestedEvent { id createdAt actor { login } requestedReviewer { __typename ... on User { login } ... on Team { combinedSlug } } }
          ... on LabeledEvent { id createdAt actor { login } label { name color } }
          ... on UnlabeledEvent { id createdAt actor { login } label { name color } }
          ... on MergedEvent { id createdAt actor { login } mergeCommit: commit { oid } }
          ... on ClosedEvent { id createdAt actor { login } }
          ... on ReopenedEvent { id createdAt actor { login } }
          ... on ReadyForReviewEvent { id createdAt actor { login } }
          ... on ConvertToDraftEvent { id createdAt actor { login } }
        }
      }
    }
  }
}`

const convThreadsQuery = `query($owner: String!, $repo: String!, $number: Int!, $cursor: String) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      reviewThreads(first: 50, after: $cursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id isResolved isOutdated path line originalLine startLine diffSide viewerCanResolve viewerCanUnresolve
          comments(first: 50) {
            nodes { id databaseId author { login avatarUrl } body createdAt url diffHunk pullRequestReview { id } }
          }
        }
      }
    }
  }
}`

// ---------------------------------------------------------------- GraphQL shapes

type gqlActor struct {
	Login     string `json:"login"`
	AvatarURL string `json:"avatarUrl"`
}

func (a *gqlActor) login() string {
	if a == nil {
		return "ghost" // a deleted account, as GitHub shows it
	}
	return a.Login
}

func (a *gqlActor) avatar() string {
	if a == nil {
		return ""
	}
	return a.AvatarURL
}

type gqlOid struct {
	Oid string `json:"oid"`
}

type gqlReviewer struct {
	Typename     string `json:"__typename"`
	Login        string `json:"login"`
	CombinedSlug string `json:"combinedSlug"`
}

func (r *gqlReviewer) name() string {
	if r == nil {
		return ""
	}
	if r.Login != "" {
		return r.Login
	}
	return r.CombinedSlug
}

type gqlTimelineNode struct {
	Typename    string    `json:"__typename"`
	ID          string    `json:"id"`
	DatabaseID  int64     `json:"databaseId"`
	Author      *gqlActor `json:"author"`
	Actor       *gqlActor `json:"actor"`
	Body        string    `json:"body"`
	CreatedAt   string    `json:"createdAt"`
	SubmittedAt string    `json:"submittedAt"`
	URL         string    `json:"url"`
	State       string    `json:"state"`
	Commit      *struct {
		Oid             string `json:"oid"`
		MessageHeadline string `json:"messageHeadline"`
		CommittedDate   string `json:"committedDate"`
		Author          *struct {
			Name string    `json:"name"`
			User *gqlActor `json:"user"`
		} `json:"author"`
	} `json:"commit"`
	MergeCommit       *gqlOid      `json:"mergeCommit"`
	BeforeCommit      *gqlOid      `json:"beforeCommit"`
	AfterCommit       *gqlOid      `json:"afterCommit"`
	RequestedReviewer *gqlReviewer `json:"requestedReviewer"`
	Label             *PRLabel     `json:"label"`
}

type gqlPageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type gqlPR struct {
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	URL            string    `json:"url"`
	Body           string    `json:"body"`
	CreatedAt      string    `json:"createdAt"`
	IsDraft        bool      `json:"isDraft"`
	State          string    `json:"state"`
	Merged         bool      `json:"merged"`
	Mergeable      string    `json:"mergeable"`
	ReviewDecision string    `json:"reviewDecision"`
	BaseRefName    string    `json:"baseRefName"`
	HeadRefName    string    `json:"headRefName"`
	HeadRefOid     string    `json:"headRefOid"`
	Author         *gqlActor `json:"author"`
	Labels         struct {
		Nodes []PRLabel `json:"nodes"`
	} `json:"labels"`
	Assignees struct {
		Nodes []gqlActor `json:"nodes"`
	} `json:"assignees"`
	ReviewRequests struct {
		Nodes []struct {
			RequestedReviewer *gqlReviewer `json:"requestedReviewer"`
		} `json:"nodes"`
	} `json:"reviewRequests"`
	HeadCommit struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"headCommit"`
	TimelineItems struct {
		PageInfo gqlPageInfo       `json:"pageInfo"`
		Nodes    []gqlTimelineNode `json:"nodes"`
	} `json:"timelineItems"`
}

type gqlThread struct {
	ID           string `json:"id"`
	IsResolved   bool   `json:"isResolved"`
	IsOutdated   bool   `json:"isOutdated"`
	Path         string `json:"path"`
	Line         int    `json:"line"`
	OriginalLine int    `json:"originalLine"`
	StartLine    int    `json:"startLine"`
	DiffSide     string `json:"diffSide"`
	CanResolve   bool   `json:"viewerCanResolve"`
	CanUnresolve bool   `json:"viewerCanUnresolve"`
	Comments     struct {
		Nodes []struct {
			ID                string    `json:"id"`
			DatabaseID        int64     `json:"databaseId"`
			Author            *gqlActor `json:"author"`
			Body              string    `json:"body"`
			CreatedAt         string    `json:"createdAt"`
			URL               string    `json:"url"`
			DiffHunk          string    `json:"diffHunk"`
			PullRequestReview *struct {
				ID string `json:"id"`
			} `json:"pullRequestReview"`
		} `json:"nodes"`
	} `json:"comments"`
}

type gqlPRData struct {
	Repository *struct {
		PullRequest *gqlPR `json:"pullRequest"`
	} `json:"repository"`
}

type gqlThreadsData struct {
	Repository *struct {
		PullRequest *struct {
			ReviewThreads struct {
				PageInfo gqlPageInfo `json:"pageInfo"`
				Nodes    []gqlThread `json:"nodes"`
			} `json:"reviewThreads"`
		} `json:"pullRequest"`
	} `json:"repository"`
}

// ---------------------------------------------------------------- fetch

// fetchConversation pages through the timeline and the review threads and
// normalises them. It stops at the page limits and marks the result
// truncated rather than make an unbounded number of calls.
func fetchConversation(ctx context.Context, owner, repo string, num int, token string) (PRConversation, error) {
	vars := map[string]any{"owner": owner, "repo": repo, "number": num, "cursor": nil}
	var head *gqlPR
	var timeline []gqlTimelineNode
	truncated := false
	for page := 0; ; page++ {
		var d gqlPRData
		if err := githubGraphQL(ctx, token, convTimelineQuery, vars, &d); err != nil {
			return PRConversation{}, err
		}
		if d.Repository == nil || d.Repository.PullRequest == nil {
			return PRConversation{}, fmt.Errorf("github: PR #%d not found in %s/%s", num, owner, repo)
		}
		pr := d.Repository.PullRequest
		if head == nil {
			head = pr
		}
		timeline = append(timeline, pr.TimelineItems.Nodes...)
		if !pr.TimelineItems.PageInfo.HasNextPage {
			break
		}
		if page+1 >= convTimelinePages {
			truncated = true
			break
		}
		vars["cursor"] = pr.TimelineItems.PageInfo.EndCursor
	}

	var threads []gqlThread
	vars["cursor"] = nil
	for page := 0; ; page++ {
		var d gqlThreadsData
		if err := githubGraphQL(ctx, token, convThreadsQuery, vars, &d); err != nil {
			return PRConversation{}, err
		}
		if d.Repository == nil || d.Repository.PullRequest == nil {
			break
		}
		rt := d.Repository.PullRequest.ReviewThreads
		threads = append(threads, rt.Nodes...)
		if !rt.PageInfo.HasNextPage {
			break
		}
		if page+1 >= convThreadPages {
			truncated = true
			break
		}
		vars["cursor"] = rt.PageInfo.EndCursor
	}

	conv := buildConversation(head, timeline, threads)
	conv.Truncated = truncated
	if conv.Header.HeadSHA != "" {
		// Checks are extra: a failure here leaves the panel empty, not the tab.
		if checks, err := fetchCheckRuns(ctx, owner, repo, conv.Header.HeadSHA, token); err == nil {
			conv.Checks = checks
		}
	}
	return conv, nil
}

// fetchCheckRuns lists the check runs on a commit (REST; up to 100, which
// covers any real CI setup).
func fetchCheckRuns(ctx context.Context, owner, repo, sha, token string) ([]PRCheck, error) {
	resp, err := githubRequest(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs?per_page=100", owner, repo, sha), token, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github: check runs: %s", resp.Status)
	}
	var out struct {
		CheckRuns []struct {
			Name        string `json:"name"`
			Status      string `json:"status"`
			Conclusion  string `json:"conclusion"`
			StartedAt   string `json:"started_at"`
			CompletedAt string `json:"completed_at"`
			HTMLURL     string `json:"html_url"`
			DetailsURL  string `json:"details_url"`
		} `json:"check_runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	checks := make([]PRCheck, 0, len(out.CheckRuns))
	for _, c := range out.CheckRuns {
		u := c.HTMLURL
		if u == "" {
			u = c.DetailsURL
		}
		checks = append(checks, PRCheck{Name: c.Name, Status: c.Status, Conclusion: c.Conclusion,
			StartedAt: c.StartedAt, CompletedAt: c.CompletedAt, URL: u})
	}
	// Failures first, then running, then the rest, each by name.
	rank := func(c PRCheck) int {
		switch {
		case c.Conclusion == "failure" || c.Conclusion == "timed_out" || c.Conclusion == "action_required" || c.Conclusion == "cancelled":
			return 0
		case c.Status != "completed":
			return 1
		}
		return 2
	}
	sort.SliceStable(checks, func(i, j int) bool {
		if rank(checks[i]) != rank(checks[j]) {
			return rank(checks[i]) < rank(checks[j])
		}
		return checks[i].Name < checks[j].Name
	})
	return checks, nil
}

const resolveThreadMutation = `mutation($id: ID!) { resolveReviewThread(input: {threadId: $id}) { thread { id isResolved } } }`
const unresolveThreadMutation = `mutation($id: ID!) { unresolveReviewThread(input: {threadId: $id}) { thread { id isResolved } } }`

// handlePRThreadResolve resolves or unresolves a review thread on GitHub,
// right away, like the button on GitHub: POST {threadId, resolved}.
func (s *Server) handlePRThreadResolve(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) || !localPost(w, r) {
		return
	}
	p := s.pr
	if p.token == "" {
		fail(w, http.StatusForbidden, "resolving threads needs a GitHub token")
		return
	}
	var body struct {
		ThreadID string `json:"threadId"`
		Resolved bool   `json:"resolved"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil || body.ThreadID == "" {
		fail(w, http.StatusBadRequest, "threadId is required")
		return
	}
	q := resolveThreadMutation
	if !body.Resolved {
		q = unresolveThreadMutation
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := githubGraphQL(ctx, p.token, q, map[string]any{"id": body.ThreadID}, nil); err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	p.mu.Lock()
	p.conv = nil // the next load shows the new state
	p.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true, "resolved": body.Resolved})
}

// ---------------------------------------------------------------- normalise

// buildConversation turns the GraphQL results into what the tab draws. The
// timeline keeps GitHub's order; consecutive commits collapse into one item;
// a review with no body whose threads all live elsewhere (a reply posted on
// its own) is dropped, since the reply already shows inside its thread.
func buildConversation(pr *gqlPR, timeline []gqlTimelineNode, gthreads []gqlThread) PRConversation {
	conv := PRConversation{Items: []TimelineItem{}, Threads: []ReviewThread{}, Checks: []PRCheck{}}
	conv.Header = buildHeader(pr)

	rooted := map[string]bool{} // review ids that start at least one thread
	for _, t := range gthreads {
		th := ReviewThread{
			ID: t.ID, Path: t.Path, Line: t.Line, OriginalLine: t.OriginalLine, StartLine: t.StartLine,
			Side: strings.ToUpper(t.DiffSide), Resolved: t.IsResolved, Outdated: t.IsOutdated,
			CanResolve: t.CanResolve, CanUnresolve: t.CanUnresolve,
			Comments: []ThreadComment{},
		}
		if th.Side == "" {
			th.Side = "RIGHT"
		}
		for i, c := range t.Comments.Nodes {
			if i == 0 {
				th.DiffHunk = c.DiffHunk
				if c.PullRequestReview != nil {
					th.ReviewID = c.PullRequestReview.ID
					rooted[th.ReviewID] = true
				}
			}
			th.Comments = append(th.Comments, ThreadComment{
				ID: c.ID, DatabaseID: c.DatabaseID, Author: c.Author.login(), AvatarURL: c.Author.avatar(),
				Body: c.Body, CreatedAt: c.CreatedAt, URL: c.URL,
			})
		}
		conv.Threads = append(conv.Threads, th)
	}

	for _, n := range timeline {
		it := TimelineItem{ID: n.ID, CreatedAt: n.CreatedAt}
		switch n.Typename {
		case "IssueComment":
			it.Kind, it.DatabaseID, it.Body, it.URL = "comment", n.DatabaseID, n.Body, n.URL
			it.Author, it.AvatarURL = n.Author.login(), n.Author.avatar()
		case "PullRequestReview":
			state := strings.ToLower(n.State)
			if state == "pending" {
				continue // the viewer's own unsubmitted review
			}
			if state == "commented" && strings.TrimSpace(n.Body) == "" && !rooted[n.ID] {
				continue
			}
			it.Kind, it.DatabaseID, it.Body, it.URL, it.State = "review", n.DatabaseID, n.Body, n.URL, state
			it.Author, it.AvatarURL = n.Author.login(), n.Author.avatar()
			if n.SubmittedAt != "" {
				it.CreatedAt = n.SubmittedAt
			}
		case "PullRequestCommit":
			if n.Commit == nil {
				continue
			}
			c := TimelineCommit{SHA: n.Commit.Oid, Subject: n.Commit.MessageHeadline, Date: n.Commit.CommittedDate}
			if a := n.Commit.Author; a != nil {
				c.Author = a.Name
				if a.User != nil && a.User.Login != "" {
					c.Author = a.User.Login
				}
			}
			if last := len(conv.Items) - 1; last >= 0 && conv.Items[last].Kind == "commits" {
				conv.Items[last].Commits = append(conv.Items[last].Commits, c)
				continue
			}
			it.Kind, it.CreatedAt, it.Author, it.Commits = "commits", c.Date, c.Author, []TimelineCommit{c}
		case "HeadRefForcePushedEvent":
			it.Kind, it.Author = "force_push", n.Actor.login()
			if n.AfterCommit != nil {
				it.SHA = n.AfterCommit.Oid
			}
			if n.BeforeCommit != nil {
				it.BeforeSHA = n.BeforeCommit.Oid
			}
		case "ReviewRequestedEvent":
			it.Kind, it.Author, it.Subject = "review_requested", n.Actor.login(), n.RequestedReviewer.name()
		case "LabeledEvent", "UnlabeledEvent":
			it.Kind, it.Author = "labeled", n.Actor.login()
			if n.Typename == "UnlabeledEvent" {
				it.Kind = "unlabeled"
			}
			if n.Label != nil {
				it.Subject, it.Color = n.Label.Name, n.Label.Color
			}
		case "MergedEvent":
			it.Kind, it.Author = "merged", n.Actor.login()
			if n.MergeCommit != nil {
				it.SHA = n.MergeCommit.Oid
			}
		case "ClosedEvent":
			it.Kind, it.Author = "closed", n.Actor.login()
		case "ReopenedEvent":
			it.Kind, it.Author = "reopened", n.Actor.login()
		case "ReadyForReviewEvent":
			it.Kind, it.Author = "ready_for_review", n.Actor.login()
		case "ConvertToDraftEvent":
			it.Kind, it.Author = "convert_to_draft", n.Actor.login()
		default:
			continue
		}
		conv.Items = append(conv.Items, it)
	}
	return conv
}

func buildHeader(pr *gqlPR) PRHeader {
	h := PRHeader{Labels: []PRLabel{}, Assignees: []string{}, RequestedReviewers: []string{}}
	if pr == nil {
		return h
	}
	h.Number, h.Title, h.URL, h.CreatedAt, h.Body = pr.Number, pr.Title, pr.URL, pr.CreatedAt, pr.Body
	h.Author, h.AvatarURL = pr.Author.login(), pr.Author.avatar()
	h.BaseRef, h.HeadRef, h.HeadSHA = pr.BaseRefName, pr.HeadRefName, pr.HeadRefOid
	switch {
	case pr.Merged || strings.EqualFold(pr.State, "MERGED"):
		h.State = "merged"
	case strings.EqualFold(pr.State, "CLOSED"):
		h.State = "closed"
	case pr.IsDraft:
		h.State = "draft"
	default:
		h.State = "open"
	}
	h.Mergeable = strings.ToLower(pr.Mergeable)
	h.ReviewDecision = strings.ToLower(pr.ReviewDecision)
	if n := pr.HeadCommit.Nodes; len(n) > 0 && n[0].Commit.StatusCheckRollup != nil {
		switch strings.ToUpper(n[0].Commit.StatusCheckRollup.State) {
		case "SUCCESS":
			h.ChecksState = "success"
		case "FAILURE", "ERROR":
			h.ChecksState = "failure"
		default: // PENDING, EXPECTED
			h.ChecksState = "pending"
		}
	}
	h.Labels = append(h.Labels, pr.Labels.Nodes...)
	for _, a := range pr.Assignees.Nodes {
		h.Assignees = append(h.Assignees, a.Login)
	}
	for _, r := range pr.ReviewRequests.Nodes {
		if name := r.RequestedReviewer.name(); name != "" {
			h.RequestedReviewers = append(h.RequestedReviewers, name)
		}
	}
	return h
}

// renderConversationHTML fills every BodyHTML with goldmark output. A body
// that fails to render is left without HTML and the client shows its text.
func renderConversationHTML(conv *PRConversation) {
	render := func(md string) string {
		if strings.TrimSpace(md) == "" {
			return ""
		}
		html, err := renderMarkdown([]byte(md))
		if err != nil {
			return ""
		}
		return html
	}
	conv.Header.BodyHTML = render(conv.Header.Body)
	for i := range conv.Items {
		conv.Items[i].BodyHTML = render(conv.Items[i].Body)
	}
	for i := range conv.Threads {
		for j := range conv.Threads[i].Comments {
			c := &conv.Threads[i].Comments[j]
			c.BodyHTML = render(c.Body)
		}
	}
}

// headerFromMeta is the fallback header when there is no token (GraphQL
// needs one): what the REST fetch at checkout already knows.
func headerFromMeta(m PRMeta, url string) PRHeader {
	h := PRHeader{
		Number: m.Number, Title: m.Title, Author: m.Author, URL: url, Body: m.Body,
		BaseRef: m.BaseRef, HeadRef: m.HeadRef, Labels: []PRLabel{},
		Assignees: nonNilStrings(m.Assignees), RequestedReviewers: nonNilStrings(m.RequestedReviewers),
	}
	for _, l := range m.Labels {
		h.Labels = append(h.Labels, PRLabel{Name: l})
	}
	switch {
	case m.Merged:
		h.State = "merged"
	case m.State == "closed":
		h.State = "closed"
	case m.Draft:
		h.State = "draft"
	default:
		h.State = "open"
	}
	switch m.Mergeable {
	case "dirty":
		h.Mergeable = "conflicting"
	case "", "unknown":
		h.Mergeable = "unknown"
	default:
		h.Mergeable = "mergeable"
	}
	return h
}

// ---------------------------------------------------------------- HTTP

// convCache holds the last conversation for convCacheTTL, so switching back
// to the tab does not spend GraphQL rate limit. Guarded by prSession.mu.
type convCache struct {
	conv PRConversation
	at   time.Time
}

// handlePRConversation serves the Conversation tab: GET, ?refresh=1 skips
// the cache.
func (s *Server) handlePRConversation(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) {
		return
	}
	p := s.pr
	p.mu.Lock()
	meta, target, token, cached := p.meta, p.target, p.token, p.conv
	p.mu.Unlock()

	if token == "" {
		conv := PRConversation{Header: headerFromMeta(meta, target.URL), Items: []TimelineItem{}, Threads: []ReviewThread{}, Checks: []PRCheck{}}
		renderConversationHTML(&conv)
		writeJSON(w, map[string]any{"conversation": conv, "needsToken": true})
		return
	}
	if cached != nil && r.URL.Query().Get("refresh") == "" && time.Since(cached.at) < convCacheTTL {
		writeJSON(w, map[string]any{"conversation": cached.conv, "fetchedAt": cached.at})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	conv, err := p.provider.FetchConversation(ctx, target, token)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	renderConversationHTML(&conv)
	now := time.Now()
	p.mu.Lock()
	p.conv = &convCache{conv: conv, at: now}
	p.mu.Unlock()
	writeJSON(w, map[string]any{"conversation": conv, "fetchedAt": now})
}
