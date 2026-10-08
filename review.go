package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// review.go is AI draft review: a review skill runs through the selected
// harness, read-only, in the PR worktree, and what it returns becomes
// suggestions -- AI-origin drafts in prSession.comments with status
// "pending". Nothing reaches GitHub until the reviewer accepts or edits a
// suggestion and then submits the review (pr.go handlePRSubmit sends only
// submittable drafts). Anchoring against the real diff is in anchor.go.

//go:embed review_skill.md
var defaultReviewSkill string

const (
	reviewBeginMarker = "PX0-REVIEW-BEGIN"
	reviewEndMarker   = "PX0-REVIEW-END"

	reviewOutBytes     = 1 << 20   // stdout kept for parsing
	reviewInlineDiff   = 200 << 10 // above this the diff is left out of review.md
	reviewMaxDiffBytes = 32 << 20  // hard cap on what is read from git diff
)

// aiSuggestion is what an AI-origin draft carries beyond a human one.
type aiSuggestion struct {
	Severity     string  `json:"severity"` // critical, high, medium, low
	Category     string  `json:"category,omitempty"`
	Confidence   float64 `json:"confidence"`
	Suggestion   string  `json:"suggestion,omitempty"` // replacement code for the anchored line(s)
	Quote        string  `json:"quote,omitempty"`
	Anchor       string  `json:"anchor"` // anchored, reanchored, file, summary
	ReportedPath string  `json:"reportedPath,omitempty"`
	ReportedLine int     `json:"reportedLine,omitempty"`
	ReportedSide string  `json:"reportedSide,omitempty"`
	OriginalBody string  `json:"originalBody,omitempty"` // as the model wrote it, kept once the reviewer edits
	ModelBody    string  `json:"modelBody,omitempty"`    // the model's text, before any "Line N:" prefix
	ReportedFrom int     `json:"reportedStartLine,omitempty"`
	// HeadSHA is the PR head the suggestion was anchored against. When the
	// head moves (a Pull, or new commits seen at submit time) a pending
	// suggestion is stale: its line may no longer be where it says, so it
	// cannot be accepted until it is re-checked against the new diff.
	HeadSHA string `json:"headSha,omitempty"`
	Stale   bool   `json:"stale,omitempty"` // computed when listed, never stored
}

// reviewRun is one Run AI Review, from dispatch to parsed suggestions.
type reviewRun struct {
	ID      int64    `json:"id"`
	JobID   int64    `json:"jobId"`
	Harness string   `json:"harness"`
	Focus   string   `json:"focus,omitempty"`
	Status  string   `json:"status"` // running, done, failed, cancelled
	Error   string   `json:"error,omitempty"`
	Raw     string   `json:"raw,omitempty"` // harness output, kept when it could not be parsed
	Tainted bool     `json:"tainted,omitempty"`
	Changed []string `json:"changed,omitempty"`
	Summary string   `json:"summary,omitempty"`
	// Verdict pre-fills the review form: "comment" or "request_changes".
	// A model's "approve" is reported in SuggestedApprove and never selected.
	Verdict          string         `json:"verdict,omitempty"`
	SuggestedApprove bool           `json:"suggestedApprove,omitempty"`
	Counts           map[string]int `json:"counts,omitempty"` // suggestions per anchor outcome
	HeadSHA          string         `json:"headSha"`
	DiffOmitted      bool           `json:"diffOmitted,omitempty"` // the diff was too large to inline
	StartedAt        time.Time      `json:"startedAt"`
	FinishedAt       *time.Time     `json:"finishedAt,omitempty"`

	tmpdir string
}

// reviewState hangs off prSession; guarded by prSession.mu.
type reviewState struct {
	seq int64
	cur *reviewRun
}

// ---------------------------------------------------------------- skill

// resolveReviewSkill returns the review instructions: the review.skillPath
// setting, then ~/.px0/skills/review.md, then the built-in skill.
func resolveReviewSkill(cfg settings) (text, source string) {
	if cfg.ReviewSkillPath != nil {
		if p := strings.TrimSpace(*cfg.ReviewSkillPath); p != "" {
			if strings.HasPrefix(p, "~/") {
				if home, err := os.UserHomeDir(); err == nil {
					p = filepath.Join(home, p[2:])
				}
			}
			if b, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(b)) != "" {
				return string(b), p
			}
		}
	}
	if sp := settingsPath(); sp != "" {
		p := filepath.Join(filepath.Dir(sp), "skills", "review.md")
		if b, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(b)) != "" {
			return string(b), p
		}
	}
	return defaultReviewSkill, "built-in"
}

