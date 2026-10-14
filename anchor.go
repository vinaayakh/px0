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
	anchorUnverified = "unverified" // kept on the reported line, which is in the diff, though the quote did not match it
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
	ctxNew    map[int]int // base line -> new line, for context lines
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
			cur = &diffFile{Right: map[int]string{}, Left: map[int]string{}, rightHunk: map[int]int{}, leftHunk: map[int]int{}, ctxNew: map[int]int{}}
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
			cur.ctxNew[oldLine] = newLine
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

// normCode reduces a line of code for comparison: whitespace runs collapsed
// to one space, ends trimmed.
func normCode(s string) string { return strings.Join(strings.Fields(s), " ") }

// quoteLines returns a quote's non-blank lines, normalised.
func quoteLines(q string) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(q, "\r", ""), "\n") {
		if s := normCode(l); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// lineMatches reports whether a diff line's text is one of the quote lines:
// exactly, or once a "+"/"-" copied from the diff is dropped, or (loose) as a
// distinctive part of the line -- models often quote only part of a line.
func lineMatches(text string, qs []string, loose bool) bool {
	t := normCode(text)
	if t == "" {
		return false
	}
	for _, q := range qs {
		if t == q || (len(q) > 1 && (q[0] == '+' || q[0] == '-') && t == normCode(q[1:])) {
			return true
		}
		if loose && len(q) >= 8 && strings.Contains(t, q) {
			return true
		}
	}
	return false
}

// anchorRangeMax bounds how many lines a reported range is searched for its
// quote.
const anchorRangeMax = 50

// Anchor validates one suggestion against the diff:
//  1. a file outside the PR becomes a summary note;
//  2. a line inside a hunk on its side stays when the quote matches it (or,
//     for a range, any line of the range);
//  3. otherwise the quote is searched within anchorWindow lines on the same
//     side, then the other side (models mix them up); the nearest match moves
//     the anchor there, and two equally near ones are ambiguous;
//  4. failing that, a quote matching exactly one line of the file moves the
//     anchor there (models count lines from the hunk headers and drift);
//  5. a reported line that is in the diff stays, marked unverified;
//  6. anything else becomes a file-level comment, keeping the reported line
//     as a hint.
//
// A range (startLine < line) moves with its end line and is kept only when its
// start is in the same hunk on the same side. A context line is placed on the
// RIGHT side, where GitHub and Files changed show it, whichever side was named.
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
	qs := quoteLines(quote)
	lines := func(s string) map[int]string {
		if s == "LEFT" {
			return f.Left
		}
		return f.Right
	}
	_, inDiff := lines(side)[line]

	if inDiff && (len(qs) == 0 || f.quoteInRange(lines(side), line, startLine, qs)) {
		return f.place(res, side, line, startLine, anchorAnchored)
	}
	if len(qs) > 0 {
		key := qs[len(qs)-1:] // the range's end line
		other := "LEFT"
		if side == "LEFT" {
			other = "RIGHT"
		}
		ambiguous := false
		for _, s := range []string{side, other} {
			n, found, amb := nearestMatch(lines(s), line, key)
			if amb {
				ambiguous = true
				break // don't guess, and don't look for a luckier side
			}
			if found {
				start := 0
				if startLine > 0 && s == side {
					start = startLine + n - line
				}
				return f.place(res, s, n, start, anchorReanchored)
			}
		}
		if !ambiguous {
			for _, s := range []string{side, other} {
				if n, ok := uniqueMatch(lines(s), key); ok {
					return f.place(res, s, n, 0, anchorReanchored)
				}
			}
		}
	}
	if inDiff {
		return f.place(res, side, line, startLine, anchorUnverified)
	}
	res.Status = anchorFile
	return res
}

// quoteInRange reports whether the quote matches the reported line or, for a
// range, any line of it.
func (f *diffFile) quoteInRange(m map[int]string, line, start int, qs []string) bool {
	lo := line
	if start > 0 && start < line && line-start <= anchorRangeMax {
		lo = start
	}
	for n := lo; n <= line; n++ {
		if text, ok := m[n]; ok && lineMatches(text, qs, true) {
			return true
		}
	}
	return false
}

// nearestMatch finds the line within anchorWindow of line matching the quote,
// nearest first; two at the same distance are ambiguous.
func nearestMatch(m map[int]string, line int, qs []string) (n int, found, ambiguous bool) {
	match := func(k int) bool { text, ok := m[k]; return ok && lineMatches(text, qs, true) }
	if match(line) { // on the other side: the same number there
		return line, true, false
	}
	for d := 1; d <= anchorWindow; d++ {
		lo, hi := match(line-d), match(line+d)
		switch {
		case lo && hi:
			return 0, false, true
		case lo:
			return line - d, true, false
		case hi:
			return line + d, true, false
		}
	}
	return 0, false, false
}

// uniqueMatch finds the one line in m exactly matching the quote.
func uniqueMatch(m map[int]string, qs []string) (int, bool) {
	hit, n := 0, 0
	for k, text := range m {
		if lineMatches(text, qs, false) {
			hit, n = k, n+1
		}
	}
	return hit, n == 1
}

// place sets where a suggestion lands. A context line exists on both sides;
// one named on the LEFT moves to its RIGHT number.
func (f *diffFile) place(res anchorResult, side string, line, start int, status string) anchorResult {
	if side == "LEFT" {
		if n, ok := f.ctxNew[line]; ok {
			ns, ok := f.ctxNew[start]
			if !ok {
				ns = 0
			}
			side, line, start = "RIGHT", n, ns
		}
	}
	res.Status, res.Side, res.Line = status, side, line
	res.StartLine = f.rangeStart(side, line, start)
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
