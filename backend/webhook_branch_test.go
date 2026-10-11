package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Paca-AI/plugin-sdk-go/plugintest"
)

func TestExtractBranchTaskRef(t *testing.T) {
	cases := []struct {
		branch string
		prefix string
		number int64
		ok     bool
	}{
		// Hyphen form (UI default with prefix)
		{"feat/SKUI-1-prova", "SKUI", 1, true},
		{"feat/skui-1-prova", "SKUI", 1, true},
		{"SKUI-42", "SKUI", 42, true},
		{"chore/PROJ-99-add-thing", "PROJ", 99, true},
		{"feat/SKUI-1", "SKUI", 1, true},
		{"fix/ABC1-2-hotfix", "ABC1", 2, true},

		// Slash form
		{"feat/SKUI/1-prova", "SKUI", 1, true},
		{"SKUI/7", "SKUI", 7, true},
		{"chore/PROJ/12-docs", "PROJ", 12, true},

		// Number-only (UI when project has no task_id_prefix)
		{"feat/1-prova", "", 1, true},
		{"feat/42", "", 42, true},
		{"1-slug", "", 1, true},
		{"fix/3-hotfix", "", 3, true},

		// Non-matches
		{"main", "", 0, false},
		{"feat/add-login", "", 0, false},
		{"feat/SKUI-", "", 0, false},
		{"feat/-1-x", "", 0, false},
		{"feature/9-x", "", 9, true}, // conventional type + number-only
	}
	for _, tc := range cases {
		prefix, n, ok := extractBranchTaskRef(tc.branch)
		if ok != tc.ok || prefix != tc.prefix || n != tc.number {
			t.Errorf("extractBranchTaskRef(%q) = (%q, %d, %v); want (%q, %d, %v)",
				tc.branch, prefix, n, ok, tc.prefix, tc.number, tc.ok)
		}
	}
}

func TestIsNewBranchPush(t *testing.T) {
	zeros := strings.Repeat("0", 40)
	sha := "abc123def456789012345678901234567890abcd"

	if !isNewBranchPush(false, zeros, sha) {
		t.Error("GitLab new branch (before=zeros) should be created")
	}
	if !isNewBranchPush(true, sha, sha) {
		t.Error("created=true should be created")
	}
	if !isNewBranchPush(false, "", sha) {
		t.Error("empty before should be treated as created")
	}
	if isNewBranchPush(false, sha, sha) {
		t.Error("normal push on existing branch should not be created")
	}
	if isNewBranchPush(false, zeros, zeros) {
		t.Error("branch delete (after=zeros) should not be created")
	}
	if isNewBranchPush(true, zeros, zeros) {
		t.Error("delete must win even if created=true")
	}
}

func TestHandlePushEvent_AutoLinksPrefixedBranch(t *testing.T) {
	tc := setupPlugin(t)
	seedProjectAndTask(t, tc, "SKUI", int64(1))
	seedEncryptedRepo(t, tc, "repo-1", testProjectID, "gorlix/documentazione-vault", "push-secret")

	payload := pushPayload("feat/SKUI-1-auto-link-smoke", true)
	res := tc.Call("POST", "/webhook", signedPushWebhookRequest(testProjectID, payload, "push-secret"))
	if res.StatusCode != 204 {
		t.Fatalf("expected 204, got %d: %s", res.StatusCode, res.BodyString())
	}
	if !tc.Log.HasMessage("branch auto-linked to task") {
		t.Fatalf("expected auto-link log; entries: %+v", tc.Log.Entries())
	}
	rows := tc.DB.AllRows("gitlab_task_branches")
	if len(rows) != 1 {
		t.Fatalf("expected 1 linked branch, got %d", len(rows))
	}
}

func TestHandlePushEvent_AutoLinksSlashPrefixedBranch(t *testing.T) {
	tc := setupPlugin(t)
	seedProjectAndTask(t, tc, "SKUI", int64(1))
	seedEncryptedRepo(t, tc, "repo-1", testProjectID, "gorlix/documentazione-vault", "push-secret")

	payload := pushPayload("feat/SKUI/1-slash-form", true)
	res := tc.Call("POST", "/webhook", signedPushWebhookRequest(testProjectID, payload, "push-secret"))
	if res.StatusCode != 204 {
		t.Fatalf("expected 204, got %d", res.StatusCode)
	}
	if !tc.Log.HasMessage("branch auto-linked to task") {
		t.Fatalf("expected auto-link for slash form; entries: %+v", tc.Log.Entries())
	}
}

