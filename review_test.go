package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseReviewOutput(t *testing.T) {
	result := `{"summary":"Looks fine.","verdict":"comment","suggestions":[{"path":"a.go","line":4,"quote":"x := 2","severity":"major","body":"Why 2?","confidence":0.9}]}`
	cases := []struct {
		name    string
		out     string
		wantErr bool
		wantN   int
	}{
		{"markers", "thinking...\n" + reviewBeginMarker + "\n" + result + "\n" + reviewEndMarker + "\n", false, 1},
		{"markers around a code fence", reviewBeginMarker + "\n```json\n" + result + "\n```\n" + reviewEndMarker, false, 1},
		{"echoed prompt names the markers first", "print the JSON between " + reviewBeginMarker + " and " + reviewEndMarker + ".\n" +
			reviewBeginMarker + "\n" + result + "\n" + reviewEndMarker, false, 1},
		{"echoed output format template, then the result", reviewOutputFormat + "\n\n" + reviewBeginMarker + "\n" + result + "\n" + reviewEndMarker, false, 1},
		{"no markers, bare JSON", "Here is my review:\n" + result, false, 1},
		{"missing end marker", reviewBeginMarker + "\n" + result, false, 1},
		{"empty suggestion list is valid", reviewBeginMarker + `{"summary":"Nothing to add.","suggestions":[]}` + reviewEndMarker, false, 0},
		{"no JSON at all", "I could not review this.", true, 0},
		{"broken JSON", reviewBeginMarker + `{"summary": "x", "suggestions": [` + reviewEndMarker, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := parseReviewOutput(c.out)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if !c.wantErr && len(r.Suggestions) != c.wantN {
				t.Fatalf("%d suggestions, want %d: %+v", len(r.Suggestions), c.wantN, r)
			}
			if c.name == "echoed output format template, then the result" && r.Summary != "Looks fine." {
				t.Fatalf("picked the template instead of the result: %+v", r)
			}
		})
	}
}

