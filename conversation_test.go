package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// convTimelinePage1 and convTimelinePage2 are one PR's timeline split over two
// pages; convThreadsPage holds its review threads.
const convTimelinePage1 = `{"data":{"repository":{"pullRequest":{
  "number": 12, "title": "Add inbox", "url": "https://github.com/o/r/pull/12", "body": "## Why\n<script>alert(1)</script>Because.",
  "createdAt": "2026-09-01T10:00:00Z", "isDraft": false, "state": "OPEN", "merged": false,
  "mergeable": "CONFLICTING", "reviewDecision": "CHANGES_REQUESTED", "baseRefName": "main", "headRefName": "inbox",
  "author": {"login": "alice", "avatarUrl": "https://a/alice"},
  "labels": {"nodes": [{"name": "feature", "color": "a2eeef"}]},
  "assignees": {"nodes": [{"login": "bob"}]},
  "reviewRequests": {"nodes": [{"requestedReviewer": {"__typename": "User", "login": "carol"}}, {"requestedReviewer": {"__typename": "Team", "combinedSlug": "o/core"}}]},
  "headCommit": {"nodes": [{"commit": {"statusCheckRollup": {"state": "PENDING"}}}]},
  "timelineItems": {"pageInfo": {"hasNextPage": true, "endCursor": "C1"}, "nodes": [
    {"__typename": "PullRequestCommit", "id": "PC1", "url": "u", "commit": {"oid": "aaa", "messageHeadline": "first", "committedDate": "2026-09-01T10:01:00Z", "author": {"name": "Alice A", "user": {"login": "alice"}}}},
    {"__typename": "PullRequestCommit", "id": "PC2", "url": "u", "commit": {"oid": "bbb", "messageHeadline": "second", "committedDate": "2026-09-01T10:02:00Z", "author": {"name": "No Account", "user": null}}},
    {"__typename": "ReviewRequestedEvent", "id": "RR1", "createdAt": "2026-09-01T10:03:00Z", "actor": {"login": "alice"}, "requestedReviewer": {"__typename": "Team", "combinedSlug": "o/core"}},
    {"__typename": "IssueComment", "id": "IC1", "databaseId": 501, "author": {"login": "dave", "avatarUrl": "https://a/dave"}, "body": "Looks **good**", "createdAt": "2026-09-01T11:00:00Z", "url": "https://c/501"},
    {"__typename": "PullRequestReview", "id": "R1", "databaseId": 601, "author": {"login": "carol"}, "body": "Needs work", "state": "CHANGES_REQUESTED", "createdAt": "2026-09-01T12:00:00Z", "submittedAt": "2026-09-01T12:05:00Z", "url": "https://r/601"}
  ]}
}}}}`

const convTimelinePage2 = `{"data":{"repository":{"pullRequest":{
  "number": 12, "title": "Add inbox", "state": "OPEN",
  "timelineItems": {"pageInfo": {"hasNextPage": false, "endCursor": "C2"}, "nodes": [
    {"__typename": "PullRequestReview", "id": "R2", "databaseId": 602, "author": {"login": "alice"}, "body": "", "state": "COMMENTED", "createdAt": "2026-09-01T13:00:00Z", "url": "https://r/602"},
    {"__typename": "PullRequestReview", "id": "R3", "databaseId": 603, "author": {"login": "carol"}, "body": "", "state": "COMMENTED", "createdAt": "2026-09-01T13:30:00Z", "url": "https://r/603"},
    {"__typename": "PullRequestReview", "id": "R4", "databaseId": 604, "author": {"login": "me"}, "body": "draft", "state": "PENDING", "createdAt": "2026-09-01T13:40:00Z", "url": "https://r/604"},
    {"__typename": "HeadRefForcePushedEvent", "id": "FP1", "createdAt": "2026-09-01T14:00:00Z", "actor": {"login": "alice"}, "beforeCommit": {"oid": "bbb"}, "afterCommit": {"oid": "ccc"}},
    {"__typename": "PullRequestCommit", "id": "PC3", "url": "u", "commit": {"oid": "ddd", "messageHeadline": "third", "committedDate": "2026-09-01T14:10:00Z", "author": {"name": "Alice A", "user": {"login": "alice"}}}},
    {"__typename": "LabeledEvent", "id": "L1", "createdAt": "2026-09-01T15:00:00Z", "actor": null, "label": {"name": "ui", "color": "ff0000"}},
    {"__typename": "MergedEvent", "id": "M1", "createdAt": "2026-09-01T16:00:00Z", "actor": {"login": "bob"}, "mergeCommit": {"oid": "eee"}},
    {"__typename": "SomeFutureEvent", "id": "X1", "createdAt": "2026-09-01T17:00:00Z"}
  ]}
}}}}`

