package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// inbox.go is the PR inbox (web/src/inbox.js): the open pull requests of one
// GitHub repository -- the workspace's by default, or any the user picks --
// each listed with CI and review state from one GraphQL search, optionally
// narrowed to those waiting on the user's review or the user's own.
// Opening one goes through /api/pr/launch, which starts a child px0 on the PR
// and remembers it, so a second click brings back the same session instead
// of checking the PR out again.

const (
	inboxCacheTTL = 60 * time.Second // GitHub allows 30 searches a minute
	inboxPageSize = 50
	inboxMaxPages = 2
	inboxTokenTTL = 60 * time.Second // `gh auth token` is a process spawn
)

// inboxSections maps a section to its search qualifiers ({repo} is the
// selected owner/name). Review requests list oldest first, so nothing waits
// forever at the bottom.
var inboxSections = map[string]string{
	"all":    "is:open is:pr repo:{repo} sort:updated-desc",
	"review": "is:open is:pr repo:{repo} review-requested:@me sort:created-asc",
	"mine":   "is:open is:pr repo:{repo} author:@me sort:updated-desc",
}

// inboxRepoRe is an owner/name safe to drop into a search query: anything
// else (a space, a colon) could smuggle in qualifiers of its own.
var inboxRepoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

const inboxSearchQuery = `query($q: String!, $cursor: String) {
  search(query: $q, type: ISSUE, first: 50, after: $cursor) {
    issueCount
    pageInfo { hasNextPage endCursor }
    nodes {
      ... on PullRequest {
        number title url isDraft createdAt updatedAt reviewDecision
        author { login }
        repository { nameWithOwner }
        commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }
      }
    }
  }
}`

type gqlSearchData struct {
	Search struct {
		IssueCount int         `json:"issueCount"`
		PageInfo   gqlPageInfo `json:"pageInfo"`
		Nodes      []struct {
			Number         int       `json:"number"`
			Title          string    `json:"title"`
			URL            string    `json:"url"`
			IsDraft        bool      `json:"isDraft"`
			CreatedAt      string    `json:"createdAt"`
			UpdatedAt      string    `json:"updatedAt"`
			ReviewDecision string    `json:"reviewDecision"`
			Author         *gqlActor `json:"author"`
			Repository     *struct {
				NameWithOwner string `json:"nameWithOwner"`
			} `json:"repository"`
			Commits struct {
				Nodes []struct {
					Commit struct {
						StatusCheckRollup *struct {
							State string `json:"state"`
						} `json:"statusCheckRollup"`
					} `json:"commit"`
				} `json:"nodes"`
			} `json:"commits"`
		} `json:"nodes"`
	} `json:"search"`
}

// parseInboxSearch turns one search page into summaries. Search can return
// issues too when a query is loose; anything without a number and URL is
// skipped.
func parseInboxSearch(d gqlSearchData) []PRSummary {
	out := make([]PRSummary, 0, len(d.Search.Nodes))
	for _, n := range d.Search.Nodes {
		if n.Number == 0 || n.URL == "" {
			continue
		}
		s := PRSummary{
			URL: n.URL, Number: n.Number, Title: n.Title, Author: n.Author.login(), Draft: n.IsDraft,
			CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt,
		}
		if n.Repository != nil {
			s.Repo = n.Repository.NameWithOwner
		}
		if c := n.Commits.Nodes; len(c) > 0 && c[0].Commit.StatusCheckRollup != nil {
			switch strings.ToUpper(c[0].Commit.StatusCheckRollup.State) {
			case "SUCCESS":
				s.CI = "pass"
			case "FAILURE", "ERROR":
				s.CI = "fail"
			default:
				s.CI = "pending"
			}
		}
		switch strings.ToUpper(n.ReviewDecision) {
		case "APPROVED":
			s.Review = "approved"
		case "CHANGES_REQUESTED":
			s.Review = "changes_requested"
		}
		out = append(out, s)
	}
	return out
}

