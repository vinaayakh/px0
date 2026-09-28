package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// graph_branches.go sorts every branch into a health category against the
// default branch, and computes focus sets: which commits make up a branch's
// life, for the Graph tab to highlight.

// Branch categories, most actionable first.
const (
	catDefault      = "default"
	catOrphan       = "orphan"        // no history in common with the default branch
	catMerged       = "merged"        // tip already in the default branch
	catSquashMerged = "squash_merged" // GitHub has a merged PR from it (probable squash or rebase merge)
	catGone         = "gone"          // local branch whose upstream was deleted
	catStale        = "stale"         // unmerged, no commits for graph.staleDays
	catActive       = "active"
)

type graphBranch struct {
	Name     string `json:"name"` // "feature" or "origin/feature"
	Ref      string `json:"ref"`  // full ref name
	Kind     string `json:"kind"` // local or remote
	SHA      string `json:"sha"`
	Category string `json:"category"`
	Ahead    int    `json:"ahead"`  // commits on the branch not in the default branch
	Behind   int    `json:"behind"` // commits in the default branch not on the branch
	Date     int64  `json:"date"`   // tip committer date
	Author   string `json:"author"`
	Current  bool   `json:"current,omitempty"`
	PRURL    string `json:"prUrl,omitempty"` // the merged PR, for squash_merged
}

// graphDefaultBranch is the name of the branch everything is compared
// against (graphRefInfo.defaultBranch), or "" when there is none.
func graphDefaultBranch(root string, cfg settings) string {
	if ri, err := readRefs(root); err == nil {
		name, _ := ri.defaultBranch(root, cfg)
		return name
	}
	return ""
}

// parallelEach runs fn over items with the process-wide git concurrency
// budget (NumCPU x 4).
func parallelEach[T any](items []T, fn func(T)) {
	sem := make(chan struct{}, runtime.NumCPU()*4)
	var wg sync.WaitGroup
	for _, it := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func(it T) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(it)
		}(it)
	}
	wg.Wait()
}

