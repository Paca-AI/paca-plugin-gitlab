package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	plugin "github.com/Paca-AI/plugin-sdk-go"
)

// ─── DTOs ────────────────────────────────────────────────────────────────────

type integrationResponse struct {
	ProjectID   string  `json:"project_id"`
	Connected   bool    `json:"connected"`
	InstanceURL string  `json:"instance_url,omitempty"`
	TokenKind   string  `json:"token_kind,omitempty"`
	CreatedAt   *string `json:"created_at,omitempty"`
	UpdatedAt   *string `json:"updated_at,omitempty"`
}

// accessibleRepoResponse is returned by GET /integration/accessible-repos.
// It reflects data fetched live from the GitLab API.
type accessibleRepoResponse struct {
	FullName      string `json:"full_name"`
	Owner         string `json:"owner"`
	RepoName      string `json:"repo_name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
}

// repositoryResponse is the canonical DTO for a project-linked repository.
// Returned by GET /repositories and POST /repositories.
type repositoryResponse struct {
	ID            string `json:"id"`
	ProjectID     string `json:"project_id"`
	Owner         string `json:"owner"`
	RepoName      string `json:"repo_name"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	CloneURL      string `json:"clone_url"`
	WebhookActive bool   `json:"webhook_active"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

// repoCloneInfo is returned by GET /repositories/:repoId/clone-info.
// It includes a short-lived token for cloning.
type repoCloneInfo struct {
	ID        string  `json:"id"`
	FullName  string  `json:"full_name"`
	Owner     string  `json:"owner"`
	RepoName  string  `json:"repo_name"`
	CloneURL  string  `json:"clone_url"`
	Token     string  `json:"token"`
	ExpiresAt float64 `json:"expires_at"`
}

const gitlabPluginID = "com.gorlix.gitlab"

func webhookURLFromPublicURL(cfg *plugin.Config, projectID string) (string, error) {
	publicURL, ok := cfg.Get("PUBLIC_URL")
	if !ok || strings.TrimSpace(publicURL) == "" {
		return "", errors.New("PUBLIC_URL is not configured")
	}
	base := strings.TrimRight(strings.TrimSpace(publicURL), "/")
	return fmt.Sprintf("%s/api/v1/plugins/%s/projects/%s/webhook", base, gitlabPluginID, projectID), nil
}

// ─── Helper: decrypt credentials for a project ───────────────────────────────

type gitlabCreds struct {
	Token       string
	InstanceURL string
	TokenKind   string
}

func (p *gitlabPlugin) loadCreds(projectID string) (*gitlabCreds, error) {
	result, err := p.db.Query(
		`SELECT access_token_enc, instance_url, token_kind FROM gitlab_integrations WHERE project_id = $1`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	if len(result.Rows) == 0 {
		return nil, &appError{code: "GITLAB_INTEGRATION_NOT_FOUND", status: 404, msg: "GitLab integration not found"}
	}
	sc := newRowScanner(result.Columns, result.Rows[0])
	token, err := p.decrypt(sc.str("access_token_enc"))
	if err != nil {
		return nil, err
	}
	instance := sc.str("instance_url")
	if instance == "" {
		instance = defaultGitLabInstance
	}
	kind := sc.str("token_kind")
	if kind == "" {
		kind = "personal"
	}
	return &gitlabCreds{Token: token, InstanceURL: instance, TokenKind: kind}, nil
}

func (p *gitlabPlugin) clientForProject(projectID string) (*glClient, error) {
	c, err := p.loadCreds(projectID)
	if err != nil {
		return nil, err
	}
	return newGLClient(c.Token, c.InstanceURL), nil
}

// ─── GET /integration ────────────────────────────────────────────────────────

func (p *gitlabPlugin) getIntegration(req *plugin.Request, res *plugin.Response) {
	projectID := req.Caller.ProjectID
	result, err := p.db.Query(
		`SELECT project_id, instance_url, token_kind, created_at, updated_at FROM gitlab_integrations WHERE project_id = $1`,
		projectID,
	)
	if err != nil {
		apiError(res, 500, "INTERNAL_ERROR", err.Error())
		return
	}
	if len(result.Rows) == 0 {
		ok(res, integrationResponse{ProjectID: projectID, Connected: false})
		return
	}
	sc := newRowScanner(result.Columns, result.Rows[0])
	ca := sc.str("created_at")
	ua := sc.str("updated_at")
	ok(res, integrationResponse{
		ProjectID:   sc.str("project_id"),
		Connected:   true,
		InstanceURL: sc.str("instance_url"),
		TokenKind:   sc.str("token_kind"),
		CreatedAt:   &ca,
		UpdatedAt:   &ua,
	})
}

// ─── POST /integration/token ─────────────────────────────────────────────────

func (p *gitlabPlugin) setToken(req *plugin.Request, res *plugin.Response) {
	projectID := req.Caller.ProjectID

	type setTokenBody struct {
		Token       string `json:"token"`
		InstanceURL string `json:"instance_url"`
		TokenKind   string `json:"token_kind"` // personal | project | group
	}
	b, err := plugin.JSONBody[setTokenBody](req)
	if err != nil || b.Token == "" {
		apiError(res, 400, "BAD_REQUEST", "token is required")
		return
	}

	instanceURL := normalizeInstanceURL(b.InstanceURL)
	kind := strings.ToLower(strings.TrimSpace(b.TokenKind))
	switch kind {
	case "", "personal", "pat":
		kind = "personal"
	case "project":
		kind = "project"
	case "group":
		kind = "group"
	default:
		apiError(res, 400, "BAD_REQUEST", "token_kind must be personal, project, or group")
		return
	}

	// Validate against GitLab API (works for Personal, Project, and Group tokens).
	ghc := newGLClient(b.Token, instanceURL)
	if err := ghc.validateToken(context.Background()); err != nil {
		var apiErr *glAPIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == 401 || apiErr.StatusCode == 403) {
			apiError(res, 422, "GITLAB_INVALID_TOKEN", "GitLab token is invalid or expired for this instance")
			return
		}
		apiError(res, 502, "INTERNAL_ERROR", fmt.Sprintf("failed to validate token: %s", err))
		return
	}

	enc, err := p.encrypt(b.Token)
	if err != nil {
		apiError(res, 500, "INTERNAL_ERROR", "failed to encrypt token")
		return
	}

	now := nowStr()
	_, err = p.db.Exec(`
		INSERT INTO gitlab_integrations (project_id, access_token_enc, instance_url, token_kind, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (project_id) DO UPDATE SET
			access_token_enc = EXCLUDED.access_token_enc,
			instance_url = EXCLUDED.instance_url,
			token_kind = EXCLUDED.token_kind,
			updated_at = EXCLUDED.updated_at
	`, projectID, enc, instanceURL, kind, now, now)
	if err != nil {
		apiError(res, 500, "INTERNAL_ERROR", err.Error())
		return
	}

	result, qErr := p.db.Query(
		`SELECT project_id, instance_url, token_kind, created_at, updated_at FROM gitlab_integrations WHERE project_id = $1`,
		projectID,
	)
	if qErr != nil || len(result.Rows) == 0 {
		ok(res, integrationResponse{ProjectID: projectID, Connected: true, InstanceURL: instanceURL, TokenKind: kind})
		return
	}
	sc := newRowScanner(result.Columns, result.Rows[0])
	ca := sc.str("created_at")
	ua := sc.str("updated_at")
	ok(res, integrationResponse{
		ProjectID:   sc.str("project_id"),
		Connected:   true,
		InstanceURL: sc.str("instance_url"),
		TokenKind:   sc.str("token_kind"),
		CreatedAt:   &ca,
		UpdatedAt:   &ua,
	})
}

// ─── DELETE /integration/token ───────────────────────────────────────────────

func (p *gitlabPlugin) deleteToken(req *plugin.Request, res *plugin.Response) {
	projectID := req.Caller.ProjectID

	// Best-effort: delete webhooks for all linked repositories first.
	if ghc, err := p.clientForProject(projectID); err == nil {
		rows, qErr := p.db.Query(
			`SELECT owner, repo_name, webhook_id FROM gitlab_repositories WHERE project_id = $1`,
			projectID,
		)
		if qErr == nil {
			for _, row := range rows.Rows {
				sc := newRowScanner(rows.Columns, row)
				whID := sc.int64Val("webhook_id")
				if whID > 0 {
					_ = ghc.deleteWebhook(context.Background(), sc.str("owner"), sc.str("repo_name"), whID)
				}
			}
		}
	}

	_, execErr := p.db.Exec(`DELETE FROM gitlab_integrations WHERE project_id = $1`, projectID)
	if execErr != nil {
		apiError(res, 500, "INTERNAL_ERROR", execErr.Error())
		return
	}
	noContent(res)
}

// ─── GET /integration/accessible-repos ──────────────────────────────────────

func (p *gitlabPlugin) listAccessibleRepos(req *plugin.Request, res *plugin.Response) {
	projectID := req.Caller.ProjectID

	ghc, err := p.clientForProject(projectID)
	if err != nil {
		writeAppError(res, err)
		return
	}

	repos, err := ghc.listRepositories(context.Background())
	if err != nil {
		apiError(res, 502, "INTERNAL_ERROR", fmt.Sprintf("failed to list repositories: %s", err))
		return
	}

	items := make([]accessibleRepoResponse, len(repos))
	for i, r := range repos {
		items[i] = accessibleRepoResponse{
			FullName:      r.PathWithNamespace,
			Owner:         r.Owner(),
			RepoName:      r.Path,
			DefaultBranch: r.DefaultBranch,
			Private:       r.Private(),
		}
	}
	ok(res, items)
}

// ─── GET /repositories ───────────────────────────────────────────────────────

func (p *gitlabPlugin) listRepositories(req *plugin.Request, res *plugin.Response) {
	projectID := req.Caller.ProjectID

	result, err := p.db.Query(`
		SELECT id, project_id, owner, repo_name, full_name, default_branch, webhook_id, created_at, updated_at
		FROM gitlab_repositories WHERE project_id = $1 ORDER BY created_at ASC
	`, projectID)
	if err != nil {
		apiError(res, 500, "INTERNAL_ERROR", err.Error())
		return
	}

	creds, _ := p.loadCreds(projectID)
	host := defaultGitLabInstance
	if creds != nil {
		host = creds.InstanceURL
	}
	client := newGLClient("", host)

	items := make([]repositoryResponse, 0, len(result.Rows))
	for _, row := range result.Rows {
		sc := newRowScanner(result.Columns, row)
		fullName := sc.str("full_name")
		items = append(items, repositoryResponse{
			ID:            sc.str("id"),
			ProjectID:     sc.str("project_id"),
			Owner:         sc.str("owner"),
			RepoName:      sc.str("repo_name"),
			FullName:      fullName,
			DefaultBranch: sc.str("default_branch"),
			CloneURL:      client.cloneURL(fullName),
			WebhookActive: sc.int64Val("webhook_id") > 0,
			CreatedAt:     sc.str("created_at"),
			UpdatedAt:     sc.str("updated_at"),
		})
	}
	ok(res, items)
}

// ─── POST /repositories ───────────────────────────────────────────────────────

func (p *gitlabPlugin) linkRepository(req *plugin.Request, res *plugin.Response) {
	projectID := req.Caller.ProjectID

	type linkRepositoryBody struct {
		Owner    string `json:"owner"`
		RepoName string `json:"repo_name"`
	}
	b, err := plugin.JSONBody[linkRepositoryBody](req)
	if err != nil || b.Owner == "" || b.RepoName == "" {
		apiError(res, 400, "BAD_REQUEST", "owner and repo_name are required")
		return
	}

	webhookURL, err := webhookURLFromPublicURL(p.cfg, projectID)
	if err != nil {
		apiError(res, 422, "GITLAB_WEBHOOK_URL_REQUIRED", "PUBLIC_URL is not configured")
		return
	}

	ghc, err := p.clientForProject(projectID)
	if err != nil {
		writeAppError(res, err)
		return
	}
	ghRepo, err := ghc.getRepository(context.Background(), b.Owner, b.RepoName)
	if err != nil {
		var apiErr *glAPIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == 403 || apiErr.StatusCode == 404) {
			apiError(res, 422, "GITLAB_REPO_NOT_ACCESSIBLE", "Project not accessible with the provided token")
			return
		}
		apiError(res, 502, "INTERNAL_ERROR", fmt.Sprintf("failed to get project: %s", err))
		return
	}

	existResult, _ := p.db.Query(
		`SELECT id FROM gitlab_repositories WHERE project_id = $1 AND full_name = $2`,
		projectID, ghRepo.PathWithNamespace,
	)
	if existResult != nil && len(existResult.Rows) > 0 {
		apiError(res, 409, "GITLAB_REPO_ALREADY_LINKED", "Repository is already linked to this project")
		return
	}

	integResult, iErr := p.db.Query(`SELECT id FROM gitlab_integrations WHERE project_id = $1`, projectID)
	if iErr != nil || len(integResult.Rows) == 0 {
		apiError(res, 404, "GITLAB_INTEGRATION_NOT_FOUND", "GitLab integration not found")
		return
	}
	integrationID := newRowScanner(integResult.Columns, integResult.Rows[0]).str("id")

	signingToken, randErr := generateSigningToken()
	if randErr != nil {
		apiError(res, 500, "INTERNAL_ERROR", "failed to generate webhook signing token")
		return
	}

	webhookID, err := ghc.createWebhook(context.Background(), ghRepo.Owner(), ghRepo.Path, webhookURL, signingToken)
	if err != nil {
		var apiErr *glAPIError
		if errors.As(err, &apiErr) && isWebhookURLNotPublic(apiErr) {
			apiError(res, 422, "GITLAB_WEBHOOK_URL_NOT_PUBLIC", "Webhook URL is not publicly accessible")
			return
		}
		apiError(res, 502, "GITLAB_WEBHOOK_CREATION_FAILED", fmt.Sprintf("failed to create webhook: %s", err))
		return
	}

	encSecret, err := p.encrypt(signingToken)
	if err != nil {
		_ = ghc.deleteWebhook(context.Background(), ghRepo.Owner(), ghRepo.Path, webhookID)
		apiError(res, 500, "INTERNAL_ERROR", "failed to encrypt webhook signing token")
		return
	}

	now := nowStr()
	owner := ghRepo.Owner()
	inserted, dbErr := p.db.Query(`
		INSERT INTO gitlab_repositories
			(project_id, integration_id, owner, repo_name, full_name, gitlab_project_id, webhook_id, webhook_secret_enc, default_branch, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10)
		RETURNING id
	`, projectID, integrationID, owner, ghRepo.Path, ghRepo.PathWithNamespace, ghRepo.ID, webhookID, encSecret, ghRepo.DefaultBranch, now)
	if dbErr != nil || len(inserted.Rows) == 0 {
		_ = ghc.deleteWebhook(context.Background(), owner, ghRepo.Path, webhookID)
		if dbErr != nil {
			apiError(res, 500, "INTERNAL_ERROR", dbErr.Error())
		} else {
			apiError(res, 500, "INTERNAL_ERROR", "insert returned no rows")
		}
		return
	}
	repoID := newRowScanner(inserted.Columns, inserted.Rows[0]).str("id")

	created(res, repositoryResponse{
		ID:            repoID,
		ProjectID:     projectID,
		Owner:         owner,
		RepoName:      ghRepo.Path,
		FullName:      ghRepo.PathWithNamespace,
		DefaultBranch: ghRepo.DefaultBranch,
		CloneURL:      ghc.cloneURL(ghRepo.PathWithNamespace),
		WebhookActive: webhookID > 0,
		CreatedAt:     now,
		UpdatedAt:     now,
	})
}

// ─── DELETE /repositories/:repoId ────────────────────────────────────────────

func (p *gitlabPlugin) unlinkRepository(req *plugin.Request, res *plugin.Response) {
	projectID := req.Caller.ProjectID
	repoID := req.PathParam("repoId")

	// Fetch repository details to delete webhook.
	result, err := p.db.Query(
		`SELECT owner, repo_name, webhook_id FROM gitlab_repositories WHERE id = $1 AND project_id = $2`,
		repoID, projectID,
	)
	if err != nil {
		apiError(res, 500, "INTERNAL_ERROR", err.Error())
		return
	}
	if len(result.Rows) == 0 {
		apiError(res, 404, "GITLAB_REPOSITORY_NOT_FOUND", "Repository not found")
		return
	}
	sc := newRowScanner(result.Columns, result.Rows[0])
	owner := sc.str("owner")
	repoName := sc.str("repo_name")
	webhookID := sc.int64Val("webhook_id")

	// Best-effort: delete the webhook.
	if webhookID > 0 {
		if ghc, tErr := p.clientForProject(projectID); tErr == nil {
			_ = ghc.deleteWebhook(context.Background(), owner, repoName, webhookID)
		}
	}

	_, err = p.db.Exec(`DELETE FROM gitlab_repositories WHERE id = $1`, repoID)
	if err != nil {
		apiError(res, 500, "INTERNAL_ERROR", err.Error())
		return
	}
	noContent(res)
}

// ─── GET /repositories/:repoId/clone-info ────────────────────────────────────

func (p *gitlabPlugin) getRepoCloneInfo(req *plugin.Request, res *plugin.Response) {
	projectID := req.Caller.ProjectID
	repoID := req.PathParam("repoId")
	if repoID == "" {
		apiError(res, 400, "BAD_REQUEST", "repoId path parameter is required")
		return
	}
	result, err := p.db.Query(
		`SELECT id, full_name, owner, repo_name FROM gitlab_repositories WHERE project_id = $1 AND id = $2`,
		projectID, repoID,
	)
	if err != nil {
		apiError(res, 500, "INTERNAL_ERROR", err.Error())
		return
	}
	if len(result.Rows) == 0 {
		apiError(res, 404, "GITLAB_REPOSITORY_NOT_FOUND", "repository not found")
		return
	}
	creds, err := p.loadCreds(projectID)
	if err != nil {
		writeAppError(res, err)
		return
	}
	sc := newRowScanner(result.Columns, result.Rows[0])
	fullName := sc.str("full_name")
	ok(res, repoCloneInfo{
		ID:        sc.str("id"),
		FullName:  fullName,
		Owner:     sc.str("owner"),
		RepoName:  sc.str("repo_name"),
		CloneURL:  newGLClient("", creds.InstanceURL).cloneURL(fullName),
		Token:     creds.Token,
		ExpiresAt: 0,
	})
}

// ─── GET /github/repo-info (REMOVED) ─────────────────────────────────────────
// Replaced by GET /repositories (list all) and GET /repositories/:repoId/clone-info

// ─── Error helpers ────────────────────────────────────────────────────────────

type appError struct {
	code   string
	status int
	msg    string
}

func (e *appError) Error() string { return e.msg }

func writeAppError(res *plugin.Response, err error) {
	var ae *appError
	if errors.As(err, &ae) {
		apiError(res, ae.status, ae.code, ae.msg)
		return
	}
	apiError(res, 500, "INTERNAL_ERROR", err.Error())
}

func isWebhookURLNotPublic(apiErr *glAPIError) bool {
	if apiErr == nil || apiErr.StatusCode != 422 {
		return false
	}
	msg := strings.ToLower(apiErr.Message + " " + apiErr.Details)
	return strings.Contains(msg, "isn't reachable over the public internet") ||
		strings.Contains(msg, "not publicly reachable") ||
		strings.Contains(msg, "localhost")
}
