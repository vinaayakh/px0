package main

import (
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// graph_commit.go serves what the Graph tab shows about one commit (its
// message, parents, refs and changed files, and a file's diff), commit
// search, and the cheap ref signature the tab polls to notice that refs moved.

// emptyTree is git's well-known empty tree, the "parent" of a root commit.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

var (
	shaRe = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
	hexRe = regexp.MustCompile(`^[0-9a-fA-F]+$`)
)

type graphCommitFile struct {
	Path    string `json:"path"`
	OldPath string `json:"oldPath,omitempty"` // for a rename or copy
	Status  string `json:"status"`            // A, M, D, R, C, T
	Added   int    `json:"added"`             // -1 for a binary file
	Deleted int    `json:"deleted"`
}

type graphCommitDetail struct {
	SHA       string            `json:"sha"`
	Parents   []string          `json:"parents"`
	Author    string            `json:"author"`
	Email     string            `json:"email"`
	Date      int64             `json:"date"`
	Committer string            `json:"committer"`
	CDate     int64             `json:"committerDate"`
	Message   string            `json:"message"`
	Refs      []graphRef        `json:"refs"`
	Files     []graphCommitFile `json:"files"`
	// Against is what the file list compares with: the first parent, or the
	// empty tree for a root commit. A merge's changes are those it brought
	// relative to its first parent, as `git show --first-parent` shows them.
	Against string `json:"against"`
}

// graphCommitInfo reads a commit's header and its changed files against its
// first parent in two git calls.
func graphCommitInfo(root, sha string) (graphCommitDetail, error) {
	out, err := exec.Command("git", "-C", root, "show", "-s", "--no-color",
		"--format=%H%x1f%P%x1f%an%x1f%ae%x1f%at%x1f%cn%x1f%ct%x1f%B", sha).Output()
	if err != nil {
		return graphCommitDetail{}, err
	}
	f := strings.SplitN(string(out), "\x1f", 8)
	if len(f) < 8 {
		return graphCommitDetail{}, errGraphNoRow
	}
	d := graphCommitDetail{SHA: f[0], Author: f[2], Email: f[3], Committer: f[5], Message: strings.TrimRight(f[7], "\n"), Refs: []graphRef{}, Files: []graphCommitFile{}}
	d.Parents = strings.Fields(f[1])
	if d.Parents == nil {
		d.Parents = []string{}
	}
	d.Date, _ = strconv.ParseInt(f[4], 10, 64)
	d.CDate, _ = strconv.ParseInt(f[6], 10, 64)
	d.Against = emptyTree
	if len(d.Parents) > 0 {
		d.Against = d.Parents[0]
	}

	// --raw and --numstat in one call (with --name-status instead of --raw,
	// git prints only one of the two), NUL-separated so any path is safe.
	raw, err := exec.Command("git", "-C", root, "-c", "core.quotepath=false", "diff-tree", "-r", "-M", "--no-commit-id",
		"--raw", "--numstat", "-z", d.Against, d.SHA).Output()
	if err != nil {
		return d, nil // a commit without a readable diff still has its message
	}
	d.Files = parseDiffTreeZ(string(raw))
	return d, nil
}

// parseDiffTreeZ reads `git diff-tree -z --raw --numstat`: every raw record
// (":<modes> <shas> <status>", then the path, or old and new paths for a
// rename or copy), then every numstat record ("add\tdel\tpath", or
// "add\tdel\t" and then old and new paths).
func parseDiffTreeZ(raw string) []graphCommitFile {
	parts := strings.Split(raw, "\x00")
	type stat struct{ add, del int }
	stats := map[string]stat{}
	var files []graphCommitFile
	for i := 0; i < len(parts); i++ {
		p := parts[i]
		if p == "" {
			continue
		}
		if fields := strings.Split(p, "\t"); len(fields) == 3 {
			// numstat: "add\tdel\tpath", or "add\tdel\t" followed by old and new path for a rename.
			add, err1 := strconv.Atoi(fields[0])
			del, err2 := strconv.Atoi(fields[1])
			if fields[0] == "-" {
				add, err1, del, err2 = -1, nil, 0, nil
			}
			if err1 == nil && err2 == nil {
				path := fields[2]
				if path == "" && i+2 < len(parts) {
					path = parts[i+2]
					i += 2
				}
				stats[path] = stat{add, del}
				continue
			}
		}
		// raw: ":100644 100644 <old> <new> M", then path; "R100", then old and new path.
		if !strings.HasPrefix(p, ":") || i+1 >= len(parts) {
			continue
		}
		hdr := strings.Fields(p)
		status := hdr[len(hdr)-1]
		if !strings.ContainsRune("ACDMRTUX", rune(status[0])) {
			continue
		}
		f := graphCommitFile{Status: status[:1], Path: parts[i+1]}
		i++
		if (f.Status == "R" || f.Status == "C") && i+1 < len(parts) {
			f.OldPath, f.Path = f.Path, parts[i+1]
			i++
		}
		files = append(files, f)
	}
	for i := range files {
		if st, ok := stats[files[i].Path]; ok {
			files[i].Added, files[i].Deleted = st.add, st.del
		}
	}
	if files == nil {
		files = []graphCommitFile{}
	}
	return files
}

// handleGraphCommit: GET ?sha= -> graphCommitDetail.
func (s *Server) handleGraphCommit(w http.ResponseWriter, r *http.Request) {
	root, ok := s.graphRoot(w)
	if !ok {
		return
	}
	sha := r.URL.Query().Get("sha")
	if !shaRe.MatchString(sha) {
		fail(w, http.StatusBadRequest, "sha must be a commit hash")
		return
	}
	d, err := graphCommitInfo(root, sha)
	if err != nil {
		fail(w, http.StatusNotFound, "no such commit")
		return
	}
	if ri, err := readRefs(root); err == nil && ri.refs[d.SHA] != nil {
		d.Refs = ri.refs[d.SHA]
	}
	writeJSON(w, d)
}

// handleGraphDiff: GET ?sha=&path=[&old=] -> {diff}, the file's change in
// that commit against its first parent (or the empty tree).
func (s *Server) handleGraphDiff(w http.ResponseWriter, r *http.Request) {
	root, ok := s.graphRoot(w)
	if !ok {
		return
	}
	q := r.URL.Query()
	sha, path, old := q.Get("sha"), q.Get("path"), q.Get("old")
	if !shaRe.MatchString(sha) || path == "" {
		fail(w, http.StatusBadRequest, "sha and path are required")
		return
	}
	against := emptyTree
	if out, err := exec.Command("git", "-C", root, "rev-parse", "--verify", "--quiet", sha+"^1").Output(); err == nil {
		against = strings.TrimSpace(string(out))
	}
	args := []string{"-C", root, "-c", "core.quotepath=false", "diff", "--no-color", "--no-ext-diff", "-M", against, sha, "--", path}
	if old != "" && old != path {
		args = append(args, old)
	}
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		fail(w, http.StatusInternalServerError, "git diff failed")
		return
	}
	const maxDiff = 2 << 20
	truncated := len(out) > maxDiff
	if truncated {
		out = out[:maxDiff]
	}
	writeJSON(w, map[string]any{"diff": string(out), "truncated": truncated})
}

