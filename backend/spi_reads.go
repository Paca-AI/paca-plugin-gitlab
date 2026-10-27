package main

import (
	"context"
	"encoding/base64"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	gp "github.com/Paca-AI/plugin-sdk-go/gitprovider"
)

const (
	listPerPage     = 100
	maxCheckPages   = 5
	maxDiffPages    = 20
	defaultLogBytes = 256 * 1024
)

// glRef is the part of a user GitLab embeds in other objects.
type glRef struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type glMR struct {
	IID             int       `json:"iid"`
	Title           string    `json:"title"`
	Description     string    `json:"description"`
	State           string    `json:"state"` // opened | closed | merged | locked
	Draft           bool      `json:"draft"`
	WorkInProgress  bool      `json:"work_in_progress"`
	SourceBranch    string    `json:"source_branch"`
	TargetBranch    string    `json:"target_branch"`
	SHA             string    `json:"sha"`
	MergeCommitSHA  string    `json:"merge_commit_sha"`
	SquashCommitSHA string    `json:"squash_commit_sha"`
	WebURL          string    `json:"web_url"`
	Author          glRef     `json:"author"`
	Labels          []string  `json:"labels"`
	Reviewers       []glRef   `json:"reviewers"`
	UpdatedAt       time.Time `json:"updated_at"`
	SourceProjectID int64     `json:"source_project_id"`
	TargetProjectID int64     `json:"target_project_id"`
	DiffRefs        struct {
		BaseSHA  string `json:"base_sha"`
		HeadSHA  string `json:"head_sha"`
		StartSHA string `json:"start_sha"`
	} `json:"diff_refs"`
}

func (m glMR) isDraft() bool { return m.Draft || m.WorkInProgress }

// headSHA is the commit the merge request currently points at.
func (m glMR) headSHA() string {
	if m.DiffRefs.HeadSHA != "" {
		return m.DiffRefs.HeadSHA
	}
	return m.SHA
}

// toCR maps a merge request onto the neutral shape. sourcePath is the full path
// of the source project (the target's own unless the request comes from a fork).
func (m glMR) toCR(targetPath, sourcePath string) gp.ChangeRequest {
	cr := gp.ChangeRequest{
		Number: strconv.Itoa(m.IID), Title: m.Title, Body: m.Description, Draft: m.isDraft(),
		Author: m.Author.Username, URL: m.WebURL, UpdatedAt: m.UpdatedAt, Labels: m.Labels,
		Head: gp.Branch{Ref: m.SourceBranch, SHA: m.headSHA(), Repo: sourcePath, FromFork: m.SourceProjectID != m.TargetProjectID},
		Base: gp.Branch{Ref: m.TargetBranch, SHA: m.DiffRefs.StartSHA, Repo: targetPath},
	}
	switch strings.ToLower(m.State) {
	case "merged":
		cr.State = gp.CRMerged
	case "closed":
		cr.State = gp.CRClosed
	default: // opened, locked (a merge in flight)
		cr.State = gp.CROpen
	}
	for _, r := range m.Reviewers {
		cr.Reviewers = append(cr.Reviewers, r.Username)
	}
	return cr
}

// number validates a change-request or issue number before it goes into a URL.
func number(n string) (string, error) {
	if _, err := strconv.ParseUint(n, 10, 32); err != nil {
		return "", gp.Errorf(gp.CodeValidation, "%q is not a valid number", n)
	}
	return n, nil
}

func page(cursor string) (int, error) {
	if cursor == "" {
		return 1, nil
	}
	n, err := strconv.Atoi(cursor)
	if err != nil || n < 1 {
		return 0, gp.Errorf(gp.CodeValidation, "invalid cursor")
	}
	return n, nil
}

func nextPage(got, cur int) string {
	if got >= listPerPage {
		return strconv.Itoa(cur + 1)
	}
	return ""
}

func paged(path string, pg int) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "per_page=" + strconv.Itoa(listPerPage) + "&page=" + strconv.Itoa(pg)
}

