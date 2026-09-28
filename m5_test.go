package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- AI review (AIR-8, AIR-9)

func reviewResultJob(t *testing.T, suggestions string) *agentJob {
	t.Helper()
	return &agentJob{Stdout: reviewBeginMarker + "\n" + `{"summary":"s","verdict":"comment","suggestions":[` + suggestions + `]}` + "\n" + reviewEndMarker}
}

func TestReviewRerunHidesDecidedSuggestions(t *testing.T) {
	pd := parseUnifiedDiff(anchorFixtureDiff)
	s := reviewTestServer(t)
	s.pr.meta.HeadSHA = "h1"
	s.pr.comments = []prComment{
		{ID: 1, Origin: prOriginAI, Status: prStatusDismissed, Fingerprint: suggestionFingerprint("a.go", "x := 2", "Why 2?"), AI: &aiSuggestion{Anchor: anchorAnchored}},
		{ID: 2, Origin: prOriginAI, Status: prStatusAccepted, Fingerprint: suggestionFingerprint("a.go", "return x", "Name it."), AI: &aiSuggestion{Anchor: anchorAnchored}},
	}
	s.pr.nextID = 2
	run := &reviewRun{ID: 2, Status: "running", HeadSHA: "h1"}
	s.finishReview(run, pd, reviewResultJob(t,
		`{"path":"a.go","line":4,"quote":"x := 2","body":"Why 2?"},`+
			`{"path":"a.go","line":5,"quote":"return x","body":"Name it."},`+
			`{"path":"a.go","line":22,"quote":"b++","body":"New point."}`))
	if run.Status != "done" || run.Counts["repeat"] != 2 || run.Counts[anchorAnchored] != 1 {
		t.Fatalf("run = %+v counts %v", run, run.Counts)
	}
	var fresh []prComment
	for _, c := range s.pr.comments {
		if c.RunID == 2 {
			fresh = append(fresh, c)
		}
	}
	if len(fresh) != 1 || fresh[0].Body != "New point." || fresh[0].AI.HeadSHA != "h1" {
		t.Fatalf("new suggestions = %+v", fresh)
	}
}

func TestStaleSuggestionsCannotBeAccepted(t *testing.T) {
	s := reviewTestServer(t) // ids 2 and 3 are pending AI suggestions
	s.pr.meta.HeadSHA = "h2"
	s.pr.comments[1].AI.HeadSHA = "h1" // made before a Pull moved the head
	s.pr.comments[2].AI.HeadSHA = "h2"

	_, m := getJSON(t, s, "/api/pr/review/suggestions")
	if m["stale"].(float64) != 1 {
		t.Fatalf("stale count = %v", m["stale"])
	}
	for _, raw := range m["suggestions"].([]any) {
		sg := raw.(map[string]any)
		if stale := sg["ai"].(map[string]any)["stale"] == true; stale != (sg["id"].(float64) == 2) {
			t.Errorf("suggestion %v stale = %v", sg["id"], stale)
		}
	}
	if code, _ := agentPostJSON(t, s, "/api/pr/review/triage", map[string]any{"ids": []int64{2}, "action": "accept"}); code != http.StatusConflict {
		t.Errorf("accepting a stale suggestion = %d, want 409", code)
	}
	if code, _ := agentPostJSON(t, s, "/api/pr/review/triage", map[string]any{"ids": []int64{2}, "action": "dismiss"}); code != 200 {
		t.Errorf("dismissing a stale suggestion must still work, got %d", code)
	}
	if code, m := agentPostJSON(t, s, "/api/pr/review/triage", map[string]any{"ids": []int64{3}, "action": "accept"}); code != 200 || m["staleRefused"].(float64) != 0 {
		t.Errorf("accepting a current suggestion = %d %v", code, m)
	}
	s.pr.remoteHead = "h3" // GitHub has moved on too: now everything pending is stale
	s.pr.comments[2].Status = prStatusPending
	if !s.pr.isStale(s.pr.comments[2]) {
		t.Error("a suggestion made on h2 is stale once GitHub reports h3")
	}
}

func TestRevalidateReanchorsAfterPull(t *testing.T) {
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
	// The PR head after a Pull: two new lines above the one the suggestion is on.
	os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n\n// A returns two.\n// Always.\nfunc A() int {\n\tx := 2\n\treturn x\n}\n"), 0o644)
	git("commit", "-q", "-am", "head")

	s := reviewTestServer(t)
	s.pr.worktree, s.pr.diffBase, s.pr.meta.HeadSHA = root, base, "h2"
	c := &s.pr.comments[1]
	c.Path, c.Line, c.Side = "a.go", 4, "RIGHT"
	c.AI = &aiSuggestion{Anchor: anchorAnchored, HeadSHA: "h1", ReportedPath: "a.go", ReportedLine: 4, ReportedSide: "RIGHT",
		Quote: "x := 2", ModelBody: "ai says"}

	code, m := agentPostJSON(t, s, "/api/pr/review/revalidate", map[string]any{})
	if code != 200 {
		t.Fatalf("revalidate = %d %v", code, m)
	}
	c = &s.pr.comments[1]
	if c.Line != 6 || c.AI.Anchor != anchorReanchored || c.AI.HeadSHA != "h2" || s.pr.isStale(*c) {
		t.Fatalf("after revalidate: line %d anchor %s head %s", c.Line, c.AI.Anchor, c.AI.HeadSHA)
	}
	if code, _ := agentPostJSON(t, s, "/api/pr/review/triage", map[string]any{"ids": []int64{2}, "action": "accept"}); code != 200 {
		t.Errorf("a re-checked suggestion can be accepted, got %d", code)
	}
}

