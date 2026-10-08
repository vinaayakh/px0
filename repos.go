package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// repos.go keeps the reviewer's local repositories: clones px0 can check a
// pull request out of, saved in ~/.px0/settings.json under "repositories" so
// a PR of any of them can be reviewed from anywhere, the way GitHub Desktop
// remembers the repositories added to it. A repository px0 is opened in is
// remembered too.
//
// A clone is matched to a PR by its remotes: origin first, then upstream (a
// fork's clone), then any other.

// localRepo is one saved repository.
type localRepo struct {
	Path   string `json:"path"`   // the clone's top-level directory
	Repo   string `json:"repo"`   // owner/name its remote points at
	Remote string `json:"remote"` // the remote that points there
	Added  string `json:"added,omitempty"`
}

const reposSettingsKey = "repositories"

// remoteRepoRe takes owner/name from the end of any remote URL:
// https://host/owner/name(.git), git@host:owner/name.git, ssh://...
var remoteRepoRe = regexp.MustCompile(`[:/]([^/:\s]+)/([^/\s]+?)(?:\.git)?/?$`)

func repoFromRemoteURL(u string) string {
	m := remoteRepoRe.FindStringSubmatch(strings.TrimSpace(u))
	if m == nil {
		return ""
	}
	return m[1] + "/" + m[2]
}

// repoRemotes lists dir's remotes as name -> owner/name, origin and upstream
// first.
func repoRemotes(dir string) (names []string, repos map[string]string) {
	out, err := exec.Command("git", "-C", dir, "remote", "-v").Output()
	if err != nil {
		return nil, nil
	}
	repos = map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || repos[f[0]] != "" {
			continue
		}
		if r := repoFromRemoteURL(f[1]); r != "" {
			repos[f[0]] = r
			names = append(names, f[0])
		}
	}
	rank := func(n string) int {
		switch n {
		case "origin":
			return 0
		case "upstream":
			return 1
		}
		return 2
	}
	for i := 1; i < len(names); i++ { // stable insertion sort by rank
		for j := i; j > 0 && rank(names[j]) < rank(names[j-1]); j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names, repos
}

// cloneOf reports whether dir is a clone of owner/name, and with which
// remote and top-level directory.
func cloneOf(dir, owner, name string) (toplevel, remote string, ok bool) {
	// Asked fresh rather than through gitProbe's cache: a folder may become
	// a clone, or stop being one, while px0 runs.
	if gitDisabled || dir == "" {
		return "", "", false
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", "", false
	}
	top := filepath.Clean(filepath.FromSlash(strings.TrimSpace(string(out))))
	want := owner + "/" + name
	names, repos := repoRemotes(top)
	for _, n := range names {
		if strings.EqualFold(repos[n], want) {
			return top, n, true
		}
	}
	return "", "", false
}

// describeClone validates dir as a repository px0 can review from: a git
// clone with a remote naming owner/name. It prefers origin, then upstream.
func describeClone(dir string) (localRepo, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return localRepo{}, errors.New("choose a folder")
	}
	if strings.HasPrefix(dir, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, dir[1:])
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return localRepo{}, err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return localRepo{}, fmt.Errorf("%s is not a folder", abs)
	}
	top, err := exec.Command("git", "-C", abs, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return localRepo{}, fmt.Errorf("%s is not a git repository", abs)
	}
	root := filepath.Clean(filepath.FromSlash(strings.TrimSpace(string(top))))
	names, repos := repoRemotes(root)
	if len(names) == 0 {
		return localRepo{}, fmt.Errorf("%s has no remote to match pull requests to; add one with git remote add origin <url>", root)
	}
	return localRepo{Path: root, Repo: repos[names[0]], Remote: names[0]}, nil
}

// savedRepos reads the saved list. Never fails: no list is an empty one.
func savedRepos() []localRepo {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	return savedReposLocked()
}

func savedReposLocked() []localRepo {
	raw := readSettingsRawMap()[reposSettingsKey]
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var list []localRepo
	_ = json.Unmarshal(b, &list)
	out := list[:0]
	for _, r := range list {
		if r.Path != "" {
			out = append(out, r)
		}
	}
	return out
}

// editRepos changes the saved list under the settings lock.
func editRepos(fn func([]localRepo) []localRepo) ([]localRepo, error) {
	p := settingsPath()
	if p == "" {
		return nil, errors.New("no home directory to save repositories in")
	}
	settingsMu.Lock()
	defer settingsMu.Unlock()
	list := fn(savedReposLocked())
	raw := readSettingsRawMap()
	if len(list) == 0 {
		delete(raw, reposSettingsKey)
	} else {
		raw[reposSettingsKey] = list
	}
	return list, writeRawMapLocked(p, raw)
}

func samePath(a, b string) bool { return normRepoPath(a) == normRepoPath(b) }

// addRepo saves dir, or refreshes its entry if it is saved already.
func addRepo(dir string) (localRepo, error) {
	r, err := describeClone(dir)
	if err != nil {
		return localRepo{}, err
	}
	r.Added = time.Now().UTC().Format(time.RFC3339)
	_, err = editRepos(func(list []localRepo) []localRepo {
		for i, cur := range list {
			if samePath(cur.Path, r.Path) {
				r.Added = cur.Added
				list[i] = r
				return list
			}
		}
		return append(list, r)
	})
	return r, err
}

