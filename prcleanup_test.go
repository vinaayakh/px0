package main

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// cleanupRepo is a repository with one commit, plus a temp dir for
// checkouts. It returns a git runner for the repository.
func cleanupRepo(t *testing.T) (repo, tmp string, git func(dir string, args ...string) string) {
	t.Helper()
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	base := t.TempDir()
	if r, err := filepath.EvalSymlinks(base); err == nil {
		base = r
	}
	repo, tmp = filepath.Join(base, "repo"), filepath.Join(base, "tmp")
	os.MkdirAll(repo, 0o755)
	os.MkdirAll(tmp, 0o755)
	git = func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n"), 0o644)
	git(repo, "init", "-q")
	git(repo, "config", "user.email", "t@example.com")
	git(repo, "config", "user.name", "T")
	git(repo, "config", "commit.gpgsign", "false")
	git(repo, "add", "-A")
	git(repo, "commit", "-qm", "init")
	return repo, tmp, git
}

// addReview makes what checkoutPR makes for PR num: its refs in repo and a
// worktree of them in tmp, with a marker owned by pid.
func addReview(t *testing.T, repo, tmp string, git func(string, ...string) string, num, pid int) string {
	t.Helper()
	wt := filepath.Join(tmp, prCheckoutPrefix+strings.Repeat("x", num))
	head := git(repo, "rev-parse", "HEAD")
	git(repo, "update-ref", "refs/px0/pr/"+strconv.Itoa(num), head)
	git(repo, "update-ref", "refs/px0/base/"+strconv.Itoa(num), head)
	git(repo, "worktree", "add", "-q", "--detach", wt, "refs/px0/pr/"+strconv.Itoa(num))
	m := prMarker{PID: pid, Worktree: wt, SrcRepo: repo, Number: num}
	b, _ := json.Marshal(m)
	os.WriteFile(prMarkerPath(wt), b, 0o644)
	return wt
}

func hasRef(git func(string, ...string) string, repo, ref string) bool {
	return strings.Contains(git(repo, "for-each-ref", "--format=%(refname)", "refs/px0/"), ref)
}

func TestPRSessionCloseRemovesEverything(t *testing.T) {
	repo, tmp, git := cleanupRepo(t)
	wt := addReview(t, repo, tmp, git, 7, os.Getpid())
	session := filepath.Join(tmp, "session.json")
	os.WriteFile(session, []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(wt, "edited.go"), []byte("dirty\n"), 0o644) // Close removes a dirty checkout too

	p := &prSession{worktree: wt, srcRepo: repo, sessionFile: session, meta: PRMeta{Number: 7}}
	p.Close()
	p.Close() // twice is fine

	for _, path := range []string{wt, prMarkerPath(wt), session} {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s still exists", path)
		}
	}
	if strings.Contains(git(repo, "worktree", "list"), wt) || strings.Contains(git(repo, "worktree", "list"), filepath.ToSlash(wt)) {
		t.Errorf("worktree still registered:\n%s", git(repo, "worktree", "list"))
	}
	if hasRef(git, repo, "refs/px0/") {
		t.Errorf("refs left: %s", git(repo, "for-each-ref", "refs/px0/"))
	}
}

