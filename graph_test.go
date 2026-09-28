package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata/graph golden files")

// fixture builds commits in topological order from "sha:parent,parent" specs.
func fixture(specs ...string) []graphCommit {
	out := make([]graphCommit, 0, len(specs))
	for _, s := range specs {
		sha, ps, _ := strings.Cut(s, ":")
		c := graphCommit{SHA: sha, Subject: "subject " + sha}
		if ps != "" {
			c.Parents = strings.Split(ps, ",")
		}
		out = append(out, c)
	}
	return out
}

// renderLayout draws a layout as text, one line per row: the lane columns
// ('*' the commit, '|' a line passing through), then the commit, its
// segment, and every edge into and out of its dot.
func renderLayout(rows []graphRow) string {
	width := 0
	for _, r := range rows {
		width = max(width, r.maxLane()+1)
	}
	var b strings.Builder
	for _, r := range rows {
		cols := []byte(strings.Repeat("  ", width))
		var in, out []string
		for _, e := range r.Edges {
			switch e[3] {
			case edgeThrough:
				cols[e[0]*2] = '|'
			case edgeIn:
				if e[0] != r.Lane {
					in = append(in, fmt.Sprintf("%d>%d", e[0], e[1]))
				}
			case edgeOut:
				if e[1] != r.Lane {
					out = append(out, fmt.Sprintf("%d>%d(g%d)", e[0], e[1], e[2]))
				}
			}
		}
		cols[r.Lane*2] = '*'
		line := fmt.Sprintf("%s %-3s g%d", strings.TrimRight(string(cols), " "), r.SHA, r.Seg)
		if len(in) > 0 {
			line += " joins " + strings.Join(in, " ")
		}
		if len(out) > 0 {
			line += " forks " + strings.Join(out, " ")
		}
		if len(r.Parents) == 0 {
			line += " root"
		}
		b.WriteString(strings.TrimRight(line, " ") + "\n")
	}
	return b.String()
}

func layoutAll(commits []graphCommit) ([]graphRow, *laneLayout) {
	ll := newLaneLayout()
	rows := make([]graphRow, 0, len(commits))
	for _, c := range commits {
		rows = append(rows, ll.place(c))
	}
	return rows, ll
}

var graphFixtures = map[string][]graphCommit{
	// c <- b <- a
	"linear": fixture("c:b", "b:a", "a:"),
	// main: r - a1 - a2 - m (merge of feature b1 - b2, forked at a1)
	"branch_merge": fixture("m:a2,b2", "b2:b1", "b1:a1", "a2:a1", "a1:r", "r:"),
	// o merges p1, p2, p3, each one commit off r
	"octopus": fixture("o:p1,p2,p3", "p3:r", "p2:r", "p1:r", "r:"),
	// a1 and b1 fork from r; a2 merges b1 into a1 and b2 merges a1 into b1
	"criss_cross": fixture("a2:a1,b1", "b2:b1,a1", "b1:r", "a1:r", "r:"),
	// two histories with no common root, joined by j
	"orphan_roots": fixture("j:x2,y2", "y2:y1", "y1:", "x2:x1", "x1:"),
	// feature f1 merged at m1, then continued with f2 and merged again at m2
	"merged_twice": fixture("m2:m1,f2", "f2:f1", "m1:a1,f1", "f1:a0", "a1:a0", "a0:"),
	// two unmerged branch tips over a mainline
	"two_tips": fixture("t1:a2", "t2:a1", "a2:a1", "a1:a0", "a0:"),
}

