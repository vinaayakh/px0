package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPRWarmTake(t *testing.T) {
	var nilWarm *prWarm[int]
	if _, _, ok := nilWarm.take(context.Background()); ok {
		t.Error("nil warm must not be ok")
	}
	w := startWarm(func(context.Context) (int, error) { return 42, nil })
	if v, at, ok := w.take(context.Background()); !ok || v != 42 || at.IsZero() {
		t.Errorf("take = %v %v %v", v, at, ok)
	}
	failed := startWarm(func(context.Context) (int, error) { return 0, errors.New("boom") })
	if _, _, ok := failed.take(context.Background()); ok {
		t.Error("a failed fetch must not be ok")
	}
	old := startWarm(func(context.Context) (int, error) { return 1, nil })
	<-old.done
	old.at = time.Now().Add(-2 * prWarmMaxAge)
	if _, _, ok := old.take(context.Background()); ok {
		t.Error("a result older than prWarmMaxAge must not be used")
	}
	slow := startWarm(func(ctx context.Context) (int, error) { time.Sleep(time.Second); return 1, nil })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, ok := slow.take(ctx); ok {
		t.Error("take must give up when its context ends")
	}
}

// checkoutFixture is a local stand-in for GitHub: an "upstream" repository
// at .../o/r.git with main and refs/pull/5/head, a clone of it to review
// from, and a fake API that counts its calls.
func checkoutFixture(t *testing.T) (clone, prHead, mergeBase string, calls map[string]*int32) {
	t.Helper()
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	isolateSettings(t)
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	base := t.TempDir()
	if r, err := filepath.EvalSymlinks(base); err == nil {
		base = r
	}
	work := filepath.Join(base, "work")
	upstream := filepath.ToSlash(filepath.Join(base, "o", "r.git"))
	clone = filepath.Join(base, "clone")
	os.MkdirAll(work, 0o755)
	gitTestRun(t, work, "init", "-q", "-b", "main")
	gitTestRun(t, work, "config", "user.email", "t@example.com")
	gitTestRun(t, work, "config", "user.name", "T")
	os.WriteFile(filepath.Join(work, "a.go"), []byte("package a\n"), 0o644)
	gitTestRun(t, work, "add", "-A")
	gitTestRun(t, work, "commit", "-qm", "base")
	mergeBase = strings.TrimSpace(gitTestRun(t, work, "rev-parse", "HEAD"))
	gitTestRun(t, work, "checkout", "-qb", "feature")
	os.WriteFile(filepath.Join(work, "b.go"), []byte("package a\n\nvar B = 1\n"), 0o644)
	gitTestRun(t, work, "add", "-A")
	gitTestRun(t, work, "commit", "-qm", "feature")
	prHead = strings.TrimSpace(gitTestRun(t, work, "rev-parse", "HEAD"))
	gitTestRun(t, work, "checkout", "-q", "main")
	os.WriteFile(filepath.Join(work, "c.go"), []byte("package a\n"), 0o644) // main moves on after the PR branched
	gitTestRun(t, work, "add", "-A")
	gitTestRun(t, work, "commit", "-qm", "main moves")
	gitTestRun(t, base, "clone", "-q", "--bare", work, upstream)
	gitTestRun(t, upstream, "update-ref", "refs/pull/5/head", prHead)
	gitTestRun(t, base, "clone", "-q", upstream, clone)

	calls = map[string]*int32{"meta": new(int32), "comments": new(int32), "graphql": new(int32)}
	orig := githubHTTPClient.Transport
	t.Cleanup(func() { githubHTTPClient.Transport = orig })
	githubHTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body, status := "[]", http.StatusOK
		switch p := req.URL.Path; {
		case strings.HasSuffix(p, "/pulls/5"):
			atomic.AddInt32(calls["meta"], 1)
			body = `{"number":5,"title":"Add B","state":"open","user":{"login":"bob"},"base":{"ref":"main"},
				"head":{"ref":"feature","sha":"` + prHead + `","repo":{"clone_url":"` + upstream + `","full_name":"o/r"}}}`
		case strings.Contains(p, "/comments"):
			atomic.AddInt32(calls["comments"], 1)
		case strings.HasSuffix(p, "/graphql"):
			atomic.AddInt32(calls["graphql"], 1)
			body = `{"data":{}}`
		default:
			status, body = http.StatusNotFound, `{"message":"Not Found"}`
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	return clone, prHead, mergeBase, calls
}

func TestCheckoutPRFromLocalClone(t *testing.T) {
	clone, prHead, mergeBase, calls := checkoutFixture(t)
	target := PRTarget{Provider: "github", Owner: "o", Repo: "r", Number: 5, URL: "https://github.com/o/r/pull/5"}
	p, err := checkoutPR(context.Background(), &GitHubProvider{}, target, clone, nil)
	if err != nil {
		t.Fatalf("checkoutPR: %v", err)
	}
	t.Cleanup(p.Close)

	if !samePath(p.srcRepo, clone) || p.srcRemote != "origin" {
		t.Errorf("checked out of %q via %q, want the clone via origin", p.srcRepo, p.srcRemote)
	}
	if head := strings.TrimSpace(gitTestRun(t, p.worktree, "rev-parse", "HEAD")); head != prHead {
		t.Errorf("worktree HEAD = %s, want the PR head %s", head, prHead)
	}
	if p.diffBase != mergeBase || p.diffBaseWarning != "" {
		t.Errorf("diffBase = %q (%q), want the merge-base %s", p.diffBase, p.diffBaseWarning, mergeBase)
	}
	if _, err := os.Stat(filepath.Join(p.worktree, "b.go")); err != nil {
		t.Error("the PR's files must be checked out")
	}
	refs := gitTestRun(t, clone, "for-each-ref", "--format=%(refname)", "refs/px0/")
	if !strings.Contains(refs, "refs/px0/pr/5") || !strings.Contains(refs, "refs/px0/base/5") {
		t.Errorf("refs = %q", refs)
	}
	if m, ok := readPRMarker(prMarkerPath(p.worktree)); !ok || m.Number != 5 {
		t.Error("the review's marker must be written")
	}

	// The page's first requests were started with the checkout: the first
	// takes the result, the next fetches again.
	if atomic.LoadInt32(calls["meta"]) != 1 {
		t.Errorf("metadata fetched %d times", *calls["meta"])
	}
	before := atomic.LoadInt32(calls["comments"])
	if before == 0 {
		t.Error("existing comments should have been fetched alongside the checkout")
	}
	s := NewServer(NewIndex(p.worktree), nil)
	s.SetPR(p)
	for i := 0; i < 2; i++ {
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/pr/existing-comments", nil))
		if rr.Code != 200 {
			t.Fatalf("existing-comments = %d %s", rr.Code, rr.Body)
		}
	}
	if got := atomic.LoadInt32(calls["comments"]) - before; got == 0 {
		t.Error("the second request must fetch fresh, not reuse the warmed result")
	}
	if p.warm.comments != nil {
		t.Error("the warmed comments must be handed out once")
	}
	if atomic.LoadInt32(calls["graphql"]) == 0 || p.warm.conv == nil {
		t.Error("with a token, the conversation should be warming")
	}
}

// A PR whose metadata cannot be fetched leaves nothing behind: no checkout
// and no ref from the head fetch that ran alongside.
func TestCheckoutPRFailureLeavesNothing(t *testing.T) {
	clone, _, _, _ := checkoutFixture(t)
	githubHTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(`{"message":"Not Found"}`)), Header: make(http.Header)}, nil
	})
	before, _ := filepath.Glob(filepath.Join(os.TempDir(), prCheckoutPrefix+"*"))
	target := PRTarget{Provider: "github", Owner: "o", Repo: "r", Number: 5}
	if _, err := checkoutPR(context.Background(), &GitHubProvider{}, target, clone, nil); err == nil {
		t.Fatal("checkoutPR should fail without metadata")
	}
	if refs := gitTestRun(t, clone, "for-each-ref", "refs/px0/"); refs != "" {
		t.Errorf("refs left: %s", refs)
	}
	after, _ := filepath.Glob(filepath.Join(os.TempDir(), prCheckoutPrefix+"*"))
	if len(after) > len(before) {
		t.Errorf("a checkout was left in the temp dir: %v", after)
	}
}

