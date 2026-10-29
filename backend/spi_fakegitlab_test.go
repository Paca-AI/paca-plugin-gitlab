package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	plugin "github.com/Paca-AI/plugin-sdk-go"
)

// fakeGitLab answers the subset of the GitLab REST API the provider uses, from
// seeded in-memory data. It enforces what GitLab enforces and what bugs in the
// provider would violate: every call needs a token this fake knows, projects a
// token cannot see answer 404, and state-changing calls change state the reads
// then show.
type fakeGitLab struct {
	t  *testing.T
	mu sync.Mutex

	calls      []string
	forceError map[string]fakeResponse // "METHOD /path" → canned answer

	mrs      map[int]*fakeMR
	nextIID  int
	notes    []fakeNote
	nextID   int64
	threads  map[string]*fakeThread
	branches map[string]string // branch → commit sha
	hooks    map[int64]string  // hook id → url
	nextHook int64
	statuses []map[string]any
	approved []int
	retried  []string
}

type fakeMR struct {
	IID       int
	Title     string
	Body      string
	State     string
	SourceRef string
	SHA       string
	Labels    []string
	Reviewers []int64
}

type fakeNote struct {
	ID   int64
	IID  int
	Kind string // merge_requests | issues
	Body string
}

type fakeThread struct {
	ID       string
	Resolved bool
	Replies  int
}

type fakeResponse struct {
	status  int
	body    string
	headers map[string]string
}

const (
	fakeHost          = "gitlab.com"
	fakeToken         = "glpat-fake-token"
	fakeReadToken     = "glpat-read-token"
	fakeNewToken      = "glpat-new-token"
	fakeWebhookSecret = "hook-secret-value-123456"

	// Tokens of the three kinds. The project token was issued for acme/ui, the
	// group token for acme/frontend; the dev token is a personal one with the
	// Developer role on acme/ui and the outsider belongs to nothing.
	fakeProjectToken         = "glpat-project-ui"
	fakeGroupToken           = "glpat-group-frontend"
	fakeDevToken             = "glpat-dev"
	fakeReporterProjectToken = "glpat-project-reporter"
	fakeOutsiderToken        = "glpat-outsider"
	fakeUsername             = "paca-bot"
	fakeIdentityID           = "ident-1"
	fakeHeadSHA              = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"
)

var fakeUsers = map[string]int64{"octocat": 1, "reader": 2, "owner": 3, "lead": 4, "bob": 5, fakeUsername: 77}

// fakeAccess is each user's role on acme/web.
var fakeAccess = map[int64]int{1: 30, 2: 10, 3: 50, 4: 40}

type fakeProject struct {
	ID     int64
	Path   string
	Public bool
	Level  int // the token's role on it
}

var fakeProjects = []fakeProject{
	{1, "acme/web", false, 40},
	{2, "acme/api", true, 20}, // Reporter: may read, not push
	{3, "acme/platform/infra", false, 30},
}

// extraProjects exist for the token-reach tests only; the default tokens do not
// see them, and ListRepositories does not list them.
var extraProjects = []fakeProject{
	{4, "acme/ui", false, 0},
	{5, "acme/frontend/app", false, 0},
	{6, "acme/frontend/sub/lib", false, 0},
}

// reachTokens maps each reach-test token to its user name and its role per
// project id. A project missing from the map is invisible unless public, and a
// public one is then read with no role at all.
var reachTokens = map[string]struct {
	user   string
	levels map[int64]int
}{
	fakeProjectToken:         {"project_4_bot_ab12", map[int64]int{4: 40}},
	fakeGroupToken:           {"group_9_bot_cd34", map[int64]int{5: 30, 6: 30}},
	fakeDevToken:             {"dev", map[int64]int{4: 30}},
	fakeReporterProjectToken: {"project_4_bot_ee56", map[int64]int{4: 20}},
	fakeOutsiderToken:        {"stranger", map[int64]int{}},
}

