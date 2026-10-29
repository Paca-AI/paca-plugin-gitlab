package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gp "github.com/Paca-AI/plugin-sdk-go/gitprovider"
	"github.com/Paca-AI/plugin-sdk-go/gitprovider/conformance"
)

// memStore is an in-memory spiStore.
type memStore struct {
	mu         sync.Mutex
	identities map[string]identityRecord
	setups     map[string]setupSession
	seq        int
	// legacyHooks are each identity's community-plugin hooks still to remove.
	legacyHooks map[string][]legacyWebhook
}

func newMemStore() *memStore {
	return &memStore{identities: map[string]identityRecord{}, setups: map[string]setupSession{}, legacyHooks: map[string][]legacyWebhook{}}
}

func (m *memStore) loadIdentity(id string) (identityRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.identities[id]
	if !ok {
		return identityRecord{}, errNoSuchIdentity
	}
	return r, nil
}

func (m *memStore) createIdentity(r identityRecord) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	r.ID = "created-" + strconv.Itoa(m.seq)
	m.identities[r.ID] = r
	return r.ID, nil
}

func (m *memStore) deleteIdentity(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.identities, id)
	return nil
}

func (m *memStore) saveReach(id string, r tokenReach) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.identities[id]
	if !ok {
		return errNoSuchIdentity
	}
	rec.Reach = r
	m.identities[id] = rec
	return nil
}

func (m *memStore) completeIdentity(r identityRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.identities[r.ID]
	if !ok {
		return errNoSuchIdentity
	}
	if rec.Username == "" {
		rec.UserID, rec.Username, rec.Name, rec.Bot, rec.ExpiresAt = r.UserID, r.Username, r.Name, r.Bot, r.ExpiresAt
	}
	if rec.WebhookSecret == "" {
		rec.WebhookSecret = r.WebhookSecret
	}
	m.identities[r.ID] = rec
	return nil
}

func (m *memStore) legacyWebhooks(identityID string, gitlabProjectID int64, fullPath string) ([]legacyWebhook, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []legacyWebhook
	for _, h := range m.legacyHooks[identityID] {
		if (gitlabProjectID > 0 && h.GitLabProjectID == gitlabProjectID) || strings.EqualFold(h.FullPath, fullPath) {
			out = append(out, h)
		}
	}
	return out, nil
}

func (m *memStore) forgetLegacyWebhook(identityID string, webhookID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.legacyHooks[identityID][:0]
	for _, h := range m.legacyHooks[identityID] {
		if h.WebhookID != webhookID {
			kept = append(kept, h)
		}
	}
	m.legacyHooks[identityID] = kept
	return nil
}

func (m *memStore) saveSetup(s setupSession) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setups[s.State] = s
	return nil
}

func (m *memStore) loadSetup(state string) (setupSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.setups[state]
	if !ok {
		return setupSession{}, errNoSuchSetup
	}
	return s, nil
}

func (m *memStore) claimSetup(state string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.setups[state]
	if !ok || s.Completed {
		return false, nil
	}
	s.Completed = true
	m.setups[state] = s
	return true, nil
}

func seededIdentity() identityRecord {
	return identityRecord{
		ID: fakeIdentityID, Host: fakeHost, UserID: 77, Username: fakeUsername, Name: "Paca Bot", Bot: true,
		Token: fakeToken, ReadToken: fakeReadToken, WebhookSecret: fakeWebhookSecret,
		ExpiresAt: time.Now().Add(30 * 24 * time.Hour),
	}
}

// seededProvider returns a provider holding the fixture identity.
func seededProvider(t *testing.T) (*glProvider, *fakeGitLab, *memStore) {
	t.Helper()
	g := newFakeGitLab(t)
	st := newMemStore()
	st.identities[fakeIdentityID] = seededIdentity()
	return newGLProvider(st), g, st
}

var (
	fixtureIdentity = gp.IdentityRef{ID: fakeIdentityID, Handle: fakeUsername}
	fixtureRepo     = gp.RepoRef{Host: fakeHost, FullPath: "acme/web", ExternalID: "1", DefaultBranch: "main"}
	fixtureTarget   = gp.Target{Identity: fixtureIdentity, Repo: fixtureRepo}
)

// hookDelivery builds a GitLab webhook delivery carrying the secret token.
func hookDelivery(id string, payload map[string]any) gp.WebhookRequest {
	body, _ := json.Marshal(payload)
	return gp.WebhookRequest{
		Identity: fixtureIdentity, Method: "POST", Body: body,
		Headers: map[string]string{"x-gitlab-token": fakeWebhookSecret, "x-gitlab-event-uuid": id, "content-type": "application/json"},
	}
}