func TestSweepStalePRCheckouts(t *testing.T) {
	repo, tmp, git := cleanupRepo(t)
	const livePID, deadPID = 1001, 1002
	alive := func(pid int) bool { return pid == livePID }

	live := addReview(t, repo, tmp, git, 1, livePID)
	dead := addReview(t, repo, tmp, git, 2, deadPID)
	// A dead review whose checkout is already gone: only its marker and refs remain.
	gone := addReview(t, repo, tmp, git, 3, deadPID)
	git(repo, "worktree", "remove", "--force", gone)
	// Unmarked checkouts: a fresh one may still be in the making; an old one is debris.
	young := filepath.Join(tmp, prCheckoutPrefix+"young")
	old := filepath.Join(tmp, prCheckoutPrefix+"old")
	os.MkdirAll(young, 0o755)
	os.MkdirAll(old, 0o755)
	past := time.Now().Add(-2 * prUnmarkedGrace)
	os.Chtimes(old, past, past)
	// Refs from a review px0 has no record of at all.
	git(repo, "update-ref", "refs/px0/pr/9", git(repo, "rev-parse", "HEAD"))
	// Something else in the temp dir is never touched.
	other := filepath.Join(tmp, "not-px0")
	os.MkdirAll(other, 0o755)

	removed := sweepStalePRCheckouts(tmp, repo, alive)

	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	if !exists(live) || !exists(prMarkerPath(live)) || !hasRef(git, repo, "refs/px0/pr/1") {
		t.Error("a live review must be left alone")
	}
	if exists(dead) || exists(prMarkerPath(dead)) || hasRef(git, repo, "refs/px0/pr/2") || hasRef(git, repo, "refs/px0/base/2") {
		t.Error("a dead review must be removed with its marker and refs")
	}
	if exists(prMarkerPath(gone)) || hasRef(git, repo, "refs/px0/pr/3") {
		t.Error("a dead review's marker and refs must go even when its checkout is gone")
	}
	if !exists(young) || exists(old) {
		t.Errorf("unmarked: young kept %v, old removed %v", exists(young), !exists(old))
	}
	if hasRef(git, repo, "refs/px0/pr/9") {
		t.Error("refs no live review owns must be removed")
	}
	if !exists(other) {
		t.Error("only px0 checkouts are swept")
	}
	if len(removed) != 2 {
		t.Errorf("removed = %v, want the dead and the old checkout", removed)
	}
	if list := git(repo, "worktree", "list"); strings.Contains(list, "xx") {
		t.Errorf("dead worktree still registered:\n%s", list)
	}
}

func TestEndReview(t *testing.T) {
	repo, tmp, git := cleanupRepo(t)
	wt := addReview(t, repo, tmp, git, 4, os.Getpid())
	s := reviewTestServer(t) // one human draft
	s.pr.worktree, s.pr.srcRepo = wt, repo
	s.pr.meta.Number, s.pr.meta.HeadSHA = 4, git(wt, "rev-parse", "HEAD")

	if code, _ := agentPostJSON(t, s, "/api/pr/end", map[string]any{"force": true}); code != http.StatusNotImplemented {
		t.Errorf("end without a hook = %d, want 501", code)
	}
	ended := 0
	s.SetOnEnd(func() { ended++ })

	// Something to lose: a draft, an uncommitted edit, an unpushed commit.
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package a // edited\n"), 0o644)
	git(wt, "commit", "-qam", "local")
	os.WriteFile(filepath.Join(wt, "b.go"), []byte("package a\n"), 0o644)
	git(wt, "add", "b.go")
	code, m := agentPostJSON(t, s, "/api/pr/end", map[string]any{})
	st, _ := m["state"].(map[string]any)
	if code != http.StatusConflict || m["needsConfirm"] != true || st["drafts"] != 1.0 || st["dirty"] != true || st["unpushed"] != 1.0 || ended != 0 {
		t.Fatalf("end with unsaved work = %d %v (ended %d)", code, m, ended)
	}
	code, m = agentPostJSON(t, s, "/api/pr/end", map[string]any{"force": true})
	if code != 200 || m["ok"] != true || m["worktree"] != wt || ended != 1 {
		t.Fatalf("forced end = %d %v (ended %d)", code, m, ended)
	}

	// Nothing to lose: no confirmation needed.
	s.pr.comments = nil
	git(wt, "reset", "-q", "--hard", s.pr.meta.HeadSHA)
	if code, _ := agentPostJSON(t, s, "/api/pr/end", map[string]any{}); code != 200 || ended != 2 {
		t.Errorf("clean end = %d (ended %d)", code, ended)
	}
}

func TestMarkerWrittenAndSessionFileOwned(t *testing.T) {
	tmp := t.TempDir()
	wt := filepath.Join(tmp, prCheckoutPrefix+"m")
	os.MkdirAll(wt, 0o755)
	p := &prSession{worktree: wt, srcRepo: "/r", meta: PRMeta{Number: 5}, sessionFile: "/s.json"}
	p.writeMarker()
	m, ok := readPRMarker(prMarkerPath(wt))
	if !ok || m.PID != os.Getpid() || m.Number != 5 || m.SrcRepo != "/r" || m.SessionFile != "/s.json" {
		t.Fatalf("marker = %+v %v", m, ok)
	}
	if !processAlive(os.Getpid()) || processAlive(0) {
		t.Error("processAlive")
	}
}