const convThreadsPage = `{"data":{"repository":{"pullRequest":{"reviewThreads":{
  "pageInfo": {"hasNextPage": false, "endCursor": "T1"},
  "nodes": [
    {"id": "TH1", "isResolved": false, "isOutdated": false, "path": "inbox.go", "line": 42, "originalLine": 40, "startLine": null, "diffSide": "RIGHT",
     "comments": {"nodes": [
       {"id": "TC1", "databaseId": 701, "author": {"login": "carol"}, "body": "Why?", "createdAt": "2026-09-01T12:01:00Z", "url": "https://t/701", "diffHunk": "@@ -1 +1 @@\n-a\n+b", "pullRequestReview": {"id": "R1"}},
       {"id": "TC2", "databaseId": 702, "author": {"login": "alice"}, "body": "Because.", "createdAt": "2026-09-01T13:00:00Z", "url": "https://t/702", "diffHunk": "", "pullRequestReview": {"id": "R2"}}
     ]}},
    {"id": "TH2", "isResolved": true, "isOutdated": true, "path": "old.go", "line": null, "originalLine": 7, "startLine": null, "diffSide": "LEFT",
     "comments": {"nodes": [
       {"id": "TC3", "databaseId": 703, "author": null, "body": "Gone", "createdAt": "2026-09-01T13:30:00Z", "url": "https://t/703", "diffHunk": "@@", "pullRequestReview": {"id": "R3"}}
     ]}}
  ]
}}}}}`

// fakeGraphQL serves the fixtures by query type and cursor and counts calls.
func fakeGraphQL(t *testing.T, calls *int32) func() {
	t.Helper()
	orig := githubHTTPClient.Transport
	githubHTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(calls, 1)
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		json.NewDecoder(req.Body).Decode(&body)
		resp := convThreadsPage
		if strings.Contains(body.Query, "timelineItems") {
			resp = convTimelinePage1
			if body.Variables["cursor"] == "C1" {
				resp = convTimelinePage2
			}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(resp)), Header: make(http.Header)}, nil
	})
	return func() { githubHTTPClient.Transport = orig }
}

func TestFetchConversation(t *testing.T) {
	var calls int32
	defer fakeGraphQL(t, &calls)()
	conv, err := fetchConversation(context.Background(), "o", "r", 12, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("%d GraphQL calls, want 3 (two timeline pages, one thread page)", calls)
	}
	h := conv.Header
	if h.State != "open" || h.Mergeable != "conflicting" || h.ReviewDecision != "changes_requested" || h.ChecksState != "pending" {
		t.Errorf("header state = %+v", h)
	}
	if len(h.Labels) != 1 || h.Labels[0].Color != "a2eeef" || h.Assignees[0] != "bob" ||
		strings.Join(h.RequestedReviewers, ",") != "carol,o/core" || h.BaseRef != "main" || h.HeadRef != "inbox" {
		t.Errorf("header people/labels = %+v", h)
	}

	var kinds []string
	for _, it := range conv.Items {
		kinds = append(kinds, it.Kind)
	}
	// R3 (empty body, starts a thread) stays; R2 (empty, reply only) and R4
	// (pending) go; the two first commits group; the unknown event is skipped.
	want := "commits,review_requested,comment,review,review,force_push,commits,labeled,merged"
	if got := strings.Join(kinds, ","); got != want {
		t.Fatalf("timeline kinds =\n%s\nwant\n%s", got, want)
	}
	if c := conv.Items[0].Commits; len(c) != 2 || c[0].Author != "alice" || c[1].Author != "No Account" {
		t.Errorf("grouped commits = %+v", c)
	}
	if rv := conv.Items[3]; rv.State != "changes_requested" || rv.CreatedAt != "2026-09-01T12:05:00Z" || rv.DatabaseID != 601 {
		t.Errorf("review item = %+v (submittedAt should be its time)", rv)
	}
	if rv := conv.Items[4]; rv.ID != "R3" {
		t.Errorf("empty review that starts a thread should stay, got %+v", rv)
	}
	if fp := conv.Items[5]; fp.SHA != "ccc" || fp.BeforeSHA != "bbb" {
		t.Errorf("force push = %+v", fp)
	}
	if l := conv.Items[7]; l.Subject != "ui" || l.Color != "ff0000" || l.Author != "ghost" {
		t.Errorf("label event = %+v", l)
	}
	if m := conv.Items[8]; m.SHA != "eee" || m.Author != "bob" {
		t.Errorf("merged event = %+v", m)
	}
	if conv.Items[2].DatabaseID != 501 {
		t.Error("issue comments keep their REST id for replies")
	}

	if len(conv.Threads) != 2 {
		t.Fatalf("threads = %+v", conv.Threads)
	}
	t1, t2 := conv.Threads[0], conv.Threads[1]
	if t1.ReviewID != "R1" || t1.Line != 42 || t1.Side != "RIGHT" || len(t1.Comments) != 2 || t1.DiffHunk == "" || t1.Comments[0].DatabaseID != 701 {
		t.Errorf("thread 1 = %+v", t1)
	}
	if !t2.Resolved || !t2.Outdated || t2.Line != 0 || t2.OriginalLine != 7 || t2.Side != "LEFT" || t2.Comments[0].Author != "ghost" {
		t.Errorf("thread 2 = %+v", t2)
	}
	if conv.Truncated {
		t.Error("not truncated")
	}
}

