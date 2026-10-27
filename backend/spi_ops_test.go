package main

import (
	"strings"
	"testing"

	gp "github.com/Paca-AI/plugin-sdk-go/gitprovider"
)

func TestSetStatus_StatesAndInPlaceUpdates(t *testing.T) {
	p, g, _ := seededProvider(t)
	subj := gp.SubjectRef{Kind: gp.SubjectChangeRequest, Number: "1"}
	var ref string
	for state, want := range map[gp.RunState]string{
		gp.RunQueued: "pending", gp.RunInProgress: "running", gp.RunSuccess: "success", gp.RunFailure: "failed", gp.RunCancelled: "canceled",
	} {
		res, err := p.SetStatus(bg, gp.SetStatusRequest{Target: fixtureTarget, Subject: subj, State: state, Title: "Paca / dev", Summary: "s", URL: "https://paca.test/run/1", StatusRef: ref})
		if err != nil {
			t.Fatalf("%s: %v", state, err)
		}
		ref = res.StatusRef
		last := g.statuses[len(g.statuses)-1]
		if last["state"] != want || last["name"] != "Paca / dev" || last["target_url"] != "https://paca.test/run/1" {
			t.Errorf("%s posted %v, want state %s", state, last, want)
		}
		// Same name, commit and branch: GitLab updates the status in place.
		if last["sha"] != fakeHeadSHA || last["ref"] != "feat/PACA-12-login" {
			t.Errorf("status not tied to the merge request's head and branch: %v", last)
		}
	}
	// A status needs somewhere to go.
	_, err := p.SetStatus(bg, gp.SetStatusRequest{Target: fixtureTarget, State: gp.RunSuccess, Title: "x"})
	code(t, err, gp.CodeValidation)
	_, err = p.SetStatus(bg, gp.SetStatusRequest{Target: fixtureTarget, Subject: subj, State: gp.RunSuccess, Title: "x", StatusRef: "garbage"})
	code(t, err, gp.CodeValidation)
	_, err = p.SetStatus(bg, gp.SetStatusRequest{Target: fixtureTarget, Subject: subj, State: "exploded", Title: "x"})
	code(t, err, gp.CodeValidation)
	long := strings.Repeat("x", 600)
	if _, err := p.SetStatus(bg, gp.SetStatusRequest{Target: fixtureTarget, Subject: subj, State: gp.RunSuccess, Title: "t", Summary: long}); err != nil {
		t.Fatal(err)
	}
	if d := g.statuses[len(g.statuses)-1]["description"].(string); len([]rune(d)) > maxStatusDescription {
		t.Errorf("description is %d runes; GitLab accepts %d", len([]rune(d)), maxStatusDescription)
	}
}

func TestSubmitReview_Verdicts(t *testing.T) {
	p, g, _ := seededProvider(t)
	review := func(v gp.Verdict, summary, head string) (gp.ReviewResult, error) {
		return p.SubmitReview(bg, gp.SubmitReviewRequest{Target: fixtureTarget, Number: "1", Verdict: v, Summary: summary, HeadSHA: head})
	}
	last := func() string { return g.notes[len(g.notes)-1].Body }

	if r, err := review(gp.VerdictApprove, "LGTM", fakeHeadSHA); err != nil || !r.Emulated || r.ID == "" || !strings.Contains(r.URL, "/-/merge_requests/1#note_") {
		t.Fatalf("approve: %+v %v", r, err)
	}
	if len(g.approved) != 1 || last() != "LGTM" {
		t.Errorf("approved %v, last note %q: want the summary and one approval", g.approved, last())
	}
	// An approval without a summary posts no empty note.
	before := len(g.notes)
	if _, err := review(gp.VerdictApprove, "", ""); err != nil || len(g.notes) != before || len(g.approved) != 2 {
		t.Errorf("silent approval: notes %d→%d approvals %v err %v", before, len(g.notes), g.approved, err)
	}
	if _, err := review(gp.VerdictRequestChanges, "fix the nil check", ""); err != nil || last() != "**Changes requested**\n\nfix the nil check" || len(g.approved) != 2 {
		t.Errorf("request changes: note %q approvals %v err %v", last(), g.approved, err)
	}
	if _, err := review(gp.VerdictRequestChanges, "", ""); err != nil || last() != "Changes requested." {
		t.Errorf("request changes without a summary: note %q err %v", last(), err)
	}
	// Nothing to say is not a review.
	_, err := review(gp.VerdictComment, "  ", "")
	code(t, err, gp.CodeValidation)
	// The reviewer looked at another commit than the merge request now has.
	_, err = review(gp.VerdictApprove, "LGTM", "0ld")
	code(t, err, gp.CodeConflict)
	if len(g.approved) != 2 {
		t.Error("a stale approval must not be recorded")
	}
}

