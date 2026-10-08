package main

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cloneDir makes a git repository with the given remotes (name -> URL).
func cloneDir(t *testing.T, remotes ...[2]string) string {
	t.Helper()
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	for _, r := range remotes {
		run("remote", "add", r[0], r[1])
	}
	return dir
}

func TestRepoFromRemoteURL(t *testing.T) {
	for url, want := range map[string]string{
		"https://github.com/Octo/Hello.git":       "Octo/Hello",
		"https://github.com/octo/hello":           "octo/hello",
		"git@github.com:octo/hello.git":           "octo/hello",
		"ssh://git@github.com:22/octo/hello.git":  "octo/hello",
		"https://user@ghe.example.com/a/b.c.git/": "a/b.c",
		"https://gitlab.com/group/sub/proj.git":   "sub/proj",
		"not a url":                               "",
	} {
		if got := repoFromRemoteURL(url); got != want {
			t.Errorf("repoFromRemoteURL(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestCloneOfPrefersOriginThenUpstream(t *testing.T) {
	fork := cloneDir(t,
		[2]string{"mirror", "https://github.com/up/proj.git"},
		[2]string{"upstream", "https://github.com/up/proj.git"},
		[2]string{"origin", "git@github.com:me/proj.git"})
	sub := filepath.Join(fork, "pkg")
	os.MkdirAll(sub, 0o755)

	if top, rem, ok := cloneOf(sub, "UP", "proj"); !ok || rem != "upstream" || !samePath(top, fork) {
		t.Errorf("cloneOf(up/proj) = %q %q %v, want the fork's top level via upstream", top, rem, ok)
	}
	if _, rem, ok := cloneOf(fork, "me", "proj"); !ok || rem != "origin" {
		t.Errorf("cloneOf(me/proj) = %q %v, want origin", rem, ok)
	}
	if _, _, ok := cloneOf(fork, "other", "proj"); ok {
		t.Error("a clone of neither must not match")
	}
	if _, _, ok := cloneOf(t.TempDir(), "up", "proj"); ok {
		t.Error("a plain folder must not match")
	}
}

func TestSavedReposAddFindRemove(t *testing.T) {
	isolateSettings(t)
	a := cloneDir(t, [2]string{"origin", "https://github.com/o/a.git"})
	fork := cloneDir(t, [2]string{"origin", "https://github.com/me/b.git"}, [2]string{"upstream", "https://github.com/o/b.git"})

	if _, _, ok := findLocalClone("o", "a"); ok {
		t.Fatal("nothing is saved yet")
	}
	r, err := addRepo(filepath.Join(a, ".")) // any spelling of the folder
	if err != nil || r.Repo != "o/a" || r.Remote != "origin" || !samePath(r.Path, a) {
		t.Fatalf("addRepo = %+v, %v", r, err)
	}
	if _, err := addRepo(a); err != nil { // again: refreshed, not duplicated
		t.Fatal(err)
	}
	if _, err := addRepo(fork); err != nil {
		t.Fatal(err)
	}
	if got := savedRepos(); len(got) != 2 {
		t.Fatalf("saved = %+v, want 2", got)
	}
	if top, rem, ok := findLocalClone("O", "A"); !ok || rem != "origin" || !samePath(top, a) {
		t.Errorf("find o/a = %q %q %v", top, rem, ok)
	}
	// The fork is saved as me/b but also holds o/b through upstream.
	if top, rem, ok := findLocalClone("o", "b"); !ok || rem != "upstream" || !samePath(top, fork) {
		t.Errorf("find o/b = %q %q %v", top, rem, ok)
	}

	// A saved clone that no longer points at the repository is skipped.
	exec.Command("git", "-C", a, "remote", "set-url", "origin", "https://github.com/o/z.git").Run()
	if _, _, ok := findLocalClone("o", "a"); ok {
		t.Error("a clone whose remote moved on must not be used")
	}

	if err := removeRepo(strings.ToUpper(a[:1]) + a[1:]); err != nil {
		t.Fatal(err)
	}
	if got := savedRepos(); len(got) != 1 || !samePath(got[0].Path, fork) {
		t.Errorf("after remove = %+v", got)
	}
	if _, err := os.Stat(a); err != nil {
		t.Error("removing a repository must leave the clone on disk")
	}
}

func TestAddRepoRejects(t *testing.T) {
	isolateSettings(t)
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	noRemote := cloneDir(t)
	for _, dir := range []string{"", filepath.Join(t.TempDir(), "nope"), t.TempDir(), noRemote} {
		if _, err := addRepo(dir); err == nil {
			t.Errorf("addRepo(%q) succeeded", dir)
		}
	}
	if len(savedRepos()) != 0 {
		t.Error("nothing should be saved")
	}
}

func TestReposAPI(t *testing.T) {
	s := inboxServer(t)
	a := cloneDir(t, [2]string{"origin", "https://github.com/o/a.git"})

	code, m := agentPostJSON(t, s, "/api/repos", map[string]any{"path": a})
	if code != 200 || m["ok"] != true {
		t.Fatalf("add = %d %v", code, m)
	}
	if code, m := agentPostJSON(t, s, "/api/repos", map[string]any{"path": t.TempDir()}); code != 400 || !strings.Contains(m["error"].(string), "not a git repository") {
		t.Errorf("add a plain folder = %d %v", code, m)
	}
	os.RemoveAll(a)
	_, m = getJSON(t, s, "/api/repos")
	list, _ := m["repos"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["missing"] != true {
		t.Errorf("list = %v, want one missing", m)
	}
	if code, m := agentPostJSON(t, s, "/api/repos/remove", map[string]any{"path": a}); code != 200 || len(m["repos"].([]any)) != 0 {
		t.Errorf("remove = %d %v", code, m)
	}
}

func TestReposBrowse(t *testing.T) {
	s := inboxServer(t)
	orig := pickFolder
	t.Cleanup(func() { pickFolder = orig })
	var asked string
	pickFolder = func(prompt string) (string, error) { asked = prompt; return `D:\code\x`, nil }
	if code, m := agentPostJSON(t, s, "/api/repos/browse", map[string]any{"prompt": "Choose o/a"}); code != 200 || m["path"] != `D:\code\x` || asked != "Choose o/a" {
		t.Errorf("browse = %d %v (asked %q)", code, m, asked)
	}
	pickFolder = func(string) (string, error) { return "", errNoPicker }
	if code, _ := agentPostJSON(t, s, "/api/repos/browse", map[string]any{}); code != http.StatusNotImplemented {
		t.Errorf("no picker = %d, want 501", code)
	}
}

// A PR of a repository the workspace is not a clone of launches from the
// saved clone, or asks for one.
func TestLaunchUsesSavedClone(t *testing.T) {
	var spawns int32
	useLaunchHelper(t, "serve", &spawns)
	var dirs []string
	inner := launchCommand
	launchCommand = func(target, dir string) (*exec.Cmd, error) { dirs = append(dirs, dir); return inner(target, dir) }
	s := inboxServer(t) // not a clone of anything
	const target = "https://github.com/o/a/pull/3"

	code, m := agentPostJSON(t, s, "/api/pr/launch", map[string]any{"target": target})
	if code != http.StatusConflict || m["needsRepo"] != "o/a" || spawns != 0 {
		t.Fatalf("launch without a clone = %d %v (spawns %d)", code, m, spawns)
	}
	a := cloneDir(t, [2]string{"origin", "https://github.com/o/a.git"})
	if _, err := addRepo(a); err != nil {
		t.Fatal(err)
	}
	if code, m := agentPostJSON(t, s, "/api/pr/launch", map[string]any{"target": target}); code != 200 || m["ok"] != true {
		t.Fatalf("launch with a saved clone = %d %v", code, m)
	}
	if len(dirs) != 1 || !samePath(dirs[0], a) {
		t.Errorf("launched in %v, want %s", dirs, a)
	}
	waitLaunch(t, s, target, func(m map[string]any) bool { return m != nil && m["state"] == "running" })
	// Wait for the stand-in child to exit: it runs in the clone, and Windows
	// will not remove a folder that is a running process's working directory.
	waitLaunch(t, s, target, func(m map[string]any) bool { return m == nil })
}
