package main

import (
	"strings"
	"context"
	"encoding/json"

	plugin "github.com/Paca-AI/plugin-sdk-go"
)

// This file implements the automation-graph Condition and Action node
// types this plugin contributes, registered in Init via ctx.Condition and
// ctx.Action. Node types must match exactly what's declared in plugin.json
// under "automation" (see AutomationManifest in the core's
// domain/plugin/entity.go) — namespaced under the plugin's short name,
// "gitlab" (the last dot-separated segment of the plugin ID "com.gorlix.gitlab"),
// not the full reverse-DNS ID.
//
// Both handlers resolve the calling project from req.ProjectID — supplied
// directly by the host (the automation graph's own project), not read from
// the node's config — since a task by itself doesn't carry which GitLab
// repository/PR it's linked to; that's resolved through
// gitlab_task_mr_links the same way the HTTP handlers in pull_requests.go
// do it, keyed by (task_id, project_id).

const (
	// automationConditionPRState checks the linked pull request's state
	// (open/closed/merged) against a configured expected value.
	automationConditionPRState = "gitlab.mr_state"

	// automationActionMergePR merges the linked pull request.
	automationActionMergePR = "gitlab.merge_mr"
	// automationActionCommentPR posts a comment on the linked pull request.
	automationActionCommentPR = "gitlab.comment_mr"
)

// registerAutomationNodes wires this plugin's Condition/Action handlers
// into ctx. Called once from Init.
func (p *gitlabPlugin) registerAutomationNodes(ctx *plugin.Context) {
	ctx.Condition(automationConditionPRState, p.conditionPRState)
	ctx.Action(automationActionMergePR, p.actionMergePR)
	ctx.Action(automationActionCommentPR, p.actionCommentPR)
}

// ─── shared: resolve the most recently linked PR for a task ──────────────────

// pluginLinkedPR is what resolveLinkedPRForAutomation returns: enough to
// call the GitLab API plus the plugin's own repo_id/pr row id for logging.
type pluginLinkedPR struct {
	Owner    string
	RepoName string
	PRNumber int
}

// resolveLinkedPRForAutomation finds the most recently linked PR for a
// task, the same many-PRs-per-task relationship listTaskPRs exposes over
// HTTP — automation nodes act on the newest link since that's virtually
// always the PR the automation graph author means ("the PR for this task").
func (p *gitlabPlugin) resolveLinkedPRForAutomation(projectID, taskID string) (*pluginLinkedPR, error) {
	result, err := p.db.Query(`
		SELECT r.owner, r.repo_name, pr.pr_number
		FROM gitlab_merge_requests pr
		JOIN gitlab_task_mr_links l ON l.merge_request_id = pr.id
		JOIN gitlab_repositories r ON r.id = pr.repo_id
		WHERE l.task_id = $1 AND pr.project_id = $2
		ORDER BY l.created_at DESC
		LIMIT 1
	`, taskID, projectID)
	if err != nil {
		return nil, err
	}
	if len(result.Rows) == 0 {
		return nil, &appError{code: "GITLAB_PR_LINK_NOT_FOUND", status: 404, msg: "No pull request linked to this task"}
	}
	sc := newRowScanner(result.Columns, result.Rows[0])
	return &pluginLinkedPR{
		Owner:    sc.str("owner"),
		RepoName: sc.str("repo_name"),
		PRNumber: sc.intVal("pr_number"),
	}, nil
}

// ─── Condition: github.pr_state ───────────────────────────────────────────────