func TestSubmitReview_InlineCommentsPinToTheDiff(t *testing.T) {
	p, g, _ := seededProvider(t)
	res, err := p.SubmitReview(bg, gp.SubmitReviewRequest{Target: fixtureTarget, Number: "1", Verdict: gp.VerdictComment, Summary: "see inline", Comments: []gp.InlineComment{
		{Path: "src/main.go", Line: 2, Body: "new side"},
		{Path: "src/util.go", Line: 1, Side: "old", Body: "old side"},
	}})
	if err != nil || res.Posted != 2 || !res.Emulated {
		t.Fatalf("%+v %v", res, err)
	}
	posts := 0
	for _, c := range g.calls {
		if c == "POST /projects/1/merge_requests/1/discussions" {
			posts++
		}
	}
	if posts != 2 {
		t.Errorf("%d discussions created, want one per inline comment", posts)
	}
	// A path outside the diff fails before anything is posted.
	n := len(g.calls)
	_, err = p.SubmitReview(bg, gp.SubmitReviewRequest{Target: fixtureTarget, Number: "1", Verdict: gp.VerdictComment, Comments: []gp.InlineComment{
		{Path: "src/main.go", Line: 1, Body: "ok"}, {Path: "nope.go", Line: 1, Body: "bad"},
	}})
	code(t, err, gp.CodeValidation)
	for _, c := range g.calls[n:] {
		if strings.HasPrefix(c, "POST") {
			t.Errorf("posted %q although a comment was invalid", c)
		}
	}
	// A line GitLab refuses reports how far it got, with GitLab's code.
	g.forceError["POST /projects/1/merge_requests/1/discussions"] = fakeResponse{status: 400, body: `{"message":"line_code can't be blank"}`}
	_, err = p.SubmitReview(bg, gp.SubmitReviewRequest{Target: fixtureTarget, Number: "1", Verdict: gp.VerdictComment, Comments: []gp.InlineComment{{Path: "src/main.go", Line: 99, Body: "x"}}})
	code(t, err, gp.CodeValidation)
	if !strings.Contains(err.Error(), "posted 0 of 1") {
		t.Errorf("error %q should say how many comments were posted", err)
	}
}