// A crashed review of the same PR as a live one names the same refs: the
// sweep removes its checkout but leaves the refs the live review uses.
func TestSweepKeepsRefsOfLiveReviewOfSamePR(t *testing.T) {
	repo, tmp, git := cleanupRepo(t)
	live := addReview(t, repo, tmp, git, 6, 1001)
	_ = live
	deadDir := filepath.Join(tmp, prCheckoutPrefix+"dead6")
	os.MkdirAll(deadDir, 0o755)
	mb, _ := json.Marshal(prMarker{PID: 1002, Worktree: deadDir, SrcRepo: repo, Number: 6})
	os.WriteFile(prMarkerPath(deadDir), mb, 0o644)

	sweepStalePRCheckouts(tmp, repo, func(pid int) bool { return pid == 1001 })
	if _, err := os.Stat(deadDir); err == nil {
		t.Error("the dead checkout must be removed")
	}
	if !hasRef(git, repo, "refs/px0/pr/6") || !hasRef(git, repo, "refs/px0/base/6") {
		t.Error("the live review's refs must stay")
	}
}

func TestStaticServedPrecompressed(t *testing.T) {
	s := inboxServer(t)
	raw, err := fs.ReadFile(assets, "web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req := httptest.NewRequest(method, "/static/app.js", nil)
		req.Header.Set("Accept-Encoding", "gzip, deflate")
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		h := rr.Header()
		if rr.Code != 200 || h.Get("Content-Encoding") != "gzip" || !strings.Contains(h.Get("Content-Type"), "javascript") || h.Get("Content-Security-Policy") == "" {
			t.Fatalf("%s app.js = %d %v", method, rr.Code, h)
		}
		if method == http.MethodHead {
			if rr.Body.Len() != 0 {
				t.Error("HEAD must not send a body")
			}
			continue
		}
		zr, err := gzip.NewReader(rr.Body)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(zr)
		if !bytes.Equal(got, raw) {
			t.Error("the precompressed app.js must decompress to the embedded file")
		}
	}
	// themes.css is generated, and a missing file is a 404: neither is precompressed.
	for path, want := range map[string]int{"/static/themes.css": 200, "/static/nope.js": 404} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		if rr.Code != want {
			t.Errorf("%s = %d, want %d", path, rr.Code, want)
		}
	}
	if staticGzCache["themes.css"] != nil {
		t.Error("themes.css must not be cached")
	}
}
