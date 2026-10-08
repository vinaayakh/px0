package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// prFilesRepo commits a base, then a head that modifies, adds, deletes and
// renames files and adds one too big to send whole. It returns the root and
// both commits.
func prFilesRepo(t *testing.T) (root, base, head string) {
	t.Helper()
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	root = t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write("mod.go", "package a\n\nfunc A() int {\n\treturn 1\n}\n")
	write("del.go", "gone\n")
	write("old/name.go", "package b\n\n// a file long enough that git\n// sees the rename\n// as a rename\n// and not as\n// an add and a delete\n")
	git("init", "-q")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "T")
	git("config", "commit.gpgsign", "false")
	git("config", "core.autocrlf", "false")
	git("add", "-A")
	git("commit", "-qm", "base")
	base = git("rev-parse", "HEAD")

	write("mod.go", "package a\n\nfunc A() int {\n\treturn 2\n}\n")
	write("new.go", "package a\n\nvar New = true\n")
	os.Remove(filepath.Join(root, "del.go"))
	os.MkdirAll(filepath.Join(root, "new"), 0o755)
	git("mv", "old/name.go", "new/name.go")
	write("big.txt", strings.Repeat("line\n", prFileMaxRows+10))
	git("add", "-A")
	git("commit", "-qm", "head")
	head = git("rev-parse", "HEAD")
	return root, base, head
}

func prFilesServer(t *testing.T) *Server {
	t.Helper()
	root, base, head := prFilesRepo(t)
	isolateSettings(t)
	ix := NewIndex(root)
	ix.Build()
	s := NewServer(ix, nil)
	s.pr = &prSession{provider: &GitHubProvider{}, target: PRTarget{Provider: "github", Owner: "o", Repo: "r", Number: 1}, token: "tok"}
	s.diffBase, s.prHeadSHA = base, head
	return s
}

func TestPRFiles(t *testing.T) {
	s := prFilesServer(t)
	code, m := getJSON(t, s, "/api/pr/files")
	if code != 200 {
		t.Fatalf("files = %d %v", code, m)
	}
	var got struct {
		Files []PRFile `json:"files"`
	}
	b, _ := json.Marshal(m)
	json.Unmarshal(b, &got)
	byPath := map[string]PRFile{}
	for _, f := range got.Files {
		byPath[f.Path] = f
	}
	if len(got.Files) != 5 {
		t.Fatalf("files = %+v, want 5", got.Files)
	}
	if f := byPath["mod.go"]; f.Status != "modified" || f.Additions != 1 || f.Deletions != 1 || len(f.Hunks) != 1 {
		t.Errorf("mod.go = %+v", f)
	}
	if f := byPath["new.go"]; f.Status != "added" || f.Additions != 3 || len(f.Hunks) != 1 {
		t.Errorf("new.go = %+v", f)
	}
	if f := byPath["del.go"]; f.Status != "deleted" || f.Deletions != 1 {
		t.Errorf("del.go = %+v", f)
	}
	if f := byPath["new/name.go"]; f.Status != "renamed" || f.OldPath != "old/name.go" {
		t.Errorf("rename = %+v", f)
	}
	if f := byPath["big.txt"]; !f.TooLarge || len(f.Hunks) != 0 || f.Additions != prFileMaxRows+10 {
		t.Errorf("big.txt = tooLarge %v, %d hunks, +%d", f.TooLarge, len(f.Hunks), f.Additions)
	}
	// A highlighted row carries both its text and its line numbers.
	row := byPath["mod.go"].Hunks[0].Rows
	var sawAdd bool
	for _, r := range row {
		if r.Type == "add" && r.NewLine == 4 && strings.Contains(r.Text, "return 2") {
			sawAdd = true
		}
	}
	if !sawAdd {
		t.Errorf("mod.go rows = %+v", row)
	}

	// One file on request comes whole, however big.
	code, m = getJSON(t, s, "/api/pr/files?path=big.txt")
	b, _ = json.Marshal(m)
	got.Files = nil
	json.Unmarshal(b, &got)
	if code != 200 || len(got.Files) != 1 || got.Files[0].TooLarge || len(got.Files[0].Hunks) != 1 {
		t.Fatalf("big.txt alone = %d %+v", code, got.Files)
	}
	// A rename is found by either of its paths.
	if code, m = getJSON(t, s, "/api/pr/files?path=old/name.go"); code != 200 {
		t.Errorf("rename by old path = %d %v", code, m)
	}
	for _, bad := range []string{"../x", "untouched.go"} {
		if code, _ := getJSON(t, s, "/api/pr/files?path="+bad); code == 200 {
			t.Errorf("path %q = 200", bad)
		}
	}
}

func TestSplitDiffFiles(t *testing.T) {
	diff := "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\ndiff --git a/y b/y\nnew file mode 100644\n--- /dev/null\n+++ b/y\n@@ -0,0 +1 @@\n+c"
	blocks := splitDiffFiles(diff)
	if len(blocks) != 2 || !strings.HasPrefix(blocks[1], "diff --git a/y") || !strings.HasSuffix(blocks[0], "+b\n") {
		t.Fatalf("blocks = %q", blocks)
	}
	if splitDiffFiles("") != nil {
		t.Error("an empty diff has no files")
	}
}

// fakeReviewPost records what was posted to the reviews endpoint.
func fakeReviewPost(t *testing.T, status int) *[]map[string]any {
	t.Helper()
	var posts []map[string]any
	orig := githubHTTPClient.Transport
	t.Cleanup(func() { githubHTTPClient.Transport = orig })
	githubHTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		var m map[string]any
		json.NewDecoder(req.Body).Decode(&m)
		if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/reviews") {
			posts = append(posts, m)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
	})
	return &posts
}

