package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestReadOnlyPresetArgv(t *testing.T) {
	var claude agentPreset
	for _, p := range agentPresets {
		if p.Name == "claude" {
			claude = p
		}
	}
	got := presetArgv(claude, claude.ReadOnlyArgs, "sonnet")
	want := []string{"claude", "--permission-mode", "default",
		"--disallowedTools", "Edit,Write,MultiEdit,NotebookEdit",
		"--add-dir", "{tmpdir}", "--model", "sonnet", "-p", "{prompt}"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("claude read-only argv =\n%v\nwant\n%v", got, want)
	}

	for _, p := range agentPresets {
		if len(p.ReadOnlyArgs) == 0 {
			continue
		}
		if p.ReadOnlyArgs[0] != p.Args[0] {
			t.Errorf("%s: read-only argv runs %q, edit argv runs %q", p.Name, p.ReadOnlyArgs[0], p.Args[0])
		}
		if last := p.ReadOnlyArgs[len(p.ReadOnlyArgs)-1]; last != "{prompt}" {
			t.Errorf("%s: read-only argv must end in {prompt}, ends in %q", p.Name, last)
		}
		for _, a := range p.ReadOnlyArgs {
			if strings.Contains(a, "acceptEdits") || a == "--force" || strings.Contains(a, "auto_edit") {
				t.Errorf("%s: read-only argv carries an edit-approval flag: %v", p.Name, p.ReadOnlyArgs)
			}
		}
	}
}

