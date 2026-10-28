package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	gp "github.com/Paca-AI/plugin-sdk-go/gitprovider"
)

var bg = context.Background()

func code(t *testing.T, err error, want gp.ErrorCode) {
	t.Helper()
	var e *gp.Error
	if !errors.As(err, &e) || e.Code != want {
		t.Fatalf("error = %v, want code %s", err, want)
	}
}

func completeParams(token string) map[string]string {
	return map[string]string{fieldToken: token, fieldWebhookSecret: fakeWebhookSecret}
}

func beginShared(t *testing.T, p *glProvider) gp.SetupStep {
	t.Helper()
	step, err := p.BeginIdentitySetup(bg, gp.BeginSetupRequest{Scope: gp.SetupShared, Host: "GitLab.com", CallbackURL: "https://paca.test/cb", WebhookURL: "https://paca.test/hook"})
	if err != nil {
		t.Fatal(err)
	}
	return step
}

func TestSetup_TokenFormAsksForTheRightInputs(t *testing.T) {
	p, _, _ := seededProvider(t)
	step := beginShared(t, p)
	if step.Kind != gp.StepTokenForm || step.State == "" || !strings.Contains(step.Prompt, "Maintainer") || strings.Contains(step.Prompt, "https://paca.test/hook") {
		t.Fatalf("step %+v: want a token form whose prompt asks for the Maintainer role and does not send the user to register a webhook by hand", step)
	}
	required := map[string]bool{}
	for _, f := range step.Form {
		required[f.Name] = f.Required
		if !f.Secret {
			t.Errorf("field %s holds a secret but is not marked secret", f.Name)
		}
	}
	if len(required) != 1 || !required[fieldToken] {
		t.Errorf("form fields %v: want only the access token (the webhook secret is generated)", required)
	}
}

func TestSetup_BeginRejectsBadInput(t *testing.T) {
	p, _, _ := seededProvider(t)
	base := gp.BeginSetupRequest{Scope: gp.SetupShared, Host: "gitlab.com", WebhookURL: "https://paca.test/hook"}
	for name, mutate := range map[string]func(*gp.BeginSetupRequest){
		"url as host":     func(r *gp.BeginSetupRequest) { r.Host = "https://gitlab.com" },
		"path in host":    func(r *gp.BeginSetupRequest) { r.Host = "gitlab.com/evil" },
		"empty host":      func(r *gp.BeginSetupRequest) { r.Host = "" },
		"bad scope":       func(r *gp.BeginSetupRequest) { r.Scope = "everyone" },
		"no webhook url":  func(r *gp.BeginSetupRequest) { r.WebhookURL = "" },
		"credentials url": func(r *gp.BeginSetupRequest) { r.Host = "user:pw@gitlab.com" },
	} {
		req := base
		mutate(&req)
		if _, err := p.BeginIdentitySetup(bg, req); err == nil {
			t.Errorf("%s: want an error", name)
		} else {
			code(t, err, gp.CodeValidation)
		}
	}
	req := base
	req.Host = "GitLab.Example.com:8443"
	if _, err := p.BeginIdentitySetup(bg, req); err != nil {
		t.Errorf("a self-hosted host with a port must be accepted: %v", err)
	}
}

