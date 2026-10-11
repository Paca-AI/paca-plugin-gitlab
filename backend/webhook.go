package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	plugin "github.com/Paca-AI/plugin-sdk-go"
)

// Branch task-ref patterns (path-segment aware, matches UI CreateBranch names):
//   feat/PROJ-42-slug  /  PROJ-42
//   feat/PROJ/42-slug  /  PROJ/42
//   feat/42-slug       /  feat/42   (number-only when project has no task_id_prefix)
var (
	branchTaskRefHyphenRe     = regexp.MustCompile(`(?i)(?:^|/)([A-Z][A-Z0-9]{1,19})-(\d{1,6})(?:-|/|$)`)
	branchTaskRefSlashRe      = regexp.MustCompile(`(?i)(?:^|/)([A-Z][A-Z0-9]{1,19})/(\d{1,6})(?:-|/|$)`)
	branchTaskRefNumberOnlyRe = regexp.MustCompile(`(?i)(?:^|/)(\d{1,6})(?:-|$)`)
)

// ─── POST /webhook ────────────────────────────────────────────────────────────

func (p *gitlabPlugin) receiveWebhook(req *plugin.Request, res *plugin.Response) {
	projectID := req.PathParam("projectId")
	event := headerGet(req.Headers, "X-Gitlab-Event")
	webhookID := headerGet(req.Headers, "webhook-id")
	timestamp := headerGet(req.Headers, "webhook-timestamp")
	signature := headerGet(req.Headers, "webhook-signature")

	body := req.Body
	if len(body) == 0 {
		res.NoContent()
		return
	}

	repoFullName := extractGitLabProjectPath(body)
	if repoFullName == "" {
		res.NoContent()
		return
	}

	if err := p.handleWebhookEvent(projectID, repoFullName, event, webhookID, timestamp, signature, body); err != nil {
		p.log.Error("gitlab: webhook handler error: " + err.Error())
	}
	res.NoContent()
}

