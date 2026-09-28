package main

import (
	"reflect"
	"testing"
)

// anchorFixtureDiff covers a modified file with two hunks, a rename with an
// edit, a deleted file, an added file, a binary file, and a file whose line
// text repeats.
const anchorFixtureDiff = `diff --git a/a.go b/a.go
index 1111111..2222222 100644
--- a/a.go
+++ b/a.go
@@ -1,6 +1,7 @@
 package a

 func A() int {
-	return 1
+	x := 2
+	return x
 }

@@ -20,3 +21,4 @@ func B() {
 	b := 1
+	b++
 	_ = b
 }
diff --git a/old/name.go b/new/name.go
similarity index 90%
rename from old/name.go
rename to new/name.go
index 3333333..4444444 100644
--- a/old/name.go
+++ b/new/name.go
@@ -5,2 +5,2 @@
-const V = 1
+const V = 2
 const W = 3
diff --git a/gone.go b/gone.go
deleted file mode 100644
index 5555555..0000000
--- a/gone.go
+++ /dev/null
@@ -1,2 +0,0 @@
-package gone
-var G = 1
diff --git a/new.go b/new.go
new file mode 100644
index 0000000..6666666
--- /dev/null
+++ b/new.go
@@ -0,0 +1,3 @@
+package n
+
+func N() {}
diff --git a/img.png b/img.png
index 7777777..8888888 100644
Binary files a/img.png and b/img.png differ
diff --git a/dup.go b/dup.go
new file mode 100644
index 0000000..9999999
--- /dev/null
+++ b/dup.go
@@ -0,0 +1,5 @@
+package d
+
+	return nil
+x
+	return nil
`

func TestParseUnifiedDiff(t *testing.T) {
	pd := parseUnifiedDiff(anchorFixtureDiff)
	wantPaths := []string{"a.go", "dup.go", "gone.go", "img.png", "new.go", "new/name.go"}
	if got := pd.Paths(); !reflect.DeepEqual(got, wantPaths) {
		t.Fatalf("paths = %v, want %v", got, wantPaths)
	}
	a := pd.Files["a.go"]
	if a.Status != "modified" || len(a.Right) != 11 || len(a.Left) != 9 {
		t.Errorf("a.go: status %q, %d right lines, %d left lines; want modified, 11, 9", a.Status, len(a.Right), len(a.Left))
	}
	if a.Right[4] != "\tx := 2" || a.Left[4] != "\treturn 1" || a.Right[22] != "\tb++" || a.Left[21] != "\t_ = b" {
		t.Errorf("a.go line text wrong: R4=%q L4=%q R22=%q L21=%q", a.Right[4], a.Left[4], a.Right[22], a.Left[21])
	}
	if a.rightHunk[5] == a.rightHunk[22] {
		t.Error("lines of different hunks must carry different hunk ids")
	}
	if r := pd.Files["new/name.go"]; r.Status != "renamed" || r.OldPath != "old/name.go" {
		t.Errorf("rename: %+v", r)
	}
	if pd.byOld["old/name.go"] != "new/name.go" {
		t.Error("old path of a rename must resolve to the new path")
	}
	if g := pd.Files["gone.go"]; g.Status != "deleted" || len(g.Right) != 0 || g.Left[2] != "var G = 1" {
		t.Errorf("deleted: %+v", g)
	}
	if n := pd.Files["new.go"]; n.Status != "added" || len(n.Left) != 0 || n.Right[3] != "func N() {}" {
		t.Errorf("added: %+v", n)
	}
	if !pd.Files["img.png"].Binary {
		t.Error("img.png must be binary")
	}
}

func TestParseUnifiedDiffPureRenameAndCRLF(t *testing.T) {
	diff := "diff --git a/x.go b/y.go\r\nsimilarity index 100%\r\nrename from x.go\r\nrename to y.go\r\n" +
		"diff --git a/c.go b/c.go\r\n--- a/c.go\r\n+++ b/c.go\r\n@@ -1 +1 @@\r\n-a\r\n+b\r\n"
	pd := parseUnifiedDiff(diff)
	if y := pd.Files["y.go"]; y == nil || y.Status != "renamed" || y.OldPath != "x.go" || len(y.Right) != 0 {
		t.Fatalf("pure rename: %+v", y)
	}
	if c := pd.Files["c.go"]; c == nil || c.Right[1] != "b" || c.Left[1] != "a" {
		t.Fatalf("CRLF diff: %+v", c)
	}
}