func TestSubmitNoticesHeadMovedOnGitHub(t *testing.T) {
	orig := githubHTTPClient.Transport
	defer func() { githubHTTPClient.Transport = orig }()
	githubHTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body := "{}"
		if req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls/1") {
			body = `{"number": 1, "head": {"sha": "remote-head"}}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	s := reviewTestServer(t)
	s.pr.meta.HeadSHA = "local-head"
	s.pr.comments[2].AI.HeadSHA = "local-head"
	code, m := agentPostJSON(t, s, "/api/pr/submit", map[string]string{"event": "COMMENT"})
	if code != 200 || m["headMoved"] != true || s.pr.remoteHead != "remote-head" {
		t.Fatalf("submit = %d %v remoteHead %q", code, m, s.pr.remoteHead)
	}
	if !s.pr.isStale(s.pr.comments[len(s.pr.comments)-1]) {
		t.Error("pending suggestions go stale when GitHub has a newer head")
	}
}

// ---------------------------------------------------------------- Conversation (CNV-6, CNV-8)

func TestFetchCheckRuns(t *testing.T) {
	orig := githubHTTPClient.Transport
	defer func() { githubHTTPClient.Transport = orig }()
	var path string
	githubHTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		path = req.URL.Path + "?" + req.URL.RawQuery
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"total_count": 3, "check_runs": [
			{"name": "lint", "status": "completed", "conclusion": "success", "html_url": "https://ci/lint"},
			{"name": "test", "status": "completed", "conclusion": "failure", "started_at": "2026-09-01T10:00:00Z", "completed_at": "2026-09-01T10:04:00Z", "html_url": "https://ci/test"},
			{"name": "build", "status": "in_progress", "conclusion": null, "details_url": "https://ci/build"}]}`))}, nil
	})
	checks, err := fetchCheckRuns(t.Context(), "o", "r", "abc", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/repos/o/r/commits/abc/check-runs?per_page=100" {
		t.Errorf("path = %s", path)
	}
	var names []string
	for _, c := range checks {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "test,build,lint" || checks[1].URL != "https://ci/build" || checks[0].CompletedAt == "" {
		t.Fatalf("checks = %+v, want failures, then running, then the rest", checks)
	}
}

