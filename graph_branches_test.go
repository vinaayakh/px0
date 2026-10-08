package main

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// graphTestRepo builds a repository with one branch of every category:
//
//	main:          c0 - c1 - M (merges merged-kept: k1) - M2 (merges a deleted branch: o1, o2) - f1 (fast-forward of ffb)
//	merged-kept:   k1, merged at M, branch kept
//	ffb:           f1, fast-forwarded into main
//	stale:         s1 off c1, dated 2020
//	active:        a1 off c1, dated now
//	orphan:        r1, no common history
//	gone-b:        g1 off c1, pushed, then deleted on the remote
//
// origin (a local bare repository) has main, with origin/HEAD pointing at it.
type graphRepo struct {
	root string
	sha  map[string]string
}

func graphTestRepo(t *testing.T) graphRepo {
	t.Helper()
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	base := t.TempDir()
	if r, err := filepath.EvalSymlinks(base); err == nil {
		base = r
	}
	root := filepath.Join(base, "work")
	remote := filepath.Join(base, "remote.git")
	os.MkdirAll(root, 0o755)
	g := graphRepo{root: root, sha: map[string]string{}}
	date := "2026-09-20T12:00:00Z"
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull,
			"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date,
			"GIT_AUTHOR_NAME=Tess", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=Tess", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(name string) {
		os.WriteFile(filepath.Join(root, name+".txt"), []byte(name+"\n"), 0o644)
		run(root, "add", ".")
		run(root, "commit", "-q", "-m", name)
		g.sha[name] = run(root, "rev-parse", "HEAD")
	}
	run(base, "init", "-q", "--bare", remote)
	run(root, "init", "-q", "-b", "main")
	commit("c0")
	commit("c1")
	run(root, "branch", "stale")
	run(root, "branch", "active")
	run(root, "branch", "gone-b")

	run(root, "checkout", "-q", "-b", "merged-kept")
	commit("k1")
	run(root, "checkout", "-q", "main")
	run(root, "merge", "-q", "--no-ff", "-m", "M", "merged-kept")
	g.sha["M"] = run(root, "rev-parse", "HEAD")

	run(root, "checkout", "-q", "-b", "old-feature")
	commit("o1")
	commit("o2")
	run(root, "checkout", "-q", "main")
	run(root, "merge", "-q", "--no-ff", "-m", "M2", "old-feature")
	g.sha["M2"] = run(root, "rev-parse", "HEAD")
	run(root, "branch", "-q", "-D", "old-feature")

	run(root, "checkout", "-q", "-b", "ffb")
	commit("f1")
	run(root, "checkout", "-q", "main")
	run(root, "merge", "-q", "--ff-only", "ffb")

	date = "2020-01-01T00:00:00Z"
	run(root, "checkout", "-q", "stale")
	commit("s1")
	date = time.Now().UTC().Format(time.RFC3339)
	run(root, "checkout", "-q", "active")
	commit("a1")
	run(root, "checkout", "-q", "gone-b")
	commit("g1")
	run(root, "checkout", "-q", "--orphan", "orphan")
	run(root, "rm", "-q", "-rf", ".")
	commit("r1")
	run(root, "checkout", "-q", "main")

	run(root, "remote", "add", "origin", remote)
	run(root, "push", "-q", "origin", "main", "gone-b")
	run(root, "branch", "-q", "--set-upstream-to=origin/gone-b", "gone-b")
	run(root, "remote", "set-head", "origin", "main")
	run(root, "push", "-q", "origin", "--delete", "gone-b")
	run(root, "fetch", "-q", "--prune", "origin")
	return g
}

