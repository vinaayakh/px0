package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// graph.go is the commit graph behind the Graph tab (web/src/graph.js): every
// local and remote branch, tag and HEAD, in topological order, with lanes
// laid out here rather than in the browser. `git log` is streamed and laid
// out as it is read, one page at a time; the lane state carries from page to
// page, so the first rows of a 100k-commit repository cost what their page
// costs. Branch health and focus sets are in graph_branches.go.

const (
	graphPageSize = 2000
	graphMaxPage  = 5000
	graphIdle     = 2 * time.Minute // an unused session is dropped, git process and all
)

// graphEdge is one line segment drawn in a row: [fromLane, toLane, segment,
// kind]. Kind 0 runs through the row top to bottom, 1 runs from the top into
// the commit's dot, 2 runs from the dot to the bottom.
type graphEdge [4]int

const (
	edgeThrough = 0
	edgeIn      = 1
	edgeOut     = 2
)

// graphRef is a ref pill on a row.
type graphRef struct {
	Name string `json:"n"`
	Kind string `json:"k"`           // local, remote, tag, head (detached HEAD)
	Head bool   `json:"h,omitempty"` // the checked-out branch
}

// graphRow is one commit, laid out. Seg is the lane segment the commit sits
// on: a run of commits sharing a lane, from where it opens (a branch tip or a
// merge's second parent) to where it joins another lane.
type graphRow struct {
	SHA     string      `json:"h"`
	Parents []string    `json:"p,omitempty"`
	Author  string      `json:"a"`
	Time    int64       `json:"t"`
	Subject string      `json:"s"`
	Lane    int         `json:"l"`
	Seg     int         `json:"g"`
	Edges   []graphEdge `json:"e"`
	Refs    []graphRef  `json:"r,omitempty"`
}

type graphCommit struct {
	SHA     string
	Parents []string
	Author  string
	Time    int64
	Subject string
}

// laneLayout assigns commits to lanes. lanes[i] is the commit column i is
// waiting for ("" when free); segs[i] is the segment currently in it.
type laneLayout struct {
	lanes   []string
	segs    []int
	nextSeg int
	// Per segment: the merge that opened it ("" for a branch tip) and the
	// commit it joined when it ended ("" while open, or for a root).
	segStart map[int]string
	segFork  map[int]string
	segEnded map[int]bool
	// mainTip is the default branch's tip. Its segment outranks every other
	// when lines meet, so the default branch's history stays on its own lane
	// and a branch's segment ends where it forked from it.
	mainTip string
	mainSeg int
}

func newLaneLayout() *laneLayout {
	return &laneLayout{segStart: map[int]string{}, segFork: map[int]string{}, segEnded: map[int]bool{}, mainSeg: -1}
}

// outranks reports whether segment a should keep the lane over segment b
// when both reach the same commit: the default branch first, then the older.
func (ll *laneLayout) outranks(a, b int) bool {
	if a == ll.mainSeg || b == ll.mainSeg {
		return a == ll.mainSeg
	}
	return a < b
}

func (ll *laneLayout) newSeg(start string) int {
	id := ll.nextSeg
	ll.nextSeg++
	ll.segStart[id] = start
	return id
}

// free returns the leftmost free column, growing the layout if none is.
func (ll *laneLayout) free() int {
	for i, s := range ll.lanes {
		if s == "" {
			return i
		}
	}
	ll.lanes = append(ll.lanes, "")
	ll.segs = append(ll.segs, -1)
	return len(ll.lanes) - 1
}