func TestResolveThread(t *testing.T) {
	orig := githubHTTPClient.Transport
	defer func() { githubHTTPClient.Transport = orig }()
	var sent []string
	githubHTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		var b []byte
		if req.Body != nil {
			b, _ = io.ReadAll(req.Body)
		}
		sent = append(sent, string(b))
		return &http.Response{StatusCode: 200, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"data":{"resolveReviewThread":{"thread":{"id":"TH1","isResolved":true}}}}`))}, nil
	})
	s := convTestServer(t, "tok")
	s.pr.conv = &convCache{}
	if code, m := agentPostJSON(t, s, "/api/pr/threads/resolve", map[string]any{"threadId": "TH1", "resolved": true}); code != 200 {
		t.Fatalf("resolve = %d %v", code, m)
	}
	if len(sent) != 1 || !strings.Contains(sent[0], "resolveReviewThread") || strings.Contains(sent[0], "unresolve") || !strings.Contains(sent[0], `"TH1"`) {
		t.Fatalf("mutation = %v", sent)
	}
	if s.pr.conv != nil {
		t.Error("resolving must drop the cached conversation")
	}
	agentPostJSON(t, s, "/api/pr/threads/resolve", map[string]any{"threadId": "TH1", "resolved": false})
	if !strings.Contains(sent[1], "unresolveReviewThread") {
		t.Errorf("unresolve mutation = %s", sent[1])
	}
	s.pr.token = ""
	if code, _ := agentPostJSON(t, s, "/api/pr/threads/resolve", map[string]any{"threadId": "TH1", "resolved": true}); code != http.StatusForbidden {
		t.Errorf("without a token = %d, want 403", code)
	}
}

func TestWithQueryParam(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:7777":            "http://127.0.0.1:7777/?view=inbox",
		"http://127.0.0.1:7777/":           "http://127.0.0.1:7777/?view=inbox",
		"http://127.0.0.1:7777/rev-1/":     "http://127.0.0.1:7777/rev-1/?view=inbox",
		"http://127.0.0.1:7777/?path=a.go": "http://127.0.0.1:7777/?path=a.go&view=inbox",
	}
	for in, want := range cases {
		if got := withQueryParam(in, "view", "inbox"); got != want {
			t.Errorf("withQueryParam(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------- Graph (GRF-7, GRF-8, GRF-10)

func TestGraphCommitInfo(t *testing.T) {
	g := graphTestRepo(t)
	d, err := graphCommitInfo(g.root, g.sha["M"])
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Parents) != 2 || d.Message != "M" || d.Author != "Tess" || d.Against != d.Parents[0] {
		t.Fatalf("merge detail = %+v", d)
	}
	if len(d.Files) != 1 || d.Files[0].Path != "k1.txt" || d.Files[0].Status != "A" || d.Files[0].Added != 1 {
		t.Fatalf("merge files against its first parent = %+v", d.Files)
	}
	root, err := graphCommitInfo(g.root, g.sha["c0"])
	if err != nil || root.Against != emptyTree || len(root.Files) != 1 || root.Files[0].Path != "c0.txt" {
		t.Fatalf("root commit = %+v %v", root, err)
	}
}

func TestParseDiffTreeZRenames(t *testing.T) {
	raw := ":100644 100644 aaa bbb M\x00src/a.go\x00" + ":100644 100644 ccc ccc R100\x00old/b.go\x00new/b.go\x00" +
		":000000 100644 000 ddd A\x00img.png\x00" +
		"3\t1\tsrc/a.go\x00" + "0\t0\t\x00old/b.go\x00new/b.go\x00" + "-\t-\timg.png\x00"
	files := parseDiffTreeZ(raw)
	if len(files) != 3 {
		t.Fatalf("files = %+v", files)
	}
	if files[0].Path != "src/a.go" || files[0].Added != 3 || files[0].Deleted != 1 {
		t.Errorf("modified = %+v", files[0])
	}
	if files[1].Status != "R" || files[1].OldPath != "old/b.go" || files[1].Path != "new/b.go" {
		t.Errorf("rename = %+v", files[1])
	}
	if files[2].Added != -1 {
		t.Errorf("binary = %+v", files[2])
	}
}

func TestGraphCommitDiffSearchAndSig(t *testing.T) {
	g := graphTestRepo(t)
	isolateSettings(t)
	ix := NewIndex(g.root)
	ix.Build()
	s := NewServer(ix, nil)
	t.Cleanup(func() {
		graphMu.Lock()
		if gs := graphSessions[g.root]; gs != nil {
			delete(graphSessions, g.root)
			gs.close()
		}
		graphMu.Unlock()
		if s.gitWatcher != nil {
			s.gitWatcher.Stop()
		}
	})

	code, m := getJSON(t, s, "/api/graph/commit?sha="+g.sha["k1"][:10])
	if code != 200 || m["message"] != "k1" || len(m["refs"].([]any)) != 1 {
		t.Fatalf("commit = %d %v", code, m)
	}
	if code, _ := getJSON(t, s, "/api/graph/commit?sha=../../etc"); code != http.StatusBadRequest {
		t.Errorf("non-hash sha = %d", code)
	}
	_, m = getJSON(t, s, "/api/graph/diff?sha="+g.sha["k1"]+"&path=k1.txt")
	if !strings.Contains(m["diff"].(string), "+k1") {
		t.Errorf("diff = %v", m["diff"])
	}
	_, m = getJSON(t, s, "/api/graph/diff?sha="+g.sha["c0"]+"&path=c0.txt")
	if !strings.Contains(m["diff"].(string), "+c0") {
		t.Errorf("root commit diff = %v", m["diff"])
	}

	_, m = getJSON(t, s, "/api/graph/search?q=o1")
	if len(m["matches"].([]any)) != 1 || m["complete"] != true {
		t.Errorf("search subject = %v", m)
	}
	_, m = getJSON(t, s, "/api/graph/search?q=tess")
	if len(m["matches"].([]any)) != 12 {
		t.Errorf("search author (any case) = %d matches", len(m["matches"].([]any)))
	}
	_, m = getJSON(t, s, "/api/graph/search?q="+g.sha["a1"][:8])
	if len(m["matches"].([]any)) != 1 {
		t.Errorf("search hash prefix = %v", m)
	}

	_, m = getJSON(t, s, "/api/graph/sig")
	before := m["sig"]
	os.WriteFile(filepath.Join(g.root, "z.txt"), []byte("z"), 0o644)
	gitTestRun(t, g.root, "add", ".")
	gitTestRun(t, g.root, "-c", "user.name=T", "-c", "user.email=t@t", "commit", "-q", "-m", "z")
	_, m = getJSON(t, s, "/api/graph/sig")
	if m["sig"] == before {
		t.Error("the ref signature must change with a new commit")
	}
}