// reviewOutputFormat is always appended after the skill, so a custom skill
// cannot change what px0 parses.
const reviewOutputFormat = `# Output format (required)

When you are done, print one JSON object between the two marker lines below, and nothing after the end marker:

` + reviewBeginMarker + `
{
  "summary": "Markdown. Two to five sentences for the review body: what the PR does and your overall assessment.",
  "verdict": "comment | approve | request_changes",
  "suggestions": [
    {
      "path": "path/of/a/changed/file.go",
      "line": 42,
      "startLine": 40,
      "side": "RIGHT",
      "quote": "the exact text of line 42, copied from the diff",
      "severity": "critical | high | medium | low",
      "category": "correctness | contract | security | performance | maintainability",
      "body": "Markdown comment, written as it should be posted: what breaks, and under what conditions.",
      "suggestion": "optional replacement code for exactly lines startLine..line (or just line)",
      "confidence": 0.8
    }
  ]
}
` + reviewEndMarker + `

Rules:
- "path" must be one of the changed files listed above.
- "line" is the line number in the new version of the file for side "RIGHT" (added or unchanged lines), or in the old version for side "LEFT" (deleted lines). Use the numbers from the diff hunks.
- "quote" must be the exact text of that line; it is used to check the line number. Leave out "startLine" for a single-line comment.
- "confidence" is between 0 and 1: how sure you are the comment is correct and worth posting.
- One finding per entry in "suggestions": never put two issues in one comment.
- An empty "suggestions" list is a valid result.
`

// ---------------------------------------------------------------- context

// gitPRDiff returns the whole PR diff (working tree against base) with
// renames detected and paths unquoted.
func gitPRDiff(root, base string) (string, error) {
	cmd := exec.Command("git", "-C", root, "-c", "core.quotepath=false", "diff", "--no-color", "--no-ext-diff", "-M", base)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	b, rerr := io.ReadAll(io.LimitReader(out, reviewMaxDiffBytes))
	io.Copy(io.Discard, out)
	werr := cmd.Wait()
	if rerr != nil {
		return "", rerr
	}
	if werr != nil {
		return "", fmt.Errorf("git diff %s: %w", base, werr)
	}
	return string(b), nil
}

// reviewContext is everything that goes into review.md.
type reviewContext struct {
	Skill    string
	Focus    string
	Meta     PRMeta
	URL      string
	Diff     *prDiff
	DiffText string
	Existing []PRComment
}