// place lays out the next commit in topological order (children before
// parents) and returns its row. It is O(active lanes).
//
// Several columns can be waiting for the same commit (two branches forking
// from it). The commit takes the column of the default branch's segment if
// that is one of them, else the oldest segment's, so the mainline keeps its
// lane through the fork and the branch's segment ends there; that is what
// makes a lane click on a merged branch select exactly the branch.
func (ll *laneLayout) place(c graphCommit) graphRow {
	row := graphRow{SHA: c.SHA, Parents: c.Parents, Author: c.Author, Time: c.Time, Subject: c.Subject}
	lane := -1
	var hits []int
	for i, s := range ll.lanes {
		if s == c.SHA {
			hits = append(hits, i)
			if lane < 0 || ll.outranks(ll.segs[i], ll.segs[lane]) {
				lane = i
			}
		}
	}
	if lane < 0 { // nothing waits for it: a branch tip
		lane = ll.free()
		ll.segs[lane] = ll.newSeg("")
		if c.SHA == ll.mainTip {
			ll.mainSeg = ll.segs[lane]
		}
	}
	seg := ll.segs[lane]
	row.Lane, row.Seg = lane, seg

	row.Edges = make([]graphEdge, 0, len(ll.lanes)+len(c.Parents))
	for i, s := range ll.lanes {
		switch {
		case s == "":
		case s == c.SHA:
			row.Edges = append(row.Edges, graphEdge{i, lane, ll.segs[i], edgeIn})
		default:
			row.Edges = append(row.Edges, graphEdge{i, i, ll.segs[i], edgeThrough})
		}
	}
	for _, h := range hits {
		if h != lane {
			ll.lanes[h] = ""
			ll.segEnded[ll.segs[h]] = true
			ll.segFork[ll.segs[h]] = c.SHA
		}
	}

	if len(c.Parents) == 0 {
		ll.lanes[lane] = ""
		ll.segEnded[seg] = true
	} else {
		ll.lanes[lane] = c.Parents[0]
		row.Edges = append(row.Edges, graphEdge{lane, lane, seg, edgeOut})
		for _, p := range c.Parents[1:] {
			j := -1
			for i, s := range ll.lanes {
				if s == p {
					j = i
					break
				}
			}
			if j < 0 {
				j = ll.free()
				ll.lanes[j] = p
				ll.segs[j] = ll.newSeg(c.SHA)
			}
			row.Edges = append(row.Edges, graphEdge{lane, j, ll.segs[j], edgeOut})
		}
	}
	for n := len(ll.lanes); n > 0 && ll.lanes[n-1] == ""; n-- {
		ll.lanes = ll.lanes[:n-1]
		ll.segs = ll.segs[:n-1]
	}
	return row
}

// maxLane is the widest column index a row touches.
func (r graphRow) maxLane() int {
	m := r.Lane
	for _, e := range r.Edges {
		if e[0] > m {
			m = e[0]
		}
		if e[1] > m {
			m = e[1]
		}
	}
	return m
}

// ---------------------------------------------------------------- git

// graphRevs are the tips the graph starts from: every local branch, origin's
// branches, every tag, and HEAD (a PR worktree is often detached). Other
// remotes (upstreams, forks) are left out so the graph shows this clone and
// its origin only. Stashes are left out too; their internal commits would show
// as stray merges.
var graphRevs = []string{"--branches", "--remotes=origin", "--tags", "HEAD"}

// graphRefPatterns are the for-each-ref patterns matching graphRevs.
var graphRefPatterns = []string{"refs/heads", "refs/remotes/origin", "refs/tags"}

// parseGraphLine reads one `git log --format=%H%x1f%P%x1f%an%x1f%ct%x1f%s` line.
func parseGraphLine(line string) (graphCommit, bool) {
	f := strings.SplitN(line, "\x1f", 5)
	if len(f) < 5 || len(f[0]) < 7 {
		return graphCommit{}, false
	}
	c := graphCommit{SHA: f[0], Author: f[2], Subject: f[4]}
	if f[1] != "" {
		c.Parents = strings.Fields(f[1])
	}
	c.Time, _ = strconv.ParseInt(f[3], 10, 64)
	return c, true
}

// graphRefInfo is everything the graph needs to know about refs, read with a
// single `git for-each-ref`: process spawns dominate the cost on some systems
// (about 70 ms each on Windows with a virus scanner), so the graph avoids
// them wherever one call can answer.
type graphRefInfo struct {
	refs       map[string][]graphRef // commit -> pills
	sig        string                // changes whenever any ref or HEAD does
	head       string                // HEAD commit
	branches   map[string]string     // "main", "origin/main" -> commit
	originHead string                // what refs/remotes/origin/HEAD points at, e.g. "origin/main"
}