func TestSetup_CompleteStoresAnIdentityWithoutLeakingSecrets(t *testing.T) {
	p, g, st := seededProvider(t)
	step := beginShared(t, p)
	params := completeParams(fakeNewToken)
	params[fieldReadToken] = fakeReadToken
	id, err := p.CompleteIdentitySetup(bg, gp.CompleteSetupRequest{Scope: gp.SetupShared, Host: "gitlab.com", State: step.State, Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if id.Ref.Handle != "project_1_bot_ab12" || id.Kind != gp.IdentityBotUser || id.Host != "gitlab.com" {
		t.Errorf("identity %+v: a project access token is a bot user on the session's host", id)
	}
	// The earliest of the two token expiries is the one that matters.
	if want := time.Date(2029, 1, 2, 23, 59, 59, 0, time.UTC); !id.ExpiresAt.Equal(want) {
		t.Errorf("expires at %v, want %v (the read token expires first)", id.ExpiresAt, want)
	}
	stored, err := st.loadIdentity(id.Ref.ID)
	if err != nil || stored.Token != fakeNewToken || stored.ReadToken != fakeReadToken || stored.WebhookSecret != fakeWebhookSecret || stored.UserID != 77 {
		t.Errorf("stored identity %+v, %v", stored, err)
	}
	if g.called("GET /user") != 2 {
		t.Errorf("both tokens should be proven against the instance")
	}
}

func TestSetup_CompleteRefusesWhatItCannotTrust(t *testing.T) {
	p, _, _ := seededProvider(t)
	complete := func(state string, params map[string]string) error {
		_, err := p.CompleteIdentitySetup(bg, gp.CompleteSetupRequest{Scope: gp.SetupShared, Host: "gitlab.com", State: state, Params: params})
		return err
	}
	step := beginShared(t, p)

	code(t, complete(step.State, completeParams("glpat-wrong")), gp.CodeValidation)          // GitLab rejects it
	code(t, complete(step.State, completeParams("glpat-readonly-scope")), gp.CodeValidation) // no api scope
	code(t, complete(step.State, map[string]string{fieldToken: fakeNewToken, fieldWebhookSecret: "short"}), gp.CodeValidation)
	code(t, complete("never-issued", completeParams(fakeNewToken)), gp.CodeForbidden)
	// A rejected attempt does not burn the state: the user can fix the form.
	if err := complete(step.State, completeParams(fakeNewToken)); err != nil {
		t.Fatalf("retry after a rejected token: %v", err)
	}
	// …but a completed one is spent.
	code(t, complete(step.State, completeParams(fakeNewToken)), gp.CodeForbidden)
}

func TestSetup_ExpiredStateIsRefused(t *testing.T) {
	p, _, _ := seededProvider(t)
	step := beginShared(t, p)
	p.now = func() time.Time { return time.Now().Add(2 * setupSessionLife) }
	_, err := p.CompleteIdentitySetup(bg, gp.CompleteSetupRequest{Host: "gitlab.com", State: step.State, Params: completeParams(fakeNewToken)})
	code(t, err, gp.CodeForbidden)
}

func TestSetup_UnreachableHostSaysHowToFixIt(t *testing.T) {
	p, g, _ := seededProvider(t)
	step := beginShared(t, p)
	g.forceError["GET /user"] = fakeResponse{status: 503}
	_, err := p.CompleteIdentitySetup(bg, gp.CompleteSetupRequest{Host: "gitlab.com", State: step.State, Params: completeParams(fakeNewToken)})
	code(t, err, gp.CodeTransient)
}

func TestMintCredential_StaticTokens(t *testing.T) {
	p, _, st := seededProvider(t)
	mint := func(access gp.Access, repo gp.RepoRef) (gp.Credential, error) {
		return p.MintCredential(bg, gp.MintRequest{Target: gp.Target{Identity: fixtureIdentity, Repo: repo}, Access: access})
	}
	read, err := mint(gp.AccessRead, fixtureRepo)
	if err != nil || read.Secret != fakeReadToken || read.Username != credUsername {
		t.Errorf("read credential %+v %v: want the read-only token", read, err)
	}
	write, err := mint(gp.AccessWrite, fixtureRepo)
	if err != nil || write.Secret != fakeToken {
		t.Errorf("write credential %+v %v: want the full token", write, err)
	}
	if write.ExpiresAt.IsZero() {
		t.Error("the token's expiry should be reported")
	}

	// Without a read token, reads use the main token.
	rec := st.identities[fakeIdentityID]
	rec.ReadToken = ""
	st.identities[fakeIdentityID] = rec
	if c, err := mint(gp.AccessRead, fixtureRepo); err != nil || c.Secret != fakeToken {
		t.Errorf("read without a read token: %+v %v", c, err)
	}

	// A Reporter can read a project but may not push to it.
	reporter := gp.RepoRef{Host: fakeHost, FullPath: "acme/api", ExternalID: "2"}
	if _, err := mint(gp.AccessRead, reporter); err != nil {
		t.Errorf("read as a Reporter: %v", err)
	}
	_, err = mint(gp.AccessWrite, reporter)
	code(t, err, gp.CodeForbidden)

	// The repository must be on the identity's host.
	_, err = mint(gp.AccessRead, gp.RepoRef{Host: "gitlab.other.com", FullPath: "acme/web"})
	code(t, err, gp.CodeValidation)
}

func TestProjectAddressing(t *testing.T) {
	p, g, _ := seededProvider(t)
	// Without a numeric id the path is URL-encoded, subgroups included.
	nested := gp.RepoRef{Host: fakeHost, FullPath: "acme/platform/infra"}
	if _, err := p.GetBranchProtection(bg, gp.BranchProtectionRequest{Target: gp.Target{Identity: fixtureIdentity, Repo: nested}, Branch: "main"}); err == nil {
		t.Log("the fake only seeds acme/web in depth; reaching the project is enough")
	}
	if g.called("GET /projects/acme/platform/infra/repository/branches/main") != 1 {
		t.Errorf("calls: %v", g.calls)
	}
	for _, bad := range []string{"web", "acme/../etc", "acme//web", "acme/we b", "/"} {
		_, err := p.GetChangeRequest(bg, gp.CRRequest{Target: gp.Target{Identity: fixtureIdentity, Repo: gp.RepoRef{Host: fakeHost, FullPath: bad}}, Number: "1"})
		code(t, err, gp.CodeValidation)
	}
}

func TestResolveRepository_Inputs(t *testing.T) {
	p, _, _ := seededProvider(t)
	cands := []gp.IdentityRef{fixtureIdentity}
	for _, in := range []string{
		"acme/web", "acme/web.git", "https://gitlab.com/acme/web", "https://gitlab.com/acme/web.git",
		"git@gitlab.com:acme/web.git", "https://gitlab.com/acme/web/-/merge_requests/3", "ssh://git@gitlab.com/acme/web.git", " https://gitlab.com/acme/web/ ",
	} {
		r, err := p.ResolveRepository(bg, gp.ResolveRepoRequest{PathOrURL: in, Candidates: cands})
		if err != nil || r.Repository.Ref.FullPath != "acme/web" || r.Repository.Ref.ExternalID != "1" || r.Identity.ID != fakeIdentityID {
			t.Errorf("%q → %+v %v", in, r, err)
		}
		if got := r.Repository.Ref.CloneURL; got != "https://gitlab.com/acme/web.git" {
			t.Errorf("clone url %q must be token-free", got)
		}
	}
	_, err := p.ResolveRepository(bg, gp.ResolveRepoRequest{PathOrURL: "https://other.example.com/acme/web", Candidates: cands})
	code(t, err, gp.CodeNotFound) // no candidate lives on that host
	_, err = p.ResolveRepository(bg, gp.ResolveRepoRequest{Host: "gitlab.com", PathOrURL: "https://other.example.com/acme/web", Candidates: cands})
	code(t, err, gp.CodeValidation)
	_, err = p.ResolveRepository(bg, gp.ResolveRepoRequest{PathOrURL: "justaname", Candidates: cands})
	code(t, err, gp.CodeValidation)
	_, err = p.ResolveRepository(bg, gp.ResolveRepoRequest{PathOrURL: "secret/hidden", Candidates: cands})
	code(t, err, gp.CodeNotFound)
	_, err = p.ResolveRepository(bg, gp.ResolveRepoRequest{PathOrURL: "acme/web"})
	code(t, err, gp.CodeForbidden)
}

func TestListRepositories_PagesByNumber(t *testing.T) {
	p, _, _ := seededProvider(t)
	page, err := p.ListRepositories(bg, gp.ListReposRequest{Identity: fixtureIdentity, PageSize: 2})
	if err != nil || len(page.Items) != 2 || page.NextCursor != "2" {
		t.Fatalf("first page %+v %v", page, err)
	}
	if !page.Items[1].Private && page.Items[1].Ref.FullPath == "acme/platform/infra" {
		t.Error("a private project must be reported private")
	}
	page, err = p.ListRepositories(bg, gp.ListReposRequest{Identity: fixtureIdentity, PageSize: 2, Cursor: "2"})
	if err != nil || len(page.Items) != 1 || page.NextCursor != "" {
		t.Fatalf("last page %+v %v", page, err)
	}
	_, err = p.ListRepositories(bg, gp.ListReposRequest{Identity: fixtureIdentity, Cursor: "x"})
	code(t, err, gp.CodeValidation)
	found, err := p.ListRepositories(bg, gp.ListReposRequest{Identity: fixtureIdentity, Query: "infra"})
	if err != nil || len(found.Items) != 1 {
		t.Errorf("search: %+v %v", found, err)
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   gp.ErrorCode
	}{
		{401, `{"message":"401 Unauthorized"}`, gp.CodeForbidden},
		{403, `{"message":"403 Forbidden"}`, gp.CodeForbidden},
		{404, `{"message":"404 Project Not Found"}`, gp.CodeNotFound},
		{405, `{"message":"Method Not Allowed"}`, gp.CodeConflict},
		{406, `{"message":"Branch cannot be merged"}`, gp.CodeConflict},
		{409, `{"message":"SHA does not match HEAD of source branch"}`, gp.CodeConflict},
		{400, `{"message":"Branch already exists"}`, gp.CodeConflict},
		{400, `{"message":{"base":["is invalid"]}}`, gp.CodeValidation},
		{400, `{"error":"title is missing"}`, gp.CodeValidation},
		{422, `{"message":"Validation failed"}`, gp.CodeValidation},
		{429, `{"message":"Retry later"}`, gp.CodeRateLimited},
		{502, `<html>Bad gateway</html>`, gp.CodeTransient},
		{418, ``, gp.CodeInternal},
	}
	for _, c := range cases {
		code(t, mapGitLabError(c.status, c.body, nil), c.want)
	}
	e := mapGitLabError(429, "", map[string]string{"Retry-After": "12"}).(*gp.Error)
	if e.RetryAfterSeconds != 12 {
		t.Errorf("retry after %d, want 12", e.RetryAfterSeconds)
	}
	msg := mapGitLabError(400, `{"message":{"base":["is invalid"]}}`, nil).(*gp.Error).Message
	if !strings.Contains(msg, "is invalid") {
		t.Errorf("message %q lost GitLab's explanation", msg)
	}
	long := mapGitLabError(400, `{"message":"`+strings.Repeat("é", 500)+`"}`, nil).(*gp.Error).Message
	if len(long) > maxErrorMessage+len("…") || !strings.HasSuffix(long, "…") {
		t.Errorf("long message not clipped: %d bytes", len(long))
	}
}

func TestRateLimitAndTransientFailuresReachCore(t *testing.T) {
	p, g, _ := seededProvider(t)
	g.forceError["GET /projects/1/merge_requests/1"] = fakeResponse{status: 429, headers: map[string]string{"Retry-After": "30"}}
	_, err := p.GetChangeRequest(bg, gp.CRRequest{Target: fixtureTarget, Number: "1"})
	code(t, err, gp.CodeRateLimited)
	if e := gp.AsError(err); e.RetryAfterSeconds != 30 || !e.Retryable() {
		t.Errorf("%+v", e)
	}
}

func TestVerifyDelivery(t *testing.T) {
	secret := "whsec_" + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	nowTS := strconv.FormatInt(time.Now().Unix(), 10)
	body := []byte(`{"object_kind":"push"}`)
	key, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("msg-1." + nowTS + "." + string(body)))
	sig := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req := func(h map[string]string) gp.WebhookRequest { return gp.WebhookRequest{Body: body, Headers: h} }
	for name, tc := range map[string]struct {
		req  gp.WebhookRequest
		want bool
	}{
		"secret token":            {req(map[string]string{"x-gitlab-token": secret}), true},
		"wrong secret token":      {req(map[string]string{"x-gitlab-token": secret + "x"}), false},
		"standard webhooks":       {req(map[string]string{"webhook-id": "msg-1", "webhook-timestamp": nowTS, "webhook-signature": sig}), true},
		"signature of another id": {req(map[string]string{"webhook-id": "msg-2", "webhook-timestamp": nowTS, "webhook-signature": sig}), false},
		"one of several":          {req(map[string]string{"webhook-id": "msg-1", "webhook-timestamp": nowTS, "webhook-signature": "v1,AAAA " + sig}), true},
		"no credentials":          {req(map[string]string{}), false},
		// A present-but-wrong token must not fall through to the signature check.
		"wrong token with a valid signature": {req(map[string]string{"x-gitlab-token": "nope", "webhook-id": "msg-1", "webhook-timestamp": nowTS, "webhook-signature": sig}), false},
	} {
		if got := verifyDelivery(tc.req, secret); got != tc.want {
			t.Errorf("%s: verified = %v, want %v", name, got, tc.want)
		}
	}
	// A correctly signed but old delivery is a replay.
	oldTS := strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)
	omac := hmac.New(sha256.New, key)
	omac.Write([]byte("msg-1." + oldTS + "." + string(body)))
	osig := "v1," + base64.StdEncoding.EncodeToString(omac.Sum(nil))
	if verifyDelivery(req(map[string]string{"webhook-id": "msg-1", "webhook-timestamp": oldTS, "webhook-signature": osig}), secret) {
		t.Error("a delivery signed an hour ago must be rejected as a replay")
	}
	if verifyDelivery(req(map[string]string{"x-gitlab-token": ""}), "") {
		t.Error("an identity without a secret must verify nothing")
	}
}

