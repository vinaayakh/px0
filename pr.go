package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// pr.go handles git forge pull/merge request reviews: checking out a PR's
// source tree into a throwaway git worktree, diffing it against the merge-base
// with its target branch instead of HEAD, and letting the reviewer leave draft
// comments and submit reviews or batch apply them with AI agents.
//
// Nothing persists past the review. The worktree lives in a system temp dir
// and is removed in prSession.Close, when the review ends (End review or
// Ctrl-C); prcleanup.go sweeps up after a process that never got there.

// prSession is one checked-out PR review. Draft comments live only in
// memory (mu-guarded), same lifetime as an agentJob -- never written to
// disk, never surviving a restart.
type prSession struct {
	mu sync.Mutex

	provider        GitProvider
	target          PRTarget
	meta            PRMeta
	token           string
	writeAccess     bool
	diffBase        string // merge-base(head, base branch), or "HEAD" if the base couldn't be resolved
	diffBaseWarning string // set when diffBase fell back to "HEAD"; surfaced in the UI so an empty diff doesn't read as "no changes"

	worktree    string // temp checkout, removed in Close
	srcRepo     string // the repo the worktree was registered against ("" for a plain clone)
	srcRemote   string // srcRepo's remote that is the PR's repository: origin, or upstream in a fork's clone
	sessionFile string // UI session state kept for this checkout only, removed in Close

	comments []prComment
	nextID   int64

	review *reviewState // AI review runs (review.go)
	// remoteHead is the PR head GitHub reported at the last submit, when it
	// differs from the checkout's: the PR moved on without a Pull here.
	remoteHead string
	conv       *convCache // last Conversation tab fetch (conversation.go)
	warm       *prWarmSet // first requests, fetched alongside the checkout (prwarm.go)
}

// ErrPRMergedCancelled is returned when opening an already-merged PR is cancelled.
var ErrPRMergedCancelled = errors.New("PR is already merged; opening cancelled")

// fetchBaseRef fetches the PR's base branch to refs/px0/base/N in dir (the
// clone the worktree belongs to, which shares its refs, or the checkout
// itself without a clone) from remote, "origin" when empty. checkoutPR runs
// it alongside the head fetch, so it leaves FETCH_HEAD alone. It returns
// git's complaint, "" on success.
func fetchBaseRef(dir, remote, baseRef string, num int) string {
	if remote == "" {
		remote = "origin"
	}
	baseRefspec := fmt.Sprintf("refs/heads/%s:refs/px0/base/%d", baseRef, num)
	out, err := exec.Command("git", "-C", dir, "fetch", "--no-tags", "--no-write-fetch-head", remote, baseRefspec).CombinedOutput()
	if err == nil {
		return ""
	}
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return msg
	}
	return err.Error()
}

