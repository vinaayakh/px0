package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// reviewparse.go turns what a review harness printed into a reviewOutput.
//
// A harness with StructuredArgs (Claude Code) is given reviewSchema and
// answers with one JSON envelope whose structured_output was validated
// against it, so the result does not depend on the model's last message.
// Any other harness prints text, and parseReviewOutput (review.go) finds the
// JSON in it. Either way the JSON is read leniently: the keys models reach
// for instead of px0's (findings, issues, file, description, fix...) are
// mapped onto them, so a result in a near-miss shape still yields its
// suggestions rather than a summary alone.

// reviewSchema is the JSON Schema for a review result: the shape described in
// reviewOutputFormat.
var reviewSchema = func() string {
	str := map[string]any{"type": "string"}
	integer := map[string]any{"type": "integer", "minimum": 0}
	item := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":       str,
			"line":       integer,
			"startLine":  integer,
			"side":       map[string]any{"type": "string", "enum": []string{"RIGHT", "LEFT"}},
			"quote":      str,
			"severity":   map[string]any{"type": "string", "enum": []string{"critical", "high", "medium", "low"}},
			"category":   map[string]any{"type": "string", "enum": []string{"correctness", "contract", "security", "performance", "maintainability"}},
			"body":       str,
			"suggestion": str,
			"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
		},
		// line, side and quote are left optional so a finding about a whole
		// file (missing tests, a design concern) can still be a suggestion
		// rather than be folded into the summary; anchoring makes it
		// file-level.
		"required": []string{"path", "severity", "category", "body", "confidence"},
	}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"summary": map[string]any{"type": "string", "description": "Two to five sentences for the review body. Never put findings here: each goes in suggestions."},
			"verdict": map[string]any{"type": "string", "enum": []string{"comment", "approve", "request_changes"}},
			"suggestions": map[string]any{"type": "array", "items": item,
				"description": "Every finding, one per entry. Empty only when there is nothing worth a comment."},
		},
		"required": []string{"summary", "verdict", "suggestions"},
	}
	b, _ := json.Marshal(schema)
	return string(b)
}()

// claudeEnvelope is what claude -p --output-format json prints.
type claudeEnvelope struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
}

// parseReviewJobOutput reads a finished review job's stdout. structured says
// the job ran with StructuredArgs; its stdout is then an envelope, read for
// structured_output first and its result text after. A stdout that is not an
// envelope is read as text.
func parseReviewJobOutput(stdout string, structured bool) (reviewOutput, error) {
	if structured {
		var env claudeEnvelope
		if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &env); err == nil && env.Type == "result" {
			if len(env.StructuredOutput) > 0 && string(env.StructuredOutput) != "null" {
				if r, ok := decodeReviewJSON(string(env.StructuredOutput)); ok {
					// A model can fill the tool call thinly and print the full
					// result as text: take the text when it has more findings.
					if len(r.Suggestions) == 0 {
						if t, err := parseReviewOutput(env.Result); err == nil && len(t.Suggestions) > 0 {
							return t, nil
						}
					}
					return r, nil
				}
			}
			if env.IsError {
				msg := strings.TrimSpace(env.Result)
				if msg == "" {
					msg = env.Subtype
				}
				return reviewOutput{}, fmt.Errorf("the harness stopped with an error: %s", msg)
			}
			if r, err := parseReviewOutput(env.Result); err == nil {
				return r, nil
			}
			return reviewOutput{}, errors.New("the harness finished without a review result")
		}
	}
	return parseReviewOutput(stdout)
}

// Keys models use instead of px0's, for the result and for each suggestion.
var (
	reviewKeyAliases = map[string]string{
		"findings": "suggestions", "issues": "suggestions", "comments": "suggestions", "review_comments": "suggestions",
		"overview": "summary", "description": "summary",
		"decision": "verdict", "recommendation": "verdict",
	}
	suggestionKeyAliases = map[string]string{
		"file": "path", "filename": "path", "files": "path",
		"start_line": "startLine", "startline": "startLine", "line_start": "startLine",
		"end_line": "line", "endline": "line", "line_end": "line", "line_number": "line", "linenumber": "line",
		"code": "quote", "snippet": "quote", "quoted_line": "quote",
		"issue": "body", "comment": "body", "message": "body", "description": "body",
		"fix": "suggestion", "replacement": "suggestion", "suggested_code": "suggestion",
		"priority": "severity", "level": "severity",
	}
)

// normalizeReviewKeys rewrites a decoded result's keys onto px0's, case
// insensitively. A key px0 already has wins over its alias. A list of files
// keeps its first as the path.
func normalizeReviewKeys(m map[string]any) {
	renameKeys(m, reviewKeyAliases, []string{"summary", "verdict", "suggestions"})
	textFields(m, "summary", "verdict")
	list, _ := m["suggestions"].([]any)
	for _, it := range list {
		s, ok := it.(map[string]any)
		if !ok {
			continue
		}
		renameKeys(s, suggestionKeyAliases, []string{"path", "line", "startLine", "side", "quote", "severity", "category", "body", "suggestion", "confidence"})
		fillBody(s)
		if files, ok := s["path"].([]any); ok && len(files) > 0 {
			s["path"] = files[0]
		} else if f, ok := s["path"].(string); ok && strings.Contains(f, ",") {
			s["path"] = strings.TrimSpace(strings.Split(f, ",")[0])
		}
		textFields(s, "path", "side", "quote", "severity", "category", "body", "suggestion")
	}
}

// bodyKeys are where models put a finding's text when it has no body, in
// the order they are joined.
var bodyKeys = []string{"title", "summary", "problem", "explanation", "details", "detail", "rationale", "reason", "text", "recommendation"}

// fillBody builds a missing body from the other text a finding has, so a
// finding is not dropped for naming its text differently: a title leads in
// bold.
func fillBody(s map[string]any) {
	if b, ok := s["body"].(string); ok && strings.TrimSpace(b) != "" {
		return
	}
	var parts []string
	for _, want := range bodyKeys {
		for k, v := range s {
			t, ok := v.(string)
			if !ok || !strings.EqualFold(k, want) || strings.TrimSpace(t) == "" {
				continue
			}
			t = strings.TrimSpace(t)
			if want == "title" {
				t = "**" + t + "**"
			}
			parts = append(parts, t)
		}
	}
	if len(parts) > 0 {
		s["body"] = strings.Join(parts, "\n\n")
	}
}

// textFields makes each named field a string, so one oddly typed value (a
// list of lines, a number) does not make the whole result fail to decode.
func textFields(m map[string]any, keys ...string) {
	for _, k := range keys {
		switch v := m[k].(type) {
		case nil, string:
		case []any:
			parts := make([]string, 0, len(v))
			for _, x := range v {
				parts = append(parts, fmt.Sprint(x))
			}
			m[k] = strings.Join(parts, "\n")
		default:
			m[k] = fmt.Sprint(v)
		}
	}
}

func renameKeys(m map[string]any, aliases map[string]string, canonical []string) {
	isCanonical := map[string]string{}
	for _, k := range canonical {
		isCanonical[strings.ToLower(k)] = k
	}
	for k, v := range m {
		lk := strings.ToLower(k)
		target := isCanonical[lk]
		if target == "" {
			target = aliases[lk]
		}
		if target == "" || target == k {
			continue
		}
		if _, taken := m[target]; !taken {
			m[target] = v
		}
		delete(m, k)
	}
}
