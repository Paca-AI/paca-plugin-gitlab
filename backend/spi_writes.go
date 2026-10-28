package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	gp "github.com/Paca-AI/plugin-sdk-go/gitprovider"
)

func blank(s string) bool { return strings.TrimSpace(s) == "" }

var draftPrefixRe = regexp.MustCompile(`(?i)^\s*(?:\[draft\]|\(draft\)|draft:|wip:)\s*`)

// withDraft sets or clears a title's draft marker: GitLab's API has no draft
// flag, a merge request is a draft when its title starts with "Draft:".
func withDraft(title string, draft bool) string {
	title = draftPrefixRe.ReplaceAllString(title, "")
	if draft {
		return "Draft: " + title
	}
	return title
}

// userIDs resolves user names to GitLab ids; an unknown name is a validation
// error, not something to skip silently.
func (a *glAPI) userIDs(logins []string) ([]int64, error) {
	ids := make([]int64, 0, len(logins))
	for _, l := range logins {
		id, ok, err := a.lookupUserID(strings.TrimSpace(l))
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, gp.Errorf(gp.CodeValidation, "GitLab has no user %q", clip(l))
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// OpenChangeRequest implements gp.Provider. Labels go in the request (GitLab
// creates unknown ones); reviewers are applied afterwards and, if that fails,
// the merge request is still returned (retrying the open would only collide
// with the one just created). GitLab has no idempotency keys, so IdempotentWrites
// is not declared and core de-duplicates itself.
func (p *glProvider) OpenChangeRequest(_ context.Context, req gp.OpenCRRequest) (gp.ChangeRequest, error) {
	if blank(req.Title) || blank(req.Head) || blank(req.Base) {
		return gp.ChangeRequest{}, gp.Errorf(gp.CodeValidation, "title, head and base are required")
	}
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return gp.ChangeRequest{}, err
	}
	body := map[string]any{
		"title": withDraft(req.Title, req.Draft), "description": req.Body,
		"source_branch": req.Head, "target_branch": req.Base,
	}
	if len(req.Labels) > 0 {
		body["labels"] = strings.Join(req.Labels, ",")
	}
	var m glMR
	if err := api.send("POST", proj+"/merge_requests", body, &m); err != nil {
		return gp.ChangeRequest{}, err
	}
	if len(req.Reviewers) > 0 {
		if ids, err := api.userIDs(req.Reviewers); err == nil {
			if err := api.send("PUT", proj+"/merge_requests/"+strconv.Itoa(m.IID), map[string]any{"reviewer_ids": ids}, &m); err != nil {
				m.Reviewers = nil
			}
		}
	}
	return m.toCR(req.Target.Repo.FullPath, req.Target.Repo.FullPath), nil
}

// UpdateChangeRequest implements gp.Provider. Only open and closed can be set;
// merging is its own operation. Toggling draft rewrites the title prefix.
func (p *glProvider) UpdateChangeRequest(_ context.Context, req gp.UpdateCRRequest) (gp.ChangeRequest, error) {
	num, err := number(req.Number)
	if err != nil {
		return gp.ChangeRequest{}, err
	}
	patch := map[string]any{}
	if req.Title != nil && blank(*req.Title) {
		return gp.ChangeRequest{}, gp.Errorf(gp.CodeValidation, "title must not be empty")
	}
	if req.Body != nil {
		patch["description"] = *req.Body
	}
	if req.State != nil {
		switch *req.State {
		case gp.CROpen:
			patch["state_event"] = "reopen"
		case gp.CRClosed:
			patch["state_event"] = "close"
		default:
			return gp.ChangeRequest{}, gp.Errorf(gp.CodeValidation, "state can only be set to open or closed; merging is a separate operation")
		}
	}
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return gp.ChangeRequest{}, err
	}
	path := proj + "/merge_requests/" + num
	title := req.Title
	if req.Draft != nil {
		if title == nil {
			cur, err := p.getMR(api, proj, num)
			if err != nil {
				return gp.ChangeRequest{}, err
			}
			title = &cur.Title
		}
		t := withDraft(*title, *req.Draft)
		title = &t
	}
	if title != nil {
		patch["title"] = *title
	}
	var m glMR
	if len(patch) > 0 {
		err = api.send("PUT", path, patch, &m)
	} else {
		m, err = p.getMR(api, proj, num)
	}
	if err != nil {
		return gp.ChangeRequest{}, err
	}
	return m.toCR(req.Target.Repo.FullPath, sourcePathOf(api, m, req.Target.Repo.FullPath)), nil
}