// handleGraphSig: GET -> {sig}. One for-each-ref; the tab compares it with
// the signature of the rows it has, whenever git status changes, and reloads
// in place when refs moved.
func (s *Server) handleGraphSig(w http.ResponseWriter, r *http.Request) {
	root, ok := s.graphRoot(w)
	if !ok {
		return
	}
	ri, err := readRefs(root)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"sig": ri.sig})
}

const (
	graphSearchMax  = 1000   // matches returned
	graphSearchRead = 300000 // rows read to answer a search
)

// search returns the rows whose subject or author contains q (any case), or
// whose hash starts with it. It reads the rest of history first, up to a
// bound, so matches far below the loaded pages are found too.
func (s *graphSession) search(q string) (idx []int, complete bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fill(graphSearchRead)
	lq := strings.ToLower(q)
	hashy := len(q) >= 4 && hexRe.MatchString(q)
	idx = []int{}
	for i, r := range s.rows {
		if (hashy && strings.HasPrefix(r.SHA, lq)) ||
			strings.Contains(strings.ToLower(r.Subject), lq) || strings.Contains(strings.ToLower(r.Author), lq) {
			idx = append(idx, i)
			if len(idx) >= graphSearchMax {
				break
			}
		}
	}
	return idx, s.done
}

// handleGraphSearch: GET ?q=&sig= -> {matches: [row index], complete}.
func (s *Server) handleGraphSearch(w http.ResponseWriter, r *http.Request) {
	root, ok := s.graphRoot(w)
	if !ok {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, map[string]any{"matches": []int{}, "complete": true})
		return
	}
	gs, err := graphSessionFor(root)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if want := r.URL.Query().Get("sig"); want != "" && want != gs.sig {
		writeJSON(w, map[string]any{"reset": true, "sig": gs.sig})
		return
	}
	idx, complete := gs.search(q)
	writeJSON(w, map[string]any{"matches": idx, "complete": complete, "capped": len(idx) >= graphSearchMax})
}
