package main

import (
	"os"
	"strings"
	"testing"
)

// TestLiveGitHubQueries checks the inbox and Conversation GraphQL queries
// against the real GitHub schema, which the fixture tests cannot. It only
// reads, and runs only when asked:
//
//	PX0_LIVE_GITHUB=1 PX0_LIVE_PR=https://github.com/owner/repo/pull/123 go test -run TestLiveGitHubQueries -v .
//
// The token comes from the usual sources (github.token, GITHUB_TOKEN,
// GH_TOKEN, gh auth token).
func TestLiveGitHubQueries(t *testing.T) {
	if os.Getenv("PX0_LIVE_GITHUB") != "1" {
		t.Skip("set PX0_LIVE_GITHUB=1 to query GitHub")
	}
	token, source := resolveGitHubToken(readSettings())
	if token == "" {
		t.Fatal("no GitHub token found")
	}
	t.Logf("token from %s", source)

	for section, q := range inboxSections {
		q = strings.ReplaceAll(q, "{repo}", "px0-ai/px0")
		items, total, err := listPRs(t.Context(), token, q)
		if err != nil {
			t.Fatalf("inbox %s: %v", section, err)
		}
		t.Logf("inbox %s: %d of %d", section, len(items), total)
	}

	pr := os.Getenv("PX0_LIVE_PR")
	if pr == "" {
		t.Log("set PX0_LIVE_PR to also check the Conversation query")
		return
	}
	_, target, err := ParsePRURL(pr)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := fetchConversation(t.Context(), target.Owner, target.Repo, target.Number, token)
	if err != nil {
		t.Fatalf("conversation: %v", err)
	}
	t.Logf("conversation: %q, state %s, %d timeline items, %d threads, truncated=%v",
		conv.Header.Title, conv.Header.State, len(conv.Items), len(conv.Threads), conv.Truncated)
}