func TestWebhook_StandardWebhooksSignedDeliveryParses(t *testing.T) {
	p, _, st := seededProvider(t)
	nowTS := strconv.FormatInt(time.Now().Unix(), 10)
	rec := st.identities[fakeIdentityID]
	rec.WebhookSecret = "whsec_" + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	st.identities[fakeIdentityID] = rec
	req := noteHook("d-sw", "@paca-bot hi", hookActor, nil)
	delete(req.Headers, "x-gitlab-token")
	key, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(rec.WebhookSecret, "whsec_"))
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("msg-9." + nowTS + "." + string(req.Body)))
	req.Headers["webhook-id"], req.Headers["webhook-timestamp"] = "msg-9", nowTS
	req.Headers["webhook-signature"] = "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	evs, err := p.VerifyAndParseWebhook(bg, req)
	if err != nil || len(evs) != 1 || len(evs[0].Addressed) != 1 {
		t.Fatalf("%+v %v", evs, err)
	}
}

func TestWebhook_EventIDs(t *testing.T) {
	p, _, _ := seededProvider(t)
	// One delivery, several events: each id is unique and stable across retries.
	req := mrHook("uuid-1", map[string]any{"action": "open"}, map[string]any{
		"labels": []map[string]any{{"title": "a"}, {"title": "b"}}, "assignees": users(me)})
	a, _ := p.VerifyAndParseWebhook(bg, req)
	b, _ := p.VerifyAndParseWebhook(bg, req)
	seen := map[string]bool{}
	for i, ev := range a {
		if seen[ev.ID] || ev.ID != b[i].ID || !strings.HasPrefix(ev.ID, "uuid-1:") {
			t.Errorf("event ids %q vs %q (duplicate=%v)", ev.ID, b[i].ID, seen[ev.ID])
		}
		seen[ev.ID] = true
	}
	if len(a) != 4 {
		t.Fatalf("got %d events, want opened + 2 labels + assignment", len(a))
	}
	// Two different deliveries of the same kind never share an id.
	other, _ := p.VerifyAndParseWebhook(bg, mrHook("uuid-2", map[string]any{"action": "open"}, nil))
	if other[0].ID == a[0].ID {
		t.Error("distinct deliveries share an event id")
	}
	// An old instance that sends no uuid still gets a body-derived, stable id.
	req = mrHook("", map[string]any{"action": "close"}, nil)
	delete(req.Headers, "x-gitlab-event-uuid")
	x, _ := p.VerifyAndParseWebhook(bg, req)
	y, _ := p.VerifyAndParseWebhook(bg, req)
	if x[0].ID == "" || x[0].ID != y[0].ID {
		t.Errorf("fallback ids %q vs %q", x[0].ID, y[0].ID)
	}
}