// buildReviewDoc writes the instructions, PR details, changed files, posted
// comments, the diff (when small enough) and the output format as one
// markdown file for the harness to read. It reports whether the diff was
// left out.
func buildReviewDoc(c reviewContext) (doc string, diffOmitted bool) {
	var b strings.Builder
	b.WriteString("# Review instructions\n\n")
	b.WriteString(strings.TrimSpace(c.Skill))
	b.WriteString("\n\n")
	if f := strings.TrimSpace(c.Focus); f != "" {
		fmt.Fprintf(&b, "## Focus for this review\n\nThe reviewer asked you to focus on: %s\n\n", f)
	}
	m := c.Meta
	b.WriteString("# Pull request\n\n")
	fmt.Fprintf(&b, "- Title: %s\n- Number: #%d\n- URL: %s\n- Author: %s\n- Base: %s\n- Head: %s (%s)\n",
		m.Title, m.Number, c.URL, m.Author, m.BaseRef, m.HeadRef, m.HeadSHA)
	b.WriteString("- The current directory is a checkout of the head. Open any file you need.\n\n")
	b.WriteString("## Description\n\n")
	if strings.TrimSpace(m.Body) == "" {
		b.WriteString("(no description)\n\n")
	} else {
		b.WriteString(strings.TrimSpace(m.Body))
		b.WriteString("\n\n")
	}

	paths := c.Diff.Paths()
	fmt.Fprintf(&b, "## Changed files (%d)\n\n", len(paths))
	for _, p := range paths {
		f := c.Diff.Files[p]
		note := f.Status
		if f.Status == "renamed" {
			note = "renamed from " + f.OldPath
		}
		if f.Binary {
			note += ", binary"
		}
		fmt.Fprintf(&b, "- `%s` (%s)\n", p, note)
	}
	b.WriteString("\n")

	if len(c.Existing) > 0 {
		b.WriteString("## Comments already posted (do not repeat these)\n\n")
		for _, pc := range c.Existing {
			body := strings.Join(strings.Fields(pc.Body), " ")
			if len(body) > 300 {
				body = body[:300] + "…"
			}
			loc := "PR conversation"
			if pc.Path != "" {
				loc = fmt.Sprintf("%s:%d", pc.Path, pc.Line)
			}
			fmt.Fprintf(&b, "- %s, %s: %s\n", loc, pc.Author, body)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Diff\n\n")
	if len(c.DiffText) <= reviewInlineDiff {
		b.WriteString("```diff\n")
		b.WriteString(c.DiffText)
		if !strings.HasSuffix(c.DiffText, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("```\n\n")
	} else {
		diffOmitted = true
		fmt.Fprintf(&b, "The diff is %d KB, too large to include. It is in `pr.diff` next to this file; read the parts you need, and open the changed files directly.\n\n", len(c.DiffText)>>10)
	}
	b.WriteString(reviewOutputFormat)
	return b.String(), diffOmitted
}

// reviewPrompt is the short argv prompt; the substance is in review.md.
func reviewPrompt(tmpdir string) string {
	return fmt.Sprintf("You are reviewing a GitHub pull request; the current directory is a checkout of its head. "+
		"First read %s: it holds your instructions, the pull request, its diff, and the required output format. "+
		"Do not modify any files. When you are done, print the JSON result between the lines %s and %s.",
		filepath.ToSlash(filepath.Join(tmpdir, "review.md")), reviewBeginMarker, reviewEndMarker)
}

// ---------------------------------------------------------------- output

// flexNum accepts a JSON number or a numeric string ("0.8", "80%", "42").
type flexNum float64

func (n *flexNum) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	pct := strings.HasSuffix(s, "%")
	f, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
	if err != nil {
		return nil // a malformed number is treated as absent, not as a broken review
	}
	if pct {
		f /= 100
	}
	*n = flexNum(f)
	return nil
}

type rawSuggestion struct {
	Path       string   `json:"path"`
	Line       flexNum  `json:"line"`
	StartLine  flexNum  `json:"startLine"`
	Side       string   `json:"side"`
	Quote      string   `json:"quote"`
	Severity   string   `json:"severity"`
	Category   string   `json:"category"`
	Body       string   `json:"body"`
	Suggestion string   `json:"suggestion"`
	Confidence *flexNum `json:"confidence"`
}

type reviewOutput struct {
	Summary     string          `json:"summary"`
	Verdict     string          `json:"verdict"`
	Suggestions []rawSuggestion `json:"suggestions"`
}

// parseReviewOutput finds the result in a harness's stdout: the last
// marker-delimited block that parses, else the last JSON object in the text.
// Models wrap JSON in code fences and prose, and some harnesses echo the
// prompt (which names the markers), so every candidate is tried from the end.
func parseReviewOutput(out string) (reviewOutput, error) {
	var cands []string
	rest := out
	for {
		i := strings.Index(rest, reviewBeginMarker)
		if i < 0 {
			break
		}
		rest = rest[i+len(reviewBeginMarker):]
		j := strings.Index(rest, reviewEndMarker)
		if j < 0 {
			cands = append(cands, rest)
			break
		}
		cands = append(cands, rest[:j])
		rest = rest[j+len(reviewEndMarker):]
	}
	cands = append(cands, out)
	for i := len(cands) - 1; i >= 0; i-- {
		if r, ok := decodeReviewJSON(cands[i]); ok {
			return r, nil
		}
	}
	return reviewOutput{}, errors.New("no review result found in the harness output")
}

func decodeReviewJSON(s string) (reviewOutput, bool) {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	for start >= 0 && end > start {
		var r reviewOutput
		if err := json.Unmarshal([]byte(s[start:end+1]), &r); err == nil && (r.Suggestions != nil || r.Summary != "" || r.Verdict != "") {
			return r, true
		}
		next := strings.Index(s[start+1:], "{")
		if next < 0 {
			break
		}
		start += next + 1
	}
	return reviewOutput{}, false
}

// severityRank orders suggestions by priority, most severe first. The
// scale is the review skill's: Critical, High, Medium, Low.
var severityRank = map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3}

// normSeverity maps what a model or an older px0 wrote onto the scale;
// anything unknown is medium.
func normSeverity(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "blocker", "fatal":
		return "critical"
	case "major", "error":
		return "high"
	case "minor", "warning", "moderate":
		return "medium"
	case "nit", "nitpick", "trivial", "info", "suggestion":
		return "low"
	}
	if _, ok := severityRank[s]; ok {
		return s
	}
	return "medium"
}