// cutHead keeps at most max bytes from the start, never splitting a rune.
func cutHead(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max], true
}

// cutTail keeps at most max bytes from the end, never splitting a rune.
func cutTail(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	i := len(s) - max
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:], true
}

func (p *glProvider) getMR(api *glAPI, proj, num string) (glMR, error) {
	var m glMR
	err := api.get(proj+"/merge_requests/"+num, &m)
	return m, err
}

// sourcePathOf names the project a merge request comes from: the target itself,
// or, for a fork, the fork (looked up; empty when it was deleted).
func sourcePathOf(api *glAPI, m glMR, targetPath string) string {
	if m.SourceProjectID == m.TargetProjectID || m.SourceProjectID == 0 {
		return targetPath
	}
	var g glProject
	if err := api.get("/projects/"+strconv.FormatInt(m.SourceProjectID, 10), &g); err != nil {
		return ""
	}
	return g.PathWithNamespace
}

// GetChangeRequest implements gp.Provider.
func (p *glProvider) GetChangeRequest(_ context.Context, req gp.CRRequest) (gp.ChangeRequest, error) {
	num, err := number(req.Number)
	if err != nil {
		return gp.ChangeRequest{}, err
	}
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return gp.ChangeRequest{}, err
	}
	m, err := p.getMR(api, proj, num)
	if err != nil {
		return gp.ChangeRequest{}, err
	}
	return m.toCR(req.Target.Repo.FullPath, sourcePathOf(api, m, req.Target.Repo.FullPath)), nil
}

// glDiffFile is one entry of GET …/merge_requests/:iid/diffs.
type glDiffFile struct {
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	Diff        string `json:"diff"`
	NewFile     bool   `json:"new_file"`
	RenamedFile bool   `json:"renamed_file"`
	DeletedFile bool   `json:"deleted_file"`
	TooLarge    bool   `json:"too_large"`
}

func (f glDiffFile) status() string {
	switch {
	case f.NewFile:
		return "added"
	case f.DeletedFile:
		return "removed"
	case f.RenamedFile:
		return "renamed"
	}
	return "modified"
}

// counts returns the added and removed lines of the file's hunks.
func (f glDiffFile) counts() (add, del int) {
	for _, l := range strings.Split(f.Diff, "\n") {
		switch {
		case strings.HasPrefix(l, "+"):
			add++
		case strings.HasPrefix(l, "-"):
			del++
		}
	}
	return add, del
}

