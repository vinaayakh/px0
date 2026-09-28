package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// PRTarget identifies a pull request or merge request across any git forge.
type PRTarget struct {
	Provider string // "github", "gitlab", etc.
	Owner    string // namespace/owner
	Repo     string // project/repo name
	Number   int    // PR/MR number
	URL      string // original URL
}

// PRMeta holds the normalized metadata px0 needs to check out a PR,
// compute diffs, and label the review UI.
type PRMeta struct {
	Number           int
	Title            string
	Author           string
	State            string
	Merged           bool
	MergedAt         string
	Draft            bool
	BaseRef          string
	HeadRef          string
	HeadSHA          string
	HeadRepoCloneURL string
	HeadIsFork       bool

	Body               string   // PR description, raw markdown
	Labels             []string // label names
	Assignees          []string // logins
	RequestedReviewers []string // logins, and "org/team" slugs for team requests
	// Mergeable is the forge's merge-readiness state, lower-cased: GitHub's
	// mergeable_state ("clean", "dirty", "blocked", "behind", "unstable",
	// "draft", "unknown"). Empty when the forge has not computed it yet.
	Mergeable string
}

// errProviderUnsupported is returned by a provider method the forge (or px0's
// support for it) does not implement yet.
var errProviderUnsupported = errors.New("not supported by this provider yet")

// PRSummary is one row of the PR inbox (inbox.go): enough to list and launch a
// PR without checking it out.
type PRSummary struct {
	URL       string `json:"url"`
	Repo      string `json:"repo"` // owner/name
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Author    string `json:"author"`
	Draft     bool   `json:"draft"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
	// CI is the head commit's check rollup: "pass", "fail", "pending", or ""
	// when the head has no checks.
	CI string `json:"ci"`
	// Review is the review decision: "approved", "changes_requested", or "".
	Review string `json:"review"`
}

// TimelineItem is one entry of a PR's conversation (conversation.go),
// normalised across event types, in the forge's own order. Kind selects which
// optional fields are set: "comment", "review", "commits" (a run of
// consecutive commits), "force_push", "review_requested", "labeled",
// "unlabeled", "merged", "closed", "reopened", "ready_for_review",
// "convert_to_draft".
type TimelineItem struct {
	Kind       string           `json:"kind"`
	ID         string           `json:"id"`
	DatabaseID int64            `json:"databaseId,omitempty"` // REST id of a comment, for replies
	Author     string           `json:"author,omitempty"`
	AvatarURL  string           `json:"avatarUrl,omitempty"`
	CreatedAt  string           `json:"createdAt"`
	URL        string           `json:"url,omitempty"`
	Body       string           `json:"body,omitempty"`     // raw markdown
	BodyHTML   string           `json:"bodyHtml,omitempty"` // goldmark output; the client sanitises it
	State      string           `json:"state,omitempty"`    // review: approved, changes_requested, commented, dismissed
	SHA        string           `json:"sha,omitempty"`      // force_push (new head), merged (merge commit)
	BeforeSHA  string           `json:"beforeSha,omitempty"`
	Subject    string           `json:"subject,omitempty"` // label name, requested reviewer
	Color      string           `json:"color,omitempty"`   // label colour, hex without #
	Commits    []TimelineCommit `json:"commits,omitempty"`
}

// TimelineCommit is one commit of a "commits" timeline item.
type TimelineCommit struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
	Author  string `json:"author"`
	Date    string `json:"date"`
}

// PRHeader is the top of the Conversation tab.
type PRHeader struct {
	Number             int       `json:"number"`
	Title              string    `json:"title"`
	State              string    `json:"state"` // open, draft, merged, closed
	Author             string    `json:"author"`
	AvatarURL          string    `json:"avatarUrl,omitempty"`
	URL                string    `json:"url"`
	CreatedAt          string    `json:"createdAt"`
	Body               string    `json:"body"`
	BodyHTML           string    `json:"bodyHtml"`
	BaseRef            string    `json:"baseRef"`
	HeadRef            string    `json:"headRef"`
	Labels             []PRLabel `json:"labels"`
	Assignees          []string  `json:"assignees"`
	RequestedReviewers []string  `json:"requestedReviewers"`
	Mergeable          string    `json:"mergeable"`      // mergeable, conflicting, unknown
	ReviewDecision     string    `json:"reviewDecision"` // approved, changes_requested, review_required, ""
	ChecksState        string    `json:"checksState"`    // success, failure, pending, "" (no checks)
}

// PRLabel is a label with its colour (hex, no #).
type PRLabel struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// ReviewThread is an inline review thread. ReviewID is the timeline review
// its first comment belongs to, which is where GitHub shows the thread.
type ReviewThread struct {
	ID           string          `json:"id"`
	Path         string          `json:"path"`
	Line         int             `json:"line,omitempty"` // current line; 0 when outdated
	OriginalLine int             `json:"originalLine,omitempty"`
	StartLine    int             `json:"startLine,omitempty"`
	Side         string          `json:"side"` // LEFT or RIGHT
	Resolved     bool            `json:"resolved"`
	Outdated     bool            `json:"outdated"`
	DiffHunk     string          `json:"diffHunk,omitempty"`
	ReviewID     string          `json:"reviewId,omitempty"`
	Comments     []ThreadComment `json:"comments"`
}

// ThreadComment is one comment in a review thread.
type ThreadComment struct {
	ID         string `json:"id"`
	DatabaseID int64  `json:"databaseId"`
	Author     string `json:"author"`
	AvatarURL  string `json:"avatarUrl,omitempty"`
	Body       string `json:"body"`
	BodyHTML   string `json:"bodyHtml"`
	CreatedAt  string `json:"createdAt"`
	URL        string `json:"url,omitempty"`
}

// PRConversation is everything the Conversation tab shows.
type PRConversation struct {
	Header    PRHeader       `json:"header"`
	Items     []TimelineItem `json:"items"`
	Threads   []ReviewThread `json:"threads"`
	Checks    []PRCheck      `json:"checks"`
	Truncated bool           `json:"truncated,omitempty"` // page limits hit; the tail is missing
}

// PRCheck is one check run on the PR head.
type PRCheck struct {
	Name        string `json:"name"`
	Status      string `json:"status"`     // queued, in_progress, completed
	Conclusion  string `json:"conclusion"` // success, failure, neutral, cancelled, skipped, timed_out, action_required, ""
	StartedAt   string `json:"startedAt,omitempty"`
	CompletedAt string `json:"completedAt,omitempty"`
	URL         string `json:"url,omitempty"`
}

// PRComment is a comment already posted on the pull request, fetched
// read-only from the forge -- distinct from pr.go's prComment, which is a
// draft held in memory until a review is submitted. Kind is "issue" (a
// top-level PR conversation comment) or "review" (anchored to a diff line).
type PRComment struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Path      string `json:"path,omitempty"`
	Line      int    `json:"line,omitempty"`
	Side      string `json:"side,omitempty"`
	InReplyTo int64  `json:"inReplyTo,omitempty"`
	Author    string `json:"author"`
	AvatarURL string `json:"avatarUrl,omitempty"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
	URL       string `json:"url"`
}