func TestParseReviewOutputLenientNumbers(t *testing.T) {
	r, err := parseReviewOutput(`{"suggestions":[{"path":"a.go","line":"4","startLine":"","confidence":"80%","body":"x"},{"path":"a.go","line":5,"confidence":85,"body":"y"},{"path":"a.go","line":5,"body":"z"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if int(r.Suggestions[0].Line) != 4 || normConfidence(r.Suggestions[0].Confidence) != 0.8 {
		t.Errorf("string numbers: line %v confidence %v", r.Suggestions[0].Line, normConfidence(r.Suggestions[0].Confidence))
	}
	if normConfidence(r.Suggestions[1].Confidence) != 0.85 {
		t.Errorf("confidence 85 should read as 0.85, got %v", normConfidence(r.Suggestions[1].Confidence))
	}
	if normConfidence(r.Suggestions[2].Confidence) != 0.5 {
		t.Errorf("missing confidence should default to 0.5")
	}
}

func TestBuildSuggestions(t *testing.T) {
	pd := parseUnifiedDiff(anchorFixtureDiff)
	out := reviewOutput{Suggestions: []rawSuggestion{
		{Path: "a.go", Line: 22, Quote: "b++", Severity: "nit", Body: "Name this."},
		{Path: "a.go", Line: 4, Quote: "x := 2", Severity: "critical", Category: "Bug", Body: "Wrong constant.", Suggestion: "\tx := 1\n"},
		{Path: "a.go", Line: 40, Quote: "gone", Severity: "major", Body: "Missing test."},
		{Path: "elsewhere.go", Line: 3, Severity: "minor", Body: "Caller not updated."},
		{Path: "a.go", Line: 5, Quote: "return x", Body: "", Suggestion: ""},
		{Path: "a.go", Line: 5, Quote: "return x", Severity: "whatever", Suggestion: "\treturn x + 0"},
	}}
	var id int64
	got, counts := buildSuggestions(pd, out, 7, func() int64 { id++; return id })
	if len(got) != 5 {
		t.Fatalf("got %d suggestions, want 5 (the empty one is skipped): %+v", len(got), got)
	}
	// Sorted by path, then severity: a.go blocker, a.go major, a.go minor, a.go nit, elsewhere.go.
	order := []string{"blocker", "major", "minor", "nit", "minor"}
	for i, c := range got {
		if c.AI.Severity != order[i] {
			t.Errorf("suggestion %d severity %q, want %q (%s:%d)", i, c.AI.Severity, order[i], c.Path, c.Line)
		}
		if c.Origin != prOriginAI || c.Status != prStatusPending || c.RunID != 7 || c.Fingerprint == "" {
			t.Errorf("suggestion %d not a pending AI draft: %+v", i, c)
		}
	}
	if got[0].AI.Category != "bug" || got[0].AI.Suggestion != "\tx := 1" || got[0].AI.Anchor != anchorAnchored {
		t.Errorf("blocker: %+v", got[0].AI)
	}
	if got[1].SubjectType != "file" || !strings.HasPrefix(got[1].Body, "**Line 40:** ") || got[1].AI.Anchor != anchorFile {
		t.Errorf("unplaceable line must become a file comment with the line in its body: %+v", got[1])
	}
	if got[2].Body != "Suggested change:" {
		t.Errorf("a suggestion with only code gets a placeholder body, got %q", got[2].Body)
	}
	if got[4].SubjectType != "summary" || got[4].Path != "elsewhere.go" {
		t.Errorf("file outside the PR must become a summary note: %+v", got[4])
	}
	if counts[anchorAnchored] != 3 || counts[anchorFile] != 1 || counts[anchorSummary] != 1 {
		t.Errorf("counts = %v", counts)
	}
	if suggestionFingerprint("a.go", " x := 2 ", "Wrong constant.") != suggestionFingerprint("a.go", "x := 2", " Wrong constant.\n") {
		t.Error("fingerprint must ignore surrounding whitespace")
	}
}

func TestSubmitBody(t *testing.T) {
	code := func(s string) *aiSuggestion { return &aiSuggestion{Suggestion: s} }
	cases := []struct {
		name string
		c    prComment
		want string
	}{
		{"human draft", prComment{Body: "hi"}, "hi"},
		{"AI without code", prComment{Body: "hi", AI: &aiSuggestion{}}, "hi"},
		{"right side gets a suggestion block", prComment{Body: "Fix.", Side: "RIGHT", AI: code("x := 1")}, "Fix.\n\n```suggestion\nx := 1\n```"},
		{"left side gets a plain block", prComment{Body: "Fix.", Side: "LEFT", AI: code("x := 1")}, "Fix.\n\n```\nx := 1\n```"},
		{"file comment gets a plain block", prComment{Body: "Fix.", Side: "RIGHT", SubjectType: "file", AI: code("x")}, "Fix.\n\n```\nx\n```"},
		{"code containing a fence", prComment{Body: "Doc.", Side: "RIGHT", AI: code("```go\nx\n```")}, "Doc.\n\n````suggestion\n```go\nx\n```\n````"},
	}
	for _, c := range cases {
		if got := c.c.submitBody(); got != c.want {
			t.Errorf("%s:\ngot  %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestBuildReviewDoc(t *testing.T) {
	pd := parseUnifiedDiff(anchorFixtureDiff)
	ctx := reviewContext{
		Skill: "SKILL TEXT", Focus: "security only",
		Meta: PRMeta{Number: 9, Title: "T", Body: "Desc", BaseRef: "main", HeadRef: "f", HeadSHA: "abc"},
		URL:  "https://github.com/o/r/pull/9", Diff: pd, DiffText: anchorFixtureDiff,
		Existing: []PRComment{{Path: "a.go", Line: 4, Author: "bob", Body: "already said"}},
	}
	doc, omitted := buildReviewDoc(ctx)
	if omitted {
		t.Fatal("a small diff must be inlined")
	}
	for _, want := range []string{"SKILL TEXT", "security only", "#9", "Desc", "`new/name.go` (renamed from old/name.go)",
		"`img.png` (modified, binary)", "a.go:4, bob: already said", "```diff\n", "+\tx := 2", reviewBeginMarker, `"quote"`} {
		if !strings.Contains(doc, want) {
			t.Errorf("review doc missing %q", want)
		}
	}
	ctx.DiffText = strings.Repeat("+x\n", reviewInlineDiff)
	doc, omitted = buildReviewDoc(ctx)
	if !omitted || strings.Contains(doc, "```diff") || !strings.Contains(doc, "pr.diff") {
		t.Error("a large diff must be left out of the doc and pointed to in pr.diff")
	}
}