// graphBranches lists local and remote branches with their category.
// squashLookup, when set, asks the forge which of the given branch names had
// a PR merged, and returns name -> PR URL.
func graphBranches(root, def string, staleDays int, now time.Time, squashLookup func([]string) map[string]string) ([]graphBranch, error) {
	fields := "%(refname)%00%(objectname)%00%(committerdate:unix)%00%(authorname)%00%(upstream:track)%00%(symref)%00%(HEAD)"
	withAB := def != ""
	lines, err := gitLines(root, "for-each-ref", "--format="+fields+"%00%(ahead-behind:"+def+")", "refs/heads", "refs/remotes")
	if err != nil || !withAB {
		// Older git has no %(ahead-behind); counts are then taken per branch.
		withAB = false
		if lines, err = gitLines(root, "for-each-ref", "--format="+fields, "refs/heads", "refs/remotes"); err != nil {
			return nil, err
		}
	}
	merged := map[string]bool{}
	var defSHA string
	if def != "" {
		m, _ := gitLines(root, "for-each-ref", "--merged="+def, "--format=%(refname)", "refs/heads", "refs/remotes")
		for _, r := range m {
			merged[r] = true
		}
		if out, err := exec.Command("git", "-C", root, "rev-parse", "--verify", "--quiet", def+"^{commit}").Output(); err == nil {
			defSHA = strings.TrimSpace(string(out))
		}
	}

	var out []graphBranch
	for _, line := range lines {
		f := strings.Split(line, "\x00")
		if len(f) < 7 || f[5] != "" { // symbolic refs such as origin/HEAD
			continue
		}
		b := graphBranch{Ref: f[0], SHA: f[1], Author: f[3], Current: f[6] == "*"}
		b.Date, _ = strconv.ParseInt(f[2], 10, 64)
		if strings.HasPrefix(b.Ref, "refs/heads/") {
			b.Kind, b.Name = "local", strings.TrimPrefix(b.Ref, "refs/heads/")
		} else {
			b.Kind, b.Name = "remote", strings.TrimPrefix(b.Ref, "refs/remotes/")
		}
		if withAB && len(f) >= 8 {
			if ab := strings.Fields(f[7]); len(ab) == 2 {
				b.Ahead, _ = strconv.Atoi(ab[0])
				b.Behind, _ = strconv.Atoi(ab[1])
			}
		}
		switch {
		case def == "":
			b.Category = catActive
		case b.Name == def || (defSHA != "" && b.SHA == defSHA && (b.Name == strings.TrimPrefix(def, "origin/") || "origin/"+b.Name == def)):
			b.Category = catDefault
		case merged[b.Ref]:
			b.Category = catMerged
		case f[4] == "[gone]":
			b.Category = catGone
		}
		out = append(out, b)
	}

	// Unmerged branches: orphan check (and counts, on older git), in parallel.
	var todo []*graphBranch
	for i := range out {
		if def != "" && (out[i].Category == "" || out[i].Category == catGone || !withAB) {
			todo = append(todo, &out[i])
		}
	}
	parallelEach(todo, func(b *graphBranch) {
		if !withAB {
			if o, err := exec.Command("git", "-C", root, "rev-list", "--left-right", "--count", def+"..."+b.SHA).Output(); err == nil {
				if ab := strings.Fields(string(o)); len(ab) == 2 {
					b.Behind, _ = strconv.Atoi(ab[0])
					b.Ahead, _ = strconv.Atoi(ab[1])
				}
			}
		}
		if b.Category == "" || b.Category == catGone {
			// merge-base exits 1 with no output when the histories never meet.
			if o, err := exec.Command("git", "-C", root, "merge-base", def, b.SHA).Output(); err == nil && strings.TrimSpace(string(o)) == "" || err != nil && exitCode(err) == 1 {
				b.Category = catOrphan
			}
		}
	})

	if squashLookup != nil {
		var names []string
		seen := map[string]bool{}
		for _, b := range out {
			if b.Category == "" || b.Category == catGone {
				n := b.Name
				if b.Kind == "remote" {
					if i := strings.IndexByte(n, '/'); i >= 0 {
						n = n[i+1:]
					}
				}
				if !seen[n] {
					seen[n] = true
					names = append(names, n)
				}
			}
		}
		if len(names) > 0 {
			prs := squashLookup(names)
			for i := range out {
				b := &out[i]
				if b.Category != "" && b.Category != catGone {
					continue
				}
				n := b.Name
				if b.Kind == "remote" {
					if j := strings.IndexByte(n, '/'); j >= 0 {
						n = n[j+1:]
					}
				}
				if u := prs[n]; u != "" {
					b.Category, b.PRURL = catSquashMerged, u
				}
			}
		}
	}

	staleBefore := now.Add(-time.Duration(staleDays) * 24 * time.Hour).Unix()
	for i := range out {
		if out[i].Category == "" {
			if out[i].Date < staleBefore {
				out[i].Category = catStale
			} else {
				out[i].Category = catActive
			}
		}
	}
	rank := map[string]int{catDefault: 0, catActive: 1, catStale: 2, catGone: 3, catSquashMerged: 4, catMerged: 5, catOrphan: 6}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return rank[out[i].Category] < rank[out[j].Category]
		}
		if out[i].Date != out[j].Date {
			return out[i].Date > out[j].Date
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// githubSquashLookup asks GitHub, in one GraphQL query with an alias per
// branch, which branch names had a pull request merged into owner/repo.
// Squash and rebase merges leave no trace in git, so this is the only way to
// see them; without a token the branches stay stale or active.
func githubSquashLookup(ctx context.Context, token, owner, repo string, names []string) map[string]string {
	res := map[string]string{}
	if token == "" || len(names) == 0 {
		return res
	}
	if len(names) > 50 {
		names = names[:50]
	}
	var q strings.Builder
	q.WriteString("query($owner: String!, $repo: String!")
	vars := map[string]any{"owner": owner, "repo": repo}
	for i, n := range names {
		fmt.Fprintf(&q, ", $h%d: String!", i)
		vars[fmt.Sprintf("h%d", i)] = n
	}
	q.WriteString(") { repository(owner: $owner, name: $repo) {")
	for i := range names {
		fmt.Fprintf(&q, " b%d: pullRequests(headRefName: $h%d, states: MERGED, first: 1) { nodes { url } }", i, i)
	}
	q.WriteString(" } }")
	var d struct {
		Repository map[string]struct {
			Nodes []struct {
				URL string `json:"url"`
			} `json:"nodes"`
		} `json:"repository"`
	}
	if err := githubGraphQL(ctx, token, q.String(), vars, &d); err != nil {
		return res
	}
	for i, n := range names {
		if pr := d.Repository[fmt.Sprintf("b%d", i)]; len(pr.Nodes) > 0 {
			res[n] = pr.Nodes[0].URL
		}
	}
	return res
}

// ---------------------------------------------------------------- focus

// graphFocus is what the Graph tab highlights: a branch's own commits, the
// commit it forked from, and the merge that brought it in.
type graphFocus struct {
	Label string   `json:"label,omitempty"`
	Tip   string   `json:"tip,omitempty"`
	SHAs  []string `json:"shas"`
	Fork  string   `json:"fork,omitempty"`
	Merge string   `json:"merge,omitempty"` // the entry merge (the newest, for a branch merged more than once)
	// Merges is every merge that brought the branch in; more than one when it
	// was merged, continued, and merged again.
	Merges []string `json:"merges,omitempty"`
	Seg    int      `json:"seg,omitempty"`
}

const graphFocusMax = 5000

func revParse(root, rev string) string {
	out, err := exec.Command("git", "-C", root, "rev-parse", "--verify", "--quiet", rev+"^{commit}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func isAncestor(root, a, b string) bool {
	return exec.Command("git", "-C", root, "merge-base", "--is-ancestor", a, b).Run() == nil
}

// refFocus is the focus set of a named branch against the default branch:
//   - unmerged: its commits not in the default branch, and the fork point;
//   - merged: the entry merge M, the oldest commit on the default branch's
//     first-parent chain that contains the tip; the branch is M's second
//     parent side (rev-list M^2 ^M^1), with the fork point and M;
//   - merged by fast-forward (no merge commit): just the tip.
func refFocus(root, ref, def string) (graphFocus, error) {
	tip := revParse(root, ref)
	if tip == "" {
		return graphFocus{}, fmt.Errorf("no such branch: %s", ref)
	}
	f := graphFocus{Label: ref, Tip: tip, SHAs: []string{}}
	defTip := ""
	if def != "" {
		defTip = revParse(root, def)
	}
	limit := "--max-count=" + strconv.Itoa(graphFocusMax)
	switch {
	case defTip == "" || ref == def || "origin/"+ref == def:
		// The default branch itself, or nothing to compare against: its
		// first-parent line.
		shas, err := gitLines(root, "rev-list", "--first-parent", limit, tip)
		f.SHAs = append(f.SHAs, shas...)
		return f, err
	case !isAncestor(root, tip, defTip):
		shas, err := gitLines(root, "rev-list", limit, tip, "^"+defTip)
		if err != nil {
			return f, err
		}
		f.SHAs = append(f.SHAs, shas...)
		if mb, _ := gitLines(root, "merge-base", tip, defTip); len(mb) > 0 {
			f.Fork = mb[0]
		}
		return f, nil
	}
	desc, err := gitLines(root, "rev-list", "--ancestry-path", tip+".."+defTip)
	if err != nil {
		return f, err
	}
	chain, err := gitLines(root, "rev-list", "--first-parent", tip+".."+defTip)
	if err != nil {
		return f, err
	}
	isDesc := make(map[string]bool, len(desc))
	for _, d := range desc {
		isDesc[d] = true
	}
	m := ""
	for _, c := range chain { // newest first: the last hit is the oldest
		if isDesc[c] {
			m = c
		}
	}
	parents, _ := gitLines(root, "rev-list", "--parents", "-n", "1", m)
	var ps []string
	if len(parents) > 0 {
		ps = strings.Fields(parents[0])[1:]
	}
	if m == "" || len(ps) < 2 || isAncestor(root, tip, ps[0]) {
		f.SHAs = append(f.SHAs, tip) // fast-forwarded in: no merge marks where it began
		return f, nil
	}
	shas, err := gitLines(root, "rev-list", limit, ps[1], "^"+ps[0])
	if err != nil {
		return f, err
	}
	f.SHAs = append(f.SHAs, shas...)
	f.Merge = m
	if mb, _ := gitLines(root, "merge-base", ps[0], ps[1]); len(mb) > 0 {
		f.Fork = mb[0]
	}
	return f, nil
}

// ---------------------------------------------------------------- HTTP

type branchCache struct {
	key string
	at  time.Time
	res map[string]any
}

var (
	branchMu         sync.Mutex
	graphBranchCache *branchCache
)

// handleGraphBranches lists branches with categories: GET, ?refresh=1.
func (s *Server) handleGraphBranches(w http.ResponseWriter, r *http.Request) {
	root, ok := s.graphRoot(w)
	if !ok {
		return
	}
	ri, err := readRefs(root)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	sig := ri.sig
	cfg := readSettings()
	key := root + "\x00" + sig
	branchMu.Lock()
	c := graphBranchCache
	branchMu.Unlock()
	if c != nil && c.key == key && r.URL.Query().Get("refresh") == "" && time.Since(c.at) < 30*time.Second {
		writeJSON(w, c.res)
		return
	}

	def, _ := ri.defaultBranch(root, cfg)
	var lookup func([]string) map[string]string
	if repo := s.inboxRepo(); repo != "" {
		if token, _ := s.inboxToken(); token != "" {
			owner, name, _ := strings.Cut(repo, "/")
			lookup = func(names []string) map[string]string {
				ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
				defer cancel()
				return githubSquashLookup(ctx, token, owner, name, names)
			}
		}
	}
	branches, err := graphBranches(root, def, cfg.graphStaleDays(), time.Now(), lookup)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if branches == nil {
		branches = []graphBranch{}
	}
	res := map[string]any{"default": def, "branches": branches, "staleDays": cfg.graphStaleDays(), "squashChecked": lookup != nil}
	branchMu.Lock()
	graphBranchCache = &branchCache{key: key, at: time.Now(), res: res}
	branchMu.Unlock()
	writeJSON(w, res)
}

// handleGraphFocus returns a focus set: GET ?ref=<branch> for a named branch,
// ?sha=<commit> for the lane segment a commit is on, or ?seg=<id> for a lane
// segment clicked in the graph (which also works for deleted branches).
func (s *Server) handleGraphFocus(w http.ResponseWriter, r *http.Request) {
	root, ok := s.graphRoot(w)
	if !ok {
		return
	}
	q := r.URL.Query()
	if ref := q.Get("ref"); ref != "" {
		f, err := refFocus(root, ref, graphDefaultBranch(root, readSettings()))
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, f)
		return
	}
	gs, err := graphSessionFor(root)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	var seg int
	if v := q.Get("seg"); v != "" {
		if seg, err = strconv.Atoi(v); err != nil {
			fail(w, http.StatusBadRequest, "seg must be a number")
			return
		}
	} else if sha := q.Get("sha"); sha != "" {
		if seg, err = gs.segOf(sha); err != nil {
			fail(w, http.StatusNotFound, err.Error())
			return
		}
	} else {
		fail(w, http.StatusBadRequest, "give ref, sha or seg")
		return
	}
	writeJSON(w, gs.segmentFocus(seg))
}