func TestHandlePushEvent_AutoLinksNumberOnlyWhenNoPrefix(t *testing.T) {
	tc := setupPlugin(t)
	seedProjectAndTask(t, tc, "", int64(7))
	seedEncryptedRepo(t, tc, "repo-1", testProjectID, "gorlix/documentazione-vault", "push-secret")

	payload := pushPayload("feat/7-no-prefix", true)
	res := tc.Call("POST", "/webhook", signedPushWebhookRequest(testProjectID, payload, "push-secret"))
	if res.StatusCode != 204 {
		t.Fatalf("expected 204, got %d", res.StatusCode)
	}
	if !tc.Log.HasMessage("branch auto-linked to task") {
		t.Fatalf("expected number-only auto-link; entries: %+v", tc.Log.Entries())
	}
}

func TestHandlePushEvent_IgnoresNumberOnlyWhenProjectHasPrefix(t *testing.T) {
	tc := setupPlugin(t)
	seedProjectAndTask(t, tc, "SKUI", int64(1))
	seedEncryptedRepo(t, tc, "repo-1", testProjectID, "gorlix/documentazione-vault", "push-secret")

	payload := pushPayload("feat/1-should-not-link", true)
	res := tc.Call("POST", "/webhook", signedPushWebhookRequest(testProjectID, payload, "push-secret"))
	if res.StatusCode != 204 {
		t.Fatalf("expected 204, got %d", res.StatusCode)
	}
	if tc.Log.HasMessage("branch auto-linked to task") {
		t.Fatal("number-only branch must not auto-link when project has a prefix")
	}
	if len(tc.DB.AllRows("gitlab_task_branches")) != 0 {
		t.Fatal("expected no branch links")
	}
}

func TestHandlePushEvent_IgnoresExistingBranchPush(t *testing.T) {
	tc := setupPlugin(t)
	seedProjectAndTask(t, tc, "SKUI", int64(1))
	seedEncryptedRepo(t, tc, "repo-1", testProjectID, "gorlix/documentazione-vault", "push-secret")

	payload := pushPayload("feat/SKUI-1-existing", false)
	res := tc.Call("POST", "/webhook", signedPushWebhookRequest(testProjectID, payload, "push-secret"))
	if res.StatusCode != 204 {
		t.Fatalf("expected 204, got %d", res.StatusCode)
	}
	if tc.Log.HasMessage("branch auto-linked to task") {
		t.Fatal("push to existing branch must not auto-link")
	}
}

func TestHandlePushEvent_MismatchedPrefixIgnored(t *testing.T) {
	tc := setupPlugin(t)
	seedProjectAndTask(t, tc, "SKUI", int64(1))
	seedEncryptedRepo(t, tc, "repo-1", testProjectID, "gorlix/documentazione-vault", "push-secret")

	payload := pushPayload("feat/OTHER-1-nope", true)
	res := tc.Call("POST", "/webhook", signedPushWebhookRequest(testProjectID, payload, "push-secret"))
	if res.StatusCode != 204 {
		t.Fatalf("expected 204, got %d", res.StatusCode)
	}
	if tc.Log.HasMessage("branch auto-linked to task") {
		t.Fatal("mismatched prefix must not auto-link")
	}
}

func seedProjectAndTask(t *testing.T, tc *plugintest.Context, taskIDPrefix string, taskNumber int64) {
	t.Helper()
	tc.DB.SeedRows("projects",
		[]string{"id", "task_id_prefix"},
		[][]any{{testProjectID, taskIDPrefix}})
	// Replace tasks seed from setupPlugin with columns needed for lookup.
	tc.DB.SeedRows("tasks",
		[]string{"id", "project_id", "task_number", "deleted_at"},
		[][]any{{testTaskID, testProjectID, taskNumber, nil}})
}

func pushPayload(branch string, isNew bool) []byte {
	before := "abc123def456789012345678901234567890abcd"
	if isNew {
		before = strings.Repeat("0", 40)
	}
	body := fmt.Sprintf(`{
		"object_kind":"push",
		"ref":"refs/heads/%s",
		"before":"%s",
		"after":"def456abc789012345678901234567890abcdef0",
		"project":{"path_with_namespace":"gorlix/documentazione-vault"}
	}`, branch, before)
	return []byte(body)
}

func signedPushWebhookRequest(projectID string, payload []byte, signingToken string) plugintest.Request {
	req := signedWebhookRequest(projectID, payload, signingToken)
	req.Headers["X-Gitlab-Event"] = "Push Hook"
	return req
}

// Ensure payload is valid JSON (catches typos in test helpers).
func TestPushPayloadJSON(t *testing.T) {
	var v map[string]any
	if err := json.Unmarshal(pushPayload("feat/SKUI-1-x", true), &v); err != nil {
		t.Fatal(err)
	}
}