func TestOpenAndUpdateChangeRequest(t *testing.T) {
	p, g, _ := seededProvider(t)
	cr, err := p.OpenChangeRequest(bg, gp.OpenCRRequest{Target: fixtureTarget, Title: "New", Head: "feat/new", Base: "main", Draft: true, Labels: []string{"paca", "bug"}, Reviewers: []string{"reader", "ghost-user"}})
	if err != nil {
		t.Fatal(err)
	}
	if cr.Title != "Draft: New" || !cr.Draft || len(cr.Labels) != 2 || cr.Head.Ref != "feat/new" || cr.Base.Ref != "main" || cr.Head.FromFork {
		t.Errorf("%+v", cr)
	}
	if len(cr.Reviewers) != 0 {
		t.Errorf("reviewers %v: an unknown reviewer must not fail the open, and nobody is applied then", cr.Reviewers)
	}
	cr2, err := p.OpenChangeRequest(bg, gp.OpenCRRequest{Target: fixtureTarget, Title: "Other", Head: "draft-branch", Base: "main", Reviewers: []string{"reader"}})
	if err != nil || len(cr2.Reviewers) != 1 || cr2.Reviewers[0] != "reader" {
		t.Errorf("%+v %v", cr2, err)
	}

	// Toggling draft rewrites the title prefix and keeps the rest.
	off := false
	up, err := p.UpdateChangeRequest(bg, gp.UpdateCRRequest{Target: fixtureTarget, Number: cr.Number, Draft: &off})
	if err != nil || up.Draft || up.Title != "New" {
		t.Errorf("mark ready: %+v %v", up, err)
	}
	title := "Renamed"
	on := true
	up, err = p.UpdateChangeRequest(bg, gp.UpdateCRRequest{Target: fixtureTarget, Number: cr.Number, Title: &title, Draft: &on})
	if err != nil || up.Title != "Draft: Renamed" {
		t.Errorf("rename and draft: %+v %v", up, err)
	}
	closed := gp.CRClosed
	up, err = p.UpdateChangeRequest(bg, gp.UpdateCRRequest{Target: fixtureTarget, Number: cr.Number, State: &closed})
	if err != nil || up.State != gp.CRClosed {
		t.Errorf("close: %+v %v", up, err)
	}
	reopened := gp.CROpen
	if up, err = p.UpdateChangeRequest(bg, gp.UpdateCRRequest{Target: fixtureTarget, Number: cr.Number, State: &reopened}); err != nil || up.State != gp.CROpen {
		t.Errorf("reopen: %+v %v", up, err)
	}
	empty := " "
	_, err = p.UpdateChangeRequest(bg, gp.UpdateCRRequest{Target: fixtureTarget, Number: cr.Number, Title: &empty})
	code(t, err, gp.CodeValidation)
	// No changes is a read, not a write.
	n := g.called("PUT")
	if _, err := p.UpdateChangeRequest(bg, gp.UpdateCRRequest{Target: fixtureTarget, Number: "1"}); err != nil || g.called("PUT") != n {
		t.Errorf("an empty update must not write: %v", err)
	}
}

func TestRequestReviewers_KeepsExistingReviewers(t *testing.T) {
	p, g, _ := seededProvider(t)
	subj := gp.SubjectRef{Kind: gp.SubjectChangeRequest, Number: "1"}
	for _, who := range []string{"reader", "octocat", "reader"} {
		if _, err := p.RequestReviewers(bg, gp.PeopleRequest{Target: fixtureTarget, Subject: subj, Logins: []string{who}}); err != nil {
			t.Fatal(err)
		}
	}
	cr, _ := p.GetChangeRequest(bg, gp.CRRequest{Target: fixtureTarget, Number: "1"})
	if strings.Join(cr.Reviewers, ",") != "reader,octocat" {
		t.Errorf("reviewers %v: GitLab replaces the list, so earlier reviewers must be carried over once", cr.Reviewers)
	}
	_, err := p.RequestReviewers(bg, gp.PeopleRequest{Target: fixtureTarget, Subject: subj, Logins: []string{"ghost-user"}})
	code(t, err, gp.CodeValidation)
	_, err = p.RequestReviewers(bg, gp.PeopleRequest{Target: fixtureTarget, Subject: gp.SubjectRef{Kind: gp.SubjectIssue, Number: "7"}, Logins: []string{"reader"}})
	code(t, err, gp.CodeValidation)
	_ = g
}

func TestMerge_Methods(t *testing.T) {
	p, g, _ := seededProvider(t)
	_, err := p.MergeChangeRequest(bg, gp.MergeRequest{Target: fixtureTarget, Number: "1", ExpectedHeadSHA: fakeHeadSHA, Method: "rebase"})
	code(t, err, gp.CodeValidation)
	_, err = p.MergeChangeRequest(bg, gp.MergeRequest{Target: fixtureTarget, Number: "1", ExpectedHeadSHA: fakeHeadSHA, Method: "octopus"})
	code(t, err, gp.CodeValidation)
	if g.mrs[1].State != "opened" {
		t.Fatal("a refused merge must not merge")
	}
	res, err := p.MergeChangeRequest(bg, gp.MergeRequest{Target: fixtureTarget, Number: "1", ExpectedHeadSHA: fakeHeadSHA})
	if err != nil || res.SHA != "merge-sha" || g.mrs[1].State != "merged" {
		t.Errorf("%+v %v", res, err)
	}
	// A merged request cannot be merged again.
	g.forceError["PUT /projects/1/merge_requests/1/merge"] = fakeResponse{status: 405, body: `{"message":"405 Method Not Allowed"}`}
	_, err = p.MergeChangeRequest(bg, gp.MergeRequest{Target: fixtureTarget, Number: "1", ExpectedHeadSHA: fakeHeadSHA})
	code(t, err, gp.CodeConflict)
}

