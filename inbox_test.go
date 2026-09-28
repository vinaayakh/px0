package main

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const inboxSearchPage = `{"data":{"search":{"issueCount": 3, "pageInfo": {"hasNextPage": false, "endCursor": "S1"}, "nodes": [
  {"number": 7, "title": "Fix race", "url": "https://github.com/o/r/pull/7", "isDraft": false, "createdAt": "2026-09-01T00:00:00Z", "updatedAt": "2026-09-20T00:00:00Z",
   "reviewDecision": "APPROVED", "author": {"login": "alice"}, "repository": {"nameWithOwner": "o/r"},
   "commits": {"nodes": [{"commit": {"statusCheckRollup": {"state": "SUCCESS"}}}]}},
  {"number": 8, "title": "WIP", "url": "https://github.com/o/r/pull/8", "isDraft": true, "createdAt": "2026-09-02T00:00:00Z", "updatedAt": "2026-09-21T00:00:00Z",
   "reviewDecision": "CHANGES_REQUESTED", "author": null, "repository": {"nameWithOwner": "o/r"},
   "commits": {"nodes": [{"commit": {"statusCheckRollup": {"state": "ERROR"}}}]}},
  {"number": 9, "title": "No checks", "url": "https://github.com/x/y/pull/9", "isDraft": false, "createdAt": "2026-09-03T00:00:00Z", "updatedAt": "2026-09-22T00:00:00Z",
   "reviewDecision": "REVIEW_REQUIRED", "author": {"login": "bob"}, "repository": {"nameWithOwner": "x/y"},
   "commits": {"nodes": [{"commit": {"statusCheckRollup": null}}]}},
  {"number": 10, "title": "Running", "url": "https://github.com/x/y/pull/10", "reviewDecision": null, "author": {"login": "bob"}, "repository": {"nameWithOwner": "x/y"},
   "commits": {"nodes": [{"commit": {"statusCheckRollup": {"state": "PENDING"}}}]}},
  {}
]}}}`

func TestParseInboxSearch(t *testing.T) {
	var d gqlSearchData
	if err := decodeGraphQL([]byte(inboxSearchPage), &d); err != nil {
		t.Fatal(err)
	}
	got := parseInboxSearch(d)
	if len(got) != 4 {
		t.Fatalf("got %d summaries, want 4 (the empty node is skipped): %+v", len(got), got)
	}
	want := []struct {
		ci, review, author, repo string
		draft                    bool
	}{
		{"pass", "approved", "alice", "o/r", false},
		{"fail", "changes_requested", "ghost", "o/r", true},
		{"", "", "bob", "x/y", false},
		{"pending", "", "bob", "x/y", false},
	}
	for i, w := range want {
		g := got[i]
		if g.CI != w.ci || g.Review != w.review || g.Author != w.author || g.Repo != w.repo || g.Draft != w.draft {
			t.Errorf("summary %d = %+v, want %+v", i, g, w)
		}
	}
	if got[0].URL != "https://github.com/o/r/pull/7" || got[0].Number != 7 || got[0].UpdatedAt == "" {
		t.Errorf("summary 0 fields = %+v", got[0])
	}
}

func fakeSearch(t *testing.T, calls *int32, pages int) func() {
	t.Helper()
	orig := githubHTTPClient.Transport
	githubHTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		n := atomic.AddInt32(calls, 1)
		resp := inboxSearchPage
		if int(n) < pages {
			resp = strings.Replace(resp, `"hasNextPage": false`, `"hasNextPage": true`, 1)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(resp)), Header: make(http.Header)}, nil
	})
	return func() { githubHTTPClient.Transport = orig }
}

func TestListPRsPaging(t *testing.T) {
	var calls int32
	defer fakeSearch(t, &calls, 99)() // GitHub keeps saying there is more
	items, total, err := listPRs(t.Context(), "tok", "is:pr")
	if err != nil {
		t.Fatal(err)
	}
	if calls != inboxMaxPages || len(items) != 4*inboxMaxPages || total != 3 {
		t.Fatalf("%d calls, %d items, total %d; want %d calls", calls, len(items), total, inboxMaxPages)
	}
}

