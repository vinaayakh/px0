package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// prfiles.go backs the PR "Files changed" tab (web/src/prfiles.js): the whole
// PR diff (diffBase..prHeadSHA, the change a review is posted against) split
// per file and highlighted, plus the comment actions GitHub's own page has
// beside the composer -- posting one comment right away, and editing a
// draft in place.

const (
	// A file whose diff is bigger than this comes without hunks; the tab
	// loads it on request (?path=), like GitHub's "Load diff".
	prFileMaxBytes = 256 << 10
	prFileMaxRows  = 3000
)

// PRFile is one changed file of the PR.
type PRFile struct {
	Path      string     `json:"path"`
	OldPath   string     `json:"oldPath,omitempty"` // set when renamed
	Status    string     `json:"status"`            // added, deleted, renamed, modified
	Binary    bool       `json:"binary,omitempty"`
	Additions int        `json:"additions"`
	Deletions int        `json:"deletions"`
	Hunks     []DiffHunk `json:"hunks"`
	TooLarge  bool       `json:"tooLarge,omitempty"` // hunks left out; ask for this path alone
}

// splitDiffFiles cuts a multi-file git diff at each "diff --git" header.
func splitDiffFiles(diff string) []string {
	var blocks []string
	start := -1
	for i := 0; i < len(diff); {
		end := strings.IndexByte(diff[i:], '\n')
		next := len(diff)
		if end >= 0 {
			next = i + end + 1
		}
		if strings.HasPrefix(diff[i:], "diff --git ") {
			if start >= 0 {
				blocks = append(blocks, diff[start:i])
			}
			start = i
		}
		i = next
	}
	if start >= 0 {
		blocks = append(blocks, diff[start:])
	}
	return blocks
}

// buildPRFile turns one file's diff block into a PRFile; full keeps the
// hunks whatever the size.
func buildPRFile(block string, full bool) (PRFile, bool) {
	pd := parseUnifiedDiff(block)
	var df *diffFile
	for _, f := range pd.Files {
		df = f
	}
	if df == nil {
		return PRFile{}, false
	}
	f := PRFile{Path: df.Path, Status: df.Status, Binary: df.Binary, Hunks: []DiffHunk{}}
	if df.OldPath != df.Path {
		f.OldPath = df.OldPath
	}
	rows := 0
	for _, line := range strings.Split(block, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ "), strings.HasPrefix(line, "--- "):
		case strings.HasPrefix(line, "+"):
			f.Additions++
			rows++
		case strings.HasPrefix(line, "-"):
			f.Deletions++
			rows++
		case strings.HasPrefix(line, " "):
			rows++
		}
	}
	if !full && (len(block) > prFileMaxBytes || rows > prFileMaxRows) {
		f.TooLarge = true
		return f, true
	}
	if !f.Binary {
		f.Hunks = highlightDiff(f.Path, block)
	}
	return f, true
}

// gitDiffRange is the two-commit diff of the whole tree, renames detected,
// paths unquoted, capped like the AI review's.
func gitDiffRange(root, from, to string, paths ...string) (diff string, truncated bool, err error) {
	args := []string{"-C", root, "--literal-pathspecs", "-c", "core.quotepath=false", "diff", "--no-color", "--no-ext-diff", "-M", from, to}
	if len(paths) > 0 {
		args = append(append(args, "--"), paths...)
	}
	cmd := exec.Command("git", args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", false, err
	}
	if err := cmd.Start(); err != nil {
		return "", false, err
	}
	b, rerr := io.ReadAll(io.LimitReader(out, reviewMaxDiffBytes+1))
	io.Copy(io.Discard, out)
	werr := cmd.Wait()
	if rerr != nil {
		return "", false, rerr
	}
	if werr != nil {
		return "", false, fmt.Errorf("git diff %s %s: %w", from, to, werr)
	}
	if len(b) > reviewMaxDiffBytes {
		// Drop the file cut off mid-way rather than show half of it.
		b = b[:reviewMaxDiffBytes]
		if i := strings.LastIndex(string(b), "\ndiff --git "); i >= 0 {
			b = b[:i+1]
		}
		truncated = true
	}
	return string(b), truncated, nil
}

// handlePRFiles serves the PR's changed files: GET for all of them (large
// ones without hunks), or ?path= (the new path, or the old one of a rename)
// for one file in full.
func (s *Server) handlePRFiles(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) {
		return
	}
	base, head := s.diffBase, s.prHeadSHA
	if base == "" || head == "" {
		fail(w, http.StatusConflict, "the PR diff base is not known yet")
		return
	}
	want := r.URL.Query().Get("path")
	var paths []string
	if want != "" {
		if _, rel, ok := s.safePath(want); !ok || rel != want {
			fail(w, http.StatusBadRequest, "bad path")
			return
		}
		paths = []string{want}
	}
	diff, truncated, err := gitDiffRange(s.ix.Root(), base, head, paths...)
	if want != "" && err == nil && diff == "" {
		// A renamed file's diff needs both of its paths: ask for the whole
		// diff and pick it out.
		diff, truncated, err = gitDiffRange(s.ix.Root(), base, head)
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	files := []PRFile{}
	for _, block := range splitDiffFiles(diff) {
		f, ok := buildPRFile(block, want != "")
		if !ok {
			continue
		}
		if want != "" && f.Path != want && f.OldPath != want {
			continue
		}
		files = append(files, f)
	}
	if want != "" && len(files) == 0 {
		fail(w, http.StatusNotFound, "the PR does not change "+want)
		return
	}
	writeJSON(w, map[string]any{"files": files, "base": base, "head": head, "truncated": truncated})
}