func noteResult(host, fullPath, web, num string, id int64) gp.CommentResult {
	return gp.CommentResult{ID: strconv.FormatInt(id, 10), URL: webURL(host, fullPath, "/-/"+web+"/"+num+"#note_"+strconv.FormatInt(id, 10))}
}

// Comment implements gp.Provider: a conversation note on an issue or merge request.
func (p *glProvider) Comment(_ context.Context, req gp.CommentRequest) (gp.CommentResult, error) {
	if blank(req.Body) {
		return gp.CommentResult{}, gp.Errorf(gp.CodeValidation, "the comment is empty")
	}
	num, err := number(req.Subject.Number)
	if err != nil {
		return gp.CommentResult{}, err
	}
	seg, web, err := subjectPath(req.Subject.Kind)
	if err != nil {
		return gp.CommentResult{}, err
	}
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return gp.CommentResult{}, err
	}
	var n glNote
	if err := api.send("POST", proj+"/"+seg+"/"+num+"/notes", map[string]any{"body": req.Body}, &n); err != nil {
		return gp.CommentResult{}, err
	}
	return noteResult(hostOf(req.Target), req.Target.Repo.FullPath, web, num, n.ID), nil
}

// ReplyThread implements gp.Provider: a note in a merge-request discussion; the
// thread id is the discussion id ListThreads returned.
func (p *glProvider) ReplyThread(_ context.Context, req gp.ReplyThreadRequest) (gp.CommentResult, error) {
	if blank(req.Body) {
		return gp.CommentResult{}, gp.Errorf(gp.CodeValidation, "the reply is empty")
	}
	if blank(req.ThreadID) {
		return gp.CommentResult{}, gp.Errorf(gp.CodeValidation, "thread_id is required")
	}
	num, err := number(req.Number)
	if err != nil {
		return gp.CommentResult{}, err
	}
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return gp.CommentResult{}, err
	}
	var n glNote
	if err := api.send("POST", proj+"/merge_requests/"+num+"/discussions/"+pathPart(req.ThreadID)+"/notes", map[string]any{"body": req.Body}, &n); err != nil {
		return gp.CommentResult{}, err
	}
	return noteResult(hostOf(req.Target), req.Target.Repo.FullPath, "merge_requests", num, n.ID), nil
}