func readRefs(root string) (graphRefInfo, error) {
	args := append([]string{"-C", root, "for-each-ref",
		"--format=%(objectname)%00%(*objectname)%00%(refname)%00%(HEAD)%00%(symref)"}, graphRefPatterns...)
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return graphRefInfo{}, err
	}
	ri := graphRefInfo{refs: map[string][]graphRef{}, branches: map[string]string{}}
	headName := ""
	type entry struct{ sha, name string }
	var entries []entry
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\x00")
		if len(f) != 5 {
			continue
		}
		sha, name := f[0], f[2]
		if f[1] != "" {
			sha = f[1] // an annotated tag points at its commit through the tag object
		}
		if f[3] == "*" {
			headName, ri.head = name, sha
		}
		if name == "refs/remotes/origin/HEAD" && f[4] != "" {
			ri.originHead = strings.TrimPrefix(f[4], "refs/remotes/")
		}
		entries = append(entries, entry{sha, name})
	}
	if ri.head == "" {
		// Detached (a PR worktree) or unborn: only then is HEAD worth a spawn.
		if b, err := exec.Command("git", "-C", root, "rev-parse", "--verify", "--quiet", "HEAD").Output(); err == nil {
			ri.head = strings.TrimSpace(string(b))
		}
	}
	h := sha256.New()
	h.Write(out)
	h.Write([]byte("\x00" + ri.head + "\x00" + headName))
	ri.sig = hex.EncodeToString(h.Sum(nil)[:8])

	for _, e := range entries {
		var r graphRef
		switch {
		case strings.HasPrefix(e.name, "refs/heads/"):
			r = graphRef{Name: strings.TrimPrefix(e.name, "refs/heads/"), Kind: "local", Head: e.name == headName}
			ri.branches[r.Name] = e.sha
		case strings.HasPrefix(e.name, "refs/remotes/"):
			if strings.HasSuffix(e.name, "/HEAD") {
				continue // origin/HEAD only restates the default branch
			}
			r = graphRef{Name: strings.TrimPrefix(e.name, "refs/remotes/"), Kind: "remote"}
			ri.branches[r.Name] = e.sha
		case strings.HasPrefix(e.name, "refs/tags/"):
			r = graphRef{Name: strings.TrimPrefix(e.name, "refs/tags/"), Kind: "tag"}
		default:
			continue
		}
		ri.refs[e.sha] = append(ri.refs[e.sha], r)
	}
	if ri.head != "" && headName == "" {
		ri.refs[ri.head] = append([]graphRef{{Name: "HEAD", Kind: "head"}}, ri.refs[ri.head]...)
	}
	return ri, nil
}

// defaultBranch picks the branch everything is compared against: the
// graph.defaultBranch setting, else what origin/HEAD points at, else main or
// master (local, then origin's). It returns the name and its commit. Only a
// configured name that is not a branch (a tag or a commit) costs a git call.
func (ri graphRefInfo) defaultBranch(root string, cfg settings) (string, string) {
	if cfg.GraphDefaultBranch != nil {
		if b := strings.TrimSpace(*cfg.GraphDefaultBranch); b != "" {
			if sha, ok := ri.branches[b]; ok {
				return b, sha
			}
			if sha := revParse(root, b); sha != "" {
				return b, sha
			}
		}
	}
	if sha, ok := ri.branches[ri.originHead]; ok && ri.originHead != "" {
		return ri.originHead, sha
	}
	for _, b := range []string{"main", "master", "origin/main", "origin/master"} {
		if sha, ok := ri.branches[b]; ok {
			return b, sha
		}
	}
	return "", ""
}

// ---------------------------------------------------------------- session

// graphSession streams one `git log --topo-order` and keeps the rows laid out
// so far. Pages are served from it in order; a request past the end reads
// more. It is replaced when the refs change and dropped when idle.
type graphSession struct {
	mu       sync.Mutex
	root     string
	sig      string
	head     string
	refs     map[string][]graphRef
	cmd      *exec.Cmd
	cancel   context.CancelFunc
	sc       *bufio.Scanner
	done     bool
	err      error
	layout   *laneLayout
	rows     []graphRow
	index    map[string]int // sha -> row
	maxLane  int
	total    int
	lastUsed time.Time

	noCommitGraph bool // the repository has no commit-graph file
}

var (
	graphMu       sync.Mutex
	graphSessions = map[string]*graphSession{}
)