func TestResolveReviewSkill(t *testing.T) {
	dir := isolateSettings(t)
	if text, src := resolveReviewSkill(settings{}); src != "built-in" || !strings.Contains(text, "Correctness") {
		t.Fatalf("default skill: %q from %s", text[:40], src)
	}
	userSkill := filepath.Join(dir, "px0", "skills", "review.md")
	os.MkdirAll(filepath.Dir(userSkill), 0o755)
	os.WriteFile(userSkill, []byte("USER SKILL"), 0o644)
	if text, _ := resolveReviewSkill(settings{}); text != "USER SKILL" {
		t.Fatalf("~/.px0/skills/review.md should win over the built-in, got %q", text)
	}
	custom := filepath.Join(t.TempDir(), "mine.md")
	os.WriteFile(custom, []byte("SETTING SKILL"), 0o644)
	if text, _ := resolveReviewSkill(settings{ReviewSkillPath: &custom}); text != "SETTING SKILL" {
		t.Fatalf("review.skillPath should win, got %q", text)
	}
	missing := filepath.Join(t.TempDir(), "nope.md")
	if text, _ := resolveReviewSkill(settings{ReviewSkillPath: &missing}); text != "USER SKILL" {
		t.Fatalf("a missing skillPath falls through, got %q", text)
	}
}

// reviewTestServer is a PR session with one pending suggestion and one
// human draft, no harness.
func reviewTestServer(t *testing.T) *Server {
	t.Helper()
	isolateSettings(t)
	ix := NewIndex(t.TempDir())
	ix.Build()
	s := NewServer(ix, nil)
	s.pr = &prSession{
		provider: &GitHubProvider{},
		target:   PRTarget{Provider: "github", Owner: "o", Repo: "r", Number: 1},
		token:    "tok",
		nextID:   3,
		comments: []prComment{
			{ID: 1, Path: "a.go", Line: 1, Side: "RIGHT", Body: "mine", Origin: prOriginHuman, Status: prStatusAccepted},
			{ID: 2, Path: "a.go", Line: 4, Side: "RIGHT", Body: "ai says", Origin: prOriginAI, Status: prStatusPending,
				AI: &aiSuggestion{Severity: "major", Suggestion: "x := 1", Anchor: anchorAnchored}},
			{ID: 3, Path: "a.go", Line: 5, Side: "RIGHT", Body: "ai nit", Origin: prOriginAI, Status: prStatusPending,
				AI: &aiSuggestion{Severity: "nit", Anchor: anchorAnchored}},
		},
	}
	return s
}

func TestReviewTriage(t *testing.T) {
	s := reviewTestServer(t)
	post := func(body any) (int, map[string]any) { return agentPostJSON(t, s, "/api/pr/review/triage", body) }

	if code, _ := post(map[string]any{"ids": []int64{1}, "action": "dismiss"}); code != http.StatusNotFound {
		t.Errorf("triage of a human draft = %d, want 404", code)
	}
	if code, _ := post(map[string]any{"ids": []int64{2, 3}, "action": "edit", "body": "x"}); code != http.StatusBadRequest {
		t.Errorf("edit of two ids = %d, want 400", code)
	}
	if code, _ := post(map[string]any{"ids": []int64{2}, "action": "approve"}); code != http.StatusBadRequest {
		t.Errorf("unknown action = %d, want 400", code)
	}

	code, resp := post(map[string]any{"ids": []int64{2}, "action": "edit", "body": "  ai says, reworded  "})
	if code != 200 || resp["draftCount"].(float64) != 2 {
		t.Fatalf("edit = %d %v", code, resp)
	}
	if c := s.pr.comments[1]; c.Status != prStatusEdited || c.Body != "ai says, reworded" || c.AI.OriginalBody != "ai says" {
		t.Fatalf("after edit: %+v %+v", c, c.AI)
	}
	// Accepting an edited suggestion keeps it edited.
	post(map[string]any{"ids": []int64{2}, "action": "accept"})
	if s.pr.comments[1].Status != prStatusEdited {
		t.Errorf("accept after edit changed status to %q", s.pr.comments[1].Status)
	}
	post(map[string]any{"ids": []int64{3}, "action": "dismiss"})
	if s.pr.comments[2].Status != prStatusDismissed {
		t.Error("dismiss did not stick")
	}
	post(map[string]any{"ids": []int64{3}, "action": "restore"})
	if s.pr.comments[2].Status != prStatusPending {
		t.Error("restore did not bring the suggestion back to pending")
	}

	// Submit sends the edited suggestion with its suggestion block, not the pending nit.
	orig := githubHTTPClient.Transport
	defer func() { githubHTTPClient.Transport = orig }()
	var captured []byte
	githubHTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Body != nil {
			captured, _ = io.ReadAll(req.Body)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
	})
	if code, resp := agentPostJSON(t, s, "/api/pr/submit", map[string]string{"event": "COMMENT"}); code != 200 {
		t.Fatalf("submit = %d %v", code, resp)
	}
	var payload struct {
		Comments []struct{ Body string } `json:"comments"`
	}
	json.Unmarshal(captured, &payload)
	if len(payload.Comments) != 2 || payload.Comments[1].Body != "ai says, reworded\n\n```suggestion\nx := 1\n```" {
		t.Fatalf("submitted comments = %+v", payload.Comments)
	}
}