// ---------------------------------------------------------------- comments

// handlePRCommentPostNow posts one comment to the PR right away, as a
// single-comment review (what GitHub's "Add single comment" does), instead
// of keeping it for the next review. Either id names a draft or AI
// suggestion (body, when set, replaces its text), or path/line/side/body
// describe a new comment. A posted draft leaves the list; a posted AI
// suggestion stays as "posted", so a re-run does not offer it again.
func (s *Server) handlePRCommentPostNow(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) || !localPost(w, r) {
		return
	}
	p := s.pr
	if p.token == "" {
		fail(w, http.StatusForbidden, "no auth token configured; comments cannot be posted")
		return
	}
	var body struct {
		ID        int64  `json:"id"`
		Path      string `json:"path"`
		Line      int    `json:"line"`
		StartLine int    `json:"startLine"`
		Side      string `json:"side"`
		Body      string `json:"body"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Body)
	var c prComment
	p.mu.Lock()
	if body.ID != 0 {
		found := false
		for _, x := range p.comments {
			if x.ID == body.ID {
				c, found = x, true
				break
			}
		}
		if !found {
			p.mu.Unlock()
			fail(w, http.StatusNotFound, "no draft or suggestion with that id")
			return
		}
		if c.Status == prStatusDismissed || c.Status == prStatusPosted {
			p.mu.Unlock()
			fail(w, http.StatusConflict, "that suggestion was already "+c.Status)
			return
		}
		if c.Origin == prOriginAI && c.Status == prStatusPending && p.isStale(c) {
			p.mu.Unlock()
			fail(w, http.StatusConflict, "the PR head moved since this suggestion was made; re-check suggestions against the new head first")
			return
		}
		if text != "" && text != c.Body {
			if c.AI != nil && c.AI.OriginalBody == "" {
				c.AI.OriginalBody = c.Body
			}
			c.Body = text
		}
	} else {
		if body.Path == "" || body.Line <= 0 || text == "" {
			p.mu.Unlock()
			fail(w, http.StatusBadRequest, "path, line, and body are required")
			return
		}
		c = prComment{Path: body.Path, Line: body.Line, Side: normSide(body.Side), Body: text, Origin: prOriginHuman}
		if body.StartLine > 0 && body.StartLine < body.Line {
			c.StartLine = body.StartLine
		}
	}
	head := p.meta.HeadSHA
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := p.provider.SubmitReview(ctx, p.target, p.token, head, []prComment{c}, "COMMENT", ""); err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	if body.ID != 0 {
		p.mu.Lock()
		for i := range p.comments {
			if p.comments[i].ID != body.ID {
				continue
			}
			if p.comments[i].Origin == prOriginAI {
				p.comments[i].Body, p.comments[i].AI = c.Body, c.AI
				p.comments[i].Status = prStatusPosted
			} else {
				p.comments = append(p.comments[:i], p.comments[i+1:]...)
			}
			break
		}
		s.persistDrafts(p)
		p.mu.Unlock()
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handlePRCommentEdit replaces the text of one of the reviewer's own drafts:
// POST ?id= {body}. AI suggestions are edited through review triage.
func (s *Server) handlePRCommentEdit(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) || !localPost(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	var body struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || strings.TrimSpace(body.Body) == "" {
		fail(w, http.StatusBadRequest, "a non-empty body is required")
		return
	}
	p := s.pr
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.comments {
		c := &p.comments[i]
		if c.ID != id || c.Origin == prOriginAI {
			continue
		}
		c.Body = strings.TrimSpace(body.Body)
		s.persistDrafts(p)
		writeJSON(w, *c)
		return
	}
	fail(w, http.StatusNotFound, "no draft of yours with that id")
}

func normSide(side string) string {
	if strings.ToUpper(side) == "LEFT" {
		return "LEFT"
	}
	return "RIGHT"
}

// handleMarkdownRender renders comment text for a composer's Preview tab:
// POST {text}. The page sanitizes the HTML as it does a forge's.
func (s *Server) handleMarkdownRender(w http.ResponseWriter, r *http.Request) {
	if !localPost(w, r) {
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<18)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	out, err := renderMarkdown([]byte(body.Text))
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"html": out})
}