// TestGraphOriginOnly: a branch of another remote (an upstream or a fork) is
// left out of the branch list, the ref pills and the graph's history.
func TestGraphOriginOnly(t *testing.T) {
	g := graphTestRepo(t)
	tree := strings.TrimSpace(gitTestRun(t, g.root, "rev-parse", "main^{tree}"))
	fork := strings.TrimSpace(gitTestRun(t, g.root, "-c", "user.name=U", "-c", "user.email=u@u",
		"commit-tree", tree, "-p", g.sha["c1"], "-m", "upstream only"))
	gitTestRun(t, g.root, "update-ref", "refs/remotes/upstream/feature", fork)

	branches, err := graphBranches(g.root, "origin/main", 30, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range branches {
		if b.Remote == "upstream" {
			t.Errorf("branch list has %s", b.Name)
		}
	}
	ri, err := readRefs(g.root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ri.branches["upstream/feature"]; ok || ri.refs[fork] != nil {
		t.Error("readRefs has upstream/feature")
	}
	s, err := graphSessionFor(g.root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	s.mu.Lock()
	s.fill(1000)
	_, inGraph := s.index[fork]
	_, hasMain := s.index[g.sha["M2"]]
	s.mu.Unlock()
	if inGraph || !hasMain {
		t.Errorf("graph has upstream commit: %v, has main's M2: %v", inGraph, hasMain)
	}
}

func TestGraphBranchCategories(t *testing.T) {
	g := graphTestRepo(t)
	def := graphDefaultBranch(g.root, settings{})
	if def != "origin/main" {
		t.Fatalf("default branch = %q, want origin/main (from origin/HEAD)", def)
	}
	branches, err := graphBranches(g.root, def, 30, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]graphBranch{}
	for _, b := range branches {
		got[b.Name] = b
	}
	want := map[string]string{
		"main": catDefault, "origin/main": catDefault, "merged-kept": catMerged, "ffb": catMerged,
		"stale": catStale, "active": catActive, "orphan": catOrphan, "gone-b": catGone,
	}
	for name, cat := range want {
		if got[name].Category != cat {
			t.Errorf("%s: category %q, want %q", name, got[name].Category, cat)
		}
	}
	if len(got) != len(want) {
		t.Errorf("branches = %v", branches)
	}
	if a := got["active"]; a.Ahead != 1 || a.Behind != 6 || a.Kind != "local" || a.Author != "Tess" {
		t.Errorf("active = %+v, want 1 ahead, 6 behind (k1, M, o1, o2, M2, f1)", a)
	}
	if o := got["origin/main"]; o.Kind != "remote" || o.Remote != "origin" {
		t.Errorf("origin/main = %+v, want remote branch of origin", o)
	}
	if got["main"].Remote != "" {
		t.Errorf("local main has remote %q", got["main"].Remote)
	}
	if !got["main"].Current {
		t.Error("main is checked out")
	}
	if branches[0].Category != catDefault {
		t.Errorf("default branches sort first, got %s", branches[0].Name)
	}

	// With the forge saying active had a merged PR, it is a probable squash merge.
	branches, _ = graphBranches(g.root, def, 30, time.Now(), func(names []string) map[string]string {
		return map[string]string{"active": "https://github.com/o/r/pull/9"}
	})
	for _, b := range branches {
		if b.Name == "active" && (b.Category != catSquashMerged || b.PRURL == "") {
			t.Errorf("active with a merged PR = %+v", b)
		}
		if b.Name == "stale" && b.Category != catStale {
			t.Errorf("stale must stay stale: %+v", b)
		}
	}
}

func TestRemoteOf(t *testing.T) {
	remotes := []string{"origin", "upstream", "team/fork"}
	for name, want := range map[string]string{
		"origin/main":         "origin",
		"upstream/feat/x":     "upstream",
		"team/fork/main":      "team/fork",
		"unknown/topic":       "unknown",
		"originals/something": "originals",
	} {
		if got := remoteOf(name, remotes); got != want {
			t.Errorf("remoteOf(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestGraphDefaultBranchSetting(t *testing.T) {
	g := graphTestRepo(t)
	s := "merged-kept"
	if def := graphDefaultBranch(g.root, settings{GraphDefaultBranch: &s}); def != "merged-kept" {
		t.Errorf("setting = %q", def)
	}
	bad := "nope"
	if def := graphDefaultBranch(g.root, settings{GraphDefaultBranch: &bad}); def != "origin/main" {
		t.Errorf("a missing configured branch falls back, got %q", def)
	}
}

func TestGraphRefFocus(t *testing.T) {
	g := graphTestRepo(t)
	f, err := refFocus(g.root, "merged-kept", "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.SHAs, ",") != g.sha["k1"] || f.Merge != g.sha["M"] || f.Fork != g.sha["c1"] {
		t.Errorf("merged branch focus = %+v", f)
	}
	f, _ = refFocus(g.root, "active", "origin/main")
	if strings.Join(f.SHAs, ",") != g.sha["a1"] || f.Fork != g.sha["c1"] || f.Merge != "" {
		t.Errorf("unmerged branch focus = %+v", f)
	}
	f, _ = refFocus(g.root, "ffb", "origin/main")
	if strings.Join(f.SHAs, ",") != g.sha["f1"] || f.Merge != "" {
		t.Errorf("fast-forwarded branch focus = %+v, want just its tip", f)
	}
	f, _ = refFocus(g.root, "orphan", "origin/main")
	if strings.Join(f.SHAs, ",") != g.sha["r1"] || f.Fork != "" {
		t.Errorf("orphan focus = %+v", f)
	}
	if _, err := refFocus(g.root, "no-such-branch", "origin/main"); err == nil {
		t.Error("unknown branch must fail")
	}
}

// The acceptance case: a merged branch that was deleted can still be focused
// by clicking its lane, and exactly its commits, fork point and entry merge
// light up.
func TestGraphLaneFocusOnDeletedBranch(t *testing.T) {
	g := graphTestRepo(t)
	gs, err := graphSessionFor(g.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		graphMu.Lock()
		delete(graphSessions, g.root)
		graphMu.Unlock()
		gs.close()
	})
	rows, _ := gs.page(0, graphPageSize)
	if len(rows) != 12 {
		t.Fatalf("%d rows, want 12 commits", len(rows))
	}
	seg, err := gs.segOf(g.sha["o1"])
	if err != nil {
		t.Fatal(err)
	}
	f := gs.segmentFocus(seg)
	if strings.Join(f.SHAs, ",") != g.sha["o2"]+","+g.sha["o1"] || f.Merge != g.sha["M2"] || f.Fork != g.sha["M"] {
		t.Fatalf("lane focus = %+v\nwant o2,o1 merged at M2, forked from M", f)
	}
	// Refs land on their rows.
	for _, r := range rows {
		if r.SHA == g.sha["k1"] && (len(r.Refs) != 1 || r.Refs[0].Name != "merged-kept" || r.Refs[0].Kind != "local") {
			t.Errorf("k1 refs = %+v", r.Refs)
		}
		if r.SHA == g.sha["f1"] {
			names := []string{}
			for _, ref := range r.Refs {
				names = append(names, ref.Name)
			}
			if strings.Join(names, ",") != "ffb,main,origin/main" && strings.Join(names, ",") != "ffb,main,origin/main" {
				t.Errorf("f1 refs = %v", names)
			}
		}
	}
}

func TestGraphEndpointsAndReset(t *testing.T) {
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

	code, m := getJSON(t, s, "/api/graph?cursor=0&limit=4")
	if code != 200 || len(m["rows"].([]any)) != 4 || m["next"].(float64) != 4 {
		t.Fatalf("page 1 = %d %v", code, m)
	}
	sig := m["sig"].(string)
	_, m = getJSON(t, s, "/api/graph?cursor=4&limit=100&sig="+sig)
	if len(m["rows"].([]any)) != 8 || m["next"].(float64) != -1 {
		t.Fatalf("page 2 = %v", m)
	}
	if m["total"].(float64) != 12 {
		t.Errorf("total = %v", m["total"])
	}

	_, m = getJSON(t, s, "/api/graph/branches")
	if m["default"] != "origin/main" || len(m["branches"].([]any)) != 8 {
		t.Fatalf("branches = %v", m)
	}
	code, m = getJSON(t, s, "/api/graph/focus?ref=merged-kept")
	if code != 200 || m["merge"] != g.sha["M"] {
		t.Fatalf("focus by ref = %d %v", code, m)
	}
	code, m = getJSON(t, s, "/api/graph/focus?sha="+g.sha["o2"][:10])
	if code != 200 || m["merge"] != g.sha["M2"] {
		t.Fatalf("focus by sha prefix = %d %v", code, m)
	}
	if code, _ := getJSON(t, s, "/api/graph/focus"); code != http.StatusBadRequest {
		t.Errorf("focus without a target = %d", code)
	}

	// A new commit moves the refs: a client holding the old signature is told to start over.
	os.WriteFile(filepath.Join(g.root, "new.txt"), []byte("x"), 0o644)
	gitTestRun(t, g.root, "add", ".")
	gitTestRun(t, g.root, "-c", "user.name=T", "-c", "user.email=t@t", "commit", "-q", "-m", "new")
	_, m = getJSON(t, s, "/api/graph?cursor=4&sig="+sig)
	if m["reset"] != true || m["sig"] == sig {
		t.Fatalf("after a new commit = %v", m)
	}
}