func TestAnchorSuggestion(t *testing.T) {
	pd := parseUnifiedDiff(anchorFixtureDiff)
	cases := []struct {
		name             string
		path             string
		line, startLine  int
		side, quote      string
		wantStatus       string
		wantPath         string
		wantLine, wantSt int
		wantSide         string
	}{
		{"exact right", "a.go", 4, 0, "RIGHT", "x := 2", anchorAnchored, "a.go", 4, 0, "RIGHT"},
		{"whitespace-insensitive quote", "a.go", 4, 0, "RIGHT", "   x := 2  ", anchorAnchored, "a.go", 4, 0, "RIGHT"},
		{"side defaults to right", "a.go", 4, 0, "", "x := 2", anchorAnchored, "a.go", 4, 0, "RIGHT"},
		{"drift within window", "a.go", 3, 0, "RIGHT", "return x", anchorReanchored, "a.go", 5, 0, "RIGHT"},
		{"drift beyond window", "a.go", 12, 0, "RIGHT", "x := 2", anchorFile, "a.go", 12, 0, "RIGHT"},
		{"left side exact", "a.go", 4, 0, "LEFT", "return 1", anchorAnchored, "a.go", 4, 0, "LEFT"},
		{"side mislabelled as right", "a.go", 4, 0, "RIGHT", "return 1", anchorReanchored, "a.go", 4, 0, "LEFT"},
		{"context line in second hunk", "a.go", 21, 0, "RIGHT", "b := 1", anchorAnchored, "a.go", 21, 0, "RIGHT"},
		{"deleted file, right side given", "gone.go", 2, 0, "RIGHT", "var G = 1", anchorReanchored, "gone.go", 2, 0, "LEFT"},
		{"deleted file, left side", "gone.go", 1, 0, "LEFT", "package gone", anchorAnchored, "gone.go", 1, 0, "LEFT"},
		{"renamed file via old path", "old/name.go", 5, 0, "RIGHT", "const V = 2", anchorAnchored, "new/name.go", 5, 0, "RIGHT"},
		{"renamed file, base side", "new/name.go", 5, 0, "LEFT", "const V = 1", anchorAnchored, "new/name.go", 5, 0, "LEFT"},
		{"quote missing from the diff", "a.go", 4, 0, "RIGHT", "nonexistent()", anchorFile, "a.go", 4, 0, "RIGHT"},
		{"no quote, line in hunk", "a.go", 5, 0, "RIGHT", "", anchorAnchored, "a.go", 5, 0, "RIGHT"},
		{"no quote, line outside hunks", "a.go", 12, 0, "RIGHT", "", anchorFile, "a.go", 12, 0, "RIGHT"},
		{"quote appears twice near the line", "dup.go", 4, 0, "RIGHT", "return nil", anchorFile, "dup.go", 4, 0, "RIGHT"},
		{"quote appears twice, one is the line", "dup.go", 3, 0, "RIGHT", "return nil", anchorAnchored, "dup.go", 3, 0, "RIGHT"},
		{"file not in the PR", "other.go", 1, 0, "RIGHT", "x", anchorSummary, "other.go", 1, 0, "RIGHT"},
		{"binary file", "img.png", 1, 0, "RIGHT", "x", anchorFile, "img.png", 1, 0, "RIGHT"},
		{"./ prefix", "./a.go", 4, 0, "RIGHT", "x := 2", anchorAnchored, "a.go", 4, 0, "RIGHT"},
		{"b/ prefix from diff headers", "b/a.go", 4, 0, "RIGHT", "x := 2", anchorAnchored, "a.go", 4, 0, "RIGHT"},
		{"backslash path", "new\\name.go", 5, 0, "RIGHT", "const V = 2", anchorAnchored, "new/name.go", 5, 0, "RIGHT"},
		{"range kept", "a.go", 5, 4, "RIGHT", "return x", anchorAnchored, "a.go", 5, 4, "RIGHT"},
		{"range shifted with re-anchor", "a.go", 4, 3, "RIGHT", "return x", anchorReanchored, "a.go", 5, 4, "RIGHT"},
		{"range across hunks dropped", "a.go", 22, 5, "RIGHT", "b++", anchorAnchored, "a.go", 22, 0, "RIGHT"},
		{"start after end dropped", "a.go", 4, 6, "RIGHT", "x := 2", anchorAnchored, "a.go", 4, 0, "RIGHT"},
		{"multi-line quote uses its last line", "a.go", 5, 4, "RIGHT", "x := 2\n\treturn x\n", anchorAnchored, "a.go", 5, 4, "RIGHT"},
		{"line zero re-anchors by quote", "new.go", 0, 0, "RIGHT", "func N() {}", anchorReanchored, "new.go", 3, 0, "RIGHT"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := pd.Anchor(c.path, c.line, c.startLine, c.side, c.quote)
			if got.Status != c.wantStatus || got.Path != c.wantPath || got.Line != c.wantLine ||
				got.StartLine != c.wantSt || got.Side != c.wantSide {
				t.Fatalf("got %+v\nwant status=%s path=%s line=%d start=%d side=%s",
					got, c.wantStatus, c.wantPath, c.wantLine, c.wantSt, c.wantSide)
			}
		})
	}
}