func TestWebhook_NoteDetails(t *testing.T) {
	p, _, _ := seededProvider(t)
	req := noteHook("d", "nit", hookActor, map[string]any{"type": "DiffNote", "position": map[string]any{"old_path": "a.go", "old_line": 9}})
	evs, err := p.VerifyAndParseWebhook(bg, req)
	if err != nil || len(evs) != 1 {
		t.Fatal(evs, err)
	}
	ev := evs[0]
	if ev.Location.ThreadID != "disc-1" || ev.Location.Path != "a.go" || ev.Location.Line != 9 || ev.Location.CommentID != "11" {
		t.Errorf("location %+v", ev.Location)
	}
	if ev.Subject.Head == nil || ev.Subject.Head.Ref != "feat/PACA-12-login" || ev.Subject.Head.FromFork || ev.Subject.Base != "main" || ev.Subject.Author != "octocat" {
		t.Errorf("subject %+v", ev.Subject)
	}
	if ev.Subject.URL == "" || ev.Repo.Host != fakeHost || ev.Repo.ExternalID != "1" {
		t.Errorf("subject url / repo: %+v %+v", ev.Subject, ev.Repo)
	}
}

func TestWebhook_ForkMergeRequest(t *testing.T) {
	p, _, _ := seededProvider(t)
	evs, _ := p.VerifyAndParseWebhook(bg, mrHook("d", map[string]any{"action": "open", "source_project_id": 2, "source": map[string]any{"path_with_namespace": "someone/web"}}, nil))
	if len(evs) == 0 || !evs[0].Subject.Head.FromFork || evs[0].Subject.Head.Repo != "someone/web" {
		t.Errorf("%+v", evs)
	}
}