func newFakeGitLab(t *testing.T) *fakeGitLab {
	t.Helper()
	g := &fakeGitLab{
		t: t, forceError: map[string]fakeResponse{}, mrs: map[int]*fakeMR{}, nextIID: 2, nextID: 9000,
		threads:  map[string]*fakeThread{"disc-1": {ID: "disc-1"}},
		branches: map[string]string{"main": "sha-main", "feat/PACA-12-login": fakeHeadSHA, "feat/new": "sha-new", "draft-branch": "sha-draft"},
		hooks:    map[int64]string{}, nextHook: 500,
	}
	g.mrs[1] = &fakeMR{IID: 1, Title: "Add login", Body: "body", State: "opened", SourceRef: "feat/PACA-12-login", SHA: fakeHeadSHA}
	g.notes = []fakeNote{{ID: 8001, IID: 1, Kind: "merge_requests", Body: "first"}}
	old := fetch
	fetch = g.fetch
	t.Cleanup(func() { fetch = old })
	return g
}

func resp(status int, v any) (*plugin.FetchResponse, error) {
	b, _ := json.Marshal(v)
	return &plugin.FetchResponse{Status: status, Body: string(b), Headers: map[string]string{}}, nil
}

func text(status int, s string) (*plugin.FetchResponse, error) {
	return &plugin.FetchResponse{Status: status, Body: s, Headers: map[string]string{}}, nil
}

func notFound() (*plugin.FetchResponse, error) {
	return resp(404, map[string]string{"message": "404 Not Found"})
}