func headerGet(headers map[string]string, key string) string {
	if headers == nil {
		return ""
	}
	if v, ok := headers[key]; ok {
		return v
	}
	// Host may normalize header keys.
	for k, v := range headers {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

func (p *gitlabPlugin) handleWebhookEvent(projectID, repoFullName, event, webhookID, timestamp, signature string, payload []byte) error {
	p.log.Info("gitlab: webhook received, repo=" + repoFullName + ", event=" + event)

	result, err := p.db.Query(
		`SELECT id, project_id, integration_id, owner, repo_name, full_name, default_branch, webhook_secret_enc FROM gitlab_repositories WHERE full_name = $1 AND project_id = $2`,
		repoFullName, projectID,
	)
	if err != nil {
		p.log.Error("gitlab: failed to query repository: " + err.Error() + ", repo=" + repoFullName)
		return err
	}
	if len(result.Rows) == 0 {
		p.log.Info("gitlab: repository not found, repo=" + repoFullName)
		return nil
	}
	sc := newRowScanner(result.Columns, result.Rows[0])
	repoID := sc.str("id")
	projectID = sc.str("project_id")
	webhookSecretEnc := sc.str("webhook_secret_enc")

	if webhookSecretEnc == "" {
		p.log.Error("gitlab: repository has no webhook signing token configured, refusing delivery, repo=" + repoFullName)
		return nil
	}
	signingToken, dErr := p.decrypt(webhookSecretEnc)
	if dErr != nil {
		p.log.Error("gitlab: failed to decrypt webhook signing token: " + dErr.Error() + ", repo=" + repoFullName)
		return dErr
	}
	if !verifyGitLabSigningToken(payload, signingToken, webhookID, timestamp, signature) {
		p.log.Info("gitlab: invalid webhook signature, repo=" + repoFullName)
		return nil
	}

	switch strings.ToLower(event) {
	case "merge request hook", "merge_request":
		p.log.Info("gitlab: handling merge_request event, repo=" + repoFullName)
		return p.handlePREvent(repoID, projectID, payload)
	case "push hook", "push":
		return p.handlePushEvent(repoID, projectID, payload)
	default:
		p.log.Info("gitlab: unhandled event type, event=" + event)
	}
	return nil
}

func (p *gitlabPlugin) handlePREvent(repoID, projectID string, payload []byte) error {
	var event struct {
		ObjectKind       string `json:"object_kind"`
		ObjectAttributes struct {
			ID           int64      `json:"id"`
			IID          int        `json:"iid"`
			Title        string     `json:"title"`
			State        string     `json:"state"`
			Action       string     `json:"action"`
			URL          string     `json:"url"`
			SourceBranch string     `json:"source_branch"`
			TargetBranch string     `json:"target_branch"`
			MergedAt     *time.Time `json:"merged_at"`
		} `json:"object_attributes"`
		User struct {
			Username string `json:"username"`
		} `json:"user"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		p.log.Error("gitlab: failed to parse merge_request event: " + err.Error())
		return err
	}
	oa := event.ObjectAttributes
	action := oa.Action
	state := normalizeMRState(oa.State, oa.MergedAt != nil)

	p.log.Info("gitlab: processing merge_request, action=" + action + ", mr_iid=" + strconv.Itoa(oa.IID) + ", title=" + oa.Title + ", repo_id=" + repoID)

	var previousState string
	if existing, exErr := p.db.Query(`SELECT state FROM gitlab_merge_requests WHERE repo_id = $1 AND pr_number = $2`, repoID, oa.IID); exErr == nil && len(existing.Rows) > 0 {
		previousState = newRowScanner(existing.Columns, existing.Rows[0]).str("state")
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	var mergedAtStr *string
	if oa.MergedAt != nil {
		s := oa.MergedAt.UTC().Format(time.RFC3339Nano)
		mergedAtStr = &s
	}

	upserted, err := p.db.Query(`
		INSERT INTO gitlab_merge_requests
			(project_id, repo_id, pr_number, gitlab_mr_id, title, state, html_url, head_branch, base_branch, author, merged_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (repo_id, pr_number) DO UPDATE SET
			title=$5, state=$6, html_url=$7, head_branch=$8, base_branch=$9,
			author=$10, merged_at=$11, updated_at=$13
		RETURNING id
	`, projectID, repoID, oa.IID, oa.ID, oa.Title, state,
		oa.URL, oa.SourceBranch, oa.TargetBranch, event.User.Username, mergedAtStr, now, now)
	if err != nil {
		p.log.Error("gitlab: failed to upsert MR: " + err.Error() + ", repo_id=" + repoID + ", mr_iid=" + strconv.Itoa(oa.IID))
		return err
	}
	var prID string
	if len(upserted.Rows) > 0 {
		prID = newRowScanner(upserted.Columns, upserted.Rows[0]).str("id")
	}

	if action == "open" || action == "reopen" {
		brResult, _ := p.db.Query(
			`SELECT task_id FROM gitlab_task_branches WHERE repo_id = $1 AND branch_name = $2`,
			repoID, oa.SourceBranch,
		)
		if brResult != nil && len(brResult.Rows) > 0 {
			taskID := newRowScanner(brResult.Columns, brResult.Rows[0]).str("task_id")
			_, _ = p.db.Exec(`
				INSERT INTO gitlab_task_mr_links (task_id, merge_request_id, created_at)
				VALUES ($1,$2,$3)
				ON CONFLICT (task_id, merge_request_id) DO NOTHING
			`, taskID, prID, now)

			plugin.EmitEvent("gitlab.mr_linked", map[string]any{
				"project_id": projectID,
				"task_id":    taskID,
				"repo_id":    repoID,
				"pr_number":  oa.IID,
			})
			p.log.Info("gitlab: MR auto-linked to task, task_id=" + taskID + ", mr_iid=" + strconv.Itoa(oa.IID))
		}
	}

	stateChanged := previousState != "" && previousState != state
	linkedResult, _ := p.db.Query(`SELECT task_id FROM gitlab_task_mr_links WHERE merge_request_id = $1`, prID)
	if linkedResult != nil {
		for _, row := range linkedResult.Rows {
			taskID := newRowScanner(linkedResult.Columns, row).str("task_id")
			plugin.EmitEvent("gitlab.mr_updated", map[string]any{
				"project_id": projectID,
				"task_id":    taskID,
				"repo_id":    repoID,
				"pr_number":  oa.IID,
				"action":     action,
			})
			if stateChanged {
				plugin.EmitEvent("gitlab.mr_state_changed", map[string]any{
					"project_id": projectID,
					"task_id":    taskID,
					"repo_id":    repoID,
					"pr_number":  oa.IID,
					"from_state": previousState,
					"to_state":   state,
				})
			}
		}
	}
	return nil
}

func (p *gitlabPlugin) handlePushEvent(repoID, projectID string, payload []byte) error {
	var event struct {
		Ref     string `json:"ref"`
		Before  string `json:"before"`
		After   string `json:"after"`
		Created bool   `json:"created"` // GitHub-style; GitLab usually omits this
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil
	}
	branchName := strings.TrimPrefix(event.Ref, "refs/heads/")
	if branchName == event.Ref {
		return nil // not a branch ref (e.g. tag)
	}
	if !isNewBranchPush(event.Created, event.Before, event.After) {
		return nil
	}

	refPrefix, taskNumber, ok := extractBranchTaskRef(branchName)
	if !ok {
		return nil
	}

	taskID, found := p.resolveTaskForBranchRef(projectID, refPrefix, taskNumber)
	if !found {
		return nil
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = p.db.Exec(`
		INSERT INTO gitlab_task_branches (task_id, repo_id, branch_name, created_at)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (task_id, repo_id, branch_name) DO NOTHING
	`, taskID, repoID, branchName, now)

	plugin.EmitEvent("gitlab.branch_linked", map[string]any{
		"project_id":  projectID,
		"task_id":     taskID,
		"repo_id":     repoID,
		"branch_name": branchName,
	})
	p.log.Info("gitlab: branch auto-linked to task, task_id=" + taskID + ", branch=" + branchName)
	return nil
}

// isNewBranchPush detects a newly created branch on push.
// GitLab sends before = 40 zero hex chars; GitHub may set created=true.
func isNewBranchPush(created bool, before, after string) bool {
	zeroSHA := strings.Repeat("0", 40)
	isDeleted := after == zeroSHA
	isCreated := created || before == "" || before == zeroSHA
	return isCreated && !isDeleted
}

// resolveTaskForBranchRef finds a task in projectID matching the branch ref.
// Prefixed refs (PROJ-1 / PROJ/1) require the project's task_id_prefix to match.
// Number-only refs (feat/1-slug) are allowed only when the project has an empty prefix.
func (p *gitlabPlugin) resolveTaskForBranchRef(projectID, refPrefix string, taskNumber int64) (taskID string, ok bool) {
	prefixResult, err := p.db.Query(`SELECT task_id_prefix FROM projects WHERE id = $1`, projectID)
	if err != nil || len(prefixResult.Rows) == 0 {
		return "", false
	}
	projectPrefix := strings.TrimSpace(newRowScanner(prefixResult.Columns, prefixResult.Rows[0]).str("task_id_prefix"))

	if refPrefix != "" {
		if projectPrefix == "" || !strings.EqualFold(refPrefix, projectPrefix) {
			return "", false
		}
	} else if projectPrefix != "" {
		// Number-only branch, but project uses a prefix — ignore (matches UI).
		return "", false
	}

	taskResult, tErr := p.db.Query(`
		SELECT id FROM tasks
		WHERE project_id = $1 AND task_number = $2 AND deleted_at IS NULL
		LIMIT 1
	`, projectID, taskNumber)
	if tErr != nil || len(taskResult.Rows) == 0 {
		return "", false
	}
	return newRowScanner(taskResult.Columns, taskResult.Rows[0]).str("id"), true
}

// verifyGitLabSigningToken validates Standard Webhooks HMAC (GitLab 19+ signing_token).
func verifyGitLabSigningToken(payload []byte, signingToken, msgID, timestamp, signatureHeader string) bool {
	if signingToken == "" || msgID == "" || timestamp == "" || signatureHeader == "" {
		return false
	}
	keyPart := strings.TrimPrefix(signingToken, "whsec_")
	key, err := base64.StdEncoding.DecodeString(keyPart)
	if err != nil || len(key) == 0 {
		return false
	}
	signed := msgID + "." + timestamp + "." + string(payload)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signed))
	digest := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	want := "v1," + digest
	for _, part := range strings.Fields(signatureHeader) {
		if hmac.Equal([]byte(part), []byte(want)) {
			return true
		}
	}
	return false
}

func extractGitLabProjectPath(payload []byte) string {
	var v struct {
		Project struct {
			PathWithNamespace string `json:"path_with_namespace"`
		} `json:"project"`
		Repository struct {
			PathWithNamespace string `json:"path_with_namespace"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(payload, &v); err != nil {
		return ""
	}
	if v.Project.PathWithNamespace != "" {
		return v.Project.PathWithNamespace
	}
	return v.Repository.PathWithNamespace
}

// conventionalBranchTypes are gitflow-style path segments, not Paca task prefixes.
// Needed so "feat/1-slug" is number-only, not prefix=FEAT via the slash form.
var conventionalBranchTypes = map[string]struct{}{
	"feat": {}, "feature": {}, "fix": {}, "bugfix": {}, "chore": {},
	"docs": {}, "doc": {}, "refactor": {}, "style": {}, "test": {},
	"tests": {}, "ci": {}, "build": {}, "perf": {}, "revert": {},
	"hotfix": {}, "release": {}, "wip": {},
}

func isConventionalBranchType(s string) bool {
	_, ok := conventionalBranchTypes[strings.ToLower(s)]
	return ok
}

// extractBranchTaskRef parses a Paca task reference from a git branch name.
// Returns prefix="" for number-only branches (used when project has no task_id_prefix).
func extractBranchTaskRef(branchName string) (prefix string, taskNumber int64, ok bool) {
	if m := branchTaskRefHyphenRe.FindStringSubmatch(branchName); m != nil {
		n, err := strconv.ParseInt(m[2], 10, 64)
		if err == nil && n > 0 {
			return strings.ToUpper(m[1]), n, true
		}
	}
	for _, m := range branchTaskRefSlashRe.FindAllStringSubmatch(branchName, -1) {
		if isConventionalBranchType(m[1]) {
			continue
		}
		n, err := strconv.ParseInt(m[2], 10, 64)
		if err == nil && n > 0 {
			return strings.ToUpper(m[1]), n, true
		}
	}
	if m := branchTaskRefNumberOnlyRe.FindStringSubmatch(branchName); m != nil {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err == nil && n > 0 {
			return "", n, true
		}
	}
	return "", 0, false
}

func normalizeMRState(state string, merged bool) string {
	if merged || strings.EqualFold(state, "merged") {
		return "merged"
	}
	switch strings.ToLower(state) {
	case "opened", "open", "locked", "reopened":
		return "open"
	case "closed":
		return "closed"
	default:
		return strings.ToLower(state)
	}
}