func removeRepo(dir string) error {
	_, err := editRepos(func(list []localRepo) []localRepo {
		out := list[:0]
		for _, r := range list {
			if !samePath(r.Path, dir) {
				out = append(out, r)
			}
		}
		return out
	})
	return err
}

// rememberWorkspaceRepo saves the repository px0 was opened in, so its PRs
// can be reviewed later from anywhere. Quiet and best-effort.
func rememberWorkspaceRepo(root string) {
	r, err := describeClone(root)
	if err != nil {
		return
	}
	for _, cur := range savedRepos() {
		if samePath(cur.Path, r.Path) {
			return
		}
	}
	_, _ = addRepo(r.Path)
}

// findLocalClone is the saved clone of owner/name, checked against the disk:
// a clone that moved, was deleted, or no longer points there is skipped.
func findLocalClone(owner, name string) (toplevel, remote string, ok bool) {
	want := owner + "/" + name
	for _, r := range savedRepos() {
		if !strings.EqualFold(r.Repo, want) {
			continue
		}
		if top, rem, ok := cloneOf(r.Path, owner, name); ok {
			return top, rem, true
		}
	}
	// A saved clone of a fork may also have the PR's repo as another remote.
	for _, r := range savedRepos() {
		if top, rem, ok := cloneOf(r.Path, owner, name); ok {
			return top, rem, true
		}
	}
	return "", "", false
}

// ---------------------------------------------------------------- folder picker

// pickFolder shows the system's folder dialog on this machine and returns
// the chosen folder, "" if cancelled. errNoPicker when there is none.
var pickFolder = func(prompt string) (string, error) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		// A topmost owner keeps the dialog above the browser that asked.
		script := `Add-Type -AssemblyName System.Windows.Forms
$o = New-Object System.Windows.Forms.Form -Property @{TopMost=$true; ShowInTaskbar=$false}
$d = New-Object System.Windows.Forms.FolderBrowserDialog
$d.Description = $env:PX0_PROMPT
$d.ShowNewFolderButton = $false
if ($d.ShowDialog($o) -eq 'OK') { [Console]::Out.Write($d.SelectedPath) }`
		cmd = exec.Command("powershell", "-NoProfile", "-NonInteractive", "-STA", "-Command", script)
	case "darwin":
		cmd = exec.Command("osascript", "-e", `try`, "-e",
			`POSIX path of (choose folder with prompt (system attribute "PX0_PROMPT"))`, "-e", `end try`)
	default:
		if p, err := exec.LookPath("zenity"); err == nil {
			cmd = exec.Command(p, "--file-selection", "--directory", "--title", prompt)
		} else if p, err := exec.LookPath("kdialog"); err == nil {
			cmd = exec.Command(p, "--getexistingdirectory", ".", "--title", prompt)
		} else {
			return "", errNoPicker
		}
	}
	cmd.Env = append(os.Environ(), "PX0_PROMPT="+prompt)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", nil // closed or cancelled (zenity and kdialog exit 1)
		}
		return "", errNoPicker
	}
	return strings.TrimRight(strings.TrimSpace(string(out)), `/\`), nil
}

var errNoPicker = errors.New("no folder dialog is available here; type the folder's path instead")

// ---------------------------------------------------------------- HTTP

type repoView struct {
	localRepo
	Missing bool `json:"missing,omitempty"` // the folder is gone or is no longer that clone
	Current bool `json:"current,omitempty"` // this px0's workspace
}

func (s *Server) repoViews() []repoView {
	list := savedRepos()
	out := make([]repoView, 0, len(list))
	root := ""
	if s.ix != nil {
		root = s.ix.Root()
	}
	for _, r := range list {
		v := repoView{localRepo: r, Current: root != "" && samePath(root, r.Path)}
		owner, name, _ := strings.Cut(r.Repo, "/")
		if _, _, ok := cloneOf(r.Path, owner, name); !ok {
			v.Missing = true
		}
		out = append(out, v)
	}
	return out
}

// handleRepos lists the saved repositories (GET) or adds one (POST {path}).
func (s *Server) handleRepos(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, map[string]any{"repos": s.repoViews()})
		return
	}
	if !localPost(w, r) {
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "path is required")
		return
	}
	added, err := addRepo(body.Path)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "added": added, "repos": s.repoViews()})
}

// handleReposRemove forgets a saved repository (POST {path}). The clone on
// disk is not touched.
func (s *Server) handleReposRemove(w http.ResponseWriter, r *http.Request) {
	if !localPost(w, r) {
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&body); err != nil || body.Path == "" {
		fail(w, http.StatusBadRequest, "path is required")
		return
	}
	if err := removeRepo(body.Path); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "repos": s.repoViews()})
}

// handleReposBrowse shows the system folder dialog (POST {prompt}) and
// answers with the folder chosen, "" if cancelled. localPost keeps it to a
// page on this machine: the dialog opens where px0 runs.
func (s *Server) handleReposBrowse(w http.ResponseWriter, r *http.Request) {
	if !localPost(w, r) {
		return
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		fail(w, http.StatusNotImplemented, "the folder dialog would open on the machine px0 runs on; type the folder's path instead")
		return
	}
	var body struct {
		Prompt string `json:"prompt"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body)
	if body.Prompt == "" {
		body.Prompt = "Choose a local clone"
	}
	path, err := pickFolder(body.Prompt)
	if err != nil {
		fail(w, http.StatusNotImplemented, err.Error())
		return
	}
	writeJSON(w, map[string]any{"path": path})
}