func TestGetChecks(t *testing.T) {
	p, g, _ := seededProvider(t)
	checks, err := p.GetChecks(bg, gp.ChecksRequest{Target: fixtureTarget, Ref: fakeHeadSHA})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]gp.Check{}
	for _, c := range checks {
		byID[c.ID] = c
	}
	if len(checks) != 3 {
		t.Fatalf("%+v: want two jobs and one external status (the job's own status is the same check)", checks)
	}
	if c := byID["job:5001"]; c.State != gp.CheckFailure || !c.Required || c.Summary != "script_failure" || c.Name != "test" {
		t.Errorf("failed job %+v", c)
	}
	if c := byID["job:5002"]; c.State != gp.CheckSuccess || c.Required {
		t.Errorf("an allow_failure job is not required: %+v", c)
	}
	if c := byID["status:ci/external"]; c.State != gp.CheckSuccess || c.URL != "https://ci.example/1" {
		t.Errorf("external status %+v", c)
	}
	// The external pipeline (id 950) holds statuses, not jobs: its jobs are never asked for.
	if g.called("GET /projects/1/pipelines/950") != 0 {
		t.Error("jobs of an external pipeline were requested")
	}
	// A branch name works as a ref, via the ref filter rather than sha.
	if c, err := p.GetChecks(bg, gp.ChecksRequest{Target: fixtureTarget, Ref: "feat/PACA-12-login"}); err != nil || len(c) == 0 {
		t.Errorf("by branch: %v %v", c, err)
	}
	_, err = p.GetChecks(bg, gp.ChecksRequest{Target: fixtureTarget})
	code(t, err, gp.CodeValidation)
	if c, err := p.GetChecks(bg, gp.ChecksRequest{Target: fixtureTarget, Ref: strings.Repeat("0", 40)}); err != nil || len(c) != 1 {
		t.Errorf("a commit without a pipeline still has its statuses: %v %v", c, err)
	}
}

func TestGetCheckLogs_JobsOnly(t *testing.T) {
	p, _, _ := seededProvider(t)
	logs, err := p.GetCheckLogs(bg, gp.CheckLogsRequest{Target: fixtureTarget, CheckID: "job:5001"})
	if err != nil || logs.Text != "FAIL TestLogin\nexpected 1, got 2\n" || logs.Truncated {
		t.Errorf("%q %v %v: terminal noise should be stripped", logs.Text, logs.Truncated, err)
	}
	for _, id := range []string{"status:ci/external", "job:", "job:abc", "job:../x", "5001"} {
		_, err := p.GetCheckLogs(bg, gp.CheckLogsRequest{Target: fixtureTarget, CheckID: id})
		code(t, err, gp.CodeNotFound)
	}
	_, err = p.GetCheckLogs(bg, gp.CheckLogsRequest{Target: fixtureTarget, CheckID: "job:7777"})
	code(t, err, gp.CodeNotFound)
}