func TestGraphLayoutGolden(t *testing.T) {
	for name, commits := range graphFixtures {
		t.Run(name, func(t *testing.T) {
			rows, _ := layoutAll(commits)
			got := renderLayout(rows)
			path := filepath.Join("testdata", "graph", name+".golden")
			if *updateGolden {
				os.MkdirAll(filepath.Dir(path), 0o755)
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if got != strings.ReplaceAll(string(want), "\r\n", "\n") {
				t.Fatalf("layout changed:\n--- got\n%s--- want\n%s", got, want)
			}
			// Deterministic: the same input lays out the same way twice.
			again, _ := layoutAll(commits)
			if renderLayout(again) != got {
				t.Fatal("layout is not deterministic")
			}
		})
	}
}

// Every edge must start and end on a lane that is actually in use at that
// point, so the client never draws a line to nowhere.
func TestGraphLayoutEdgesConnect(t *testing.T) {
	for name, commits := range graphFixtures {
		rows, _ := layoutAll(commits)
		for i, r := range rows {
			bottom := map[int]bool{}
			for _, e := range r.Edges {
				if e[3] == edgeThrough || e[3] == edgeOut {
					bottom[e[1]] = true
				}
			}
			if i+1 < len(rows) {
				top := map[int]bool{}
				for _, e := range rows[i+1].Edges {
					if e[3] == edgeThrough || e[3] == edgeIn {
						top[e[0]] = true
					}
				}
				for l := range bottom {
					if !top[l] {
						t.Errorf("%s: row %d (%s) sends lane %d down, but row %d (%s) has nothing there", name, i, r.SHA, l, i+1, rows[i+1].SHA)
					}
				}
				for l := range top {
					if !bottom[l] {
						t.Errorf("%s: row %d (%s) takes lane %d from above, but row %d (%s) sends nothing", name, i+1, rows[i+1].SHA, l, i, r.SHA)
					}
				}
			}
		}
	}
}

func TestGraphSegments(t *testing.T) {
	rows, ll := layoutAll(graphFixtures["branch_merge"])
	seg := map[string]int{}
	for _, r := range rows {
		seg[r.SHA] = r.Seg
	}
	// The feature is its own segment, opened by the merge and forked from a1;
	// the mainline keeps its segment through the fork point.
	if seg["b2"] != seg["b1"] || seg["b1"] == seg["a2"] {
		t.Fatalf("segments = %v", seg)
	}
	if seg["m"] != seg["a2"] || seg["a2"] != seg["a1"] || seg["a1"] != seg["r"] {
		t.Fatalf("mainline must keep one segment: %v", seg)
	}
	fs := seg["b1"]
	if ll.segStart[fs] != "m" || ll.segFork[fs] != "a1" || !ll.segEnded[fs] {
		t.Fatalf("feature segment: start %q fork %q ended %v", ll.segStart[fs], ll.segFork[fs], ll.segEnded[fs])
	}

	// Merged twice: m1's second parent joins the lane the feature already
	// holds, so both rounds (f2, f1) are one segment from m2 down to a0.
	rows, ll = layoutAll(graphFixtures["merged_twice"])
	for _, r := range rows {
		seg[r.SHA] = r.Seg
	}
	if seg["f2"] != seg["f1"] || ll.segStart[seg["f1"]] != "m2" || ll.segFork[seg["f1"]] != "a0" {
		t.Fatalf("merged twice: f2 g%d f1 g%d start %q fork %q", seg["f2"], seg["f1"], ll.segStart[seg["f1"]], ll.segFork[seg["f1"]])
	}
	gs := &graphSession{rows: rows, layout: ll, done: true}
	f := gs.segmentFocus(seg["f1"])
	if strings.Join(f.SHAs, ",") != "f2,f1" || f.Merge != "m2" || f.Fork != "a0" || strings.Join(f.Merges, ",") != "m2,m1" {
		t.Fatalf("focus = %+v, want f2,f1 with merges m2,m1 and fork a0", f)
	}
}

// A branch tip that git lists before the default branch's must not take over
// the default branch's history where they meet.
func TestGraphLayoutDefaultBranchKeepsItsLane(t *testing.T) {
	ll := newLaneLayout()
	ll.mainTip = "m2"
	var rows []graphRow
	for _, c := range fixture("a1:c1", "m2:m1", "m1:c1", "c1:c0", "c0:") {
		rows = append(rows, ll.place(c))
	}
	seg := map[string]int{}
	for _, r := range rows {
		seg[r.SHA] = r.Seg
	}
	if seg["c1"] != seg["m2"] || seg["c0"] != seg["m2"] || seg["a1"] == seg["m2"] {
		t.Fatalf("segments = %v: c1 and c0 belong to the default branch", seg)
	}
	if ll.segFork[seg["a1"]] != "c1" {
		t.Fatalf("the branch forks at c1, got %q", ll.segFork[seg["a1"]])
	}
}

func TestParseGraphLine(t *testing.T) {
	c, ok := parseGraphLine("abcdef1234\x1fp1 p2\x1fAlice\x1f1700000000\x1fFix: a\x1fb")
	if !ok || c.SHA != "abcdef1234" || len(c.Parents) != 2 || c.Author != "Alice" || c.Time != 1700000000 || c.Subject != "Fix: a\x1fb" {
		t.Fatalf("%+v %v", c, ok)
	}
	if c, ok := parseGraphLine("abcdef1234\x1f\x1fBob\x1f1\x1froot"); !ok || c.Parents != nil {
		t.Fatalf("root: %+v %v", c, ok)
	}
	if _, ok := parseGraphLine("garbage"); ok {
		t.Fatal("garbage must be skipped")
	}
}

// synthHistory is a mainline of n commits with a two-commit feature branch
// merged every 10 commits, newest first (a valid topological order).
func synthHistory(n int) []graphCommit {
	var out []graphCommit
	id := func(p string, i int) string { return fmt.Sprintf("%s%07d", p, i) }
	for i := n; i >= 1; i-- {
		if i%10 == 0 && i > 1 {
			out = append(out,
				graphCommit{SHA: id("m", i), Parents: []string{id("m", i-1), id("f", i)}},
				graphCommit{SHA: id("f", i), Parents: []string{id("g", i)}},
				graphCommit{SHA: id("g", i), Parents: []string{id("m", i-1)}})
			continue
		}
		c := graphCommit{SHA: id("m", i)}
		if i > 1 {
			c.Parents = []string{id("m", i-1)}
		}
		out = append(out, c)
	}
	return out
}

func TestGraphLayoutLargeHistoryStaysNarrow(t *testing.T) {
	rows, _ := layoutAll(synthHistory(20000))
	widest := 0
	for _, r := range rows {
		widest = max(widest, r.maxLane())
	}
	if widest > 2 {
		t.Fatalf("a mainline with short merged branches should need 2 lanes, used %d", widest+1)
	}
}

// BenchmarkGraphLayoutPage lays out 2000-commit pages of a 100k-commit
// history; the budget is 150 ms per page.
func BenchmarkGraphLayoutPage(b *testing.B) {
	commits := synthHistory(100000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ll := newLaneLayout()
		for _, c := range commits[:graphPageSize] {
			ll.place(c)
		}
	}
}

func BenchmarkGraphLayoutAll100k(b *testing.B) {
	commits := synthHistory(100000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ll := newLaneLayout()
		for _, c := range commits {
			ll.place(c)
		}
	}
}