// graphSessionFor returns the session for root, starting a new one when the
// refs moved since the current one began.
func graphSessionFor(root string) (*graphSession, error) {
	ri, err := readRefs(root)
	if err != nil {
		return nil, err
	}
	sig := ri.sig
	graphMu.Lock()
	defer graphMu.Unlock()
	if s := graphSessions[root]; s != nil {
		if s.sig == sig {
			s.mu.Lock()
			s.lastUsed = time.Now()
			s.mu.Unlock()
			return s, nil
		}
		s.close()
	}
	s := &graphSession{root: root, sig: sig, head: ri.head, refs: ri.refs, layout: newLaneLayout(), index: map[string]int{}, total: -1, lastUsed: time.Now()}
	_, s.layout.mainTip = ri.defaultBranch(root, readSettings())
	if err := s.start(); err != nil {
		return nil, err
	}
	graphSessions[root] = s
	go s.reapWhenIdle()
	return s, nil
}

func (s *graphSession) start() error {
	ctx, cancel := context.WithCancel(context.Background())
	args := append([]string{"-C", s.root, "log", "--topo-order", "--no-color", "--format=%H%x1f%P%x1f%an%x1f%ct%x1f%s"}, graphRevs...)
	args = append(args, "--")
	cmd := exec.CommandContext(ctx, "git", args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return err
	}
	s.cmd, s.cancel = cmd, cancel
	s.sc = bufio.NewScanner(out)
	s.sc.Buffer(make([]byte, 64<<10), 1<<20)
	// The count is for the scrollbar only, so it is not waited for. Neither is
	// the commit-graph check: without that file git walks all of history
	// before printing the first --topo-order line, which is what makes a very
	// large repository slow to open.
	go func() {
		if b, err := exec.Command("git", "-C", s.root, "rev-parse", "--git-common-dir").Output(); err == nil {
			dir := strings.TrimSpace(string(b))
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(s.root, dir)
			}
			_, e1 := os.Stat(filepath.Join(dir, "objects", "info", "commit-graph"))
			_, e2 := os.Stat(filepath.Join(dir, "objects", "info", "commit-graphs"))
			s.mu.Lock()
			s.noCommitGraph = e1 != nil && e2 != nil
			s.mu.Unlock()
		}
	}()
	go func() {
		cargs := append([]string{"-C", s.root, "rev-list", "--count"}, graphRevs...)
		cargs = append(cargs, "--")
		if b, err := exec.Command("git", cargs...).Output(); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				s.mu.Lock()
				if s.total < 0 {
					s.total = n
				}
				s.mu.Unlock()
			}
		}
	}()
	return nil
}

// fill reads and lays out rows until there are n or history ends. Callers
// hold s.mu.
func (s *graphSession) fill(n int) {
	for len(s.rows) < n && !s.done {
		if !s.sc.Scan() {
			s.done = true
			s.err = s.sc.Err()
			if werr := s.cmd.Wait(); werr != nil && s.err == nil && len(s.rows) == 0 {
				s.err = fmt.Errorf("git log: %w", werr) // e.g. a repository with no commits
			}
			s.cancel()
			s.total = len(s.rows)
			return
		}
		c, ok := parseGraphLine(s.sc.Text())
		if !ok {
			continue
		}
		row := s.layout.place(c)
		row.Refs = s.refs[c.SHA]
		s.maxLane = max(s.maxLane, row.maxLane())
		s.index[c.SHA] = len(s.rows)
		s.rows = append(s.rows, row)
	}
}

func (s *graphSession) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil && !s.done {
		s.cancel()
		go s.cmd.Wait()
	}
	s.done = true
	s.rows, s.index = nil, nil
}

func (s *graphSession) reapWhenIdle() {
	for {
		time.Sleep(graphIdle / 4)
		s.mu.Lock()
		idle := time.Since(s.lastUsed) > graphIdle
		s.mu.Unlock()
		if !idle {
			continue
		}
		graphMu.Lock()
		if graphSessions[s.root] == s {
			delete(graphSessions, s.root)
		}
		graphMu.Unlock()
		s.close()
		return
	}
}

