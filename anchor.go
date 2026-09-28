package main

import (
	"sort"
	"strconv"
	"strings"
)

// anchor.go places AI review suggestions on lines GitHub will accept. A
// review comment must sit on a line inside one of the PR's diff hunks, on the
// side it names: RIGHT for added and context lines (new-file numbering), LEFT
// for deleted and context lines (base numbering). Language models misreport
// line numbers, so every suggestion carries the text of its line (quote) and
// is checked against the real diff here before it can become a draft. Nothing
// is dropped: a suggestion that cannot be placed becomes a file-level comment,
// or, for a file outside the PR, a note in the review summary.

// Anchor outcomes, shown on each suggestion.
const (
	anchorAnchored   = "anchored"   // reported line and quote agree
	anchorReanchored = "reanchored" // moved to the one nearby line matching the quote
	anchorFile       = "file"       // no line could be trusted; comment on the whole file
	anchorSummary    = "summary"    // file is not part of the PR; goes in the review body
)

// anchorWindow is how far from the reported line a quote is searched for.
const anchorWindow = 5

// diffFile is one file of a parsed PR diff.
type diffFile struct {
	Path    string // new path; the old path for a deleted file
	OldPath string // base path; equals Path unless renamed
	Status  string // "added", "deleted", "renamed", "modified"
	Binary  bool

	Right map[int]string // new-file line -> text, for added and context lines in hunks
	Left  map[int]string // base line -> text, for deleted and context lines in hunks

	rightHunk map[int]int // line -> hunk index, for range checks
	leftHunk  map[int]int
}

// prDiff is a whole PR diff (git diff -M <merge-base>), keyed by new path.
type prDiff struct {
	Files map[string]*diffFile
	byOld map[string]string // old path -> new path, for renames
}