// unified renders the file as the git-style diff text agents expect: GitLab
// returns only the hunks.
func (f glDiffFile) unified() string {
	oldP, newP := "a/"+f.OldPath, "b/"+f.NewPath
	var b strings.Builder
	b.WriteString("diff --git " + oldP + " " + newP + "\n")
	if f.NewFile {
		oldP = "/dev/null"
		b.WriteString("new file mode 100644\n")
	}
	if f.DeletedFile {
		newP = "/dev/null"
		b.WriteString("deleted file mode 100644\n")
	}
	switch {
	case f.TooLarge:
		b.WriteString("# diff too large to display\n")
	case strings.TrimSpace(f.Diff) == "":
		b.WriteString("# no textual changes (binary file, mode change or rename)\n")
	default:
		b.WriteString("--- " + oldP + "\n+++ " + newP + "\n" + f.Diff)
		if !strings.HasSuffix(f.Diff, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func (p *glProvider) diffPage(api *glAPI, proj, num string, pg int) ([]glDiffFile, error) {
	var files []glDiffFile
	err := api.get(paged(proj+"/merge_requests/"+num+"/diffs", pg), &files)
	return files, err
}

// ListChangedFiles implements gp.Provider.
func (p *glProvider) ListChangedFiles(_ context.Context, req gp.PagedCRRequest) (gp.ChangedFilePage, error) {
	num, err := number(req.Number)
	if err != nil {
		return gp.ChangedFilePage{}, err
	}
	pg, err := page(req.Cursor)
	if err != nil {
		return gp.ChangedFilePage{}, err
	}
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return gp.ChangedFilePage{}, err
	}
	files, err := p.diffPage(api, proj, num, pg)
	if err != nil {
		return gp.ChangedFilePage{}, err
	}
	out := gp.ChangedFilePage{Items: []gp.ChangedFile{}, NextCursor: nextPage(len(files), pg)}
	for _, f := range files {
		add, del := f.counts()
		cf := gp.ChangedFile{Path: f.NewPath, Status: f.status(), Additions: add, Deletions: del}
		if f.RenamedFile {
			cf.OldPath = f.OldPath
		}
		if f.DeletedFile {
			cf.Path = f.OldPath
		}
		out.Items = append(out.Items, cf)
	}
	return out, nil
}

// GetDiff implements gp.Provider. A per-file diff is cut out of the pages; a
// diff longer than MaxBytes is cut and flagged, never silently.
func (p *glProvider) GetDiff(_ context.Context, req gp.DiffRequest) (gp.Diff, error) {
	num, err := number(req.Number)
	if err != nil {
		return gp.Diff{}, err
	}
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return gp.Diff{}, err
	}
	var b strings.Builder
	found, more := false, false
	for pg := 1; pg <= maxDiffPages; pg++ {
		files, err := p.diffPage(api, proj, num, pg)
		if err != nil {
			return gp.Diff{}, err
		}
		for _, f := range files {
			if req.Path != "" && f.NewPath != req.Path && f.OldPath != req.Path {
				continue
			}
			found = true
			b.WriteString(f.unified())
		}
		more = len(files) >= listPerPage
		if !more || (req.MaxBytes > 0 && b.Len() > req.MaxBytes) {
			break
		}
	}
	if req.Path != "" && !found {
		return gp.Diff{}, gp.Errorf(gp.CodeNotFound, "%s is not part of this change", req.Path)
	}
	cut, truncated := cutHead(b.String(), req.MaxBytes)
	// Stopping at the page cap with more files behind it is a cut too.
	return gp.Diff{Text: cut, Truncated: truncated || (more && req.Path == "")}, nil
}

type glPosition struct {
	NewPath string `json:"new_path"`
	OldPath string `json:"old_path"`
	NewLine int    `json:"new_line"`
	OldLine int    `json:"old_line"`
}

type glNote struct {
	ID         int64       `json:"id"`
	Body       string      `json:"body"`
	Author     glRef       `json:"author"`
	CreatedAt  time.Time   `json:"created_at"`
	System     bool        `json:"system"`
	Resolvable bool        `json:"resolvable"`
	Resolved   bool        `json:"resolved"`
	Type       *string     `json:"type"`
	Position   *glPosition `json:"position"`
}

func (n glNote) isDiffNote() bool {
	return n.Position != nil || (n.Type != nil && *n.Type == "DiffNote")
}

func (n glNote) toComment(webBase string) gp.Comment {
	return gp.Comment{
		ID: strconv.FormatInt(n.ID, 10), Author: n.Author.Username, AuthorKind: loginKind(n.Author.Username),
		Body: n.Body, URL: webBase + "#note_" + strconv.FormatInt(n.ID, 10), CreatedAt: n.CreatedAt,
	}
}

type glDiscussion struct {
	ID             string   `json:"id"`
	IndividualNote bool     `json:"individual_note"`
	Notes          []glNote `json:"notes"`
}

// ListThreads implements gp.Provider: the review discussions of a merge request.
// Plain conversation comments (individual notes) come from ListComments.
func (p *glProvider) ListThreads(_ context.Context, req gp.PagedCRRequest) (gp.ThreadPage, error) {
	num, err := number(req.Number)
	if err != nil {
		return gp.ThreadPage{}, err
	}
	pg, err := page(req.Cursor)
	if err != nil {
		return gp.ThreadPage{}, err
	}
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return gp.ThreadPage{}, err
	}
	var ds []glDiscussion
	if err := api.get(paged(proj+"/merge_requests/"+num+"/discussions", pg), &ds); err != nil {
		return gp.ThreadPage{}, err
	}
	webBase := webURL(hostOf(req.Target), req.Target.Repo.FullPath, "/-/merge_requests/"+num)
	out := gp.ThreadPage{Items: []gp.Thread{}, NextCursor: nextPage(len(ds), pg)}
	for _, d := range ds {
		if d.IndividualNote {
			continue
		}
		th := gp.Thread{ID: d.ID}
		resolvable, resolved := false, true
		for _, n := range d.Notes {
			if n.System {
				continue
			}
			if th.Path == "" && n.Position != nil {
				th.Path, th.Line = n.Position.NewPath, n.Position.NewLine
				if th.Path == "" {
					th.Path, th.Line = n.Position.OldPath, n.Position.OldLine
				}
			}
			if n.Resolvable {
				resolvable = true
				resolved = resolved && n.Resolved
			}
			th.Comments = append(th.Comments, n.toComment(webBase))
		}
		th.Resolved = resolvable && resolved
		if len(th.Comments) > 0 {
			out.Items = append(out.Items, th)
		}
	}
	return out, nil
}