func (g *fakeGitLab) called(prefix string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, c := range g.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (g *fakeGitLab) project(ref string) *fakeProject {
	for _, list := range [][]fakeProject{fakeProjects, extraProjects} {
		for i := range list {
			if ref == strconv.FormatInt(list[i].ID, 10) || ref == list[i].Path {
				return &list[i]
			}
		}
	}
	return nil
}

func projectJSON(p fakeProject) map[string]any {
	vis := "private"
	if p.Public {
		vis = "public"
	}
	// A user with no role on a public project gets no project_access, as on GitLab.
	var access any
	if p.Level > 0 {
		access = map[string]any{"access_level": p.Level}
	}
	return map[string]any{
		"id": p.ID, "path_with_namespace": p.Path, "default_branch": "main", "visibility": vis, "description": "d-" + p.Path,
		"permissions": map[string]any{"project_access": access, "group_access": nil},
	}
}

func (m *fakeMR) json(g *fakeGitLab) map[string]any {
	reviewers := []map[string]any{}
	for _, id := range m.Reviewers {
		reviewers = append(reviewers, map[string]any{"id": id, "username": usernameOf(id)})
	}
	labels := m.Labels
	if labels == nil {
		labels = []string{}
	}
	return map[string]any{
		"iid": m.IID, "title": m.Title, "description": m.Body, "state": m.State, "draft": strings.HasPrefix(m.Title, "Draft:"),
		"source_branch": m.SourceRef, "target_branch": "main", "sha": m.SHA,
		"web_url": fmt.Sprintf("https://gitlab.com/acme/web/-/merge_requests/%d", m.IID),
		"author":  map[string]any{"id": 1, "username": "octocat"}, "labels": labels, "reviewers": reviewers,
		"source_project_id": 1, "target_project_id": 1,
		"diff_refs": map[string]string{"base_sha": "base1", "start_sha": "start1", "head_sha": m.SHA},
	}
}

func usernameOf(id int64) string {
	for n, i := range fakeUsers {
		if i == id {
			return n
		}
	}
	return ""
}

var fakeDiffFiles = []map[string]any{
	{"old_path": "docs/guide.md", "new_path": "docs/guide.md", "diff": "@@ -1 +1,2 @@\n # Guide\n+more\n"},
	{"old_path": "src/main.go", "new_path": "src/main.go", "diff": "@@ -1,3 +1,4 @@\n package main\n+// hi\n"},
	{"old_path": "src/util.go", "new_path": "src/util.go", "diff": "@@ -1 +1,2 @@\n package main\n+func f() {}\n"},
}

func (g *fakeGitLab) fetch(method, rawURL string, headers map[string]string, body string) (*plugin.FetchResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	u, err := url.Parse(rawURL)
	if err != nil || u.Host != fakeHost || !strings.HasPrefix(u.EscapedPath(), apiPrefix+"/") {
		g.t.Errorf("request outside the allow-list or the API: %s", rawURL)
		return nil, fmt.Errorf("blocked")
	}
	// Segments stay escaped until after the split: "acme%2Fweb" is one segment.
	var segs []string
	for _, s := range strings.Split(strings.TrimPrefix(u.EscapedPath(), apiPrefix+"/"), "/") {
		d, _ := url.PathUnescape(s)
		segs = append(segs, d)
	}
	g.calls = append(g.calls, method+" /"+strings.Join(segs, "/"))
	if f, ok := g.forceError[method+" /"+strings.Join(segs, "/")]; ok {
		return &plugin.FetchResponse{Status: f.status, Body: f.body, Headers: f.headers}, nil
	}
	tok := headers["PRIVATE-TOKEN"]
	_, reach := reachTokens[tok]
	if tok != fakeToken && tok != fakeNewToken && tok != "glpat-readonly-scope" && tok != fakeReadToken && !reach {
		return resp(401, map[string]string{"message": "401 Unauthorized"})
	}
	var payload map[string]any
	if body != "" {
		_ = json.Unmarshal([]byte(body), &payload)
	}
	return g.route(method, segs, u.Query(), tok, payload)
}

func (g *fakeGitLab) route(method string, segs []string, q url.Values, tok string, payload map[string]any) (*plugin.FetchResponse, error) {
	switch {
	case method == "GET" && len(segs) == 1 && segs[0] == "user":
		name := map[string]string{fakeToken: fakeUsername, fakeNewToken: "project_1_bot_ab12", "glpat-readonly-scope": "someone", fakeReadToken: fakeUsername}[tok]
		if r, ok := reachTokens[tok]; ok {
			name = r.user
		}
		return resp(200, map[string]any{"id": 77, "username": name, "name": "Paca Bot", "bot": strings.Contains(name, "_bot_")})
	case method == "GET" && len(segs) == 2 && segs[0] == "personal_access_tokens" && segs[1] == "self":
		if tok == fakeReadToken {
			return resp(200, map[string]any{"scopes": []string{"read_repository"}, "expires_at": "2029-01-02", "active": true})
		}
		scopes := []string{"api"}
		if tok == "glpat-readonly-scope" {
			scopes = []string{"read_api"}
		}
		return resp(200, map[string]any{"user_id": 77, "scopes": scopes, "expires_at": "2030-06-01", "active": true})
	case method == "GET" && len(segs) == 2 && segs[0] == "users" && segs[1] == "77":
		return resp(200, map[string]any{"id": 77, "username": "project_1_bot_ab12", "name": "Paca Bot", "bot": true})
	case method == "GET" && len(segs) == 1 && segs[0] == "users":
		name := q.Get("username")
		if id, ok := fakeUsers[name]; ok {
			return resp(200, []map[string]any{{"id": id, "username": name}})
		}
		return resp(200, []any{})
	case method == "GET" && len(segs) == 1 && segs[0] == "projects":
		return g.listProjects(q)
	case method == "GET" && len(segs) == 2 && segs[0] == "groups":
		if tok == fakeGroupToken && segs[1] == "9" {
			return resp(200, map[string]any{"id": 9, "full_path": "acme/frontend", "path": "frontend"})
		}
		return notFound()
	case method == "GET" && len(segs) == 2 && segs[0] == "projects" && reachTokens[tok].user != "":
		// The reach-test tokens see projects through their own roles.
		p := g.project(segs[1])
		if p == nil {
			return notFound()
		}
		lvl, member := reachTokens[tok].levels[p.ID]
		if !member && !p.Public {
			return notFound()
		}
		q := *p
		q.Level = lvl
		return resp(200, projectJSON(q))
	case len(segs) >= 2 && segs[0] == "projects":
		for _, x := range extraProjects {
			if (segs[1] == x.Path || segs[1] == strconv.FormatInt(x.ID, 10)) && reachTokens[tok].user == "" {
				return notFound() // the default tokens do not see these projects
			}
		}
		return g.projectAPI(method, segs[1], segs[2:], q, payload)
	}
	return notFound()
}

func (g *fakeGitLab) listProjects(q url.Values) (*plugin.FetchResponse, error) {
	var all []map[string]any
	paths := []string{}
	for _, p := range fakeProjects {
		paths = append(paths, p.Path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		p := g.project(path)
		if s := q.Get("search"); s != "" && !strings.Contains(p.Path, s) {
			continue
		}
		all = append(all, projectJSON(*p))
	}
	per, _ := strconv.Atoi(q.Get("per_page"))
	pg, _ := strconv.Atoi(q.Get("page"))
	if per == 0 {
		per = 20
	}
	start := min((pg-1)*per, len(all))
	end := min(start+per, len(all))
	return resp(200, all[start:end])
}

func pageOf[T any](all []T, q url.Values) []T {
	per, _ := strconv.Atoi(q.Get("per_page"))
	pg, _ := strconv.Atoi(q.Get("page"))
	if per == 0 {
		per = 20
	}
	if pg == 0 {
		pg = 1
	}
	start := min((pg-1)*per, len(all))
	end := min(start+per, len(all))
	return all[start:end]
}

func (g *fakeGitLab) projectAPI(method, ref string, rest []string, q url.Values, payload map[string]any) (*plugin.FetchResponse, error) {
	p := g.project(ref)
	if p == nil {
		return notFound() // a project the token cannot see
	}
	if p.ID != 1 {
		// Only acme/web is seeded in depth; the others answer the project itself.
		if len(rest) == 0 && method == "GET" {
			return resp(200, projectJSON(*p))
		}
		return notFound()
	}
	if len(rest) == 0 {
		return resp(200, projectJSON(*p))
	}
	switch rest[0] {
	case "members":
		if len(rest) == 3 && rest[1] == "all" {
			id, _ := strconv.ParseInt(rest[2], 10, 64)
			if lvl, ok := fakeAccess[id]; ok {
				return resp(200, map[string]any{"id": id, "access_level": lvl})
			}
		}
		return notFound()
	case "repository":
		return g.repositoryAPI(method, rest[1:], q, payload)
	case "merge_requests":
		return g.mrAPI(method, rest[1:], q, payload)
	case "issues":
		return g.issueAPI(method, rest[1:], payload)
	case "pipelines":
		return g.pipelineAPI(method, rest[1:], q)
	case "jobs":
		return g.jobAPI(method, rest[1:])
	case "statuses":
		if method == "POST" && len(rest) == 2 {
			s := map[string]any{"sha": rest[1]}
			for k, v := range payload {
				s[k] = v
			}
			g.statuses = append(g.statuses, s)
			return resp(201, s)
		}
	case "hooks":
		return g.hookAPI(method, rest[1:], payload)
	case "search":
		if q.Get("scope") == "blobs" && strings.Contains("package main", q.Get("search")) {
			return resp(200, []map[string]any{{"path": "src/main.go", "startline": 1, "data": "package main"}})
		}
		return resp(200, []any{})
	}
	return notFound()
}

func (g *fakeGitLab) repositoryAPI(method string, rest []string, q url.Values, payload map[string]any) (*plugin.FetchResponse, error) {
	switch {
	case rest[0] == "branches" && method == "GET" && len(rest) == 2:
		sha, ok := g.branches[rest[1]]
		if !ok {
			return resp(404, map[string]string{"message": "404 Branch Not Found"})
		}
		return resp(200, map[string]any{"name": rest[1], "protected": rest[1] == "main", "commit": map[string]string{"id": sha}, "web_url": "https://gitlab.com/acme/web/-/tree/" + rest[1]})
	case rest[0] == "branches" && method == "POST":
		name, _ := payload["branch"].(string)
		ref, _ := payload["ref"].(string)
		if _, taken := g.branches[name]; taken {
			return resp(400, map[string]string{"message": "Branch already exists"})
		}
		var sha string
		for _, s := range g.branches {
			if s == ref {
				sha = s
			}
		}
		if sha == "" {
			return resp(400, map[string]string{"message": "Invalid reference name: " + ref})
		}
		g.branches[name] = sha
		return resp(201, map[string]any{"name": name, "commit": map[string]string{"id": sha}})
	case rest[0] == "files" && method == "GET" && len(rest) == 2:
		if rest[1] != "README.md" || q.Get("ref") != "main" {
			return resp(404, map[string]string{"message": "404 File Not Found"})
		}
		return resp(200, map[string]any{"blob_id": "blob1", "encoding": "base64", "content": "IyBSZWFkbWUK"}) // "# Readme\n"
	case rest[0] == "commits" && len(rest) == 3 && rest[2] == "statuses":
		return resp(200, []map[string]any{
			{"name": "ci/external", "status": "success", "target_url": "https://ci.example/1", "description": "ok"},
			{"name": "test", "status": "failed", "target_url": "https://gitlab.com/acme/web/-/jobs/5001", "description": ""},
		})
	}
	return notFound()
}

func (g *fakeGitLab) mrAPI(method string, rest []string, q url.Values, payload map[string]any) (*plugin.FetchResponse, error) {
	if len(rest) == 0 {
		if method != "POST" {
			return notFound()
		}
		src, _ := payload["source_branch"].(string)
		if _, ok := g.branches[src]; !ok {
			return resp(400, map[string]string{"message": "Source branch does not exist"})
		}
		for _, m := range g.mrs {
			if m.SourceRef == src && m.State == "opened" {
				return resp(409, map[string]string{"message": "Another open merge request already exists for this source branch: !" + strconv.Itoa(m.IID)})
			}
		}
		m := &fakeMR{IID: g.nextIID, State: "opened", SourceRef: src, SHA: g.branches[src]}
		g.nextIID++
		m.Title, _ = payload["title"].(string)
		m.Body, _ = payload["description"].(string)
		if l, ok := payload["labels"].(string); ok && l != "" {
			m.Labels = strings.Split(l, ",")
		}
		g.mrs[m.IID] = m
		return resp(201, m.json(g))
	}
	iid, _ := strconv.Atoi(rest[0])
	m, ok := g.mrs[iid]
	if !ok {
		return resp(404, map[string]string{"message": "404 Merge Request Not Found"})
	}
	sub := ""
	if len(rest) > 1 {
		sub = rest[1]
	}
	switch {
	case sub == "" && method == "GET":
		return resp(200, m.json(g))
	case sub == "" && method == "PUT":
		if v, ok := payload["title"].(string); ok {
			m.Title = v
		}
		if v, ok := payload["description"].(string); ok {
			m.Body = v
		}
		switch payload["state_event"] {
		case "close":
			m.State = "closed"
		case "reopen":
			m.State = "opened"
		}
		if v, ok := payload["add_labels"].(string); ok {
			m.Labels = append(m.Labels, strings.Split(v, ",")...)
		}
		if v, ok := payload["reviewer_ids"].([]any); ok {
			m.Reviewers = nil
			for _, id := range v {
				m.Reviewers = append(m.Reviewers, int64(id.(float64)))
			}
		}
		return resp(200, m.json(g))
	case sub == "diffs":
		return resp(200, pageOf(fakeDiffFiles, q))
	case sub == "merge" && method == "PUT":
		if sha, _ := payload["sha"].(string); sha != m.SHA {
			return resp(409, map[string]string{"message": "SHA does not match HEAD of source branch"})
		}
		m.State = "merged"
		out := m.json(g)
		out["merge_commit_sha"] = "merge-sha"
		return resp(200, out)
	case sub == "approve" && method == "POST":
		g.approved = append(g.approved, iid)
		return resp(201, map[string]any{"id": iid})
	case sub == "discussions" && method == "GET":
		return resp(200, g.discussions())
	case sub == "discussions" && method == "POST" && len(rest) == 2:
		if _, ok := payload["position"].(map[string]any); !ok {
			return resp(400, map[string]string{"message": "position is missing"})
		}
		g.nextID++
		return resp(201, map[string]any{"id": "disc-new", "notes": []map[string]any{{"id": g.nextID, "body": payload["body"]}}})
	case sub == "discussions" && len(rest) >= 3:
		th, ok := g.threads[rest[2]]
		if !ok {
			return resp(404, map[string]string{"message": "404 Discussion Not Found"})
		}
		if method == "PUT" {
			th.Resolved, _ = payload["resolved"].(bool)
			return resp(200, map[string]any{"id": th.ID})
		}
		th.Replies++
		g.nextID++
		return resp(201, map[string]any{"id": g.nextID, "body": payload["body"]})
	case sub == "notes":
		return g.noteAPI("merge_requests", iid, method, q, payload)
	}
	return notFound()
}

func (g *fakeGitLab) discussions() []map[string]any {
	th := g.threads["disc-1"]
	return []map[string]any{
		{"id": "ind-1", "individual_note": true, "notes": []map[string]any{{"id": 8001, "body": "first", "author": map[string]any{"username": "octocat"}}}},
		{"id": th.ID, "individual_note": false, "notes": []map[string]any{{
			"id": 8101, "body": "please rename", "author": map[string]any{"username": "octocat"}, "type": "DiffNote",
			"resolvable": true, "resolved": th.Resolved, "position": map[string]any{"new_path": "src/main.go", "new_line": 3},
		}, {
			"id": 8102, "body": "added a commit", "system": true, "author": map[string]any{"username": "octocat"},
		}}},
	}
}

func (g *fakeGitLab) noteAPI(kind string, iid int, method string, q url.Values, payload map[string]any) (*plugin.FetchResponse, error) {
	if method == "POST" {
		g.nextID++
		g.notes = append(g.notes, fakeNote{ID: g.nextID, IID: iid, Kind: kind, Body: fmt.Sprint(payload["body"])})
		return resp(201, map[string]any{"id": g.nextID, "body": payload["body"]})
	}
	var out []map[string]any
	for _, n := range g.notes {
		if n.IID == iid && n.Kind == kind {
			out = append(out, map[string]any{"id": n.ID, "body": n.Body, "author": map[string]any{"username": "octocat"}})
		}
	}
	// Activity and review notes are returned too; the provider must drop them.
	out = append(out,
		map[string]any{"id": 9998, "body": "added 1 commit", "system": true, "author": map[string]any{"username": "octocat"}},
		map[string]any{"id": 9999, "body": "inline", "type": "DiffNote", "position": map[string]any{"new_path": "a"}, "author": map[string]any{"username": "octocat"}})
	return resp(200, pageOf(out, q))
}

func (g *fakeGitLab) issueAPI(method string, rest []string, payload map[string]any) (*plugin.FetchResponse, error) {
	if len(rest) == 0 {
		return notFound()
	}
	iid, _ := strconv.Atoi(rest[0])
	if iid != 7 {
		return resp(404, map[string]string{"message": "404 Issue Not Found"})
	}
	switch {
	case len(rest) == 1 && method == "GET":
		return resp(200, map[string]any{"iid": 7, "title": "Bug", "description": "It breaks", "state": "opened",
			"web_url": "https://gitlab.com/acme/web/-/issues/7", "author": map[string]any{"username": "reader"},
			"labels": []string{"bug"}, "assignees": []map[string]any{{"username": "bob"}}})
	case len(rest) == 1 && method == "PUT":
		return resp(200, map[string]any{"iid": 7})
	case len(rest) == 2 && rest[1] == "notes":
		return g.noteAPI("issues", iid, method, nil, payload)
	}
	return notFound()
}

func (g *fakeGitLab) pipelineAPI(method string, rest []string, q url.Values) (*plugin.FetchResponse, error) {
	switch {
	case len(rest) == 0 && method == "GET":
		if q.Get("sha") != fakeHeadSHA && q.Get("ref") != "feat/PACA-12-login" {
			return resp(200, []any{})
		}
		return resp(200, []map[string]any{{"id": 950, "source": "external"}, {"id": 900, "source": "push"}})
	case len(rest) == 2 && rest[1] == "jobs" && rest[0] == "900":
		return resp(200, []map[string]any{
			{"id": 5001, "name": "test", "status": "failed", "failure_reason": "script_failure", "web_url": "https://gitlab.com/acme/web/-/jobs/5001"},
			{"id": 5002, "name": "lint", "status": "success", "allow_failure": true, "web_url": "https://gitlab.com/acme/web/-/jobs/5002"},
		})
	case len(rest) == 2 && rest[1] == "retry" && method == "POST":
		g.retried = append(g.retried, "pipeline:"+rest[0])
		return resp(201, map[string]any{"id": 900})
	}
	return notFound()
}

func (g *fakeGitLab) jobAPI(method string, rest []string) (*plugin.FetchResponse, error) {
	switch {
	case len(rest) == 2 && rest[0] == "5001" && rest[1] == "trace":
		return text(200, "\x1b[0Ksection_start:1:step\r\x1b[0K\x1b[31;1mFAIL\x1b[0m TestLogin\r\nexpected 1, got 2\n")
	case len(rest) == 2 && rest[1] == "retry" && method == "POST":
		g.retried = append(g.retried, "job:"+rest[0])
		return resp(201, map[string]any{"id": 5003})
	}
	return notFound()
}

func (g *fakeGitLab) hookAPI(method string, rest []string, payload map[string]any) (*plugin.FetchResponse, error) {
	switch {
	case len(rest) == 0 && method == "GET":
		var out []map[string]any
		for id, u := range g.hooks {
			out = append(out, map[string]any{"id": id, "url": u})
		}
		return resp(200, out)
	case len(rest) == 0 && method == "POST":
		g.nextHook++
		g.hooks[g.nextHook] = fmt.Sprint(payload["url"])
		g.checkHookBody(payload)
		return resp(201, map[string]any{"id": g.nextHook})
	case len(rest) == 1:
		id, _ := strconv.ParseInt(rest[0], 10, 64)
		if _, ok := g.hooks[id]; !ok {
			return notFound()
		}
		switch method {
		case "GET":
			return resp(200, map[string]any{"id": id, "url": g.hooks[id]})
		case "DELETE":
			delete(g.hooks, id)
			return text(204, "")
		}
		g.checkHookBody(payload)
		return resp(200, map[string]any{"id": id})
	}
	return notFound()
}

func (g *fakeGitLab) checkHookBody(p map[string]any) {
	if p["token"] != fakeWebhookSecret {
		g.t.Errorf("hook registered with the wrong secret token: %v", p["token"])
	}
	for _, ev := range []string{"push_events", "merge_requests_events", "note_events", "issues_events", "pipeline_events"} {
		if p[ev] != true {
			g.t.Errorf("hook does not subscribe to %s", ev)
		}
	}
}