// listPRs runs a search across up to inboxMaxPages pages. total is GitHub's
// count, which may exceed what was fetched.
func listPRs(ctx context.Context, token, query string) (items []PRSummary, total int, err error) {
	vars := map[string]any{"q": query, "cursor": nil}
	items = []PRSummary{}
	for page := 0; page < inboxMaxPages; page++ {
		var d gqlSearchData
		if err := githubGraphQL(ctx, token, inboxSearchQuery, vars, &d); err != nil {
			return nil, 0, err
		}
		items = append(items, parseInboxSearch(d)...)
		total = d.Search.IssueCount
		if !d.Search.PageInfo.HasNextPage {
			break
		}
		vars["cursor"] = d.Search.PageInfo.EndCursor
	}
	return items, total, nil
}

func (g *GitHubProvider) ListPRs(ctx context.Context, token, query string) ([]PRSummary, error) {
	items, _, err := listPRs(ctx, token, query)
	return items, err
}

// githubRepoFromRemote reads owner/name from a github.com remote URL in any
// of its usual spellings, or "" for anything else.
var githubRemoteRe = regexp.MustCompile(`^(?:https?://(?:[^@/]+@)?github\.com/|git@github\.com:|ssh://git@github\.com(?::\d+)?/|git://github\.com/)([^/\s]+)/([^/\s]+?)(?:\.git)?/?$`)

func githubRepoFromRemote(remote string) string {
	m := githubRemoteRe.FindStringSubmatch(strings.TrimSpace(remote))
	if m == nil {
		return ""
	}
	return m[1] + "/" + m[2]
}

// inboxRepo is the workspace's GitHub repository: the PR's own in a PR
// session, else the origin remote's.
func (s *Server) inboxRepo() string {
	if s.pr != nil {
		return s.pr.target.Owner + "/" + s.pr.target.Repo
	}
	if s.ix == nil || !gitAvailable(s.ix.Root()) {
		return ""
	}
	out, err := exec.Command("git", "-C", s.ix.Root(), "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	return githubRepoFromRemote(string(out))
}

// ---------------------------------------------------------------- caches

var inboxMu sync.Mutex

type inboxEntry struct {
	items []PRSummary
	total int
	at    time.Time
}

var (
	inboxCache   = map[string]inboxEntry{} // query -> result
	inboxTok     string
	inboxTokAt   time.Time
	inboxTokFrom string
)

// inboxToken is the session's PR token when there is one, else the usual
// four sources, resolved at most once a minute.
func (s *Server) inboxToken() (token, source string) {
	if s.pr != nil && s.pr.token != "" {
		return s.pr.token, "session"
	}
	inboxMu.Lock()
	defer inboxMu.Unlock()
	if time.Since(inboxTokAt) > inboxTokenTTL {
		inboxTok, inboxTokFrom = resolveGitHubToken(readSettings())
		inboxTokAt = time.Now()
	}
	return inboxTok, inboxTokFrom
}

// handleInbox serves one section of one repository: GET
// ?section=all|review|mine&repo=owner/name, &refresh=1 to skip the cache.
// Without repo it is the workspace's; with neither, needsRepo is set.
func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	section := r.URL.Query().Get("section")
	q, ok := inboxSections[section]
	if !ok {
		fail(w, http.StatusBadRequest, "section must be all, review or mine")
		return
	}
	def := s.inboxRepo()
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	if repo == "" {
		repo = def
	}
	resp := map[string]any{"section": section, "items": []PRSummary{}, "defaultRepo": def}
	if repo == "" {
		resp["needsRepo"] = true // no GitHub origin and nothing picked yet
		writeJSON(w, resp)
		return
	}
	if !inboxRepoRe.MatchString(repo) {
		fail(w, http.StatusBadRequest, "repo must be owner/name")
		return
	}
	q = strings.ReplaceAll(q, "{repo}", repo)
	resp["repo"] = repo
	if s.pr != nil {
		resp["current"] = s.pr.target.URL
	}
	token, _ := s.inboxToken()
	if token == "" {
		resp["needsToken"] = true
		writeJSON(w, resp)
		return
	}

	inboxMu.Lock()
	e, hit := inboxCache[q]
	inboxMu.Unlock()
	if !hit || r.URL.Query().Get("refresh") != "" || time.Since(e.at) > inboxCacheTTL {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		items, total, err := listPRs(ctx, token, q)
		cancel()
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		e = inboxEntry{items: items, total: total, at: time.Now()}
		inboxMu.Lock()
		inboxCache[q] = e
		inboxMu.Unlock()
	}
	resp["items"], resp["total"], resp["fetchedAt"] = e.items, e.total, e.at
	writeJSON(w, resp)
}

