package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// prcleanup.go gives every PR review a clear end, after which nothing of it
// is left on disk: the temp checkout, its worktree registration in the
// reviewer's repository, the refs px0 fetched there (refs/px0/pr/N and
// refs/px0/base/N), and the session file kept for the checkout.
//
// The normal end is the End review button (/api/pr/end) or Ctrl-C; both go
// through prSession.Close. A review that never got there -- px0 killed,
// crashed, or its terminal closed -- is caught the next time px0 starts: a
// marker beside each checkout names the process that owns it, and
// sweepStalePRCheckouts removes the checkouts whose owner is gone.

// prMarkerSuffix names the marker beside a checkout: <checkout>.px0-review.json.
// It sits outside the checkout so it never shows up as a change in the PR.
const prMarkerSuffix = ".px0-review.json"

// prCheckoutPrefix is the temp-directory prefix of every PR checkout.
const prCheckoutPrefix = "px0-pr-"

// An unmarked checkout (made before markers existed, or one whose process
// died before writing its marker) is only removed once it is this old, so a
// checkout still being prepared is never swept from under its process.
const prUnmarkedGrace = time.Hour

// prMarker records who owns a checkout and what to remove with it.
type prMarker struct {
	PID         int    `json:"pid"`
	Worktree    string `json:"worktree"`
	SrcRepo     string `json:"srcRepo,omitempty"` // repo the worktree is registered in; "" for a plain clone
	Number      int    `json:"number"`
	SessionFile string `json:"sessionFile,omitempty"` // session state kept for this checkout only
	Started     string `json:"started"`
}

func prMarkerPath(worktree string) string { return worktree + prMarkerSuffix }

// writeMarker records this process as the checkout's owner. Best-effort: a
// checkout without a marker is still swept, only later.
func (p *prSession) writeMarker() {
	if p == nil || p.worktree == "" {
		return
	}
	m := prMarker{
		PID: os.Getpid(), Worktree: p.worktree, SrcRepo: p.srcRepo, Number: p.meta.Number,
		SessionFile: p.sessionFile, Started: time.Now().UTC().Format(time.RFC3339),
	}
	if b, err := json.MarshalIndent(m, "", "  "); err == nil {
		_ = os.WriteFile(prMarkerPath(p.worktree), append(b, '\n'), 0o644)
	}
}

// removePRCheckout deletes everything a review left: the worktree (and its
// registration), the refs it fetched, its session file and its marker.
// Every step is best-effort and safe to repeat.
func removePRCheckout(srcRepo, worktree string, num int, sessionFile string) {
	if srcRepo != "" {
		// --force twice also removes a locked or dirty worktree.
		exec.Command("git", "-C", srcRepo, "worktree", "remove", "--force", "--force", worktree).Run()
		if num > 0 {
			exec.Command("git", "-C", srcRepo, "update-ref", "-d", fmt.Sprintf("refs/px0/pr/%d", num)).Run()
			exec.Command("git", "-C", srcRepo, "update-ref", "-d", fmt.Sprintf("refs/px0/base/%d", num)).Run()
		}
	}
	if worktree != "" {
		os.RemoveAll(worktree)
		os.Remove(prMarkerPath(worktree))
	}
	if srcRepo != "" {
		// Drops the registration even when the directory was already gone.
		exec.Command("git", "-C", srcRepo, "worktree", "prune").Run()
	}
	if sessionFile != "" {
		os.Remove(sessionFile)
	}
}

// processAlive reports whether pid is a running process. On doubt it says
// yes: a checkout is left for later rather than removed from under a review.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if pid == os.Getpid() {
		return true
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false // Windows: no such process
	}
	defer proc.Release()
	if runtime.GOOS == "windows" {
		return true // FindProcess opened it, so it exists
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || err == syscall.EPERM
}