// hostOf is the host a target's repository lives on.
func hostOf(t gp.Target) string {
	if t.Repo.Host != "" {
		return t.Repo.Host
	}
	return defaultHost
}

// subjectPath is the REST segment and web segment of an issue or merge request.
func subjectPath(k gp.SubjectKind) (api, web string, err error) {
	switch k {
	case gp.SubjectChangeRequest:
		return "merge_requests", "merge_requests", nil
	case gp.SubjectIssue:
		return "issues", "issues", nil
	}
	return "", "", gp.Errorf(gp.CodeValidation, "subject kind must be issue or change_request")
}

// ListComments implements gp.Provider: the conversation comments of an issue or
// change request. Activity notes ("added 1 commit") and review-thread comments
// are left out; the latter come from ListThreads.
func (p *glProvider) ListComments(_ context.Context, req gp.ListCommentsRequest) (gp.CommentPage, error) {
	num, err := number(req.Subject.Number)
	if err != nil {
		return gp.CommentPage{}, err
	}
	seg, web, err := subjectPath(req.Subject.Kind)
	if err != nil {
		return gp.CommentPage{}, err
	}
	pg, err := page(req.Cursor)
	if err != nil {
		return gp.CommentPage{}, err
	}
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return gp.CommentPage{}, err
	}
	var ns []glNote
	if err := api.get(paged(proj+"/"+seg+"/"+num+"/notes?sort=asc&order_by=created_at", pg), &ns); err != nil {
		return gp.CommentPage{}, err
	}
	webBase := webURL(hostOf(req.Target), req.Target.Repo.FullPath, "/-/"+web+"/"+num)
	out := gp.CommentPage{Items: []gp.Comment{}, NextCursor: nextPage(len(ns), pg)}
	for _, n := range ns {
		if n.System || n.isDiffNote() {
			continue
		}
		out.Items = append(out.Items, n.toComment(webBase))
	}
	return out, nil
}

// GetIssue implements gp.Provider.
func (p *glProvider) GetIssue(_ context.Context, req gp.IssueRequest) (gp.Issue, error) {
	num, err := number(req.Number)
	if err != nil {
		return gp.Issue{}, err
	}
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return gp.Issue{}, err
	}
	var is struct {
		IID         int      `json:"iid"`
		Title       string   `json:"title"`
		Description string   `json:"description"`
		State       string   `json:"state"`
		WebURL      string   `json:"web_url"`
		Author      glRef    `json:"author"`
		Labels      []string `json:"labels"`
		Assignees   []glRef  `json:"assignees"`
	}
	if err := api.get(proj+"/issues/"+num, &is); err != nil {
		return gp.Issue{}, err
	}
	state := "open"
	if is.State == "closed" {
		state = "closed"
	}
	out := gp.Issue{Number: strconv.Itoa(is.IID), Title: is.Title, Body: is.Description, State: state, Author: is.Author.Username, URL: is.WebURL, Labels: is.Labels}
	for _, a := range is.Assignees {
		out.Assignees = append(out.Assignees, a.Username)
	}
	return out, nil
}