var (
	hookRepo  = map[string]any{"id": 1, "path_with_namespace": "acme/web", "default_branch": "main", "git_http_url": "https://gitlab.com/acme/web.git"}
	hookActor = map[string]any{"id": 1, "username": "octocat"}
	hookMRAt  = func(extra map[string]any) map[string]any {
		m := map[string]any{
			"id": 31, "iid": 1, "title": "Add login", "description": "", "state": "opened", "url": "https://gitlab.com/acme/web/-/merge_requests/1",
			"source_branch": "feat/PACA-12-login", "target_branch": "main", "author_id": 1, "last_commit": map[string]any{"id": fakeHeadSHA},
			"source": map[string]any{"path_with_namespace": "acme/web"}, "target": map[string]any{"path_with_namespace": "acme/web"},
			"source_project_id": 1, "target_project_id": 1,
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
)

func mrHook(id string, oa map[string]any, extra map[string]any) gp.WebhookRequest {
	p := map[string]any{"object_kind": "merge_request", "user": hookActor, "project": hookRepo, "object_attributes": hookMRAt(oa)}
	for k, v := range extra {
		p[k] = v
	}
	return hookDelivery(id, p)
}

func noteHook(id, body string, user map[string]any, oa map[string]any) gp.WebhookRequest {
	attrs := map[string]any{"id": 11, "note": body, "noteable_type": "MergeRequest", "discussion_id": "disc-1", "action": "create"}
	for k, v := range oa {
		attrs[k] = v
	}
	return hookDelivery(id, map[string]any{"object_kind": "note", "user": user, "project": hookRepo, "object_attributes": attrs, "merge_request": hookMRAt(nil)})
}

func issueNoteHook(id, body string, user map[string]any) gp.WebhookRequest {
	return hookDelivery(id, map[string]any{"object_kind": "note", "user": user, "project": hookRepo,
		"object_attributes": map[string]any{"id": 12, "note": body, "noteable_type": "Issue", "action": "create"},
		"issue":             map[string]any{"iid": 7, "title": "Bug", "author_id": 2}})
}

func issueHook(id string, oa, extra map[string]any) gp.WebhookRequest {
	attrs := map[string]any{"iid": 7, "title": "Bug", "description": "It breaks", "action": "open", "url": "https://gitlab.com/acme/web/-/issues/7", "author_id": 1}
	for k, v := range oa {
		attrs[k] = v
	}
	p := map[string]any{"object_kind": "issue", "user": hookActor, "project": hookRepo, "object_attributes": attrs}
	for k, v := range extra {
		p[k] = v
	}
	return hookDelivery(id, p)
}

var (
	me     = map[string]any{"id": 77, "username": fakeUsername}
	person = map[string]any{"id": 5, "username": "bob"}
)

func users(us ...map[string]any) []map[string]any { return us }

func conformanceFixtures() conformance.Fixtures {
	bad := noteHook("d-bad", "@paca-bot hi", hookActor, nil)
	bad.Headers["x-gitlab-token"] = "not-the-secret"

	mention := []gp.AddressVia{gp.ViaMention}
	return conformance.Fixtures{
		Identity: fixtureIdentity, Repo: fixtureRepo,
		InvisibleRepo: gp.RepoRef{Host: fakeHost, FullPath: "secret/hidden"},
		RepoURL:       "https://gitlab.com/acme/web.git", Candidates: []gp.IdentityRef{fixtureIdentity},
		ExpectedRepoPaths: []string{"acme/web", "acme/api", "acme/platform/infra"},

		ChangeRequest: "1", MissingCR: "999",
		ChangedFiles: []string{"src/main.go", "src/util.go", "docs/guide.md"},
		HeadSHA:      fakeHeadSHA, FailingCheckID: "job:5001",
		IssueNumber: "7", ThreadID: "disc-1",
		Actors: map[string]gp.Permission{"octocat": gp.PermissionWrite, "reader": gp.PermissionRead, "owner": gp.PermissionAdmin, "lead": gp.PermissionMaintain},

		BadSignature: bad,
		Secrets:      []string{fakeWebhookSecret, fakeToken, fakeReadToken, fakeNewToken},
		Webhooks: []conformance.WebhookCase{
			{Name: "mention-in-mr-note", Request: noteHook("d1", "@paca-bot please fix the test", hookActor, nil),
				Want: []conformance.Want{{Kind: gp.EventComment, Action: "created", RepoPath: "acme/web", SubjectKind: gp.SubjectChangeRequest, SubjectNumber: "1", ActorLogin: "octocat", ActorKind: gp.ActorUser, Addressed: mention}}},
			{Name: "mention-is-case-insensitive", Request: noteHook("d1b", "hey @Paca-Bot, look", hookActor, nil),
				Want: []conformance.Want{{Kind: gp.EventComment, Addressed: mention}}},
			{Name: "mention-ending-a-sentence", Request: noteHook("d1c", "ask @paca-bot.", hookActor, nil),
				Want: []conformance.Want{{Kind: gp.EventComment, Addressed: mention}}},
			{Name: "inline-note-keeps-its-thread-and-line", Request: noteHook("d1d", "nit", hookActor, map[string]any{"type": "DiffNote", "position": map[string]any{"new_path": "src/main.go", "new_line": 3}}),
				Want: []conformance.Want{{Kind: gp.EventComment, SubjectNumber: "1"}}},
			{Name: "comment-without-mention-on-an-issue", Request: issueNoteHook("d2", "thanks!", map[string]any{"id": 2, "username": "reader"}),
				Want: []conformance.Want{{Kind: gp.EventComment, SubjectKind: gp.SubjectIssue, SubjectNumber: "7", ActorLogin: "reader", ActorKind: gp.ActorUser}}},
			{Name: "edited-note-is-not-addressed", Request: noteHook("d2b", "@paca-bot now", hookActor, map[string]any{"action": "update"}),
				Want: []conformance.Want{{Kind: gp.EventComment, Action: "edited"}}},
			{Name: "email-address-is-not-a-mention", Request: noteHook("d3", "mail me at dev@paca-bot.example", hookActor, nil),
				Want: []conformance.Want{{Kind: gp.EventComment, ActorLogin: "octocat"}}},
			{Name: "longer-handle-is-not-a-mention", Request: noteHook("d3b", "@paca-bot-2 is another bot", hookActor, nil),
				Want: []conformance.Want{{Kind: gp.EventComment}}},
			{Name: "dotted-handle-is-another-user", Request: noteHook("d3c", "@paca-bot.other check", hookActor, nil),
				Want: []conformance.Want{{Kind: gp.EventComment}}},
			{Name: "access-token-bot-actor", Request: noteHook("d4", "bump", map[string]any{"id": 9, "username": "project_9_bot_ab12cd"}, nil),
				Want: []conformance.Want{{Kind: gp.EventComment, ActorLogin: "project_9_bot_ab12cd", ActorKind: gp.ActorBot}}},
			{Name: "own-comments-are-bot-actions", Request: noteHook("d4b", "done", me, nil),
				Want: []conformance.Want{{Kind: gp.EventComment, ActorLogin: fakeUsername, ActorKind: gp.ActorBot}}},
			{Name: "system-note-is-dropped", Request: noteHook("d4c", "added 1 commit", hookActor, map[string]any{"system": true}), Want: nil},
			{Name: "commit-note-is-dropped", Request: noteHook("d4d", "nice commit", hookActor, map[string]any{"noteable_type": "Commit"}), Want: nil},

			{Name: "ci-failed-on-a-merge-request",
				Request: hookDelivery("d7", map[string]any{"object_kind": "pipeline", "user": map[string]any{"id": 9, "username": "project_9_bot_ab12cd"}, "project": hookRepo,
					"object_attributes": map[string]any{"id": 900, "sha": fakeHeadSHA, "status": "failed", "source": "merge_request_event"},
					"merge_request":     map[string]any{"iid": 1, "source_branch": "feat/PACA-12-login", "target_branch": "main"}}),
				Want: []conformance.Want{{Kind: gp.EventCheck, Action: "completed", CheckConclusion: "failure", SubjectKind: gp.SubjectChangeRequest, SubjectNumber: "1"}}},
			{Name: "ci-passed-without-a-merge-request",
				Request: hookDelivery("d7b", map[string]any{"object_kind": "pipeline", "user": hookActor, "project": hookRepo,
					"object_attributes": map[string]any{"id": 901, "sha": fakeHeadSHA, "status": "success", "source": "push"}}),
				Want: []conformance.Want{{Kind: gp.EventCheck, CheckConclusion: "success"}}},
			{Name: "ci-running-is-dropped",
				Request: hookDelivery("d7c", map[string]any{"object_kind": "pipeline", "user": hookActor, "project": hookRepo,
					"object_attributes": map[string]any{"id": 902, "sha": fakeHeadSHA, "status": "running", "source": "push"}}),
				Want: nil},
			{Name: "external-pipelines-are-dropped",
				Request: hookDelivery("d7d", map[string]any{"object_kind": "pipeline", "user": me, "project": hookRepo,
					"object_attributes": map[string]any{"id": 903, "sha": fakeHeadSHA, "status": "failed", "source": "external"}}),
				Want: nil},

			{Name: "branch-created-by-push",
				Request: hookDelivery("d10", map[string]any{"object_kind": "push", "ref": "refs/heads/feat/PACA-12-x", "before": zeroSHA, "after": "def456", "user_username": "octocat", "project": hookRepo}),
				Want:    []conformance.Want{{Kind: gp.EventPush, Action: "created", ActorLogin: "octocat"}}},
			{Name: "branch-deleted-by-push",
				Request: hookDelivery("d10c", map[string]any{"object_kind": "push", "ref": "refs/heads/old", "before": "abc", "after": zeroSHA, "user_username": "octocat", "project": hookRepo}),
				Want:    []conformance.Want{{Kind: gp.EventPush, Action: "deleted"}}},
			{Name: "tag-push-is-dropped",
				Request: hookDelivery("d10b", map[string]any{"object_kind": "tag_push", "ref": "refs/tags/v1", "after": "def456", "user_username": "octocat", "project": hookRepo}),
				Want:    nil},

			{Name: "mr-opened-with-mention-in-description",
				Request: mrHook("d11", map[string]any{"action": "open", "iid": 3, "description": "@paca-bot can you review?"}, nil),
				Want:    []conformance.Want{{Kind: gp.EventChangeRequest, Action: "opened", SubjectKind: gp.SubjectChangeRequest, SubjectNumber: "3", Addressed: mention}}},
			{Name: "mr-opened-with-labels-and-the-bot-as-assignee-and-reviewer",
				Request: mrHook("d11b", map[string]any{"action": "open"}, map[string]any{
					"labels": []map[string]any{{"title": "paca"}}, "assignees": users(me), "reviewers": users(me, person)}),
				Want: []conformance.Want{
					{Kind: gp.EventChangeRequest, Action: "opened"},
					{Kind: gp.EventLabel, Action: "labeled"},
					{Kind: gp.EventAssignment, Action: "assigned", Addressed: []gp.AddressVia{gp.ViaAssignment}},
					{Kind: gp.EventReviewRequest, Action: "requested", Addressed: []gp.AddressVia{gp.ViaReviewRequest}},
				}},
			{Name: "mr-new-commits", Request: mrHook("d11c", map[string]any{"action": "update", "oldrev": "old1"}, nil),
				Want: []conformance.Want{{Kind: gp.EventChangeRequest, Action: "synchronized"}}},
			{Name: "mr-marked-ready", Request: mrHook("d11d", map[string]any{"action": "update"}, map[string]any{"changes": map[string]any{"draft": map[string]any{"previous": true, "current": false}}}),
				Want: []conformance.Want{{Kind: gp.EventChangeRequest, Action: "ready"}}},
			{Name: "mr-title-edited", Request: mrHook("d11g", map[string]any{"action": "update", "title": "TST-2 fix"}, map[string]any{"changes": map[string]any{"title": map[string]any{"previous": "fix", "current": "TST-2 fix"}}}),
				Want: []conformance.Want{{Kind: gp.EventChangeRequest, Action: "edited", SubjectKind: gp.SubjectChangeRequest}}},
			{Name: "mr-description-edited", Request: mrHook("d11h", map[string]any{"action": "update"}, map[string]any{"changes": map[string]any{"description": map[string]any{"previous": "", "current": "TST-2"}}}),
				Want: []conformance.Want{{Kind: gp.EventChangeRequest, Action: "edited"}}},
			{Name: "mr-without-relevant-changes-is-dropped", Request: mrHook("d11e", map[string]any{"action": "update"}, map[string]any{"changes": map[string]any{"milestone_id": map[string]any{}}}), Want: nil},
			{Name: "mr-merged", Request: mrHook("d11f", map[string]any{"action": "merge", "state": "merged"}, nil),
				Want: []conformance.Want{{Kind: gp.EventChangeRequest, Action: "merged"}}},
			{Name: "mr-approved", Request: mrHook("d12", map[string]any{"action": "approved"}, nil),
				Want: []conformance.Want{{Kind: gp.EventReview, Action: "submitted", SubjectNumber: "1"}}},
			{Name: "review-requested-from-the-bot",
				Request: mrHook("d13", map[string]any{"action": "update"}, map[string]any{"changes": map[string]any{"reviewers": map[string]any{"previous": users(), "current": users(me)}}}),
				Want:    []conformance.Want{{Kind: gp.EventReviewRequest, Action: "requested", SubjectKind: gp.SubjectChangeRequest, SubjectNumber: "1", Addressed: []gp.AddressVia{gp.ViaReviewRequest}}}},
			{Name: "review-requested-again-is-not-new",
				Request: mrHook("d13b", map[string]any{"action": "update"}, map[string]any{"changes": map[string]any{"reviewers": map[string]any{"previous": users(me), "current": users(me, person)}}}),
				Want:    nil},
			{Name: "review-requested-from-a-person",
				Request: mrHook("d15", map[string]any{"action": "update"}, map[string]any{"changes": map[string]any{"reviewers": map[string]any{"previous": users(), "current": users(person)}}}),
				Want:    nil},
			{Name: "mr-assigned-to-the-bot",
				Request: mrHook("d15b", map[string]any{"action": "update"}, map[string]any{"changes": map[string]any{"assignees": map[string]any{"previous": users(), "current": users(me)}}}),
				Want:    []conformance.Want{{Kind: gp.EventAssignment, Action: "assigned", SubjectKind: gp.SubjectChangeRequest, Addressed: []gp.AddressVia{gp.ViaAssignment}}}},
			{Name: "mr-label-added",
				Request: mrHook("d15c", map[string]any{"action": "update"}, map[string]any{"changes": map[string]any{"labels": map[string]any{
					"previous": []map[string]any{{"title": "old"}}, "current": []map[string]any{{"title": "old"}, {"title": "paca"}}}}}),
				Want: []conformance.Want{{Kind: gp.EventLabel, Action: "labeled", SubjectKind: gp.SubjectChangeRequest}}},

			{Name: "issue-opened-with-mention",
				Request: issueHook("d16", map[string]any{"description": "@paca-bot please look"}, nil),
				Want:    []conformance.Want{{Kind: gp.EventComment, Action: "created", SubjectKind: gp.SubjectIssue, SubjectNumber: "7", Addressed: mention}}},
			{Name: "issue-assigned-to-the-bot",
				Request: issueHook("d16b", map[string]any{"action": "update"}, map[string]any{"changes": map[string]any{"assignees": map[string]any{"previous": users(), "current": users(me)}}}),
				Want:    []conformance.Want{{Kind: gp.EventAssignment, Action: "assigned", SubjectKind: gp.SubjectIssue, SubjectNumber: "7", Addressed: []gp.AddressVia{gp.ViaAssignment}}}},
			{Name: "issue-assigned-to-a-person",
				Request: issueHook("d17", map[string]any{"action": "update"}, map[string]any{"changes": map[string]any{"assignees": map[string]any{"previous": users(), "current": users(person)}}}),
				Want:    nil},
			{Name: "issue-label-added",
				Request: issueHook("d18", map[string]any{"action": "update"}, map[string]any{"changes": map[string]any{"labels": map[string]any{"previous": []map[string]any{}, "current": []map[string]any{{"title": "paca"}}}}}),
				Want:    []conformance.Want{{Kind: gp.EventLabel, Action: "labeled", SubjectKind: gp.SubjectIssue, SubjectNumber: "7"}}},
			{Name: "issue-closed-is-dropped", Request: issueHook("d18b", map[string]any{"action": "close"}, nil), Want: nil},
			{Name: "unmapped-event-is-dropped", Request: hookDelivery("d8", map[string]any{"object_kind": "wiki_page", "project": hookRepo, "user": hookActor}), Want: nil},
		},

		SetupHost:           fakeHost,
		SetupCompleteParams: map[string]string{fieldToken: fakeNewToken, fieldWebhookSecret: fakeWebhookSecret},

		MutationsAllowed: true,
		NewBranchHead:    "feat/new", BaseBranch: "main",
		File:        struct{ Ref, Path string }{Ref: "main", Path: "README.md"},
		SearchQuery: "package main",
		Reviewer:    "reader",
	}
}

// TestGitLabProviderConformance runs the SDK's provider conformance suite
// against the GitLab adapter wired to a fake GitLab.
func TestGitLabProviderConformance(t *testing.T) {
	conformance.Run(t, conformance.Harness{
		New: func(t *testing.T) gp.Provider {
			p, _, _ := seededProvider(t)
			return p
		},
		Fixtures: conformanceFixtures(),
	})
}