// SubmitReview implements gp.Provider. GitLab has no pending review that is
// submitted at once through the REST API, so it is emulated and reported as such:
// every inline comment becomes a discussion on its line, then the summary is
// posted as a note, then an approval is recorded. "Request changes" has no REST
// equivalent; it is a note that says so. Inline paths are checked against the
// diff up front so a bad one fails before anything is posted.
func (p *glProvider) SubmitReview(_ context.Context, req gp.SubmitReviewRequest) (gp.ReviewResult, error) {
	switch req.Verdict {
	case gp.VerdictComment, gp.VerdictApprove, gp.VerdictRequestChanges:
	default:
		return gp.ReviewResult{}, gp.Errorf(gp.CodeValidation, "verdict must be comment, approve or request_changes")
	}
	num, err := number(req.Number)
	if err != nil {
		return gp.ReviewResult{}, err
	}
	for _, c := range req.Comments {
		if blank(c.Path) || c.Line < 1 || blank(c.Body) {
			return gp.ReviewResult{}, gp.Errorf(gp.CodeValidation, "inline comments need a path, a line and a body")
		}
	}
	summary := strings.TrimSpace(req.Summary)
	if req.Verdict == gp.VerdictRequestChanges {
		if summary == "" {
			summary = "Changes requested."
		} else {
			summary = "**Changes requested**\n\n" + summary
		}
	}
	if len(req.Comments) == 0 && summary == "" && req.Verdict != gp.VerdictApprove {
		return gp.ReviewResult{}, gp.Errorf(gp.CodeValidation, "a review needs a summary or inline comments")
	}
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return gp.ReviewResult{}, err
	}
	mr, err := p.getMR(api, proj, num)
	if err != nil {
		return gp.ReviewResult{}, err
	}
	if req.HeadSHA != "" && mr.headSHA() != "" && !strings.EqualFold(req.HeadSHA, mr.headSHA()) {
		return gp.ReviewResult{}, gp.Errorf(gp.CodeConflict, "the merge request has new commits since the reviewed head")
	}

	res := gp.ReviewResult{Emulated: true}
	web := webURL(hostOf(req.Target), req.Target.Repo.FullPath, "/-/merge_requests/"+num)
	if len(req.Comments) > 0 {
		files, err := p.changedFiles(api, proj, num)
		if err != nil {
			return gp.ReviewResult{}, err
		}
		for _, c := range req.Comments {
			if _, ok := files[c.Path]; !ok {
				return gp.ReviewResult{}, gp.Errorf(gp.CodeValidation, "%s is not part of this change", c.Path)
			}
		}
		for _, c := range req.Comments {
			f := files[c.Path]
			pos := map[string]any{
				"position_type": "text", "base_sha": mr.DiffRefs.BaseSHA, "start_sha": mr.DiffRefs.StartSHA, "head_sha": mr.headSHA(),
				"old_path": f.OldPath, "new_path": f.NewPath,
			}
			if c.Side == "old" {
				pos["old_line"] = c.Line
			} else {
				pos["new_line"] = c.Line
			}
			var d glDiscussion
			if err := api.send("POST", proj+"/merge_requests/"+num+"/discussions", map[string]any{"body": c.Body, "position": pos}, &d); err != nil {
				e := gp.AsError(err)
				return gp.ReviewResult{}, gp.Errorf(e.Code, "posted %d of %d inline comments, then GitLab refused one: %s", res.Posted, len(req.Comments), e.Message)
			}
			res.Posted++
			if res.ID == "" && len(d.Notes) > 0 {
				res.ID, res.URL = strconv.FormatInt(d.Notes[0].ID, 10), web+"#note_"+strconv.FormatInt(d.Notes[0].ID, 10)
			}
		}
	}
	if summary != "" {
		var n glNote
		if err := api.send("POST", proj+"/merge_requests/"+num+"/notes", map[string]any{"body": summary}, &n); err != nil {
			return gp.ReviewResult{}, err
		}
		res.ID, res.URL = strconv.FormatInt(n.ID, 10), web+"#note_"+strconv.FormatInt(n.ID, 10)
	}
	if req.Verdict == gp.VerdictApprove {
		body := map[string]any{}
		if sha := firstNonEmpty(req.HeadSHA, mr.headSHA()); sha != "" {
			body["sha"] = sha // GitLab refuses the approval if the head moved
		}
		if err := api.send("POST", proj+"/merge_requests/"+num+"/approve", body, nil); err != nil {
			return gp.ReviewResult{}, err
		}
	}
	return res, nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// changedFiles indexes a merge request's files by new path (and old path for
// renames), reading at most maxDiffPages pages.
func (p *glProvider) changedFiles(api *glAPI, proj, num string) (map[string]glDiffFile, error) {
	out := map[string]glDiffFile{}
	for pg := 1; pg <= maxDiffPages; pg++ {
		files, err := p.diffPage(api, proj, num, pg)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			out[f.NewPath] = f
			if _, dup := out[f.OldPath]; !dup {
				out[f.OldPath] = f
			}
		}
		if len(files) < listPerPage {
			break
		}
	}
	return out, nil
}

// statusRef is what SetStatus hands back so a later call updates the same
// commit status: GitLab identifies one by (sha, name, ref).
type statusRef struct {
	SHA  string `json:"s"`
	Name string `json:"n"`
	Ref  string `json:"r,omitempty"`
}

func (r statusRef) encode() string {
	b, _ := json.Marshal(r)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeStatusRef(s string) (statusRef, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return statusRef{}, false
	}
	var r statusRef
	if json.Unmarshal(raw, &r) != nil || r.SHA == "" || r.Name == "" {
		return statusRef{}, false
	}
	return r, true
}

// maxStatusDescription is GitLab's limit on a commit status description.
const maxStatusDescription = 255

func cutRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