func TestGitHubRepoFromRemote(t *testing.T) {
	cases := map[string]string{
		"https://github.com/o/r.git":          "o/r",
		"https://github.com/o/r":              "o/r",
		"https://token@github.com/o/r.git\n":  "o/r",
		"git@github.com:o/r.git":              "o/r",
		"ssh://git@github.com/o/r.git":        "o/r",
		"ssh://git@github.com:22/o/r":         "o/r",
		"git://github.com/o/r.git":            "o/r",
		"https://github.com/o/r/":             "o/r",
		"https://gitlab.com/o/r.git":          "",
		"git@github.example.com:o/r.git":      "",
		"https://github.com/o":                "",
		"https://github.com/o/r/pull/1":       "",
		"/home/me/repos/r":                    "",
		"https://github.com.evil.com/o/r.git": "",
		"https://evil.com/github.com/o/r.git": "",
		"":                                    "",
	}
	for in, want := range cases {
		if got := githubRepoFromRemote(in); got != want {
			t.Errorf("githubRepoFromRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

// setInboxToken fixes the token the inbox resolves, whatever the machine has.
func setInboxToken(t *testing.T, tok string) {
	t.Helper()
	inboxMu.Lock()
	inboxTok, inboxTokFrom, inboxTokAt = tok, "test", time.Now().Add(time.Hour)
	inboxCache = map[string]inboxEntry{}
	inboxMu.Unlock()
	t.Cleanup(func() {
		inboxMu.Lock()
		inboxTok, inboxTokAt = "", time.Time{}
		inboxCache = map[string]inboxEntry{}
		inboxMu.Unlock()
	})
}

func inboxServer(t *testing.T) *Server {
	t.Helper()
	isolateSettings(t)
	ix := NewIndex(t.TempDir())
	ix.Build()
	return NewServer(ix, nil)
}

func TestInboxWithoutToken(t *testing.T) {
	setInboxToken(t, "")
	var calls int32
	defer fakeSearch(t, &calls, 1)()
	s := inboxServer(t)
	code, m := getJSON(t, s, "/api/inbox?section=review")
	if code != 200 || m["needsToken"] != true || len(m["items"].([]any)) != 0 || calls != 0 {
		t.Fatalf("= %d %v after %d calls", code, m, calls)
	}
}

func TestInboxSections(t *testing.T) {
	setInboxToken(t, "tok")
	var calls int32
	defer fakeSearch(t, &calls, 1)()
	s := inboxServer(t)

	if code, _ := getJSON(t, s, "/api/inbox?section=everything"); code != http.StatusBadRequest {
		t.Errorf("unknown section = %d, want 400", code)
	}
	// Not a git checkout: no origin, so the repo section is hidden.
	if _, m := getJSON(t, s, "/api/inbox?section=repo"); m["hidden"] != true || calls != 0 {
		t.Errorf("repo section without an origin = %v", m)
	}

	code, m := getJSON(t, s, "/api/inbox?section=review")
	if code != 200 || len(m["items"].([]any)) != 4 || calls != 1 {
		t.Fatalf("review = %d %v (%d calls)", code, m, calls)
	}
	getJSON(t, s, "/api/inbox?section=review")
	if calls != 1 {
		t.Errorf("a second load within %s must be served from the cache", inboxCacheTTL)
	}
	getJSON(t, s, "/api/inbox?section=review&refresh=1")
	if calls != 2 {
		t.Errorf("refresh=1 must search again")
	}

	// In a PR session the repo section is the PR's repository.
	s.pr = &prSession{target: PRTarget{Provider: "github", Owner: "o", Repo: "r", Number: 7, URL: "https://github.com/o/r/pull/7"}}
	_, m = getJSON(t, s, "/api/inbox?section=repo")
	if m["repo"] != "o/r" || m["current"] != "https://github.com/o/r/pull/7" {
		t.Errorf("repo section in a PR session = %v", m)
	}
}

// TestHelperLaunchChild is not a test: the launch tests run the test binary
// as a stand-in child px0.
func TestHelperLaunchChild(t *testing.T) {
	switch os.Getenv("PX0_HELPER_LAUNCH") {
	case "serve":
		os.Stdout.WriteString("Preparing PR #7 (o/r)...\n  \x1b[2murl:\x1b[0m        \x1b[36mhttp://127.0.0.1:65001/\x1b[0m\nctrl-c to stop\n")
		time.Sleep(1500 * time.Millisecond)
		os.Exit(0)
	case "fail":
		os.Stdout.WriteString("Failed to prepare PR #7: checkout failed\n")
		os.Exit(1)
	default:
		t.Skip("helper process")
	}
}

func useLaunchHelper(t *testing.T, mode string, spawns *int32) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	orig := launchCommand
	launchCommand = func(target, dir string) (*exec.Cmd, error) {
		atomic.AddInt32(spawns, 1)
		cmd := exec.Command(exe, "-test.run=^TestHelperLaunchChild$")
		cmd.Env = append(os.Environ(), "PX0_HELPER_LAUNCH="+mode)
		cmd.Dir = dir
		return cmd, nil
	}
	launchMu.Lock()
	launched = map[string]*launchedPR{}
	launchMu.Unlock()
	t.Cleanup(func() { launchCommand = orig })
}

func waitLaunch(t *testing.T, s *Server, target string, until func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		code, m := getJSON(t, s, "/api/pr/launch?target="+target)
		if until(m) || (code == 404 && until(nil)) {
			return m
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("launch did not reach the expected state")
	return nil
}

func TestLaunchReusesRunningChild(t *testing.T) {
	var spawns int32
	useLaunchHelper(t, "serve", &spawns)
	s := inboxServer(t)
	const target = "https://github.com/o/r/pull/7"

	code, m := agentPostJSON(t, s, "/api/pr/launch", map[string]string{"target": target})
	if code != 200 || m["already"] != false {
		t.Fatalf("first launch = %d %v", code, m)
	}
	st := waitLaunch(t, s, target, func(m map[string]any) bool { return m != nil && m["state"] == "running" })
	if st["url"] != "http://127.0.0.1:65001/" {
		t.Fatalf("child URL = %v (ANSI codes must be stripped)", st["url"])
	}
	// Another spelling of the same PR finds the same child.
	code, m = agentPostJSON(t, s, "/api/pr/launch", map[string]string{"target": "https://github.com/O/R/pull/7/files"})
	if code != 200 || m["already"] != true || spawns != 1 {
		t.Fatalf("second launch = %d %v after %d spawns; want the running child", code, m, spawns)
	}
	// When the child exits it is forgotten, and the next click starts a new one.
	waitLaunch(t, s, target, func(m map[string]any) bool { return m == nil })
}

func TestLaunchReportsFailureAndRetries(t *testing.T) {
	var spawns int32
	useLaunchHelper(t, "fail", &spawns)
	s := inboxServer(t)
	const target = "https://github.com/o/r/pull/7"
	agentPostJSON(t, s, "/api/pr/launch", map[string]string{"target": target})
	st := waitLaunch(t, s, target, func(m map[string]any) bool { return m != nil && m["state"] == "failed" })
	if !strings.Contains(st["error"].(string), "checkout failed") {
		t.Fatalf("error = %v", st["error"])
	}
	if _, m := agentPostJSON(t, s, "/api/pr/launch", map[string]string{"target": target}); m["already"] != false || spawns != 2 {
		t.Fatalf("a failed launch must be retried, got %v after %d spawns", m, spawns)
	}
}

func TestLaunchRefusesOwnPRAndBadTargets(t *testing.T) {
	var spawns int32
	useLaunchHelper(t, "serve", &spawns)
	s := inboxServer(t)
	s.pr = &prSession{target: PRTarget{Provider: "github", Owner: "o", Repo: "r", Number: 7, URL: "https://github.com/o/r/pull/7"}}
	if _, m := agentPostJSON(t, s, "/api/pr/launch", map[string]string{"target": "https://github.com/o/r/pull/7"}); m["self"] != true || spawns != 0 {
		t.Fatalf("launching the session's own PR = %v", m)
	}
	if code, _ := agentPostJSON(t, s, "/api/pr/launch", map[string]string{"target": "https://example.com/x"}); code != http.StatusBadRequest {
		t.Errorf("non-PR target = %d, want 400", code)
	}
}