func normConfidence(c *flexNum) float64 {
	if c == nil {
		return 0.5
	}
	f := float64(*c)
	if f > 1 && f <= 100 {
		f /= 100
	}
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// suggestionFingerprint identifies a suggestion across runs: same file, same
// line text, same comment.
func suggestionFingerprint(path, quote, body string) string {
	h := sha256.Sum256([]byte(path + "\x00" + quoteKey(quote) + "\x00" + strings.TrimSpace(body)))
	return hex.EncodeToString(h[:8])
}

// buildSuggestions anchors each raw suggestion and turns it into a pending
// AI draft, one per finding, sorted by priority (suggestionLess). None is
// dropped: a file-level or summary result keeps the reported line in the
// body.
func buildSuggestions(pd *prDiff, out reviewOutput, runID int64, nextID func() int64) ([]prComment, map[string]int) {
	counts := map[string]int{}
	var res []prComment
	for _, s := range out.Suggestions {
		body := strings.TrimSpace(s.Body)
		code := strings.TrimRight(s.Suggestion, "\n")
		if body == "" && code == "" {
			continue
		}
		if body == "" {
			body = "Suggested change:"
		}
		line, start := int(s.Line), int(s.StartLine)
		a := pd.Anchor(s.Path, line, start, s.Side, s.Quote)
		counts[a.Status]++
		c := prComment{
			ID:          nextID(),
			Origin:      prOriginAI,
			Status:      prStatusPending,
			RunID:       runID,
			Fingerprint: suggestionFingerprint(a.Path, s.Quote, body),
			AI: &aiSuggestion{
				Severity:     normSeverity(s.Severity),
				Category:     strings.ToLower(strings.TrimSpace(s.Category)),
				Confidence:   normConfidence(s.Confidence),
				Suggestion:   code,
				Quote:        s.Quote,
				ModelBody:    body,
				ReportedPath: s.Path,
				ReportedLine: line,
				ReportedFrom: start,
				ReportedSide: strings.ToUpper(s.Side),
			},
		}
		c.applyAnchor(a)
		res = append(res, c)
	}
	sort.SliceStable(res, func(i, j int) bool { return suggestionLess(res[i], res[j]) })
	return res, counts
}

// suggestionLess orders AI suggestions by priority: most severe first, then
// the more confident, then by file and line.
func suggestionLess(a, b prComment) bool {
	if ra, rb := severityRank[a.AI.Severity], severityRank[b.AI.Severity]; ra != rb {
		return ra < rb
	}
	if a.AI.Confidence != b.AI.Confidence {
		return a.AI.Confidence > b.AI.Confidence
	}
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	return a.Line < b.Line
}

// applyAnchor places an AI suggestion where the anchoring put it. A
// file-level one keeps the line the model meant in its body; a suggestion
// the reviewer edited keeps the reviewer's text.
func (c *prComment) applyAnchor(a anchorResult) {
	c.Path, c.Line, c.Side, c.StartLine = a.Path, a.Line, a.Side, a.StartLine
	c.AI.Anchor = a.Status
	c.SubjectType = ""
	body := c.AI.ModelBody
	switch a.Status {
	case anchorFile:
		c.SubjectType = "file"
		if c.AI.ReportedLine > 0 {
			body = fmt.Sprintf("**Line %d:** %s", c.AI.ReportedLine, body)
		}
	case anchorSummary:
		c.SubjectType = "summary"
	}
	if c.Status != prStatusEdited {
		c.Body = body
	}
}

// submitBody is the comment text GitHub receives: the body, plus the
// replacement code as a suggestion block when it can apply to a RIGHT-side
// line, or as a plain code block when it cannot.
func (c prComment) submitBody() string {
	if c.AI == nil || strings.TrimSpace(c.AI.Suggestion) == "" {
		return c.Body
	}
	fence := "```"
	for strings.Contains(c.AI.Suggestion, fence) {
		fence += "`"
	}
	lang := "suggestion"
	if c.SubjectType != "" || c.Side == "LEFT" {
		lang = ""
	}
	return c.Body + "\n\n" + fence + lang + "\n" + c.AI.Suggestion + "\n" + fence
}

// ---------------------------------------------------------------- lifecycle

var errReviewRunning = errors.New("an AI review is already running")

// startReview dispatches a run. Everything the harness reads goes in a temp
// directory outside the worktree, removed when the run finishes.
func (s *Server) startReview(focus string) (*reviewRun, error) {
	// Refuse an unsupported harness before any git or network work.
	if _, _, err := s.agent.readOnlyArgv(); err != nil {
		return nil, err
	}
	p := s.pr
	p.mu.Lock()
	if p.review == nil {
		p.review = &reviewState{}
	}
	if p.review.cur != nil && p.review.cur.Status == "running" {
		p.mu.Unlock()
		return nil, errReviewRunning
	}
	meta, target, base, root := p.meta, p.target, p.diffBase, p.worktree
	p.mu.Unlock()
	if root == "" && s.ix != nil {
		root = s.ix.Root()
	}
	if base == "" || base == "HEAD" {
		return nil, errors.New("the PR's merge-base could not be resolved, so there is no diff to review")
	}

	diffText, err := gitPRDiff(root, base)
	if err != nil {
		return nil, err
	}
	pd := parseUnifiedDiff(diffText)
	if len(pd.Files) == 0 {
		return nil, errors.New("the PR has no changes to review")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	issue, review, _ := p.provider.FetchComments(ctx, target, p.token) // best-effort: only used to avoid repeats
	cancel()

	cfg := readSettings()
	skill, _ := resolveReviewSkill(cfg)
	doc, omitted := buildReviewDoc(reviewContext{
		Skill: skill, Focus: focus, Meta: meta, URL: target.URL,
		Diff: pd, DiffText: diffText, Existing: append(review, issue...),
	})

	tmpdir, err := os.MkdirTemp("", "px0-review-*")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(tmpdir, "review.md"), []byte(doc), 0o600); err == nil && omitted {
		err = os.WriteFile(filepath.Join(tmpdir, "pr.diff"), []byte(diffText), 0o600)
	}
	if err != nil {
		os.RemoveAll(tmpdir)
		return nil, err
	}

	p.mu.Lock()
	p.review.seq++
	runID := p.review.seq
	p.mu.Unlock()

	job, err := s.agent.StartReview(fmt.Sprintf("AI review #%d of PR #%d", runID, meta.Number), reviewPrompt(tmpdir),
		reviewJobOpts{OutBytes: reviewOutBytes, Timeout: cfg.reviewTimeout(), TmpDir: tmpdir})
	if err != nil {
		os.RemoveAll(tmpdir)
		return nil, err
	}
	run := &reviewRun{
		ID: runID, JobID: job.ID, Harness: job.Harness, Focus: strings.TrimSpace(focus),
		Status: "running", HeadSHA: meta.HeadSHA, DiffOmitted: omitted, StartedAt: time.Now(),
		tmpdir: tmpdir,
	}
	p.mu.Lock()
	p.review.cur = run
	p.mu.Unlock()
	go s.watchReview(run, pd)
	return run, nil
}

// watchReview waits for the harness to exit, then finalises the run.
func (s *Server) watchReview(run *reviewRun, pd *prDiff) {
	for {
		j := s.agent.Job(run.JobID)
		if j == nil || !j.Running {
			s.finishReview(run, pd, j)
			return
		}
		time.Sleep(400 * time.Millisecond)
	}
}

// finishReview parses the harness output into suggestions. Pending
// suggestions from an earlier run are replaced; accepted, edited and
// dismissed ones stay.
func (s *Server) finishReview(run *reviewRun, pd *prDiff, j *agentJob) {
	defer os.RemoveAll(run.tmpdir)
	now := time.Now()
	p := s.pr

	var out reviewOutput
	var parseErr error
	status, errMsg, raw := "done", "", ""
	switch {
	case j == nil:
		status, errMsg = "failed", "the review job disappeared"
	case j.Error == "cancelled":
		status, errMsg = "cancelled", "cancelled"
	case j.Error != "":
		status, errMsg, raw = "failed", j.Error, tailString(j.Stdout+"\n"+j.Stderr, 64<<10)
	default:
		out, parseErr = parseReviewOutput(j.Stdout)
		if parseErr != nil {
			status, errMsg, raw = "failed", parseErr.Error(), tailString(j.Stdout+"\n"+j.Stderr, 64<<10)
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	run.Status, run.Error, run.Raw, run.FinishedAt = status, errMsg, raw, &now
	if j != nil {
		run.Tainted, run.Changed = j.Tainted, j.Changed
	}
	if status != "done" {
		return
	}
	nextID := func() int64 { p.nextID++; return p.nextID }
	sugg, counts := buildSuggestions(pd, out, run.ID, nextID)
	// A suggestion the reviewer already accepted or dismissed in this session
	// is not shown again when a re-run produces it once more.
	decided := map[string]bool{}
	for _, c := range p.comments {
		if c.Origin == prOriginAI && c.Status != prStatusPending {
			decided[c.Fingerprint] = true
		}
	}
	fresh := sugg[:0]
	for _, c := range sugg {
		if decided[c.Fingerprint] {
			counts["repeat"]++
			counts[c.AI.Anchor]--
			continue
		}
		c.AI.HeadSHA = run.HeadSHA
		fresh = append(fresh, c)
	}
	sugg = fresh
	run.Summary = strings.TrimSpace(out.Summary)
	run.Counts = counts
	switch strings.ToLower(strings.ReplaceAll(strings.TrimSpace(out.Verdict), " ", "_")) {
	case "request_changes", "changes_requested":
		run.Verdict = "request_changes"
	case "approve", "approved":
		run.Verdict, run.SuggestedApprove = "comment", true
	default:
		run.Verdict = "comment"
	}
	kept := p.comments[:0:0]
	for _, c := range p.comments {
		if c.Origin == prOriginAI && c.Status == prStatusPending {
			continue
		}
		kept = append(kept, c)
	}
	p.comments = append(kept, sugg...)
	s.persistDrafts(p)
}

func tailString(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

// persistDrafts saves the reviewer's drafts to the session file so a restart
// keeps them. Pending and dismissed AI suggestions stay in memory only.
// Callers hold p.mu.
func (s *Server) persistDrafts(p *prSession) {
	if s.session == nil {
		return
	}
	keep := submittableDrafts(p.comments)
	s.session.Update(func(ws *WorkspaceSession) { ws.Drafts = keep })
}

// ---------------------------------------------------------------- HTTP

// handleReviewRun starts a review: POST {focus}.
func (s *Server) handleReviewRun(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) || !localPost(w, r) || !s.agentOrFail(w) {
		return
	}
	var body struct {
		Focus string `json:"focus"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&body)
	run, err := s.startReview(body.Focus)
	switch {
	case errors.Is(err, errReviewRunning):
		fail(w, http.StatusConflict, err.Error())
	case errors.Is(err, errAgentNone), errors.Is(err, errAgentNoReadOnly):
		fail(w, http.StatusConflict, err.Error())
	case err != nil:
		fail(w, http.StatusBadRequest, err.Error())
	default:
		s.pr.mu.Lock()
		defer s.pr.mu.Unlock()
		writeJSON(w, map[string]any{"run": run})
	}
}

// handleReviewSuggestions returns the current run and every AI suggestion.
func (s *Server) handleReviewSuggestions(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) {
		return
	}
	p := s.pr
	p.mu.Lock()
	defer p.mu.Unlock()
	var run *reviewRun
	if p.review != nil {
		run = p.review.cur
	}
	sugg := []prComment{}
	stale := 0
	for _, c := range p.comments {
		if c.Origin == prOriginAI {
			if c.AI != nil && p.isStale(c) {
				ai := *c.AI
				ai.Stale = true
				c.AI = &ai
				stale++
			}
			sugg = append(sugg, c)
		}
	}
	writeJSON(w, map[string]any{"run": run, "suggestions": sugg, "stale": stale,
		"head": p.meta.HeadSHA, "remoteHead": p.remoteHead})
}

// isStale reports whether a pending AI suggestion was anchored against a
// head the PR has since moved from, locally (a Pull) or on GitHub (seen at
// submit time). Callers hold p.mu.
func (p *prSession) isStale(c prComment) bool {
	if c.AI == nil || c.Status != prStatusPending || c.AI.HeadSHA == "" {
		return false
	}
	return c.AI.HeadSHA != p.meta.HeadSHA || (p.remoteHead != "" && p.remoteHead != c.AI.HeadSHA)
}

// handleReviewRevalidate re-anchors every stale pending suggestion against
// the current diff, with the line, side and quote the model reported, and
// marks it current. POST.
func (s *Server) handleReviewRevalidate(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) || !localPost(w, r) {
		return
	}
	p := s.pr
	p.mu.Lock()
	base, root, head := p.diffBase, p.worktree, p.meta.HeadSHA
	p.mu.Unlock()
	if root == "" && s.ix != nil {
		root = s.ix.Root()
	}
	if base == "" || base == "HEAD" {
		fail(w, http.StatusConflict, "the PR's merge-base could not be resolved")
		return
	}
	diffText, err := gitPRDiff(root, base)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	pd := parseUnifiedDiff(diffText)

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.remoteHead != "" && p.remoteHead != head {
		fail(w, http.StatusConflict, "the PR has new commits on GitHub; Pull them first, then re-check")
		return
	}
	counts := map[string]int{}
	for i := range p.comments {
		c := &p.comments[i]
		if !p.isStale(*c) {
			continue
		}
		a := pd.Anchor(c.AI.ReportedPath, c.AI.ReportedLine, c.AI.ReportedFrom, c.AI.ReportedSide, c.AI.Quote)
		c.applyAnchor(a)
		c.AI.HeadSHA = head
		counts[a.Status]++
	}
	writeJSON(w, map[string]any{"counts": counts})
}

// handleReviewTriage applies accept, edit, dismiss or restore to AI
// suggestions: POST {ids, action, body}. Edit takes exactly one id.
func (s *Server) handleReviewTriage(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) || !localPost(w, r) {
		return
	}
	var body struct {
		IDs    []int64 `json:"ids"`
		Action string  `json:"action"`
		Body   string  `json:"body"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || len(body.IDs) == 0 {
		fail(w, http.StatusBadRequest, "ids and action are required")
		return
	}
	var status string
	switch body.Action {
	case "accept":
		status = prStatusAccepted
	case "edit":
		status = prStatusEdited
		if len(body.IDs) != 1 || strings.TrimSpace(body.Body) == "" {
			fail(w, http.StatusBadRequest, "edit takes one id and a non-empty body")
			return
		}
	case "dismiss":
		status = prStatusDismissed
	case "restore":
		status = prStatusPending
	default:
		fail(w, http.StatusBadRequest, "action must be accept, edit, dismiss or restore")
		return
	}
	want := make(map[int64]bool, len(body.IDs))
	for _, id := range body.IDs {
		want[id] = true
	}
	p := s.pr
	p.mu.Lock()
	defer p.mu.Unlock()
	changed := []prComment{}
	staleRefused := 0
	for i := range p.comments {
		c := &p.comments[i]
		if !want[c.ID] || c.Origin != prOriginAI || c.Status == prStatusPosted {
			continue // a posted suggestion is on GitHub: nothing left to decide
		}
		if (status == prStatusAccepted || status == prStatusEdited) && p.isStale(*c) {
			staleRefused++ // its line may have moved: re-check before accepting
			continue
		}
		if status == prStatusEdited {
			if c.AI != nil && c.AI.OriginalBody == "" {
				c.AI.OriginalBody = c.Body
			}
			c.Body = strings.TrimSpace(body.Body)
		}
		// Accepting a suggestion the reviewer already edited keeps it edited.
		if !(status == prStatusAccepted && c.Status == prStatusEdited) {
			c.Status = status
		}
		changed = append(changed, *c)
	}
	if len(changed) == 0 && staleRefused > 0 {
		fail(w, http.StatusConflict, "the PR head moved since this suggestion was made; re-check suggestions against the new head first")
		return
	}
	if len(changed) == 0 {
		fail(w, http.StatusNotFound, "no AI suggestion with those ids")
		return
	}
	s.persistDrafts(p)
	writeJSON(w, map[string]any{"suggestions": changed, "draftCount": countSubmittable(p.comments), "staleRefused": staleRefused})
}

// handleReviewCancel stops the running review.
func (s *Server) handleReviewCancel(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) || !localPost(w, r) || !s.agentOrFail(w) {
		return
	}
	p := s.pr
	p.mu.Lock()
	var jobID int64
	if p.review != nil && p.review.cur != nil && p.review.cur.Status == "running" {
		jobID = p.review.cur.JobID
	}
	p.mu.Unlock()
	if jobID == 0 {
		fail(w, http.StatusConflict, "no AI review is running")
		return
	}
	writeJSON(w, map[string]any{"ok": s.agent.CancelJob(jobID)})
}