// GitProvider abstracts forge-specific operations (GitHub, GitLab, etc.)
// for pull/merge request reviews.
type GitProvider interface {
	// Name returns the provider name (e.g. "github", "gitlab").
	Name() string

	// MatchURL reports whether this provider recognizes and handles the given URL.
	MatchURL(rawURL string) bool

	// ParseURL extracts the PR target from the URL.
	ParseURL(rawURL string) (PRTarget, error)

	// ResolveToken looks for an auth token across settings, env vars, and CLI tools.
	// An empty return means the session stays read-only.
	ResolveToken(cfg settings) (token, source string)

	// FetchPR fetches pull/merge request metadata from the forge API.
	FetchPR(ctx context.Context, target PRTarget, token string) (PRMeta, error)

	// CheckPushAccess reports whether the authenticated user has push access to the repository.
	CheckPushAccess(ctx context.Context, target PRTarget, token string) bool

	// SubmitReview posts draft comments and the overall review verdict back to the forge.
	SubmitReview(ctx context.Context, target PRTarget, token, headSHA string, comments []prComment, event, body string) error

	// FetchComments returns every comment already posted on the PR: top-level
	// ("issue") comments and inline ("review") comments anchored to a diff line.
	FetchComments(ctx context.Context, target PRTarget, token string) (issue, review []PRComment, err error)

	// PostIssueComment posts a new top-level PR comment immediately. GitHub has
	// no threading for these, so "replying" to one is just posting a new one.
	PostIssueComment(ctx context.Context, target PRTarget, token, body string) (PRComment, error)

	// ReplyToReviewComment posts an immediate, threaded reply to an existing
	// inline review comment.
	ReplyToReviewComment(ctx context.Context, target PRTarget, token string, commentID int64, body string) (PRComment, error)

	// ListPRs runs a forge search (e.g. "is:open is:pr review-requested:@me")
	// and returns one summary per PR, for the inbox.
	ListPRs(ctx context.Context, token, query string) ([]PRSummary, error)

	// FetchConversation returns the PR's timeline and the head's checks, for
	// the Conversation tab.
	FetchConversation(ctx context.Context, target PRTarget, token string) (PRConversation, error)
}

var defaultProviders = []GitProvider{
	&GitHubProvider{},
}

// RegisterProvider registers a custom or additional GitProvider.
func RegisterProvider(p GitProvider) {
	defaultProviders = append(defaultProviders, p)
}

// DetectPRURL checks if rawURL is a recognized pull request URL for any supported provider.
// If matched and valid, returns the matching provider, parsed target, and true.
func DetectPRURL(rawURL string) (GitProvider, PRTarget, bool) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, PRTarget{}, false
	}
	for _, p := range defaultProviders {
		if p.MatchURL(rawURL) {
			target, err := p.ParseURL(rawURL)
			if err == nil {
				return p, target, true
			}
		}
	}
	return nil, PRTarget{}, false
}

// ParsePRURL attempts to parse a PR URL, returning an error if no provider matches
// or if the URL format is invalid.
func ParsePRURL(rawURL string) (GitProvider, PRTarget, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, PRTarget{}, fmt.Errorf("empty PR URL")
	}
	for _, p := range defaultProviders {
		if p.MatchURL(rawURL) {
			target, err := p.ParseURL(rawURL)
			if err != nil {
				return nil, PRTarget{}, err
			}
			return p, target, nil
		}
	}
	return nil, PRTarget{}, fmt.Errorf("unsupported or unrecognized PR URL: %q (expected full GitHub URL like https://github.com/owner/repo/pull/123)", rawURL)
}