func jobStateOf(status string) gp.CheckState {
	switch status {
	case "success":
		return gp.CheckSuccess
	case "failed":
		return gp.CheckFailure
	case "canceled", "canceling":
		return gp.CheckCancelled
	case "skipped", "manual":
		return gp.CheckSkipped
	}
	return gp.CheckPending // created, pending, running, scheduled, waiting_for_resource, preparing
}

var fullSHARe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ciPipeline finds the newest CI pipeline of a ref (a commit sha or a branch).
// Pipelines of source "external" are the statuses other systems (and Paca)
// post; they carry no jobs, so they are read as statuses instead.
func ciPipeline(api *glAPI, proj, ref string) (id int64, found bool, err error) {
	filter := "ref=" + url.QueryEscape(ref)
	if fullSHARe.MatchString(strings.ToLower(ref)) {
		filter = "sha=" + url.QueryEscape(ref)
	}
	var ps []struct {
		ID     int64  `json:"id"`
		Source string `json:"source"`
	}
	if err := api.get(proj+"/pipelines?order_by=id&sort=desc&per_page=20&"+filter, &ps); err != nil {
		return 0, false, err
	}
	for _, p := range ps {
		if p.Source != "external" {
			return p.ID, true, nil
		}
	}
	return 0, false, nil
}

// GetChecks implements gp.Provider: the jobs of the newest pipeline plus commit
// statuses other systems posted. Ids are "job:<id>" and "status:<name>".
func (p *glProvider) GetChecks(_ context.Context, req gp.ChecksRequest) ([]gp.Check, error) {
	if strings.TrimSpace(req.Ref) == "" {
		return nil, gp.Errorf(gp.CodeValidation, "ref is required")
	}
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return nil, err
	}
	out := []gp.Check{}
	pid, found, err := ciPipeline(api, proj, req.Ref)
	if err != nil {
		return nil, err
	}
	if found {
		for pg := 1; pg <= maxCheckPages; pg++ {
			var jobs []struct {
				ID            int64  `json:"id"`
				Name          string `json:"name"`
				Status        string `json:"status"`
				AllowFailure  bool   `json:"allow_failure"`
				WebURL        string `json:"web_url"`
				FailureReason string `json:"failure_reason"`
			}
			if err := api.get(paged(proj+"/pipelines/"+strconv.FormatInt(pid, 10)+"/jobs", pg), &jobs); err != nil {
				return nil, err
			}
			for _, j := range jobs {
				c := gp.Check{ID: "job:" + strconv.FormatInt(j.ID, 10), Name: j.Name, State: jobStateOf(j.Status), Required: !j.AllowFailure, URL: j.WebURL}
				if c.State == gp.CheckFailure {
					c.Summary = j.FailureReason
				}
				out = append(out, c)
			}
			if len(jobs) < listPerPage {
				break
			}
		}
	}
	var statuses []struct {
		Name         string `json:"name"`
		Status       string `json:"status"`
		TargetURL    string `json:"target_url"`
		Description  string `json:"description"`
		AllowFailure bool   `json:"allow_failure"`
	}
	if err := api.get(paged(proj+"/repository/commits/"+pathPart(req.Ref)+"/statuses", 1), &statuses); err == nil {
		for _, s := range statuses {
			if strings.Contains(s.TargetURL, "/-/jobs/") {
				continue // a pipeline job, already listed above
			}
			out = append(out, gp.Check{ID: "status:" + s.Name, Name: s.Name, State: jobStateOf(s.Status), Required: !s.AllowFailure, URL: s.TargetURL, Summary: s.Description})
		}
	} else if gp.AsError(err).Code != gp.CodeNotFound {
		return nil, err
	}
	return out, nil
}