func TestExpandArgv(t *testing.T) {
	tmpl := []string{"h", "--add-dir", "{tmpdir}", "-p", "{prompt}"}
	cases := []struct {
		name, prompt, tmpdir string
		want                 []string
	}{
		{"with tmpdir", "review", "/tmp/r1", []string{"h", "--add-dir", "/tmp/r1", "-p", "review"}},
		{"no tmpdir drops the flag", "review", "", []string{"h", "-p", "review"}},
		{"prompt text is not expanded", "see {tmpdir}", "/tmp/r1", []string{"h", "--add-dir", "/tmp/r1", "-p", "see {tmpdir}"}},
	}
	for _, c := range cases {
		if got := expandArgv(tmpl, c.prompt, c.tmpdir); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestReadOnlyArgvRefusals(t *testing.T) {
	m := &agentManager{models: map[string]string{}}
	if _, _, err := m.readOnlyArgv(); !errors.Is(err, errAgentNone) {
		t.Fatalf("no harness: err = %v, want errAgentNone", err)
	}

	m.selected, m.args = "aider", []string{"/opt/bin/aider", "--yes-always", "{prompt}"}
	if _, _, err := m.readOnlyArgv(); !errors.Is(err, errAgentNoReadOnly) {
		t.Fatalf("aider: err = %v, want errAgentNoReadOnly", err)
	}

	m.selected, m.args = "my-script.sh", []string{"/opt/bin/my-script.sh", "{prompt}"}
	if _, _, err := m.readOnlyArgv(); !errors.Is(err, errAgentNoReadOnly) {
		t.Fatalf("custom template: err = %v, want errAgentNoReadOnly", err)
	}

	m.selected, m.args = "claude", []string{"/opt/bin/claude", "--permission-mode", "acceptEdits", "-p", "{prompt}"}
	m.models["claude"] = "opus"
	name, args, err := m.readOnlyArgv()
	if err != nil || name != "claude" {
		t.Fatalf("claude: (%q, %v), want no error", name, err)
	}
	if args[0] != "/opt/bin/claude" {
		t.Errorf("argv[0] = %q, want the resolved binary", args[0])
	}
	if strings.Contains(strings.Join(args, " "), "acceptEdits") {
		t.Errorf("read-only argv still carries acceptEdits: %v", args)
	}
	if !strings.Contains(strings.Join(args, " "), "--model opus") {
		t.Errorf("read-only argv lost the selected model: %v", args)
	}
}

// skipWithoutShellHarness skips tests whose stand-in harness is a /bin/sh
// script, which Windows cannot exec directly.
func skipWithoutShellHarness(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stand-in harness is a shell script")
	}
	if !gitInstalled() {
		t.Skip("git not installed")
	}
}

// withFakeReadOnlyPreset registers a preset whose read-only argv runs harness,
// passing the tmpdir as $1 and the prompt as $2, and selects it on s.
func withFakeReadOnlyPreset(t *testing.T, s *Server, harness string) {
	t.Helper()
	orig := agentPresets
	agentPresets = append(append([]agentPreset(nil), orig...), agentPreset{
		Name:         "fake-ro",
		Args:         []string{harness, "{prompt}"},
		ReadOnlyArgs: []string{harness, "{tmpdir}", "{prompt}"},
	})
	t.Cleanup(func() { agentPresets = orig })
	s.agent.mu.Lock()
	s.agent.selected = "fake-ro"
	s.agent.mu.Unlock()
}

func TestStartReviewCleanRun(t *testing.T) {
	skipWithoutShellHarness(t)
	root := gitRepo(t)
	tmp := t.TempDir()
	s := agentServer(t, root, writeHarness(t,
		"cat \"$1/context.md\"\nprintf 'PX0-REVIEW-BEGIN{}PX0-REVIEW-END'\n"))
	withFakeReadOnlyPreset(t, s, s.agent.args[0])
	if err := os.WriteFile(filepath.Join(tmp, "context.md"), []byte("diff goes here\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	job, err := s.agent.StartReview("review #1", "read context.md", reviewJobOpts{OutBytes: 1 << 20, Timeout: time.Minute, TmpDir: tmp})
	if err != nil {
		t.Fatal(err)
	}
	done := waitIdleID(t, s, job.ID)
	if done.Error != "" {
		t.Fatalf("review failed: %s (log %s)", done.Error, done.Log)
	}
	if !done.ReadOnly || done.Tainted {
		t.Fatalf("readOnly=%v tainted=%v, want true/false", done.ReadOnly, done.Tainted)
	}
	if !strings.Contains(done.Log, "diff goes here") || !strings.Contains(done.Log, "PX0-REVIEW-BEGIN{}PX0-REVIEW-END") {
		t.Fatalf("output did not come through: %q", done.Log)
	}
}

func TestStartReviewMarksTaintedWhenHarnessWrites(t *testing.T) {
	skipWithoutShellHarness(t)
	root := gitRepo(t)
	s := agentServer(t, root, writeHarness(t, "printf 'sneaky\\n' >> keep.go\n"))
	withFakeReadOnlyPreset(t, s, s.agent.args[0])

	job, err := s.agent.StartReview("review #1", "just look", reviewJobOpts{TmpDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	done := waitIdleID(t, s, job.ID)
	if !done.Tainted {
		t.Fatalf("a read-only run that edited keep.go must be tainted; changed=%v", done.Changed)
	}
	if len(done.Changed) != 1 || done.Changed[0] != "keep.go" {
		t.Fatalf("changed = %v, want [keep.go]", done.Changed)
	}
	// px0 never reverts what the harness wrote.
	if b, _ := os.ReadFile(filepath.Join(root, "keep.go")); !strings.Contains(string(b), "sneaky") {
		t.Fatal("the harness's write should be left in place for the reviewer to inspect")
	}
}

func TestReviewAndGraphSettingDefaults(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		secs *float64
		want time.Duration
	}{
		{nil, defaultReviewTimeout},
		{f(0), defaultReviewTimeout},
		{f(10), time.Minute},
		{f(600), 10 * time.Minute},
		{f(99999), time.Hour},
	}
	for _, c := range cases {
		if got := (settings{ReviewTimeoutSeconds: c.secs}).reviewTimeout(); got != c.want {
			t.Errorf("reviewTimeout(%v) = %s, want %s", c.secs, got, c.want)
		}
	}
	if got := (settings{}).graphStaleDays(); got != defaultGraphStaleDays {
		t.Errorf("graphStaleDays default = %d", got)
	}
	if got := (settings{GraphStaleDays: f(14)}).graphStaleDays(); got != 14 {
		t.Errorf("graphStaleDays(14) = %d", got)
	}
}

func TestStartReviewRefusesHarnessWithoutReadOnlyMode(t *testing.T) {
	root := t.TempDir()
	s := agentServer(t, root, writeHarness(t, "exit 0\n"))
	if _, err := s.agent.StartReview("review", "x", reviewJobOpts{}); !errors.Is(err, errAgentNoReadOnly) {
		t.Fatalf("custom template: err = %v, want errAgentNoReadOnly", err)
	}
}