// fetchPRHead fetches the PR's head to refs/px0/pr/N in dir from remote,
// "origin" when empty. GitHub keeps it at refs/pull/N/head on the PR's own
// repository, whether or not the fork's branch still exists.
func fetchPRHead(dir, remote string, num int) error {
	if remote == "" {
		remote = "origin"
	}
	refspec := fmt.Sprintf("refs/pull/%d/head:refs/px0/pr/%d", num, num)
	out, err := exec.Command("git", "-C", dir, "fetch", "--no-tags", "--no-write-fetch-head", remote, refspec).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git fetch PR head: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// prRepoURL is the clone URL of the PR's own repository, where its
// refs/pull/N/head lives. A variable so tests can point it at a local one.
var prRepoURL = func(target PRTarget) string {
	return fmt.Sprintf("https://github.com/%s/%s.git", target.Owner, target.Repo)
}

// initPartialRepo makes dir an empty repository whose origin is url, set up
// as `git clone --filter=blob:none` would: a fetch brings commits and trees,
// and file contents come when they are checked out. The config is written
// here rather than by the first filtered fetch because the head and base
// fetches run at the same time and would both try to write it.
func initPartialRepo(dir, url string) error {
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		return fmt.Errorf("git init: %w: %s", err, strings.TrimSpace(string(out)))
	}
	f, err := os.OpenFile(filepath.Join(dir, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	// One write instead of a git config process per key. git honours
	// extensions.partialClone in a version 0 repository.
	_, err = fmt.Fprintf(f, "[extensions]\n\tpartialClone = origin\n[remote \"origin\"]\n\turl = %s\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n\tpromisor = true\n\tpartialclonefilter = blob:none\n", url)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// newPRCheckoutDir makes the empty temp directory a review is checked out
// into and marks it at once, not when the checkout is done: a px0 killed
// midway leaves a checkout the next start can tell is dead, and can
// unregister from the clone, instead of one it must wait an hour on.
func newPRCheckoutDir(srcRepo string, num int) (string, error) {
	tmp, err := os.MkdirTemp("", prCheckoutPrefix+"*")
	if err != nil {
		return "", err
	}
	// macOS TempDir lives under /var -> /private/var; git rev-parse
	// --show-toplevel reports the resolved path, so leaving tmp unresolved
	// makes gitStatusAgainst's toplevel-relative prefix check fail for every
	// file, silently emptying the PR's diff/status view.
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = resolved
	}
	(&prSession{worktree: tmp, srcRepo: srcRepo, meta: PRMeta{Number: num}}).writeMarker()
	return tmp, nil
}

// resolveDiffBase is the merge-base of the worktree's HEAD with the fetched
// base (fetchErr is fetchBaseRef's result), so review diffs show exactly
// what the PR changes. It falls back to the clone's own remote-tracking
// branch, then to "HEAD" -- which diffs the checkout against itself and
// looks empty -- with a warning saying why, so callers can surface it and a
// blank diff never reads as "no changes". Shared by checkoutPR and
// prSession.Pull.
func resolveDiffBase(worktree, srcRepo, srcRemote, baseRef string, num int, fetchErr string, onProgress func(string)) (diffBase, diffBaseWarning string) {
	if srcRemote == "" {
		srcRemote = "origin"
	}
	diffBase = "HEAD"
	if fetchErr == "" {
		if mb := gitMergeBase(worktree, "HEAD", fmt.Sprintf("refs/px0/base/%d", num)); mb != "" {
			diffBase = mb
		}
	}
	if diffBase == "HEAD" && srcRepo != "" {
		if mb := gitMergeBase(worktree, "HEAD", srcRemote+"/"+baseRef); mb != "" {
			diffBase = mb
			fetchErr = "" // recovered via the local clone's own remote-tracking ref
		}
	}
	if diffBase == "HEAD" {
		diffBaseWarning = fmt.Sprintf("could not resolve a merge-base with %s; diff will show no changes", baseRef)
		if fetchErr != "" {
			diffBaseWarning = fmt.Sprintf("%s (%s)", diffBaseWarning, fetchErr)
		}
		fmt.Fprintln(os.Stderr, "px0: warning:", diffBaseWarning)
		if onProgress != nil {
			onProgress("Warning: " + diffBaseWarning)
		}
	}
	return diffBase, diffBaseWarning
}

// checkoutPR fetches a PR's head ref and checks it out into a system temp
// directory: a git worktree of cwd when it is a clone of the PR's
// repository (the common case -- opened inside the repo), else of a saved
// clone of it (repos.go), else a blobless repository of its own fetched
// from the PR's repository (a bare URL opened from an unrelated directory).
//
// It is almost all network round trips, so nothing waits on what it does
// not need: the head fetch needs neither the token nor the metadata and
// starts first, the page's first requests and the push check start once the
// token is known, and the base branch is fetched as soon as the metadata
// names it, while the head is still on its way.
func checkoutPR(ctx context.Context, provider GitProvider, target PRTarget, cwd string, onProgress func(string)) (*prSession, error) {
	num := target.Number
	tokenCh := make(chan string, 1)
	go func() {
		token, _ := provider.ResolveToken(readSettings())
		tokenCh <- token
	}()

	srcRepo, srcRemote := "", ""
	if target.Owner != "" && target.Repo != "" {
		if top, rem, ok := cloneOf(cwd, target.Owner, target.Repo); ok {
			srcRepo, srcRemote = top, rem
		} else if top, rem, ok := findLocalClone(target.Owner, target.Repo); ok {
			srcRepo, srcRemote = top, rem
		}
	}

	// gitDir is where the head and base are fetched to: the clone, or
	// without one the checkout itself, which then exists from the start.
	gitDir, tmp := srcRepo, ""
	if srcRepo == "" {
		var err error
		if tmp, err = newPRCheckoutDir("", num); err != nil {
			return nil, err
		}
		if err := initPartialRepo(tmp, prRepoURL(target)); err != nil {
			removePRCheckout("", tmp, 0, "")
			return nil, err
		}
		gitDir = tmp
	}
	headFetched := make(chan error, 1)
	go func() { headFetched <- fetchPRHead(gitDir, srcRemote, num) }()

	token := <-tokenCh
	warm := startPRWarm(provider, target, token)
	access := make(chan bool, 1)
	go func() { access <- provider.CheckPushAccess(ctx, target, token) }()

	if onProgress != nil {
		if srcRepo != "" {
			onProgress(fmt.Sprintf("Fetching PR #%d from %s into %s...", num, provider.Name(), srcRepo))
		} else {
			onProgress(fmt.Sprintf("Fetching PR #%d from %s...", num, provider.Name()))
		}
	}
	// giveUp removes what the checkout made once the head fetch is done.
	// dropHead also removes the head ref from the clone; only before the
	// head fetch is waited on, as later another review of the PR may share
	// it (the sweep sorts that out).
	giveUp := func(dropHead bool) {
		if headFetched != nil {
			<-headFetched
		}
		if dropHead && srcRepo != "" {
			exec.Command("git", "-C", srcRepo, "update-ref", "-d", fmt.Sprintf("refs/px0/pr/%d", num)).Run()
		}
		removePRCheckout(srcRepo, tmp, 0, "")
	}
	meta, err := provider.FetchPR(ctx, target, token)
	if err != nil {
		giveUp(true)
		return nil, err
	}
	baseFetched := make(chan string, 1)
	go func() { baseFetched <- fetchBaseRef(gitDir, srcRemote, meta.BaseRef, num) }()

	if srcRepo != "" {
		if tmp, err = newPRCheckoutDir(srcRepo, num); err != nil {
			<-baseFetched
			giveUp(true)
			return nil, err
		}
	}
	err = <-headFetched
	headFetched = nil
	if err != nil {
		<-baseFetched
		giveUp(false)
		return nil, err
	}
	if onProgress != nil {
		onProgress(fmt.Sprintf("Checking out PR #%d and fetching %s...", num, meta.BaseRef))
	}
	prRef := fmt.Sprintf("refs/px0/pr/%d", num)
	checkout := exec.Command("git", "-C", srcRepo, "-c", parallelCheckout, "worktree", "add", "--detach", tmp, prRef)
	if srcRepo == "" {
		// Downloads the files of this one commit, in one request.
		checkout = exec.Command("git", "-C", tmp, "-c", parallelCheckout, "checkout", "-q", "--detach", prRef)
	}
	if out, err := checkout.CombinedOutput(); err != nil {
		<-baseFetched
		giveUp(false)
		return nil, fmt.Errorf("git checkout PR head: %w: %s", err, strings.TrimSpace(string(out)))
	}
	diffBase, diffBaseWarning := resolveDiffBase(tmp, srcRepo, srcRemote, meta.BaseRef, num, <-baseFetched, onProgress)

	p := &prSession{
		provider:        provider,
		target:          target,
		meta:            meta,
		token:           token,
		writeAccess:     <-access,
		diffBase:        diffBase,
		diffBaseWarning: diffBaseWarning,
		worktree:        tmp,
		srcRepo:         srcRepo,
		srcRemote:       srcRemote,
		warm:            warm,
	}
	p.writeMarker()
	return p, nil
}

func (p *prSession) Root() string { return p.worktree }

// Close ends the review on disk: it removes the worktree and its
// registration, the refs px0 fetched, the session file and the marker
// (removePRCheckout). Safe on a nil receiver and safe to call twice.
func (p *prSession) Close() {
	if p == nil {
		return
	}
	removePRCheckout(p.srcRepo, p.worktree, p.meta.Number, p.sessionFile)
}

// errPRDiverged is returned by Pull when the checkout can't be fast-forwarded
// onto the PR's current head -- local commits, or a force-pushed head, that
// don't share a straight-line history with what was fetched. Resolving that
// is not supported: Pull never invokes git's merge machinery, only a reset
// --hard onto an ancestor-verified fast-forward.
var errPRDiverged = errors.New("local checkout has diverged from the PR head; resolve manually")

// Pull re-fetches the PR's current head and, if it's a clean fast-forward,
// resets the worktree onto it and refreshes meta.HeadSHA/diffBase to match.
// Refuses outright when the worktree has uncommitted changes (nothing here
// stashes) or when the fast-forward check fails, in which case the caller
// should surface errPRDiverged as "not supported, resolve manually".
func (p *prSession) Pull() (info string, err error) {
	p.mu.Lock()
	worktree, srcRepo, srcRemote, num := p.worktree, p.srcRepo, p.srcRemote, p.meta.Number
	baseRef := p.meta.BaseRef
	p.mu.Unlock()

	if gitHasUncommittedChanges(worktree) {
		return "", errors.New("commit or discard your local changes before pulling")
	}

	// The worktree shares its refs and remotes with the clone it belongs to,
	// if any, so both fetch into it. The base is fetched alongside the head.
	baseFetched := make(chan string, 1)
	go func() { baseFetched <- fetchBaseRef(worktree, srcRemote, baseRef, num) }()
	if err := fetchPRHead(worktree, srcRemote, num); err != nil {
		<-baseFetched
		return "", err
	}
	newRef := fmt.Sprintf("refs/px0/pr/%d", num)
	if exec.Command("git", "-C", worktree, "merge-base", "--is-ancestor", newRef, "HEAD").Run() == nil {
		<-baseFetched
		return "already up to date", nil
	}
	if exec.Command("git", "-C", worktree, "merge-base", "--is-ancestor", "HEAD", newRef).Run() != nil {
		<-baseFetched
		return "", errPRDiverged
	}
	if out, err := exec.Command("git", "-C", worktree, "reset", "--hard", newRef).CombinedOutput(); err != nil {
		<-baseFetched
		return "", fmt.Errorf("git reset: %w: %s", err, strings.TrimSpace(string(out)))
	}

	shaOut, err := exec.Command("git", "-C", worktree, "rev-parse", "HEAD").Output()
	if err != nil {
		<-baseFetched
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	diffBase, diffBaseWarning := resolveDiffBase(worktree, srcRepo, srcRemote, baseRef, num, <-baseFetched, nil)

	p.mu.Lock()
	p.meta.HeadSHA = strings.TrimSpace(string(shaOut))
	p.diffBase = diffBase
	p.diffBaseWarning = diffBaseWarning
	if p.remoteHead == p.meta.HeadSHA {
		p.remoteHead = "" // caught up with what GitHub reported
	}
	p.mu.Unlock()

	return "pulled the latest changes", nil
}

// Push pushes the worktree's current commit to the PR's actual head branch
// on its head repo (which may be a fork). Never force: a rejection means the
// head moved since this checkout or since the last Pull, and the caller
// should Pull before trying again.
func (p *prSession) Push() error {
	p.mu.Lock()
	worktree := p.worktree
	cloneURL, headRef := p.meta.HeadRepoCloneURL, p.meta.HeadRef
	target := p.target
	p.mu.Unlock()

	if cloneURL == "" {
		cloneURL = fmt.Sprintf("https://github.com/%s/%s.git", target.Owner, target.Repo)
	}
	refspec := fmt.Sprintf("HEAD:refs/heads/%s", headRef)
	out, err := exec.Command("git", "-C", worktree, "push", cloneURL, refspec).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git push: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ---------------------------------------------------------------- HTTP

func (s *Server) prOrFail(w http.ResponseWriter) bool {
	if s.pr == nil {
		fail(w, http.StatusNotFound, "not a PR review session")
		return false
	}
	return true
}

func (s *Server) handlePRMeta(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) {
		return
	}
	p := s.pr
	p.mu.Lock()
	defer p.mu.Unlock()
	writeJSON(w, map[string]any{
		"number":             p.meta.Number,
		"title":              p.meta.Title,
		"author":             p.meta.Author,
		"base":               p.meta.BaseRef,
		"head":               p.meta.HeadRef,
		"state":              p.meta.State,
		"merged":             p.meta.Merged,
		"mergedAt":           p.meta.MergedAt,
		"writeAccess":        p.writeAccess,
		"readOnly":           p.token == "",
		"draftCount":         countSubmittable(p.comments),
		"diffBaseWarning":    p.diffBaseWarning,
		"headSHA":            p.meta.HeadSHA,
		"url":                p.target.URL,
		"draft":              p.meta.Draft,
		"body":               p.meta.Body,
		"labels":             nonNilStrings(p.meta.Labels),
		"assignees":          nonNilStrings(p.meta.Assignees),
		"requestedReviewers": nonNilStrings(p.meta.RequestedReviewers),
		"mergeable":          p.meta.Mergeable,
	})
}

func countSubmittable(cs []prComment) int {
	n := 0
	for _, c := range cs {
		if c.submittable() {
			n++
		}
	}
	return n
}

// submittableDrafts is what the next review sends, and what the session file
// keeps across a restart: human drafts plus accepted or edited suggestions.
func submittableDrafts(cs []prComment) []prComment {
	var out []prComment
	for _, c := range cs {
		if c.submittable() {
			out = append(out, c)
		}
	}
	return out
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// handlePRExistingComments fetches every comment already posted on the PR
// (top-level and inline) directly from the forge -- always live, never
// cached, since another reviewer may have commented since the page loaded.
func (s *Server) handlePRExistingComments(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) {
		return
	}
	p := s.pr
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var issue, review []PRComment
	if got, _, ok := p.takeWarmComments().take(ctx); ok {
		issue, review = got.issue, got.review
	} else {
		var err error
		issue, review, err = p.provider.FetchComments(ctx, p.target, p.token)
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	if issue == nil {
		issue = []PRComment{}
	}
	if review == nil {
		review = []PRComment{}
	}
	writeJSON(w, map[string]any{"issueComments": issue, "reviewComments": review})
}

// handlePRIssueCommentPost posts a new top-level PR comment immediately (not
// part of the draft-then-submit review flow below, since GitHub's issue
// comments aren't tied to a review). Used both for starting a new top-level
// comment and for "replying" to one, since GitHub doesn't thread these.
func (s *Server) handlePRIssueCommentPost(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) {
		return
	}
	if !localPost(w, r) {
		return
	}
	p := s.pr
	if p.token == "" {
		fail(w, http.StatusForbidden, "no auth token configured; posting comments requires a GitHub token")
		return
	}
	var body struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || strings.TrimSpace(body.Body) == "" {
		fail(w, http.StatusBadRequest, "body is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	c, err := p.provider.PostIssueComment(ctx, p.target, p.token, strings.TrimSpace(body.Body))
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, c)
}

// handlePRReviewCommentReply posts an immediate, threaded reply to an
// existing inline review comment (not a new draft: this bypasses the
// draft-then-submit review flow, matching GitHub's own dedicated reply
// endpoint, which posts right away).
func (s *Server) handlePRReviewCommentReply(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) {
		return
	}
	if !localPost(w, r) {
		return
	}
	p := s.pr
	if p.token == "" {
		fail(w, http.StatusForbidden, "no auth token configured; posting comments requires a GitHub token")
		return
	}
	var body struct {
		CommentID int64  `json:"commentId"`
		Body      string `json:"body"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || body.CommentID <= 0 || strings.TrimSpace(body.Body) == "" {
		fail(w, http.StatusBadRequest, "commentId and body are required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	c, err := p.provider.ReplyToReviewComment(ctx, p.target, p.token, body.CommentID, strings.TrimSpace(body.Body))
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, c)
}

func (s *Server) handlePRComments(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) {
		return
	}
	p := s.pr
	switch r.Method {
	case http.MethodGet:
		p.mu.Lock()
		defer p.mu.Unlock()
		comments := p.comments
		if comments == nil {
			comments = []prComment{}
		}
		writeJSON(w, map[string]any{"comments": comments})
	case http.MethodPost:
		if !localPost(w, r) {
			return
		}
		var body struct {
			Path      string `json:"path"`
			Line      int    `json:"line"`
			StartLine int    `json:"startLine"` // first line of a range, same side
			Side      string `json:"side"`
			Body      string `json:"body"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil ||
			body.Path == "" || body.Line <= 0 || strings.TrimSpace(body.Body) == "" {
			fail(w, http.StatusBadRequest, "path, line, and body are required")
			return
		}
		side := strings.ToUpper(body.Side)
		if side != "LEFT" {
			side = "RIGHT"
		}
		p.mu.Lock()
		p.nextID++
		c := prComment{ID: p.nextID, Path: body.Path, Line: body.Line, Side: side, Body: strings.TrimSpace(body.Body),
			Origin: prOriginHuman, Status: prStatusAccepted}
		if body.StartLine > 0 && body.StartLine < body.Line {
			c.StartLine = body.StartLine
		}
		p.comments = append(p.comments, c)
		if s.session != nil {
			s.persistDrafts(p)
		}
		p.mu.Unlock()
		writeJSON(w, c)
	default:
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handlePRCommentDelete(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) {
		return
	}
	if !localPost(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	all := r.URL.Query().Get("all") != ""
	p := s.pr
	p.mu.Lock()
	defer p.mu.Unlock()
	if all {
		// Every draft goes: the reviewer's own are dropped, accepted AI
		// suggestions are dismissed (so a re-run does not offer them again,
		// and AI Review can still restore them). Pending ones are not drafts.
		kept := p.comments[:0]
		for _, c := range p.comments {
			if !c.submittable() {
				kept = append(kept, c)
			} else if c.Origin == prOriginAI {
				c.Status = prStatusDismissed
				kept = append(kept, c)
			}
		}
		p.comments = kept
	}
	for i, c := range p.comments {
		if !all && c.ID == id {
			p.comments = append(p.comments[:i], p.comments[i+1:]...)
			break
		}
	}
	if s.session != nil {
		s.persistDrafts(p)
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handlePRSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) {
		return
	}
	if !localPost(w, r) {
		return
	}
	p := s.pr
	if p.token == "" {
		fail(w, http.StatusForbidden, "no auth token configured; review submission is read-only")
		return
	}
	var body struct {
		Event string `json:"event"`
		Body  string `json:"body"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	event := strings.ToUpper(body.Event)
	if event != "APPROVE" && event != "REQUEST_CHANGES" && event != "COMMENT" {
		fail(w, http.StatusBadRequest, "event must be APPROVE, REQUEST_CHANGES, or COMMENT")
		return
	}
	if (event == "APPROVE" || event == "REQUEST_CHANGES") && !p.writeAccess {
		fail(w, http.StatusForbidden, "no push access on this repository; only Comment reviews are allowed")
		return
	}
	// Has the PR moved on GitHub since this checkout? The review still goes
	// against the commit reviewed here (GitHub accepts that, and shows the
	// comments as on an older commit), but pending AI suggestions become
	// stale until the reviewer pulls and re-checks them.
	headMoved := false
	hctx, hcancel := context.WithTimeout(r.Context(), 10*time.Second)
	if m, err := p.provider.FetchPR(hctx, p.target, p.token); err == nil && m.HeadSHA != "" {
		p.mu.Lock()
		p.remoteHead = ""
		if m.HeadSHA != p.meta.HeadSHA {
			p.remoteHead, headMoved = m.HeadSHA, true
		}
		p.mu.Unlock()
	}
	hcancel()

	// Only human drafts and AI suggestions the reviewer accepted or edited are
	// sent. Pending suggestions stay for the next review; dismissed ones stay in
	// memory so a re-run can recognise them.
	p.mu.Lock()
	comments := submittableDrafts(p.comments)
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := p.provider.SubmitReview(ctx, p.target, p.token, p.meta.HeadSHA, comments, event, body.Body); err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	p.mu.Lock()
	sent := make(map[int64]bool, len(comments))
	for _, c := range comments {
		sent[c.ID] = true
	}
	var remaining []prComment
	for _, c := range p.comments {
		if !sent[c.ID] {
			remaining = append(remaining, c)
		}
	}
	p.comments = remaining
	s.persistDrafts(p)
	p.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true, "headMoved": headMoved})
}

// handleLaunchPR (opening another PR in a child px0) is in inbox.go, next to
// the registry that lets a second click reuse the child.

// parallelCheckout has git write the checkout's files on one worker per
// core (0 = as many as there are cores), which cuts writing a worktree by
// about a third, most of all on Windows. Older git ignores the setting.
const parallelCheckout = "checkout.workers=0"