var (
	ansiRe    = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")
	sectionRe = regexp.MustCompile(`section_(?:start|end):[0-9]+:[A-Za-z0-9_.\-\[\],=]*`)
)

// cleanTrace removes the terminal control sequences and section markers GitLab
// leaves in a job log, which are noise to a reader.
func cleanTrace(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	s = sectionRe.ReplaceAllString(s, "")
	return strings.ReplaceAll(s, "\r", "")
}

// GetCheckLogs implements gp.Provider: the trace of a pipeline job.
func (p *glProvider) GetCheckLogs(_ context.Context, req gp.CheckLogsRequest) (gp.Logs, error) {
	id, ok := strings.CutPrefix(req.CheckID, "job:")
	if !ok || !allDigits(id) {
		return gp.Logs{}, gp.Errorf(gp.CodeNotFound, "no logs for check %q", clip(req.CheckID))
	}
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return gp.Logs{}, err
	}
	text, err := api.do("GET", proj+"/jobs/"+id+"/trace", nil)
	if err != nil {
		return gp.Logs{}, err
	}
	text = cleanTrace(text)
	if strings.TrimSpace(text) == "" {
		return gp.Logs{}, gp.Errorf(gp.CodeNotFound, "this job produced no log output")
	}
	limit := req.TailBytes
	if limit <= 0 {
		limit = defaultLogBytes
	}
	cut, truncated := cutTail(text, limit)
	return gp.Logs{Text: cut, Truncated: truncated}, nil
}

// GetFile implements gp.FileReader.
func (p *glProvider) GetFile(_ context.Context, req gp.FileRequest) (gp.File, error) {
	path := strings.Trim(req.Path, "/")
	if path == "" || strings.Contains(path, "..") {
		return gp.File{}, gp.Errorf(gp.CodeValidation, "a file path is required")
	}
	ref := strings.TrimSpace(req.Ref)
	if ref == "" {
		ref = req.Target.Repo.DefaultBranch
	}
	if ref == "" {
		return gp.File{}, gp.Errorf(gp.CodeValidation, "a ref is required: the repository has no known default branch")
	}
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return gp.File{}, err
	}
	var f struct {
		BlobID   string `json:"blob_id"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if err := api.get(proj+"/repository/files/"+pathPart(path)+"?ref="+url.QueryEscape(ref), &f); err != nil {
		return gp.File{}, err
	}
	content := []byte(f.Content)
	if f.Encoding == "base64" {
		b, err := base64.StdEncoding.DecodeString(strings.NewReplacer("\n", "", "\r", "").Replace(f.Content))
		if err != nil {
			return gp.File{}, gp.Errorf(gp.CodeInternal, "GitLab returned an unreadable file")
		}
		content = b
	}
	out := gp.File{Path: path, SHA: f.BlobID, Content: content}
	if req.MaxBytes > 0 && len(content) > req.MaxBytes {
		out.Content, out.Truncated = content[:req.MaxBytes], true
	}
	return out, nil
}

// SearchCode implements gp.CodeSearcher with GitLab's project blob search.
func (p *glProvider) SearchCode(_ context.Context, req gp.CodeSearchRequest) (gp.CodeHits, error) {
	q := strings.TrimSpace(req.Query)
	if q == "" {
		return gp.CodeHits{}, gp.Errorf(gp.CodeValidation, "a query is required")
	}
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return gp.CodeHits{}, err
	}
	var hits []struct {
		Path      string `json:"path"`
		Filename  string `json:"filename"`
		StartLine int    `json:"startline"`
		Data      string `json:"data"`
	}
	if err := api.get(proj+"/search?scope=blobs&per_page=20&search="+url.QueryEscape(q), &hits); err != nil {
		return gp.CodeHits{}, err
	}
	out := gp.CodeHits{Items: []gp.CodeHit{}}
	for _, h := range hits {
		path := h.Path
		if path == "" {
			path = h.Filename
		}
		out.Items = append(out.Items, gp.CodeHit{Path: path, Line: h.StartLine, Snippet: clip(h.Data)})
	}
	return out, nil
}