func TestReviewSuggestionsEndpointListsOnlyAI(t *testing.T) {
	s := reviewTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, "/api/pr/review/suggestions", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var out struct {
		Run         *reviewRun  `json:"run"`
		Suggestions []prComment `json:"suggestions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Run != nil || len(out.Suggestions) != 2 {
		t.Fatalf("got run=%v and %d suggestions, want none and 2", out.Run, len(out.Suggestions))
	}
}

func TestReviewRunRequiresReadOnlyHarness(t *testing.T) {
	root := t.TempDir()
	s := agentServer(t, root, writeHarness(t, "exit 0\n"))
	s.pr = &prSession{provider: &GitHubProvider{}, diffBase: "abc", worktree: root}
	code, resp := agentPostJSON(t, s, "/api/pr/review/run", map[string]string{})
	if code != http.StatusConflict {
		t.Fatalf("run with a custom template harness = %d %v, want 409", code, resp)
	}
}

// TestHelperReviewHarness is not a test: TestReviewRunEndToEnd runs the test
// binary itself as a stand-in read-only harness (so it works on every OS).
// Argv after "--" is {tmpdir} {prompt}.
func TestHelperReviewHarness(t *testing.T) {
	if os.Getenv("PX0_HELPER_HARNESS") != "1" {
		t.Skip("helper process")
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	if seen := os.Getenv("PX0_HELPER_SEEN"); seen != "" {
		doc, _ := os.ReadFile(filepath.Join(args[0], "review.md"))
		os.WriteFile(seen, doc, 0o644)
	}
	if w := os.Getenv("PX0_HELPER_WRITE"); w != "" { // misbehave: edit a file in the worktree
		f, _ := os.OpenFile(w, os.O_APPEND|os.O_WRONLY, 0)
		f.WriteString("sneaky\n")
		f.Close()
	}
	os.Stdout.WriteString(reviewBeginMarker + "\n" + os.Getenv("PX0_HELPER_RESULT") + "\n" + reviewEndMarker + "\n")
	os.Exit(0)
}

// helperHarnessServer serves root with the test binary selected as a
// read-only harness (TestHelperReviewHarness). Callers set PX0_HELPER_* env.
func helperHarnessServer(t *testing.T, root string) *Server {
	t.Helper()
	t.Setenv("PX0_HELPER_HARNESS", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s := agentServer(t, root, exe)
	orig := agentPresets
	agentPresets = append(append([]agentPreset(nil), orig...), agentPreset{
		Name:         "helper-ro",
		Args:         []string{exe, "{prompt}"},
		ReadOnlyArgs: []string{exe, "-test.run=^TestHelperReviewHarness$", "--", "{tmpdir}", "{prompt}"},
	})
	t.Cleanup(func() { agentPresets = orig })
	s.agent.mu.Lock()
	s.agent.selected = "helper-ro"
	s.agent.mu.Unlock()
	return s
}

// TestReviewRunEndToEnd drives a real run: a PR diff in a git repo, a fake
// read-only harness that prints a result, and the suggestions it becomes.
func TestReviewRunEndToEnd(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	git := func(args ...string) string { return gitTestRun(t, root, args...) }
	git("init", "-q")
	git("config", "user.email", "t@t")
	git("config", "user.name", "t")
	git("config", "core.autocrlf", "false")
	os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n\nfunc A() int {\n\treturn 1\n}\n"), 0o644)
	git("add", ".")
	git("commit", "-q", "-m", "base")
	base := strings.TrimSpace(git("rev-parse", "HEAD"))
	os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n\nfunc A() int {\n\tx := 2\n\treturn x\n}\n"), 0o644)
	git("commit", "-q", "-am", "head")

	orig := githubHTTPClient.Transport
	defer func() { githubHTTPClient.Transport = orig }()
	githubHTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("[]")), Header: make(http.Header)}, nil
	})

	seen := filepath.Join(t.TempDir(), "seen.md")
	t.Setenv("PX0_HELPER_SEEN", seen)
	t.Setenv("PX0_HELPER_RESULT", `{"summary":"One issue.","verdict":"approve","suggestions":[`+
		`{"path":"a.go","line":3,"side":"RIGHT","quote":"x := 2","severity":"major","body":"Why 2?","confidence":0.9},`+
		`{"path":"a.go","line":9,"quote":"nowhere","severity":"minor","body":"Somewhere."}]}`)
	s := helperHarnessServer(t, root)
	s.pr = &prSession{
		provider: &GitHubProvider{}, token: "tok", worktree: root, diffBase: base,
		target: PRTarget{Provider: "github", Owner: "o", Repo: "r", Number: 1, URL: "https://github.com/o/r/pull/1"},
		meta:   PRMeta{Number: 1, Title: "Change A", HeadSHA: "head"},
		comments: []prComment{
			{ID: 1, Path: "a.go", Line: 5, Side: "RIGHT", Body: "stale pending", Origin: prOriginAI, Status: prStatusPending},
			{ID: 2, Path: "a.go", Line: 5, Side: "RIGHT", Body: "kept", Origin: prOriginAI, Status: prStatusAccepted},
		},
		nextID: 2,
	}

	code, resp := agentPostJSON(t, s, "/api/pr/review/run", map[string]string{"focus": "logic"})
	if code != 200 {
		t.Fatalf("run = %d %v", code, resp)
	}
	run := resp["run"].(map[string]any)
	waitIdleID(t, s, int64(run["jobId"].(float64)))
	var cur *reviewRun
	for i := 0; i < 100; i++ {
		s.pr.mu.Lock()
		cur = s.pr.review.cur
		st := cur.Status
		s.pr.mu.Unlock()
		if st != "running" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if cur.Status != "done" {
		t.Fatalf("run status %q: %s\n%s", cur.Status, cur.Error, cur.Raw)
	}
	if cur.Verdict != "comment" || !cur.SuggestedApprove || cur.Summary != "One issue." {
		t.Errorf("an approve verdict must never be pre-selected: %+v", cur)
	}
	if _, err := os.Stat(cur.tmpdir); !os.IsNotExist(err) {
		t.Error("the run's temp directory should be removed when it finishes")
	}
	doc, _ := os.ReadFile(seen)
	if !strings.Contains(string(doc), "Change A") || !strings.Contains(string(doc), "+\tx := 2") || !strings.Contains(string(doc), "logic") {
		t.Errorf("harness did not get the review doc:\n%s", doc)
	}

	var ai []prComment
	for _, c := range s.pr.comments {
		if c.Body == "stale pending" {
			t.Error("a new run must replace pending suggestions from the last one")
		}
		if c.RunID == cur.ID {
			ai = append(ai, c)
		}
	}
	if len(ai) != 2 {
		t.Fatalf("new suggestions = %+v", ai)
	}
	if ai[0].Line != 4 || ai[0].AI.Anchor != anchorReanchored {
		t.Errorf("drifted line should be re-anchored to 4: %+v %+v", ai[0], ai[0].AI)
	}
	if ai[1].SubjectType != "file" {
		t.Errorf("unplaceable suggestion should be file-level: %+v", ai[1])
	}
	if s.pr.comments[0].Body != "kept" {
		t.Error("accepted suggestions from an earlier run must survive a re-run")
	}
}