// SetStatus implements gp.Provider as a commit status named after the run (the
// identity's own status line on the commit). Posting the same name on the same
// commit and ref updates it in place, so StatusRef only has to carry them.
func (p *glProvider) SetStatus(_ context.Context, req gp.SetStatusRequest) (gp.StatusResult, error) {
	if blank(req.Title) {
		return gp.StatusResult{}, gp.Errorf(gp.CodeValidation, "title is required")
	}
	state := map[gp.RunState]string{
		gp.RunQueued: "pending", gp.RunInProgress: "running", gp.RunSuccess: "success",
		gp.RunFailure: "failed", gp.RunCancelled: "canceled",
	}[req.State]
	if state == "" {
		return gp.StatusResult{}, gp.Errorf(gp.CodeValidation, "unknown state %q", req.State)
	}
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return gp.StatusResult{}, err
	}
	ref := statusRef{SHA: req.SHA, Name: req.Title}
	if req.StatusRef != "" {
		r, ok := decodeStatusRef(req.StatusRef)
		if !ok {
			return gp.StatusResult{}, gp.Errorf(gp.CodeValidation, "status_ref is not one this provider issued")
		}
		ref = r
		ref.Name = req.Title
	} else if req.Subject.Kind == gp.SubjectChangeRequest && req.Subject.Number != "" {
		num, err := number(req.Subject.Number)
		if err != nil {
			return gp.StatusResult{}, err
		}
		mr, err := p.getMR(api, proj, num)
		if err != nil {
			return gp.StatusResult{}, err
		}
		// The branch ties the status to the merge request's pipeline view.
		ref.Ref = mr.SourceBranch
		if ref.SHA == "" {
			ref.SHA = mr.headSHA()
		}
	}
	if ref.SHA == "" {
		return gp.StatusResult{}, gp.Errorf(gp.CodeValidation, "sha or a change request is required")
	}
	desc := req.Summary
	if blank(desc) {
		desc = req.Title
	}
	body := map[string]any{"state": state, "name": ref.Name, "description": cutRunes(desc, maxStatusDescription)}
	if ref.Ref != "" {
		body["ref"] = ref.Ref
	}
	if req.URL != "" {
		body["target_url"] = req.URL
	}
	if err := api.send("POST", proj+"/statuses/"+url.PathEscape(ref.SHA), body, nil); err != nil {
		return gp.StatusResult{}, err
	}
	return gp.StatusResult{StatusRef: ref.encode()}, nil
}

// ── optional operations ──────────────────────────────────────────────────────

// ResolveThread implements gp.ThreadResolver.
func (p *glProvider) ResolveThread(_ context.Context, req gp.ResolveThreadRequest) (gp.Empty, error) {
	if blank(req.ThreadID) {
		return gp.Empty{}, gp.Errorf(gp.CodeValidation, "thread_id is required")
	}
	num, err := number(req.Number)
	if err != nil {
		return gp.Empty{}, err
	}
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return gp.Empty{}, err
	}
	return gp.Empty{}, api.send("PUT", proj+"/merge_requests/"+num+"/discussions/"+pathPart(req.ThreadID), map[string]any{"resolved": req.Resolved}, nil)
}

// AddLabels implements gp.LabelManager.
func (p *glProvider) AddLabels(_ context.Context, req gp.LabelsRequest) (gp.Empty, error) {
	num, err := number(req.Subject.Number)
	if err != nil {
		return gp.Empty{}, err
	}
	seg, _, err := subjectPath(req.Subject.Kind)
	if err != nil {
		return gp.Empty{}, err
	}
	if len(req.Labels) == 0 {
		return gp.Empty{}, gp.Errorf(gp.CodeValidation, "at least one label is required")
	}
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return gp.Empty{}, err
	}
	return gp.Empty{}, api.send("PUT", proj+"/"+seg+"/"+num, map[string]any{"add_labels": strings.Join(req.Labels, ",")}, nil)
}

// RequestReviewers implements gp.ReviewerRequester. GitLab replaces the whole
// reviewer list, so the existing reviewers are kept and the new ones added.
func (p *glProvider) RequestReviewers(_ context.Context, req gp.PeopleRequest) (gp.Empty, error) {
	if req.Subject.Kind != gp.SubjectChangeRequest {
		return gp.Empty{}, gp.Errorf(gp.CodeValidation, "reviewers can only be requested on a merge request")
	}
	num, err := number(req.Subject.Number)
	if err != nil {
		return gp.Empty{}, err
	}
	if len(req.Logins) == 0 {
		return gp.Empty{}, gp.Errorf(gp.CodeValidation, "at least one login is required")
	}
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return gp.Empty{}, err
	}
	add, err := api.userIDs(req.Logins)
	if err != nil {
		return gp.Empty{}, err
	}
	mr, err := p.getMR(api, proj, num)
	if err != nil {
		return gp.Empty{}, err
	}
	ids := []int64{}
	seen := map[int64]bool{}
	for _, id := range append(refIDs(mr.Reviewers), add...) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return gp.Empty{}, api.send("PUT", proj+"/merge_requests/"+num, map[string]any{"reviewer_ids": ids}, nil)
}