// Paths returns the changed files, sorted.
func (pd *prDiff) Paths() []string {
	out := make([]string, 0, len(pd.Files))
	for p := range pd.Files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// parseUnifiedDiff reads the output of git diff (optionally with -M) into
// per-file line maps. It expects core.quotePath off, so paths are not
// C-quoted; a quoted path is unquoted best-effort.
func parseUnifiedDiff(diff string) *prDiff {
	pd := &prDiff{Files: map[string]*diffFile{}, byOld: map[string]string{}}
	var cur *diffFile
	hunk := 0
	oldLine, newLine := 0, 0
	oldLeft, newLeft := 0, 0 // lines of the current hunk still to come, per side
	inHunk := false

	finish := func() {
		if cur == nil {
			return
		}
		if cur.Path == "" {
			cur.Path = cur.OldPath
		}
		if cur.OldPath == "" {
			cur.OldPath = cur.Path
		}
		if cur.Status == "" {
			if cur.OldPath != cur.Path {
				cur.Status = "renamed"
			} else {
				cur.Status = "modified"
			}
		}
		if cur.Path != "" {
			pd.Files[cur.Path] = cur
			if cur.OldPath != cur.Path {
				pd.byOld[cur.OldPath] = cur.Path
			}
		}
		cur = nil
	}

	for _, line := range strings.Split(diff, "\n") {
		line = strings.TrimSuffix(line, "\r")
		switch {
		case strings.HasPrefix(line, "diff --git "):
			finish()
			cur = &diffFile{Right: map[int]string{}, Left: map[int]string{}, rightHunk: map[int]int{}, leftHunk: map[int]int{}}
			inHunk = false
			if a, b, ok := splitDiffGitHeader(strings.TrimPrefix(line, "diff --git ")); ok {
				cur.OldPath, cur.Path = a, b
			}
			continue
		case cur == nil:
			continue
		}
		if !inHunk || strings.HasPrefix(line, "@@") {
			switch {
			case strings.HasPrefix(line, "@@"):
				hunk++
				oldLine, oldLeft, newLine, newLeft = parseHunkHeader(line)
				inHunk = oldLeft > 0 || newLeft > 0
			case strings.HasPrefix(line, "new file mode"):
				cur.Status = "added"
			case strings.HasPrefix(line, "deleted file mode"):
				cur.Status = "deleted"
			case strings.HasPrefix(line, "rename from "):
				cur.OldPath = unquoteDiffPath(strings.TrimPrefix(line, "rename from "))
				cur.Status = "renamed"
			case strings.HasPrefix(line, "rename to "):
				cur.Path = unquoteDiffPath(strings.TrimPrefix(line, "rename to "))
				cur.Status = "renamed"
			case strings.HasPrefix(line, "Binary files ") || line == "GIT binary patch":
				cur.Binary = true
			case strings.HasPrefix(line, "--- "):
				if p := strings.TrimPrefix(line, "--- "); p != "/dev/null" {
					cur.OldPath = strings.TrimPrefix(unquoteDiffPath(p), "a/")
				}
			case strings.HasPrefix(line, "+++ "):
				if p := strings.TrimPrefix(line, "+++ "); p != "/dev/null" {
					cur.Path = strings.TrimPrefix(unquoteDiffPath(p), "b/")
				} else {
					cur.Path = cur.OldPath
					cur.Status = "deleted"
				}
			}
			continue
		}
		// Inside a hunk the header's counts, not the prefixes, say where it
		// ends, so a blank context line whose leading space was stripped
		// still counts as context.
		switch {
		case strings.HasPrefix(line, "\\"):
			// "\ No newline at end of file"
		case strings.HasPrefix(line, "+"):
			cur.Right[newLine] = line[1:]
			cur.rightHunk[newLine] = hunk
			newLine++
			newLeft--
		case strings.HasPrefix(line, "-"):
			cur.Left[oldLine] = line[1:]
			cur.leftHunk[oldLine] = hunk
			oldLine++
			oldLeft--
		default:
			text := strings.TrimPrefix(line, " ")
			cur.Right[newLine] = text
			cur.rightHunk[newLine] = hunk
			cur.Left[oldLine] = text
			cur.leftHunk[oldLine] = hunk
			newLine++
			oldLine++
			newLeft--
			oldLeft--
		}
		if oldLeft <= 0 && newLeft <= 0 {
			inHunk = false
		}
	}
	finish()
	return pd
}

// splitDiffGitHeader splits "a/<old> b/<new>". Without quoting the split is
// ambiguous when a path contains " b/", so the last occurrence wins; the
// ---/+++ and rename lines that follow correct it when present.
func splitDiffGitHeader(s string) (string, string, bool) {
	if strings.HasPrefix(s, `"`) {
		parts := strings.SplitN(s, `" "`, 2)
		if len(parts) == 2 {
			return strings.TrimPrefix(unquoteDiffPath(parts[0]+`"`), "a/"), strings.TrimPrefix(unquoteDiffPath(`"`+parts[1]), "b/"), true
		}
	}
	i := strings.LastIndex(s, " b/")
	if !strings.HasPrefix(s, "a/") || i < 0 {
		return "", "", false
	}
	return s[2:i], s[i+3:], true
}

func unquoteDiffPath(p string) string {
	if len(p) >= 2 && p[0] == '"' && p[len(p)-1] == '"' {
		if u, err := strconv.Unquote(p); err == nil {
			return u
		}
	}
	return p
}

// parseHunkHeader reads "@@ -a,b +c,d @@"; an omitted count means 1.
func parseHunkHeader(hdr string) (oldStart, oldCount, newStart, newCount int) {
	oldStart, oldCount, newStart, newCount = 1, 1, 1, 1
	fields := strings.Fields(hdr)
	for _, f := range fields[1:] { // fields[0] is the opening "@@"
		if f == "@@" {
			break // the rest is the function-context hint
		}
		if len(f) < 2 || (f[0] != '-' && f[0] != '+') {
			continue
		}
		start, count := f[1:], "1"
		if j := strings.IndexByte(start, ','); j >= 0 {
			start, count = start[:j], start[j+1:]
		}
		s, err1 := strconv.Atoi(start)
		c, err2 := strconv.Atoi(count)
		if err1 != nil || err2 != nil {
			continue
		}
		if f[0] == '-' {
			oldStart, oldCount = s, c
		} else {
			newStart, newCount = s, c
		}
	}
	return
}

// anchorResult is where a suggestion lands.
type anchorResult struct {
	Status    string `json:"status"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	StartLine int    `json:"startLine,omitempty"`
	Side      string `json:"side"`
}

// resolvePath maps a path as a model might write it to a changed file.
func (pd *prDiff) resolvePath(p string) (string, bool) {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	cands := []string{p, strings.TrimPrefix(p, "./")}
	for _, pre := range []string{"a/", "b/", "/"} {
		if strings.HasPrefix(p, pre) {
			cands = append(cands, strings.TrimPrefix(p, pre))
		}
	}
	for _, c := range cands {
		if _, ok := pd.Files[c]; ok {
			return c, true
		}
		if n, ok := pd.byOld[c]; ok {
			return n, true
		}
	}
	return p, false
}

// quoteKey reduces a quote to the text of one line: the last non-blank line,
// whitespace-trimmed.
func quoteKey(q string) string {
	lines := strings.Split(strings.ReplaceAll(q, "\r", ""), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s
		}
	}
	return ""
}

// Anchor validates one suggestion against the diff:
//  1. a file outside the PR becomes a summary note;
//  2. a line inside a hunk on its side whose text matches the quote stays;
//  3. otherwise the quote is searched within anchorWindow lines on the same
//     side, then the other side (models mix them up); exactly one match moves
//     the anchor there;
//  4. anything else becomes a file-level comment, keeping the reported line
//     as a hint.
//
// A range (startLine < line) moves with its end line and is kept only when its
// start is in the same hunk on the same side.
func (pd *prDiff) Anchor(path string, line, startLine int, side, quote string) anchorResult {
	side = strings.ToUpper(strings.TrimSpace(side))
	if side != "LEFT" {
		side = "RIGHT"
	}
	res := anchorResult{Path: path, Line: line, Side: side}
	p, ok := pd.resolvePath(path)
	res.Path = p
	if !ok {
		res.Status = anchorSummary
		return res
	}
	f := pd.Files[p]
	if f.Binary {
		res.Status = anchorFile
		return res
	}
	q := quoteKey(quote)
	lines := func(s string) map[int]string {
		if s == "LEFT" {
			return f.Left
		}
		return f.Right
	}

	if text, ok := lines(side)[line]; ok && (q == "" || strings.TrimSpace(text) == q) {
		res.Status = anchorAnchored
		res.StartLine = f.rangeStart(side, line, startLine)
		return res
	}
	if q != "" {
		other := "LEFT"
		if side == "LEFT" {
			other = "RIGHT"
		}
		for _, s := range []string{side, other} {
			var hits []int
			for n := line - anchorWindow; n <= line+anchorWindow; n++ {
				if text, ok := lines(s)[n]; ok && strings.TrimSpace(text) == q {
					hits = append(hits, n)
				}
			}
			if len(hits) == 1 {
				res.Status = anchorReanchored
				res.Side = s
				res.Line = hits[0]
				if startLine > 0 && s == side {
					res.StartLine = f.rangeStart(s, hits[0], startLine+hits[0]-line)
				}
				return res
			}
			if len(hits) > 1 {
				break // ambiguous: don't guess, and don't look for a luckier side
			}
		}
	}
	res.Status = anchorFile
	return res
}

// rangeStart returns start when [start, end] is a valid multi-line range on
// side (both ends in the same hunk), or 0 for a single-line comment.
func (f *diffFile) rangeStart(side string, end, start int) int {
	if start <= 0 || start >= end {
		return 0
	}
	hunks := f.rightHunk
	if side == "LEFT" {
		hunks = f.leftHunk
	}
	h, ok := hunks[start]
	if !ok || h != hunks[end] {
		return 0
	}
	return start
}
