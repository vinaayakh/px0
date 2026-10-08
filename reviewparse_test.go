package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const parseFixture = `{"summary":"Adds B.","verdict":"request_changes","suggestions":[` +
	`{"path":"a.go","line":4,"side":"RIGHT","quote":"x := 2","severity":"high","category":"correctness","body":"Off by one.","confidence":0.9}]}`

func envelope(t *testing.T, fields map[string]any) string {
	t.Helper()
	m := map[string]any{"type": "result", "subtype": "success", "is_error": false}
	for k, v := range fields {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseReviewJobOutputStructured(t *testing.T) {
	var structured map[string]any
	json.Unmarshal([]byte(parseFixture), &structured)

	// The validated result wins, whatever the last message said.
	out := envelope(t, map[string]any{"result": "Review complete. Let me know if you need anything else.", "structured_output": structured})
	r, err := parseReviewJobOutput(out, true)
	if err != nil || len(r.Suggestions) != 1 || r.Verdict != "request_changes" || r.Suggestions[0].Path != "a.go" {
		t.Fatalf("structured: %+v, %v", r, err)
	}

	// No structured_output: the JSON is read from the result text instead.
	out = envelope(t, map[string]any{"result": "Here is the review:\n\n```json\n" + parseFixture + "\n```\nDone {ok}."})
	if r, err := parseReviewJobOutput(out, true); err != nil || len(r.Suggestions) != 1 {
		t.Errorf("result text: %+v, %v", r, err)
	}

	// An error run names what went wrong instead of a parse failure.
	out = envelope(t, map[string]any{"is_error": true, "subtype": "error_max_turns", "result": ""})
	if _, err := parseReviewJobOutput(out, true); err == nil || !strings.Contains(err.Error(), "error_max_turns") {
		t.Errorf("error envelope: %v", err)
	}

	// A sign-off with no result at all is a failure, not an empty review.
	out = envelope(t, map[string]any{"result": "I reviewed the PR and it looks good overall."})
	if _, err := parseReviewJobOutput(out, true); err == nil {
		t.Error("a result with no review must fail, not pass as a review")
	}

	// structured, but stdout is plain text (an older CLI ignoring the flags).
	if r, err := parseReviewJobOutput("blah\n"+reviewBeginMarker+"\n"+parseFixture+"\n"+reviewEndMarker, true); err != nil || len(r.Suggestions) != 1 {
		t.Errorf("text despite structured: %+v, %v", r, err)
	}
}

func TestDecodeReviewJSONTrailingProse(t *testing.T) {
	// The old decoder cut at the last "}" and lost the result here.
	text := "```json\n" + parseFixture + "\n```\n\nNote: see `func f() { return }` for context."
	r, ok := decodeReviewJSON(text)
	if !ok || len(r.Suggestions) != 1 || r.Summary != "Adds B." {
		t.Fatalf("trailing prose: %+v %v", r, ok)
	}
}

func TestDecodeReviewJSONAliases(t *testing.T) {
	// The shape a model drifts into: findings, file(s), issue, fix, priority,
	// a body as a list of lines, a line as a string.
	text := `{"Summary":"Adds B.","findings":[
		{"files":["b.go","c.go"],"line":"3","code":"var B = 1","priority":"Critical","issue":["B is exported","but unused."],"fix":"var b = 1","confidence":"80%"},
		{"file":"a.go, d.go","line":7,"description":"Second.","severity":"low"}]}`
	r, ok := decodeReviewJSON(text)
	if !ok || len(r.Suggestions) != 2 || r.Summary != "Adds B." {
		t.Fatalf("aliases: %+v %v", r, ok)
	}
	s := r.Suggestions[0]
	if s.Path != "b.go" || int(s.Line) != 3 || s.Quote != "var B = 1" || normSeverity(s.Severity) != "critical" ||
		s.Body != "B is exported\nbut unused." || s.Suggestion != "var b = 1" || normConfidence(s.Confidence) != 0.8 {
		t.Errorf("first = %+v", s)
	}
	if r.Suggestions[1].Path != "a.go" || r.Suggestions[1].Body != "Second." {
		t.Errorf("second = %+v", r.Suggestions[1])
	}

	// px0's own key wins over an alias present beside it.
	r, _ = decodeReviewJSON(`{"summary":"S","suggestions":[{"path":"a.go","line":1,"body":"real","comment":"alias"}]}`)
	if len(r.Suggestions) != 1 || r.Suggestions[0].Body != "real" {
		t.Errorf("canonical key must win: %+v", r.Suggestions)
	}
}

func TestReviewSchemaMatchesOutput(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(reviewSchema), &schema); err != nil {
		t.Fatalf("reviewSchema is not JSON: %v", err)
	}
	props := schema["properties"].(map[string]any)
	item := props["suggestions"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	sev := item["severity"].(map[string]any)["enum"].([]any)
	for _, s := range sev {
		if _, ok := severityRank[s.(string)]; !ok {
			t.Errorf("schema severity %q is not on px0's scale", s)
		}
	}
	// Every key the schema asks for is one rawSuggestion reads.
	known := map[string]bool{}
	b, _ := json.Marshal(rawSuggestion{})
	var m map[string]any
	json.Unmarshal(b, &m)
	for k := range m {
		known[k] = true
	}
	for k := range item {
		if !known[k] {
			t.Errorf("schema property %q is not read by rawSuggestion", k)
		}
	}
	if strings.ContainsAny(reviewSchema, "&|<>^%") {
		t.Error("the schema goes on a command line: keep shell metacharacters out of it")
	}
}