func TestFetchConversationStopsAtPageLimit(t *testing.T) {
	var calls int32
	orig := githubHTTPClient.Transport
	defer func() { githubHTTPClient.Transport = orig }()
	endless := strings.Replace(convTimelinePage2, `"hasNextPage": false`, `"hasNextPage": true`, 1)
	githubHTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		b, _ := io.ReadAll(req.Body)
		resp := convThreadsPage
		if strings.Contains(string(b), "timelineItems") {
			resp = endless
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(resp)), Header: make(http.Header)}, nil
	})
	conv, err := fetchConversation(context.Background(), "o", "r", 12, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if !conv.Truncated || calls != convTimelinePages+1 {
		t.Fatalf("truncated=%v after %d calls, want true after %d", conv.Truncated, calls, convTimelinePages+1)
	}
}

func TestFetchConversationGraphQLError(t *testing.T) {
	orig := githubHTTPClient.Transport
	defer func() { githubHTTPClient.Transport = orig }()
	githubHTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"errors":[{"message":"Field 'x' doesn't exist"}]}`)), Header: make(http.Header)}, nil
	})
	if _, err := fetchConversation(context.Background(), "o", "r", 12, "tok"); err == nil || !strings.Contains(err.Error(), "doesn't exist") {
		t.Fatalf("err = %v", err)
	}
}

func TestRenderConversationHTML(t *testing.T) {
	conv := PRConversation{
		Header:  PRHeader{Body: "## Why\n\nBecause **bold**."},
		Items:   []TimelineItem{{Kind: "comment", Body: "- [x] done"}, {Kind: "merged"}},
		Threads: []ReviewThread{{Comments: []ThreadComment{{Body: "`code`"}}}},
	}
	renderConversationHTML(&conv)
	if !strings.Contains(conv.Header.BodyHTML, "<strong>bold</strong>") || !strings.Contains(conv.Header.BodyHTML, "<h2") {
		t.Errorf("header html = %q", conv.Header.BodyHTML)
	}
	if !strings.Contains(conv.Items[0].BodyHTML, `type="checkbox"`) {
		t.Errorf("GFM task list not rendered: %q", conv.Items[0].BodyHTML)
	}
	if conv.Items[1].BodyHTML != "" {
		t.Error("an item without a body gets no HTML")
	}
	if !strings.Contains(conv.Threads[0].Comments[0].BodyHTML, "<code>code</code>") {
		t.Errorf("thread comment html = %q", conv.Threads[0].Comments[0].BodyHTML)
	}
}

func convTestServer(t *testing.T, token string) *Server {
	t.Helper()
	isolateSettings(t)
	ix := NewIndex(t.TempDir())
	ix.Build()
	s := NewServer(ix, nil)
	s.pr = &prSession{
		provider: &GitHubProvider{},
		target:   PRTarget{Provider: "github", Owner: "o", Repo: "r", Number: 12, URL: "https://github.com/o/r/pull/12"},
		token:    token,
		meta: PRMeta{Number: 12, Title: "Add inbox", Author: "alice", Body: "Desc **here**", BaseRef: "main", HeadRef: "inbox",
			Draft: true, Labels: []string{"feature"}, Mergeable: "dirty"},
	}
	return s
}

func getJSON(t *testing.T, s *Server, url string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

func TestConversationHandlerWithoutToken(t *testing.T) {
	var calls int32
	defer fakeGraphQL(t, &calls)()
	s := convTestServer(t, "")
	code, m := getJSON(t, s, "/api/pr/conversation")
	if code != 200 || m["needsToken"] != true {
		t.Fatalf("= %d %v", code, m)
	}
	h := m["conversation"].(map[string]any)["header"].(map[string]any)
	if h["state"] != "draft" || h["mergeable"] != "conflicting" || !strings.Contains(h["bodyHtml"].(string), "<strong>here</strong>") {
		t.Errorf("fallback header = %v", h)
	}
	if calls != 0 {
		t.Error("no GraphQL call is possible without a token")
	}
}

func TestConversationHandlerCaches(t *testing.T) {
	var calls int32
	defer fakeGraphQL(t, &calls)()
	s := convTestServer(t, "tok")
	if code, m := getJSON(t, s, "/api/pr/conversation"); code != 200 || m["conversation"] == nil {
		t.Fatalf("= %d %v", code, m)
	}
	first := calls
	getJSON(t, s, "/api/pr/conversation")
	if calls != first {
		t.Errorf("a second load within %s must come from the cache (%d -> %d calls)", convCacheTTL, first, calls)
	}
	getJSON(t, s, "/api/pr/conversation?refresh=1")
	if calls == first {
		t.Error("refresh=1 must refetch")
	}
}