func TestRerunChecks(t *testing.T) {
	p, g, _ := seededProvider(t)
	if _, err := p.RerunChecks(bg, gp.RerunRequest{Target: fixtureTarget, CheckID: "job:5001"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.RerunChecks(bg, gp.RerunRequest{Target: fixtureTarget, Ref: fakeHeadSHA}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(g.retried, ",") != "job:5001,pipeline:900" {
		t.Errorf("retried %v: want the job, then the newest CI pipeline (not the external one)", g.retried)
	}
	_, err := p.RerunChecks(bg, gp.RerunRequest{Target: fixtureTarget, CheckID: "status:ci/external"})
	code(t, err, gp.CodeNotFound)
	_, err = p.RerunChecks(bg, gp.RerunRequest{Target: fixtureTarget, Ref: strings.Repeat("0", 40)})
	code(t, err, gp.CodeNotFound)
	_, err = p.RerunChecks(bg, gp.RerunRequest{Target: fixtureTarget})
	code(t, err, gp.CodeValidation)
}

func TestReads_FiltersAndShapes(t *testing.T) {
	p, _, _ := seededProvider(t)

	threads, err := p.ListThreads(bg, gp.PagedCRRequest{Target: fixtureTarget, Number: "1"})
	if err != nil || len(threads.Items) != 1 {
		t.Fatalf("%+v %v: plain conversation comments are not threads", threads, err)
	}
	th := threads.Items[0]
	if th.ID != "disc-1" || th.Path != "src/main.go" || th.Line != 3 || th.Resolved || len(th.Comments) != 1 {
		t.Errorf("thread %+v: system notes must be left out", th)
	}
	if c := th.Comments[0]; c.URL != "https://gitlab.com/acme/web/-/merge_requests/1#note_8101" || c.AuthorKind != gp.ActorUser {
		t.Errorf("comment %+v", c)
	}

	comments, err := p.ListComments(bg, gp.ListCommentsRequest{Target: fixtureTarget, Subject: gp.SubjectRef{Kind: gp.SubjectChangeRequest, Number: "1"}})
	if err != nil || len(comments.Items) != 1 || comments.Items[0].Body != "first" {
		t.Errorf("%+v %v: activity notes and review-thread notes belong elsewhere", comments, err)
	}
	_, err = p.ListComments(bg, gp.ListCommentsRequest{Target: fixtureTarget, Subject: gp.SubjectRef{Kind: "commit", Number: "1"}})
	code(t, err, gp.CodeValidation)

	files, err := p.ListChangedFiles(bg, gp.PagedCRRequest{Target: fixtureTarget, Number: "1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files.Items {
		if f.Status != "modified" || f.Additions != 1 || f.Deletions != 0 {
			t.Errorf("file %+v: one added line each", f)
		}
	}

	diff, err := p.GetDiff(bg, gp.DiffRequest{Target: fixtureTarget, Number: "1", Path: "src/main.go"})
	want := "diff --git a/src/main.go b/src/main.go\n--- a/src/main.go\n+++ b/src/main.go\n@@ -1,3 +1,4 @@\n package main\n+// hi\n"
	if err != nil || diff.Text != want || diff.Truncated {
		t.Errorf("per-file diff %q %v", diff.Text, err)
	}

	is, err := p.GetIssue(bg, gp.IssueRequest{Target: fixtureTarget, Number: "7"})
	if err != nil || is.State != "open" || is.Author != "reader" || len(is.Assignees) != 1 || is.Labels[0] != "bug" {
		t.Errorf("%+v %v", is, err)
	}

	cr, err := p.GetChangeRequest(bg, gp.CRRequest{Target: fixtureTarget, Number: "1"})
	if err != nil || cr.State != gp.CROpen || cr.Head.SHA != fakeHeadSHA || cr.Head.Repo != "acme/web" || cr.Base.Repo != "acme/web" || cr.Head.FromFork {
		t.Errorf("%+v %v", cr, err)
	}
	for _, bad := range []string{"", "x", "-1", "1/../2"} {
		_, err := p.GetChangeRequest(bg, gp.CRRequest{Target: fixtureTarget, Number: bad})
		code(t, err, gp.CodeValidation)
	}
}

func TestOptionalOps_ArgumentChecks(t *testing.T) {
	p, g, _ := seededProvider(t)
	subj := gp.SubjectRef{Kind: gp.SubjectChangeRequest, Number: "1"}
	_, err := p.AddLabels(bg, gp.LabelsRequest{Target: fixtureTarget, Subject: subj})
	code(t, err, gp.CodeValidation)
	_, err = p.SetAssignees(bg, gp.PeopleRequest{Target: fixtureTarget, Subject: subj, Logins: []string{"ghost-user"}})
	code(t, err, gp.CodeValidation)
	if _, err := p.SetAssignees(bg, gp.PeopleRequest{Target: fixtureTarget, Subject: gp.SubjectRef{Kind: gp.SubjectIssue, Number: "7"}}); err != nil {
		t.Errorf("clearing the assignees: %v", err)
	}
	_, err = p.ResolveThread(bg, gp.ResolveThreadRequest{Target: fixtureTarget, Number: "1"})
	code(t, err, gp.CodeValidation)
	if _, err := p.GetFile(bg, gp.FileRequest{Target: fixtureTarget, Path: "../etc/passwd"}); err == nil {
		t.Error("a path that climbs out must be refused")
	}
	// The default branch stands in for an empty ref.
	if f, err := p.GetFile(bg, gp.FileRequest{Target: fixtureTarget, Path: "README.md"}); err != nil || string(f.Content) != "# Readme\n" || f.SHA != "blob1" {
		t.Errorf("%+v %v", f, err)
	}
	_, err = p.GetFile(bg, gp.FileRequest{Target: gp.Target{Identity: fixtureIdentity, Repo: gp.RepoRef{Host: fakeHost, FullPath: "acme/web", ExternalID: "1"}}, Path: "README.md"})
	code(t, err, gp.CodeValidation)
	_, err = p.SearchCode(bg, gp.CodeSearchRequest{Target: fixtureTarget, Query: " "})
	code(t, err, gp.CodeValidation)
	_ = g
}

func TestWebhookManagement_UpdatesInsteadOfDuplicating(t *testing.T) {
	p, g, st := seededProvider(t)
	req := gp.EnsureWebhookRequest{Target: fixtureTarget, URL: "https://paca.test/api/v1/git/webhooks/abc"}
	a, err := p.EnsureWebhook(bg, req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := p.EnsureWebhook(bg, req)
	other, _ := p.EnsureWebhook(bg, gp.EnsureWebhookRequest{Target: fixtureTarget, URL: "https://paca.test/api/v1/git/webhooks/def"})
	if a.ID != b.ID || other.ID == a.ID || len(g.hooks) != 2 {
		t.Errorf("ids %q %q %q, %d hooks: a URL registers once", a.ID, b.ID, other.ID, len(g.hooks))
	}
	if g.called("PUT /projects/1/hooks/") != 1 {
		t.Error("an existing hook should be refreshed so its secret and events are right")
	}
	for _, bad := range []string{"", "ftp://x", "not a url", "https://"} {
		_, err := p.EnsureWebhook(bg, gp.EnsureWebhookRequest{Target: fixtureTarget, URL: bad})
		code(t, err, gp.CodeValidation)
	}
	_, err = p.RemoveWebhook(bg, gp.RemoveWebhookRequest{Target: fixtureTarget, Webhook: gp.WebhookRef{ID: "x"}})
	code(t, err, gp.CodeValidation)

	// A Developer cannot register hooks: say so, so the URL and secret can be pasted by hand.
	g.forceError["POST /projects/1/hooks"] = fakeResponse{status: 403, body: `{"message":"403 Forbidden"}`}
	_, err = p.EnsureWebhook(bg, gp.EnsureWebhookRequest{Target: fixtureTarget, URL: "https://paca.test/api/v1/git/webhooks/ghi"})
	code(t, err, gp.CodeForbidden)

	rec := st.identities[fakeIdentityID]
	rec.WebhookSecret = ""
	st.identities[fakeIdentityID] = rec
	_, err = p.EnsureWebhook(bg, req)
	code(t, err, gp.CodeConflict)
}

func TestDeleteIdentity_ForgetsTheTokens(t *testing.T) {
	p, _, st := seededProvider(t)
	if _, err := p.DeleteIdentity(bg, gp.DeleteIdentityRequest{Identity: fixtureIdentity}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.loadIdentity(fakeIdentityID); err != errNoSuchIdentity {
		t.Errorf("identity still stored: %v", err)
	}
	if _, err := p.DeleteIdentity(bg, gp.DeleteIdentityRequest{Identity: fixtureIdentity}); err != nil {
		t.Errorf("deleting twice must be a no-op: %v", err)
	}
	_, err := p.MintCredential(bg, gp.MintRequest{Target: fixtureTarget, Access: gp.AccessRead})
	code(t, err, gp.CodeNotFound)
}

func TestWebhook_TagRefInAPushEventIsDropped(t *testing.T) {
	p, _, _ := seededProvider(t)
	evs, err := p.VerifyAndParseWebhook(bg, hookDelivery("d", map[string]any{"object_kind": "push", "ref": "refs/tags/v1", "before": zeroSHA, "after": "abc", "user_username": "octocat", "project": hookRepo}))
	if err != nil || len(evs) != 0 {
		t.Errorf("%+v %v", evs, err)
	}
}