func TestPostCommentNow(t *testing.T) {
	posts := fakeReviewPost(t, 200)
	s := reviewTestServer(t) // 1 a human draft, 2 and 3 pending AI suggestions
	s.pr.meta.HeadSHA = "h1"

	// A new comment goes out alone, as a COMMENT review on the head reviewed.
	code, m := agentPostJSON(t, s, "/api/pr/comments/post", map[string]any{"path": "a.go", "line": 9, "startLine": 7, "side": "right", "body": " now "})
	if code != 200 || len(*posts) != 1 {
		t.Fatalf("post new = %d %v, %d posts", code, m, len(*posts))
	}
	p := (*posts)[0]
	cs := p["comments"].([]any)
	c0 := cs[0].(map[string]any)
	if p["event"] != "COMMENT" || p["commit_id"] != "h1" || len(cs) != 1 || c0["body"] != "now" || c0["start_line"] != 7.0 || c0["side"] != "RIGHT" {
		t.Errorf("posted = %v", p)
	}
	if len(s.pr.comments) != 3 {
		t.Errorf("posting a new comment must not add a draft")
	}

	// An AI suggestion posted with an edit carries its suggestion block and
	// stays behind as posted.
	code, _ = agentPostJSON(t, s, "/api/pr/comments/post", map[string]any{"id": 2, "body": "better words"})
	if code != 200 {
		t.Fatalf("post suggestion = %d", code)
	}
	body := (*posts)[1]["comments"].([]any)[0].(map[string]any)["body"].(string)
	if !strings.HasPrefix(body, "better words") || !strings.Contains(body, "```suggestion\nx := 1") {
		t.Errorf("suggestion body = %q", body)
	}
	if c := s.pr.comments[1]; c.Status != prStatusPosted || c.Body != "better words" || c.AI.OriginalBody != "ai says" {
		t.Errorf("after posting = %+v", c)
	}
	if code, _ := agentPostJSON(t, s, "/api/pr/comments/post", map[string]any{"id": 2}); code != http.StatusConflict {
		t.Errorf("posting twice = %d, want 409", code)
	}
	if code, _ := agentPostJSON(t, s, "/api/pr/review/triage", map[string]any{"ids": []int64{2}, "action": "restore"}); code != http.StatusNotFound {
		t.Errorf("restoring a posted suggestion = %d, want 404", code)
	}

	// A human draft posted on its own leaves the drafts.
	if code, _ := agentPostJSON(t, s, "/api/pr/comments/post", map[string]any{"id": 1}); code != 200 {
		t.Fatalf("post draft = %d", code)
	}
	for _, c := range s.pr.comments {
		if c.ID == 1 {
			t.Error("a posted draft must leave the list")
		}
	}
	if countSubmittable(s.pr.comments) != 0 {
		t.Errorf("drafts left = %d", countSubmittable(s.pr.comments))
	}
}

func TestPostCommentNowFailures(t *testing.T) {
	posts := fakeReviewPost(t, 422)
	s := reviewTestServer(t)
	if code, _ := agentPostJSON(t, s, "/api/pr/comments/post", map[string]any{"id": 1}); code != http.StatusBadGateway {
		t.Errorf("GitHub refusing = %d, want 502", code)
	}
	if len(*posts) != 1 || len(s.pr.comments) != 3 || s.pr.comments[0].ID != 1 {
		t.Errorf("a refused post must keep the draft: %+v", s.pr.comments)
	}
	if code, _ := agentPostJSON(t, s, "/api/pr/comments/post", map[string]any{"path": "a.go", "body": "x"}); code != http.StatusBadRequest {
		t.Errorf("no line = %d, want 400", code)
	}
	if code, _ := agentPostJSON(t, s, "/api/pr/comments/post", map[string]any{"id": 99}); code != http.StatusNotFound {
		t.Errorf("unknown id = %d, want 404", code)
	}
	s.pr.token = ""
	if code, _ := agentPostJSON(t, s, "/api/pr/comments/post", map[string]any{"id": 1}); code != http.StatusForbidden {
		t.Errorf("no token = %d, want 403", code)
	}
}

func TestEditDraftAndRangeDraft(t *testing.T) {
	s := reviewTestServer(t)
	if code, m := agentPostJSON(t, s, "/api/pr/comments/edit?id=1", map[string]any{"body": " reworded "}); code != 200 || m["body"] != "reworded" {
		t.Fatalf("edit = %d %v", code, m)
	}
	if code, _ := agentPostJSON(t, s, "/api/pr/comments/edit?id=2", map[string]any{"body": "x"}); code != http.StatusNotFound {
		t.Errorf("editing an AI suggestion here = %d, want 404 (it goes through triage)", code)
	}
	if code, _ := agentPostJSON(t, s, "/api/pr/comments/edit?id=1", map[string]any{"body": "  "}); code != http.StatusBadRequest {
		t.Errorf("empty edit = %d, want 400", code)
	}
	code, m := agentPostJSON(t, s, "/api/pr/comments", map[string]any{"path": "a.go", "line": 6, "startLine": 4, "body": "range"})
	if code != 200 || m["startLine"] != 4.0 {
		t.Errorf("range draft = %d %v", code, m)
	}
}

func TestMarkdownRender(t *testing.T) {
	s := reviewTestServer(t)
	code, m := agentPostJSON(t, s, "/api/markdown/render", map[string]any{"text": "**bold** `code`"})
	if html, _ := m["html"].(string); code != 200 || !strings.Contains(html, "<strong>bold</strong>") || !strings.Contains(html, "<code>code</code>") {
		t.Errorf("render = %d %v", code, m)
	}
}