func TestWebhook_MalformedAndUnknown(t *testing.T) {
	p, _, _ := seededProvider(t)
	req := hookDelivery("d", map[string]any{"object_kind": "note"})
	req.Body = []byte(`{not json`)
	_, err := p.VerifyAndParseWebhook(bg, req)
	code(t, err, gp.CodeValidation)
	_, err = p.VerifyAndParseWebhook(bg, gp.WebhookRequest{Identity: gp.IdentityRef{ID: "nope"}})
	code(t, err, gp.CodeNotFound)
	// A project the delivery does not name is dropped, not guessed at.
	evs, err := p.VerifyAndParseWebhook(bg, hookDelivery("d", map[string]any{"object_kind": "push", "ref": "refs/heads/x", "user_username": "u"}))
	if err != nil || len(evs) != 0 {
		t.Errorf("%+v %v", evs, err)
	}
}

func TestPermissionMapping(t *testing.T) {
	for level, want := range map[int]gp.Permission{0: gp.PermissionNone, 5: gp.PermissionNone, 10: gp.PermissionRead, 20: gp.PermissionTriage, 30: gp.PermissionWrite, 40: gp.PermissionMaintain, 50: gp.PermissionAdmin} {
		if got := permissionOf(level); got != want {
			t.Errorf("level %d → %s, want %s", level, got, want)
		}
	}
	p, _, _ := seededProvider(t)
	got, err := p.GetActorPermission(bg, gp.ActorPermissionRequest{Target: fixtureTarget, Login: "bob"})
	if err != nil || got != gp.PermissionNone {
		t.Errorf("a user who is not a member: %s %v", got, err)
	}
}