func refIDs(rs []glRef) []int64 {
	ids := make([]int64, 0, len(rs))
	for _, r := range rs {
		ids = append(ids, r.ID)
	}
	return ids
}

// SetAssignees implements gp.AssigneeSetter: the assignees become exactly logins.
func (p *glProvider) SetAssignees(_ context.Context, req gp.PeopleRequest) (gp.Empty, error) {
	num, err := number(req.Subject.Number)
	if err != nil {
		return gp.Empty{}, err
	}
	seg, _, err := subjectPath(req.Subject.Kind)
	if err != nil {
		return gp.Empty{}, err
	}
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return gp.Empty{}, err
	}
	ids, err := api.userIDs(req.Logins)
	if err != nil {
		return gp.Empty{}, err
	}
	return gp.Empty{}, api.send("PUT", proj+"/"+seg+"/"+num, map[string]any{"assignee_ids": ids}, nil)
}

// MergeChangeRequest implements gp.Merger. GitLab itself refuses (409) when the
// head moved since ExpectedHeadSHA, which is the guarantee core relies on. The
// merge strategy (merge commit, fast-forward) is a project setting in GitLab, so
// "rebase" cannot be requested per merge.
func (p *glProvider) MergeChangeRequest(_ context.Context, req gp.MergeRequest) (gp.MergeResult, error) {
	num, err := number(req.Number)
	if err != nil {
		return gp.MergeResult{}, err
	}
	if blank(req.ExpectedHeadSHA) {
		return gp.MergeResult{}, gp.Errorf(gp.CodeValidation, "expected_head_sha is required")
	}
	body := map[string]any{"sha": req.ExpectedHeadSHA}
	switch req.Method {
	case "", "merge":
	case "squash":
		body["squash"] = true
	case "rebase":
		return gp.MergeResult{}, gp.Errorf(gp.CodeValidation, "GitLab sets rebase and fast-forward merging per project; use merge or squash")
	default:
		return gp.MergeResult{}, gp.Errorf(gp.CodeValidation, "method must be merge or squash")
	}
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return gp.MergeResult{}, err
	}
	var m glMR
	if err := api.send("PUT", proj+"/merge_requests/"+num+"/merge", body, &m); err != nil {
		return gp.MergeResult{}, err
	}
	if st := strings.ToLower(m.State); st != "merged" && st != "locked" {
		return gp.MergeResult{}, gp.Errorf(gp.CodeConflict, "GitLab did not merge the merge request")
	}
	return gp.MergeResult{SHA: firstNonEmpty(m.MergeCommitSHA, m.SquashCommitSHA, m.SHA)}, nil
}

// RerunChecks implements gp.CheckRerunner: one job, or the failed jobs of the
// ref's newest pipeline.
func (p *glProvider) RerunChecks(_ context.Context, req gp.RerunRequest) (gp.Empty, error) {
	api, proj, err := p.apiFor(req.Target)
	if err != nil {
		return gp.Empty{}, err
	}
	if req.CheckID != "" {
		id, ok := strings.CutPrefix(req.CheckID, "job:")
		if !ok || !allDigits(id) {
			return gp.Empty{}, gp.Errorf(gp.CodeNotFound, "only pipeline jobs can be re-run")
		}
		return gp.Empty{}, api.send("POST", proj+"/jobs/"+id+"/retry", map[string]any{}, nil)
	}
	if blank(req.Ref) {
		return gp.Empty{}, gp.Errorf(gp.CodeValidation, "ref is required")
	}
	pid, found, err := ciPipeline(api, proj, req.Ref)
	if err != nil {
		return gp.Empty{}, err
	}
	if !found {
		return gp.Empty{}, gp.Errorf(gp.CodeNotFound, "no pipeline for %s", clip(req.Ref))
	}
	return gp.Empty{}, api.send("POST", proj+"/pipelines/"+strconv.FormatInt(pid, 10)+"/retry", map[string]any{}, nil)
}