// ---------------------------------------------------------------- repositories

const inboxReposTTL = 10 * time.Minute

// inboxReposQuery lists the repositories the user can see PRs in, most
// recently pushed first: the choices for the inbox's repository picker.
const inboxReposQuery = `query {
  viewer {
    repositories(first: 100, affiliations: [OWNER, COLLABORATOR, ORGANIZATION_MEMBER], orderBy: {field: PUSHED_AT, direction: DESC}) {
      nodes { nameWithOwner isArchived }
    }
  }
}`

type gqlViewerRepos struct {
	Viewer struct {
		Repositories struct {
			Nodes []struct {
				NameWithOwner string `json:"nameWithOwner"`
				IsArchived    bool   `json:"isArchived"`
			} `json:"nodes"`
		} `json:"repositories"`
	} `json:"viewer"`
}

var (
	inboxRepos   []string
	inboxReposAt time.Time
)

// handleInboxRepos serves the picker's choices: GET, &refresh=1 to skip the
// cache. The workspace's repository is first whether or not GitHub lists it.
func (s *Server) handleInboxRepos(w http.ResponseWriter, r *http.Request) {
	def := s.inboxRepo()
	resp := map[string]any{"repos": []string{}, "defaultRepo": def}
	token, _ := s.inboxToken()
	if token == "" {
		if def != "" {
			resp["repos"] = []string{def}
		}
		resp["needsToken"] = true
		writeJSON(w, resp)
		return
	}
	inboxMu.Lock()
	repos, at := inboxRepos, inboxReposAt
	inboxMu.Unlock()
	if repos == nil || r.URL.Query().Get("refresh") != "" || time.Since(at) > inboxReposTTL {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		var d gqlViewerRepos
		err := githubGraphQL(ctx, token, inboxReposQuery, nil, &d)
		cancel()
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		repos = []string{}
		for _, n := range d.Viewer.Repositories.Nodes {
			if n.NameWithOwner != "" && !n.IsArchived {
				repos = append(repos, n.NameWithOwner)
			}
		}
		inboxMu.Lock()
		inboxRepos, inboxReposAt = repos, time.Now()
		inboxMu.Unlock()
	}
	out := make([]string, 0, len(repos)+1)
	if def != "" {
		out = append(out, def)
	}
	for _, rp := range repos {
		if !strings.EqualFold(rp, def) {
			out = append(out, rp)
		}
	}
	resp["repos"] = out
	writeJSON(w, resp)
}

// ---------------------------------------------------------------- launching

// launchedPR is a child px0 started on a PR from this process.
type launchedPR struct {
	Key    string `json:"key"`
	Target string `json:"target"`
	URL    string `json:"url,omitempty"` // the child's own page, once it is serving
	State  string `json:"state"`         // starting, running, failed
	Error  string `json:"error,omitempty"`
}

var (
	launchMu sync.Mutex
	launched = map[string]*launchedPR{}
	// launchCommand builds the child process; tests swap it for a stand-in.
	launchCommand = func(target, dir string) (*exec.Cmd, error) {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		// -no-open: the browser page that asked opens the child itself, in a
		// named window it can bring back on the next click.
		cmd := exec.Command(exe, "-no-open", target)
		cmd.Dir = dir
		return cmd, nil
	}
)

var (
	ansiRe     = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	childURLRe = regexp.MustCompile(`(?i)\burl\b:?\s+(https?://\S+)`) // main.go prints "url:  http://..."
)

// launchKey identifies a PR however its URL was written.
func launchKey(target string) (string, error) {
	_, t, err := ParsePRURL(target)
	if err != nil {
		return "", err
	}
	return strings.ToLower(fmt.Sprintf("%s/%s/%s/%d", t.Provider, t.Owner, t.Repo, t.Number)), nil
}