func TestMentionRegexp(t *testing.T) {
	for text, want := range map[string]bool{
		"@paca-bot":              true,
		"hi @paca-bot, please":   true,
		"(@paca-bot)":            true,
		"ask @paca-bot.":         true,
		"ask @paca-bot.\nthanks": true,
		"@Paca-Bot":              true,
		"@paca-bot-2":            false,
		"@paca-bot_x":            false,
		"@paca-bot.other":        false,
		"dev@paca-bot.example":   false,
		"paca-bot":               false,
		"@@paca-bot":             false,
		"text\n@paca-bot do it":  true,
	} {
		if got := mentionsHandle(text, "paca-bot"); got != want {
			t.Errorf("mentionsHandle(%q) = %v, want %v", text, got, want)
		}
	}
	if mentionsHandle("@anyone", "") {
		t.Error("an empty handle mentions nobody")
	}
}

func TestParseRepoInput(t *testing.T) {
	for in, want := range map[string]struct{ host, path string }{
		"group/sub/proj": {"", "group/sub/proj"},
		"https://gitlab.example.com/group/sub/proj.git": {"gitlab.example.com", "group/sub/proj"},
		"git@gitlab.example.com:group/proj.git":         {"gitlab.example.com", "group/proj"},
		"https://GitLab.com/g/p/-/issues/4":             {"gitlab.com", "g/p"},
	} {
		host, path, err := parseRepoInput(in)
		if err != nil || host != want.host || path != want.path {
			t.Errorf("%q → %q %q %v", in, host, path, err)
		}
	}
	for _, bad := range []string{"", "  ", "x", "http://", "git@nohost", "a/b c", "https://gitlab.com/onlyone"} {
		if _, _, err := parseRepoInput(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestDraftTitles(t *testing.T) {
	for _, tc := range []struct {
		in    string
		draft bool
		want  string
	}{
		{"Fix it", true, "Draft: Fix it"},
		{"Draft: Fix it", true, "Draft: Fix it"},
		{"[Draft] Fix it", true, "Draft: Fix it"},
		{"WIP: Fix it", false, "Fix it"},
		{"Draft: Fix it", false, "Fix it"},
		{"Fix it", false, "Fix it"},
		{"Drafting a plan", false, "Drafting a plan"},
	} {
		if got := withDraft(tc.in, tc.draft); got != tc.want {
			t.Errorf("withDraft(%q, %v) = %q, want %q", tc.in, tc.draft, got, tc.want)
		}
	}
}

func TestCleanTrace(t *testing.T) {
	in := "\x1b[0Ksection_start:1700:build\r\x1b[0K\x1b[32;1mRunning\x1b[0;m\nline\r\nsection_end:1700:build\r\x1b[0K"
	got := cleanTrace(in)
	if got != "Running\nline\n" {
		t.Errorf("cleanTrace = %q", got)
	}
}

func TestCuts(t *testing.T) {
	if s, cut := cutHead("héllo", 2); s != "h" || !cut {
		t.Errorf("cutHead split a rune: %q %v", s, cut)
	}
	if s, cut := cutTail("héllo", 4); s != "llo" || !cut {
		t.Errorf("cutTail split a rune: %q %v", s, cut)
	}
	if got := cutRunes(strings.Repeat("é", 300), maxStatusDescription); len([]rune(got)) != maxStatusDescription {
		t.Errorf("status description has %d runes", len([]rune(got)))
	}
}

func TestStatusRefRoundTrip(t *testing.T) {
	in := statusRef{SHA: fakeHeadSHA, Name: "Paca / dev: fix", Ref: "feat/x"}
	out, ok := decodeStatusRef(in.encode())
	if !ok || out != in {
		t.Errorf("%+v %v", out, ok)
	}
	for _, bad := range []string{"", "###", base64.RawURLEncoding.EncodeToString([]byte(`{}`))} {
		if _, ok := decodeStatusRef(bad); ok {
			t.Errorf("%q should not decode", bad)
		}
	}
}

// Leaving the webhook secret empty is fine: the provider generates one.
func TestSetup_GeneratesTheWebhookSecretWhenLeftEmpty(t *testing.T) {
	p, _, st := seededProvider(t)
	step := beginShared(t, p)
	id, err := p.CompleteIdentitySetup(bg, gp.CompleteSetupRequest{Scope: gp.SetupShared, Host: "gitlab.com", State: step.State,
		Params: map[string]string{fieldToken: fakeNewToken}})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := st.loadIdentity(id.Ref.ID)
	if err != nil || len(stored.WebhookSecret) < minWebhookSecretLen {
		t.Fatalf("stored secret = %q, %v", stored.WebhookSecret, err)
	}
}

// GitLab refuses GET /user to some tokens (no read_user/api scope on a bot
// token); they are accepted when /personal_access_tokens/self names the user.
func TestSetup_TokenThatCannotReadUserIsIdentifiedByItself(t *testing.T) {
	p, g, _ := seededProvider(t)
	g.forceError["GET /user"] = fakeResponse{status: 403, body: `{"message":"403 Forbidden"}`}
	step := beginShared(t, p)
	id, err := p.CompleteIdentitySetup(bg, gp.CompleteSetupRequest{Scope: gp.SetupShared, Host: "gitlab.com", State: step.State, Params: completeParams(fakeNewToken)})
	if err != nil {
		t.Fatal(err)
	}
	if id.Ref.Handle != "project_1_bot_ab12" {
		t.Errorf("identity %+v", id)
	}
}

// A rejection says what GitLab answered, so the cause can be told apart.
func TestSetup_RejectionQuotesGitLab(t *testing.T) {
	p, g, _ := seededProvider(t)
	step := beginShared(t, p)
	_, err := p.CompleteIdentitySetup(bg, gp.CompleteSetupRequest{Scope: gp.SetupShared, Host: "gitlab.com", State: step.State, Params: completeParams("glpat-wrong")})
	code(t, err, gp.CodeValidation)
	if !strings.Contains(err.Error(), "HTTP 401") || !strings.Contains(err.Error(), "401 Unauthorized") {
		t.Errorf("error %q should quote GitLab's answer", err)
	}
	g.forceError["GET /user"] = fakeResponse{status: 404, body: `{"message":"404 Not Found"}`}
	g.forceError["GET /personal_access_tokens/self"] = fakeResponse{status: 404, body: `{"message":"404 Not Found"}`}
	_, err = p.CompleteIdentitySetup(bg, gp.CompleteSetupRequest{Scope: gp.SetupShared, Host: "gitlab.com", State: step.State, Params: completeParams(fakeNewToken)})
	code(t, err, gp.CodeValidation)
	if !strings.Contains(err.Error(), "HTTP 404") || !strings.Contains(err.Error(), "host is the GitLab instance") {
		t.Errorf("error %q should name the status and hint at the host", err)
	}
}

func TestSetup_InsufficientScopeSaysWhichScopeToUse(t *testing.T) {
	p, g, _ := seededProvider(t)
	g.forceError["GET /user"] = fakeResponse{status: 403, body: `{"error":"insufficient_scope","error_description":"scope"}`}
	g.forceError["GET /personal_access_tokens/self"] = fakeResponse{status: 403, body: `{"error":"insufficient_scope"}`}
	step := beginShared(t, p)
	_, err := p.CompleteIdentitySetup(bg, gp.CompleteSetupRequest{Scope: gp.SetupShared, Host: "gitlab.com", State: step.State, Params: completeParams(fakeNewToken)})
	code(t, err, gp.CodeValidation)
	if !strings.Contains(err.Error(), "api scope") {
		t.Errorf("error %q should name the scope to use", err)
	}
}

// brokenLoadStore cannot read identities (a schema out of step with the code)
// but can still delete them.
type brokenLoadStore struct{ *memStore }

func (brokenLoadStore) loadIdentity(string) (identityRecord, error) {
	return identityRecord{}, errors.New("column does not exist")
}

func TestBrokenStore_DeleteStillWorksAndResolveSaysSo(t *testing.T) {
	_, g, st := seededProvider(t)
	_ = g
	p := newGLProvider(brokenLoadStore{st})
	if _, err := p.DeleteIdentity(bg, gp.DeleteIdentityRequest{Identity: gp.IdentityRef{ID: fakeIdentityID}}); err != nil {
		t.Fatalf("delete must not need to read the identity: %v", err)
	}
	if _, ok := st.identities[fakeIdentityID]; ok {
		t.Fatal("the identity should be gone")
	}
	_, err := p.ResolveRepository(bg, gp.ResolveRepoRequest{PathOrURL: "acme/web", Candidates: []gp.IdentityRef{{ID: "x"}}})
	code(t, err, gp.CodeInternal) // not a misleading "no identity can see it"
}
