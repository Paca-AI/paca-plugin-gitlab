package main

import (
	"net/url"
	"strconv"
	"strings"
	"testing"

	plugin "github.com/Paca-AI/plugin-sdk-go"
	gp "github.com/Paca-AI/plugin-sdk-go/gitprovider"
)

func glIssueRow(n int, state string) map[string]any {
	return map[string]any{"iid": n, "title": "Issue " + strconv.Itoa(n), "description": "Body", "state": state,
		"web_url": "https://gitlab.com/acme/web/-/issues/" + strconv.Itoa(n), "author": map[string]any{"username": "reader"},
		"labels": []string{"bug"}, "assignees": []map[string]any{{"username": "bob"}}}
}

func TestListIssuesPagesAndMapsStates(t *testing.T) {
	p, g, _ := seededProvider(t)
	var seen []url.Values
	fetch = func(method, rawURL string, headers map[string]string, body string) (*plugin.FetchResponse, error) {
		u, _ := url.Parse(rawURL)
		if !strings.HasSuffix(u.EscapedPath(), "/issues") {
			return g.fetch(method, rawURL, headers, body)
		}
		if method != "GET" {
			t.Fatalf("unexpected request: %s %s", method, rawURL)
		}
		seen = append(seen, u.Query())
		switch u.Query().Get("page") {
		case "1": // a full page: there may be more
			rows := make([]map[string]any, listPerPage)
			for i := range rows {
				rows[i] = glIssueRow(i+1, "closed")
			}
			rows[0] = glIssueRow(500, "opened")
			return resp(200, rows)
		case "2":
			return resp(200, []map[string]any{glIssueRow(501, "opened")})
		}
		t.Fatalf("unexpected page: %s", rawURL)
		return nil, nil
	}

	first, err := p.ListIssues(bg, gp.ListIssuesRequest{Target: fixtureTarget, State: "closed"})
	if err != nil || len(first.Items) != listPerPage || first.NextCursor != "2" {
		t.Fatalf("first page: %d items, cursor %q, %v", len(first.Items), first.NextCursor, err)
	}
	if q := seen[0]; q.Get("state") != "closed" || q.Get("per_page") != "100" {
		t.Fatalf("query = %v", q)
	}
	if first.Items[0].State != "open" || first.Items[1].State != "closed" {
		t.Fatalf("states: %+v %+v", first.Items[0], first.Items[1])
	}
	second, err := p.ListIssues(bg, gp.ListIssuesRequest{Target: fixtureTarget, Cursor: first.NextCursor})
	if err != nil || len(second.Items) != 1 || second.NextCursor != "" {
		t.Fatalf("last page: %+v %v", second, err)
	}
	i := second.Items[0]
	if i.Number != "501" || i.Title != "Issue 501" || i.Body != "Body" || i.Author != "reader" || i.URL != "https://gitlab.com/acme/web/-/issues/501" ||
		len(i.Labels) != 1 || len(i.Assignees) != 1 || i.Assignees[0] != "bob" {
		t.Fatalf("issue fields: %+v", i)
	}
	// The default state is open, which GitLab calls "opened".
	if q := seen[1]; q.Get("state") != "opened" || q.Get("page") != "2" {
		t.Fatalf("default query = %v", q)
	}
}

func TestListIssuesValidatesAndPropagatesErrors(t *testing.T) {
	p, g, _ := seededProvider(t)
	for _, req := range []gp.ListIssuesRequest{
		{Target: fixtureTarget, State: "merged"},
		{Target: fixtureTarget, Cursor: "0"},
		{Target: fixtureTarget, Cursor: "x"},
	} {
		_, err := p.ListIssues(bg, req)
		code(t, err, gp.CodeValidation)
	}
	g.forceError["GET /projects/1/issues"] = fakeResponse{403, `{"message":"403 Forbidden"}`, nil}
	_, err := p.ListIssues(bg, gp.ListIssuesRequest{Target: fixtureTarget})
	code(t, err, gp.CodeForbidden)
}

func TestIssueListIsDeclared(t *testing.T) {
	p, _, _ := seededProvider(t)
	if !p.Describe().Capabilities.IssueList {
		t.Fatal("issue listing must be declared")
	}
}