// sweepStalePRCheckouts removes the PR checkouts in tmpDir whose owning
// process is gone, and returns what it removed. repo, when set, is the
// reviewer's repository: refs px0 left there for reviews no live checkout
// owns are removed too, and stale worktree registrations are pruned.
func sweepStalePRCheckouts(tmpDir, repo string, alive func(int) bool) []string {
	var removed []string
	live := map[string]map[int]bool{} // repo -> PR numbers still under review
	keep := func(m prMarker) {
		if m.SrcRepo == "" {
			return
		}
		key := normRepoPath(m.SrcRepo)
		if live[key] == nil {
			live[key] = map[int]bool{}
		}
		live[key][m.Number] = true
	}

	// Checkouts, marked or not.
	dirs, _ := filepath.Glob(filepath.Join(tmpDir, prCheckoutPrefix+"*"))
	for _, d := range dirs {
		if strings.HasSuffix(d, prMarkerSuffix) {
			continue
		}
		st, err := os.Stat(d)
		if err != nil || !st.IsDir() {
			continue
		}
		m, ok := readPRMarker(prMarkerPath(d))
		switch {
		case ok && alive(m.PID):
			keep(m)
		case ok:
			removePRCheckout(m.SrcRepo, d, m.Number, m.SessionFile)
			removed = append(removed, d)
		case time.Since(st.ModTime()) > prUnmarkedGrace:
			os.RemoveAll(d)
			removed = append(removed, d)
		}
	}
	// Markers whose checkout is already gone still name refs to remove.
	markers, _ := filepath.Glob(filepath.Join(tmpDir, prCheckoutPrefix+"*"+prMarkerSuffix))
	for _, mp := range markers {
		m, ok := readPRMarker(mp)
		if !ok {
			os.Remove(mp)
			continue
		}
		if _, err := os.Stat(m.Worktree); err == nil {
			continue // handled above
		}
		if alive(m.PID) {
			keep(m)
			continue
		}
		removePRCheckout(m.SrcRepo, m.Worktree, m.Number, m.SessionFile)
		os.Remove(mp)
	}

	if repo != "" && gitAvailable(repo) {
		exec.Command("git", "-C", repo, "worktree", "prune").Run()
		top := repo
		if out, err := exec.Command("git", "-C", repo, "rev-parse", "--show-toplevel").Output(); err == nil {
			top = strings.TrimSpace(string(out))
		}
		for _, n := range px0RefNumbers(repo) {
			if live[normRepoPath(top)][n] || live[normRepoPath(repo)][n] {
				continue
			}
			exec.Command("git", "-C", repo, "update-ref", "-d", fmt.Sprintf("refs/px0/pr/%d", n)).Run()
			exec.Command("git", "-C", repo, "update-ref", "-d", fmt.Sprintf("refs/px0/base/%d", n)).Run()
		}
	}
	return removed
}

// normRepoPath compares repository paths the way the OS does: git reports
// D:/x where Windows spells it D:\x, and Windows paths ignore case.
func normRepoPath(p string) string {
	p = filepath.Clean(filepath.FromSlash(p))
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

func readPRMarker(path string) (prMarker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return prMarker{}, false
	}
	var m prMarker
	if json.Unmarshal(b, &m) != nil || m.Worktree == "" {
		return prMarker{}, false
	}
	return m, true
}

// px0RefNumbers lists the PR numbers px0 has refs for in repo.
func px0RefNumbers(repo string) []int {
	out, err := exec.Command("git", "-C", repo, "for-each-ref", "--format=%(refname)", "refs/px0/pr/", "refs/px0/base/").Output()
	if err != nil {
		return nil
	}
	seen := map[int]bool{}
	var nums []int
	for _, ref := range strings.Fields(string(out)) {
		n, err := strconv.Atoi(ref[strings.LastIndexByte(ref, '/')+1:])
		if err != nil || seen[n] {
			continue
		}
		seen[n] = true
		nums = append(nums, n)
	}
	return nums
}

// ---------------------------------------------------------------- ending a review

// prEndState is what ending the review now would lose.
type prEndState struct {
	Drafts   int  `json:"drafts"`   // draft comments not submitted
	Dirty    bool `json:"dirty"`    // uncommitted edits in the checkout
	Unpushed int  `json:"unpushed"` // commits made in the checkout and not pushed
}

func (p *prSession) endState() prEndState {
	p.mu.Lock()
	st := prEndState{Drafts: countSubmittable(p.comments)}
	worktree, head := p.worktree, p.meta.HeadSHA
	p.mu.Unlock()
	st.Dirty = gitHasUncommittedChanges(worktree)
	if head != "" {
		if out, err := exec.Command("git", "-C", worktree, "rev-list", "--count", head+"..HEAD").Output(); err == nil {
			st.Unpushed, _ = strconv.Atoi(strings.TrimSpace(string(out)))
		}
	}
	return st
}

// SetOnEnd sets what ending the review does once the reply is sent: main
// shuts the server down, which closes the PR session and removes its
// checkout on the way out.
func (s *Server) SetOnEnd(fn func()) { s.onEnd = fn }

// handlePREnd ends the review: POST {force}. Without force, a review with
// something to lose (drafts, uncommitted edits, unpushed commits) answers
// 409 with what, so the page can ask first.
func (s *Server) handlePREnd(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) || !localPost(w, r) {
		return
	}
	var body struct {
		Force bool `json:"force"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&body)
	st := s.pr.endState()
	if !body.Force && (st.Drafts > 0 || st.Dirty || st.Unpushed > 0) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"error": "the review has unsaved work", "needsConfirm": true, "state": st})
		return
	}
	if s.onEnd == nil {
		fail(w, http.StatusNotImplemented, "this session cannot end itself; stop px0 with Ctrl-C")
		return
	}
	writeJSON(w, map[string]any{"ok": true, "worktree": s.pr.Root()})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	s.onEnd()
}
