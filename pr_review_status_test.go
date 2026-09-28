package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestParsePRMetaConversationFields(t *testing.T) {
	body := `{
		"number": 12, "title": "Add inbox", "body": "## Why\nBecause.", "state": "open",
		"draft": true, "mergeable_state": "BLOCKED",
		"user": {"login": "alice"},
		"assignees": [{"login": "bob"}, {"login": "carol"}],
		"requested_reviewers": [{"login": "dave"}],
		"requested_teams": [{"slug": "core"}],
		"labels": [{"name": "feature"}, {"name": "ui"}],
		"base": {"ref": "main"},
		"head": {"ref": "inbox", "sha": "abc", "repo": {"clone_url": "https://github.com/o/r.git", "full_name": "o/r"}}
	}`
	m, err := parsePRMeta(strings.NewReader(body), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if m.Body != "## Why\nBecause." || m.Mergeable != "blocked" || !m.Draft {
		t.Errorf("body/mergeable/draft = %q/%q/%v", m.Body, m.Mergeable, m.Draft)
	}
	if !reflect.DeepEqual(m.Labels, []string{"feature", "ui"}) {
		t.Errorf("labels = %v", m.Labels)
	}
	if !reflect.DeepEqual(m.Assignees, []string{"bob", "carol"}) {
		t.Errorf("assignees = %v", m.Assignees)
	}
	if !reflect.DeepEqual(m.RequestedReviewers, []string{"dave", "o/core"}) {
		t.Errorf("requested reviewers = %v", m.RequestedReviewers)
	}
	if m.HeadIsFork {
		t.Error("same-repo head must not be a fork")
	}
}

func TestParsePRMetaEmptyListsAreNotNil(t *testing.T) {
	m, err := parsePRMeta(strings.NewReader(`{"number": 1, "body": null}`), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if m.Labels == nil || m.Assignees == nil || m.RequestedReviewers == nil {
		t.Fatal("list fields must be empty slices so they encode as [] not null")
	}
}

func TestPRCommentSubmittable(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{"", true}, // restored from an older session file
		{prStatusAccepted, true},
		{prStatusEdited, true},
		{prStatusPending, false},
		{prStatusDismissed, false},
	}
	for _, c := range cases {
		if got := (prComment{Status: c.status}).submittable(); got != c.want {
			t.Errorf("status %q: submittable = %v, want %v", c.status, got, c.want)
		}
	}

	send := submittableDrafts([]prComment{
		{ID: 1, Origin: prOriginHuman, Status: prStatusAccepted},
		{ID: 2, Origin: prOriginAI, Status: prStatusPending},
		{ID: 3, Origin: prOriginAI, Status: prStatusEdited},
		{ID: 4, Origin: prOriginAI, Status: prStatusDismissed},
	})
	if len(send) != 2 || send[0].ID != 1 || send[1].ID != 3 {
		t.Errorf("send = %+v, want ids 1 and 3", send)
	}
}

func TestSubmitReviewRangesAndFileComments(t *testing.T) {
	orig := githubHTTPClient.Transport
	defer func() { githubHTTPClient.Transport = orig }()
	var captured []byte
	githubHTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		captured, _ = io.ReadAll(req.Body)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
	})

	comments := []prComment{
		{ID: 1, Path: "a.go", Line: 12, StartLine: 10, Side: "RIGHT", Body: "range"},
		{ID: 2, Path: "b.go", Line: 5, StartLine: 5, Side: "LEFT", Body: "single, start equals line"},
		{ID: 3, Path: "c.go", Line: 40, Side: "RIGHT", Body: "(line 40) whole-file note", SubjectType: "file"},
	}
	if err := submitReview(context.Background(), "o", "r", 1, "tok", "sha", comments, "COMMENT", "Summary"); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Body     string `json:"body"`
		Comments []struct {
			Path      string `json:"path"`
			Line      int    `json:"line"`
			StartLine int    `json:"start_line"`
			StartSide string `json:"start_side"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(captured, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Comments) != 2 {
		t.Fatalf("line comments = %+v, want 2 (the file-level one goes in the body)", payload.Comments)
	}
	if payload.Comments[0].StartLine != 10 || payload.Comments[0].StartSide != "RIGHT" {
		t.Errorf("range comment = %+v, want start_line 10 on RIGHT", payload.Comments[0])
	}
	if payload.Comments[1].StartLine != 0 || payload.Comments[1].StartSide != "" {
		t.Errorf("single-line comment must not send start_line: %+v", payload.Comments[1])
	}
	if !strings.HasPrefix(payload.Body, "Summary") || !strings.Contains(payload.Body, "`c.go`") ||
		!strings.Contains(payload.Body, "whole-file note") {
		t.Errorf("review body = %q, want the summary followed by the file note", payload.Body)
	}
}

func TestPRSubmitSendsOnlyAcceptedAndKeepsPending(t *testing.T) {
	orig := githubHTTPClient.Transport
	defer func() { githubHTTPClient.Transport = orig }()
	var captured []byte
	githubHTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		captured, _ = io.ReadAll(req.Body)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
	})

	isolateSettings(t)
	root := t.TempDir()
	ix := NewIndex(root)
	ix.Build()
	s := NewServer(ix, nil)
	s.pr = &prSession{
		provider: &GitHubProvider{},
		target:   PRTarget{Provider: "github", Owner: "o", Repo: "r", Number: 1},
		token:    "tok",
		comments: []prComment{
			{ID: 1, Path: "a.go", Line: 1, Side: "RIGHT", Body: "mine", Origin: prOriginHuman, Status: prStatusAccepted},
			{ID: 2, Path: "a.go", Line: 2, Side: "RIGHT", Body: "ai pending", Origin: prOriginAI, Status: prStatusPending},
			{ID: 3, Path: "a.go", Line: 3, Side: "RIGHT", Body: "ai accepted", Origin: prOriginAI, Status: prStatusAccepted},
			{ID: 4, Path: "a.go", Line: 4, Side: "RIGHT", Body: "ai dismissed", Origin: prOriginAI, Status: prStatusDismissed},
		},
	}
	code, resp := agentPostJSON(t, s, "/api/pr/submit", map[string]string{"event": "COMMENT", "body": ""})
	if code != 200 {
		t.Fatalf("submit = %d %v", code, resp)
	}
	sent := string(captured)
	for _, want := range []string{"mine", "ai accepted"} {
		if !strings.Contains(sent, want) {
			t.Errorf("payload missing %q: %s", want, sent)
		}
	}
	for _, not := range []string{"ai pending", "ai dismissed"} {
		if strings.Contains(sent, not) {
			t.Errorf("payload must not carry %q: %s", not, sent)
		}
	}
	if len(s.pr.comments) != 2 || s.pr.comments[0].ID != 2 || s.pr.comments[1].ID != 4 {
		t.Fatalf("after submit, drafts = %+v, want the pending and the dismissed suggestion", s.pr.comments)
	}
}

func TestDecodeGraphQL(t *testing.T) {
	var out struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
	}
	if err := decodeGraphQL([]byte(`{"data":{"viewer":{"login":"me"}}}`), &out); err != nil || out.Viewer.Login != "me" {
		t.Fatalf("ok response: login=%q err=%v", out.Viewer.Login, err)
	}
	err := decodeGraphQL([]byte(`{"data":{"viewer":null},"errors":[{"type":"NOT_FOUND","message":"nope"},{"message":"x"}]}`), &out)
	if err == nil || !strings.Contains(err.Error(), "NOT_FOUND: nope") || !strings.Contains(err.Error(), "1 more") {
		t.Fatalf("partial data with errors must fail with the first message: %v", err)
	}
	if err := decodeGraphQL([]byte(`{"data":null}`), &out); err == nil {
		t.Fatal("null data must fail")
	}
}

func TestGitHubGraphQLNeedsToken(t *testing.T) {
	if err := githubGraphQL(context.Background(), "", "{ viewer { login } }", nil, nil); err == nil {
		t.Fatal("graphql without a token must fail before any request")
	}
}
