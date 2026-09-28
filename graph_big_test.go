package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestGraphBigRepo measures the first page on a real 100k-commit repository
// (built with git fast-import: a mainline with a two-commit branch merged
// every 10 commits). It is slow to set up, so it runs only when asked:
//
//	PX0_GRAPH_BIG=1 go test -run TestGraphBigRepo -v .
//
// The budget is the first 2,000 rows in under 300 ms (server side) and
// under 40 MB for the session.
func TestGraphBigRepo(t *testing.T) {
	if os.Getenv("PX0_GRAPH_BIG") != "1" {
		t.Skip("set PX0_GRAPH_BIG=1 to build and time a 100k-commit repository")
	}
	root := t.TempDir()
	gitTestRun(t, root, "init", "-q", "-b", "main")
	var in strings.Builder
	mark := 0
	ts := int64(1600000000)
	commit := func(branch string, parents ...int) int {
		mark++
		ts++
		fmt.Fprintf(&in, "commit refs/heads/%s\nmark :%d\ncommitter T <t@t> %d +0000\ndata 8\nc%06d\n", branch, mark, ts, mark%1000000)
		for i, p := range parents {
			if i == 0 {
				fmt.Fprintf(&in, "from :%d\n", p)
			} else {
				fmt.Fprintf(&in, "merge :%d\n", p)
			}
		}
		in.WriteString("\n")
		return mark
	}
	main := commit("main")
	for mark < 100000 {
		if mark%10 == 0 {
			f1 := commit("feature", main)
			f2 := commit("feature", f1)
			main = commit("main", main, f2)
			continue
		}
		main = commit("main", main)
	}
	cmd := exec.Command("git", "fast-import", "--quiet", "--force")
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(in.String())
	start := time.Now()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fast-import: %v\n%s", err, out)
	}
	t.Logf("built %d commits in %s", mark, time.Since(start))
	if os.Getenv("PX0_GRAPH_BIG_COMMITGRAPH") == "1" {
		gitTestRun(t, root, "commit-graph", "write", "--reachable")
		t.Log("commit-graph written")
	}

	start = time.Now()
	readRefs(root)
	t.Logf("  refs: %s", time.Since(start))
	start = time.Now()
	graphDefaultBranch(root, settings{})
	t.Logf("  default branch: %s", time.Since(start))
	start = time.Now()
	logCmd := exec.Command("git", append([]string{"-C", root, "log", "--topo-order", "-n", "2000", "--format=%H%x1f%P%x1f%an%x1f%ct%x1f%s"}, graphRevs...)...)
	logCmd.Run()
	t.Logf("  git log -n 2000 alone: %s", time.Since(start))
	start = time.Now()
	exec.Command("git", "-C", root, "rev-parse", "HEAD").Run()
	t.Logf("  one git spawn: %s", time.Since(start))

	start = time.Now()
	gs, err := graphSessionFor(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		graphMu.Lock()
		delete(graphSessions, root)
		graphMu.Unlock()
		gs.close()
	}()
	rows, next := gs.page(0, graphPageSize)
	first := time.Since(start)
	t.Logf("first page: %d rows in %s (next %d)", len(rows), first, next)

	start = time.Now()
	for c := next; c >= 0; {
		_, c = gs.page(c, graphMaxPage)
	}
	gs.mu.Lock()
	n, width := len(gs.rows), gs.maxLane
	gs.mu.Unlock()
	t.Logf("whole history: %d rows in %s more, %d lanes", n, time.Since(start), width+1)
	if first > 300*time.Millisecond {
		t.Errorf("first page took %s, budget 300ms", first)
	}
}