// page returns rows [cursor, cursor+limit) and the next cursor (-1 at the end).
func (s *graphSession) page(cursor, limit int) ([]graphRow, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastUsed = time.Now()
	s.fill(cursor + limit)
	if cursor >= len(s.rows) {
		return []graphRow{}, -1
	}
	end := min(cursor+limit, len(s.rows))
	next := end
	if s.done && end == len(s.rows) {
		next = -1
	}
	return s.rows[cursor:end], next
}

// ---------------------------------------------------------------- HTTP

func (s *Server) graphRoot(w http.ResponseWriter) (string, bool) {
	if s.ix == nil || !gitAvailable(s.ix.Root()) {
		fail(w, http.StatusNotFound, "the graph needs a git repository")
		return "", false
	}
	return s.ix.Root(), true
}

// handleGraph serves one page: GET ?cursor=&limit=&sig=. When sig is given
// and the refs have moved since, the answer is {reset: true} and the client
// starts again from 0.
func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	root, ok := s.graphRoot(w)
	if !ok {
		return
	}
	q := r.URL.Query()
	cursor, _ := strconv.Atoi(q.Get("cursor"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = graphPageSize
	}
	limit = min(limit, graphMaxPage)
	cursor = max(cursor, 0)

	gs, err := graphSessionFor(root)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if want := q.Get("sig"); want != "" && want != gs.sig {
		writeJSON(w, map[string]any{"reset": true, "sig": gs.sig})
		return
	}
	rows, next := gs.page(cursor, limit)
	gs.mu.Lock()
	resp := map[string]any{
		"rows": rows, "cursor": cursor, "next": next, "total": gs.total, "maxLane": gs.maxLane,
		"sig": gs.sig, "head": gs.head,
	}
	if gs.err != nil && len(gs.rows) == 0 {
		resp["error"] = gs.err.Error()
	}
	if gs.noCommitGraph && gs.total > 20000 {
		resp["hint"] = "This repository has no commit-graph file, so git reads all of its history before the graph can show anything. Running `git commit-graph write --reachable` once (or `git gc`) makes it open much faster."
	}
	gs.mu.Unlock()
	if s.pr != nil && s.pr.srcRepo == "" {
		resp["notice"] = "This PR was checked out as a fresh clone of its branch, so the graph shows only part of the history."
	}
	writeJSON(w, resp)
}

var errGraphNoRow = errors.New("that commit is not in the graph")

// segmentFocus is the focus set of one lane segment: its commits, the merge
// that opened it and the commit it forked from. The layout only knows a
// segment's end once it has been read, so more history is read (up to a
// bound) until the segment closes.
func (s *graphSession) segmentFocus(seg int) graphFocus {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastUsed = time.Now()
	for extra := 0; !s.layout.segEnded[seg] && !s.done && extra < 20000; extra += graphPageSize {
		s.fill(len(s.rows) + graphPageSize)
	}
	f := graphFocus{Seg: seg, Merge: s.layout.segStart[seg], Fork: s.layout.segFork[seg], SHAs: []string{}}
	for _, r := range s.rows {
		if r.Seg == seg {
			f.SHAs = append(f.SHAs, r.SHA)
			if f.Tip == "" {
				f.Tip = r.SHA
			}
			continue
		}
		for _, e := range r.Edges { // a merge from another lane into this segment
			if e[3] == edgeOut && e[2] == seg && e[1] != r.Lane {
				f.Merges = append(f.Merges, r.SHA)
				break
			}
		}
	}
	return f
}

func (s *graphSession) segOf(sha string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i, ok := s.index[sha]; ok {
		return s.rows[i].Seg, nil
	}
	// A full SHA the client got from a page, or a prefix typed in the hash.
	for i, r := range s.rows {
		if len(sha) >= 7 && strings.HasPrefix(r.SHA, sha) {
			return s.rows[i].Seg, nil
		}
	}
	return 0, errGraphNoRow
}

// readAllLines is used by the branch analysis for small git outputs.
func gitLines(root string, args ...string) ([]string, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	b, rerr := io.ReadAll(io.LimitReader(out, 32<<20))
	io.Copy(io.Discard, out)
	werr := cmd.Wait()
	if rerr != nil {
		return nil, rerr
	}
	if werr != nil {
		return nil, werr
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return nil, nil
	}
	return strings.Split(s, "\n"), nil
}