// launchPR starts a child px0 on target unless one started from here is
// still alive, and reports it. A failed launch is replaced by a new attempt.
func launchPR(target, dir string) (lp launchedPR, already bool, err error) {
	key, err := launchKey(target)
	if err != nil {
		return launchedPR{}, false, err
	}
	launchMu.Lock()
	defer launchMu.Unlock()
	if cur := launched[key]; cur != nil && cur.State != "failed" {
		return *cur, true, nil
	}
	cmd, err := launchCommand(target, dir)
	if err != nil {
		return launchedPR{}, false, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return launchedPR{}, false, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return launchedPR{}, false, err
	}
	entry := &launchedPR{Key: key, Target: target, State: "starting"}
	launched[key] = entry
	go watchLaunched(entry, cmd, out)
	return *entry, false, nil
}

// watchLaunched reads the child's output for the line naming its URL, keeps
// the tail for an error report, and forgets the child when it exits.
func watchLaunched(entry *launchedPR, cmd *exec.Cmd, out io.Reader) {
	var tail []string
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(ansiRe.ReplaceAllString(sc.Text(), ""))
		if line == "" {
			continue
		}
		if len(tail) == 8 {
			tail = tail[1:]
		}
		tail = append(tail, line)
		if m := childURLRe.FindStringSubmatch(line); m != nil {
			launchMu.Lock()
			if entry.URL == "" {
				entry.URL, entry.State = m[1], "running"
			}
			launchMu.Unlock()
		}
	}
	err := cmd.Wait()
	launchMu.Lock()
	defer launchMu.Unlock()
	if entry.State == "running" {
		// The session ended (Ctrl-C, or its tab's process exited): the next
		// click starts a fresh one.
		if launched[entry.Key] == entry {
			delete(launched, entry.Key)
		}
		return
	}
	entry.State = "failed"
	msg := strings.Join(tail, "\n")
	if msg == "" && err != nil {
		msg = err.Error()
	}
	if msg == "" {
		msg = "px0 exited before it started serving"
	}
	entry.Error = msg
}

func launchStatus(target string) (launchedPR, bool) {
	key, err := launchKey(target)
	if err != nil {
		return launchedPR{}, false
	}
	launchMu.Lock()
	defer launchMu.Unlock()
	if cur := launched[key]; cur != nil {
		return *cur, true
	}
	return launchedPR{}, false
}

var errLaunchSelf = errors.New("this session is already reviewing that pull request")

// handleLaunchPR lets a running px0 open another PR without disturbing its
// own session: POST {target} starts (or finds) a child px0 on it and returns
// its state; GET ?target= polls until the child's URL is known.
func (s *Server) handleLaunchPR(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		lp, ok := launchStatus(r.URL.Query().Get("target"))
		if !ok {
			fail(w, http.StatusNotFound, "not launched from this session")
			return
		}
		writeJSON(w, lp)
		return
	}
	if !localPost(w, r) {
		return
	}
	var body struct {
		Target string `json:"target"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil || strings.TrimSpace(body.Target) == "" {
		fail(w, http.StatusBadRequest, "target is required")
		return
	}
	target := strings.TrimSpace(body.Target)
	if _, _, ok := DetectPRURL(target); !ok {
		fail(w, http.StatusBadRequest, "target must be a valid pull request URL (e.g. https://github.com/owner/repo/pull/123)")
		return
	}
	if s.pr != nil {
		if a, err1 := launchKey(target); err1 == nil {
			if b, err2 := launchKey(s.pr.target.URL); err2 == nil && a == b {
				writeJSON(w, map[string]any{"ok": true, "self": true, "error": errLaunchSelf.Error()})
				return
			}
		}
	}
	// The review checks the PR out of a local clone of its repository: this
	// workspace when it is one, else a saved one (repos.go). Without either,
	// the page asks for the clone and tries again.
	dir := s.ix.Root()
	if _, t, err := ParsePRURL(target); err == nil {
		if _, _, ok := cloneOf(dir, t.Owner, t.Repo); !ok {
			top, _, ok := findLocalClone(t.Owner, t.Repo)
			if !ok {
				repo := t.Owner + "/" + t.Repo
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]any{
					"error":     "no local clone of " + repo + " is saved; add it under Local repositories",
					"needsRepo": repo,
				})
				return
			}
			dir = top
		}
	}
	lp, already, err := launchPR(target, dir)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "already": already, "launch": lp})
}