func (p *gitlabPlugin) conditionPRState(req *plugin.ConditionRequest) plugin.ConditionResult {
	var cfg struct {
		ExpectedState string `json:"expected_state"` // "open" | "closed" | "merged"
	}
	if err := json.Unmarshal(req.Config, &cfg); err != nil || req.ProjectID == "" || cfg.ExpectedState == "" {
		p.log.Error("gitlab: pr_state condition: invalid config")
		return plugin.ConditionResult{Matched: false}
	}

	linked, err := p.resolveLinkedPRForAutomation(req.ProjectID, req.Task.ID)
	if err != nil {
		p.log.Info("gitlab: pr_state condition: " + err.Error())
		return plugin.ConditionResult{Matched: false}
	}

	ghc, err := p.clientForProject(req.ProjectID)
	if err != nil {
		p.log.Error("gitlab: pr_state condition: decrypt token: " + err.Error())
		return plugin.ConditionResult{Matched: false}
	}
	ghPR, err := ghc.getPullRequest(context.Background(), linked.Owner, linked.RepoName, linked.PRNumber)
	if err != nil {
		p.log.Error("gitlab: pr_state condition: fetch PR: " + err.Error())
		return plugin.ConditionResult{Matched: false}
	}

	state := ghPR.NormalizedState()
	return plugin.ConditionResult{Matched: state == cfg.ExpectedState}
}

// ─── Action: github.merge_pr ──────────────────────────────────────────────────

func (p *gitlabPlugin) actionMergePR(req *plugin.ActionRequest) plugin.ActionResult {
	var cfg struct {
		MergeMethod string `json:"merge_method"` // "merge" | "squash" | "rebase"; defaults to "merge"
	}
	if err := json.Unmarshal(req.Config, &cfg); err != nil || req.ProjectID == "" {
		return plugin.ActionResult{Applied: false, Error: "invalid config"}
	}
	if cfg.MergeMethod == "" {
		cfg.MergeMethod = "merge"
	}

	linked, err := p.resolveLinkedPRForAutomation(req.ProjectID, req.Task.ID)
	if err != nil {
		return plugin.ActionResult{Applied: false, Error: err.Error()}
	}

	ghc, err := p.clientForProject(req.ProjectID)
	if err != nil {
		return plugin.ActionResult{Applied: false, Error: err.Error()}
	}
	ctx := context.Background()

	// Idempotency: a plugin action can be retried by the automation
	// engine, so check current state first — merging an already-merged PR
	// is a no-op success, not an error, mirroring how built-in actions
	// treat "already at the desired state".
	ghPR, err := ghc.getPullRequest(ctx, linked.Owner, linked.RepoName, linked.PRNumber)
	if err != nil {
		return plugin.ActionResult{Applied: false, Error: "fetch PR: " + err.Error()}
	}
	if strings.EqualFold(ghPR.State, "merged") || ghPR.MergedAt != nil {
		return plugin.ActionResult{Applied: false}
	}

	if err := ghc.mergePullRequest(ctx, linked.Owner, linked.RepoName, linked.PRNumber, cfg.MergeMethod); err != nil {
		return plugin.ActionResult{Applied: false, Error: "merge PR: " + err.Error()}
	}
	return plugin.ActionResult{Applied: true}
}

// ─── Action: github.comment_pr ────────────────────────────────────────────────

func (p *gitlabPlugin) actionCommentPR(req *plugin.ActionRequest) plugin.ActionResult {
	var cfg struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(req.Config, &cfg); err != nil || req.ProjectID == "" || cfg.Body == "" {
		return plugin.ActionResult{Applied: false, Error: "invalid config: body is required"}
	}

	linked, err := p.resolveLinkedPRForAutomation(req.ProjectID, req.Task.ID)
	if err != nil {
		return plugin.ActionResult{Applied: false, Error: err.Error()}
	}

	ghc, err := p.clientForProject(req.ProjectID)
	if err != nil {
		return plugin.ActionResult{Applied: false, Error: err.Error()}
	}

	if err := ghc.createIssueComment(context.Background(), linked.Owner, linked.RepoName, linked.PRNumber, cfg.Body); err != nil {
		return plugin.ActionResult{Applied: false, Error: "comment PR: " + err.Error()}
	}
	return plugin.ActionResult{Applied: true}
}
