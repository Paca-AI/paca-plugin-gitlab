import { type PluginApiClient } from "@paca-ai/plugin-sdk-react";

const PLUGIN_ID = "com.gorlix.gitlab";

// ── Error codes ────────────────────────────────────────────────────────────────

export const ErrorCode = {
  GitLabIntegrationNotFound: "GITLAB_INTEGRATION_NOT_FOUND",
  GitLabRepositoryNotFound: "GITLAB_REPOSITORY_NOT_FOUND",
  GitLabPRNotFound: "GITLAB_PR_NOT_FOUND",
  GitLabPRAlreadyLinked: "GITLAB_PR_ALREADY_LINKED",
  GitLabInvalidToken: "GITLAB_INVALID_TOKEN",
  GitLabRepoNotAccessible: "GITLAB_REPO_NOT_ACCESSIBLE",
  GitLabRepoAlreadyLinked: "GITLAB_REPO_ALREADY_LINKED",
  GitLabWebhookCreationFailed: "GITLAB_WEBHOOK_CREATION_FAILED",
  GitLabWebhookURLNotPublic: "GITLAB_WEBHOOK_URL_NOT_PUBLIC",
  GitLabBranchAlreadyLinked: "GITLAB_BRANCH_ALREADY_LINKED",
  GitLabBranchNotFound: "GITLAB_BRANCH_NOT_FOUND",
  GitLabTokenInsufficientPermissions: "GITLAB_TOKEN_INSUFFICIENT_PERMISSIONS",
  BadRequest: "BAD_REQUEST",
} as const;

export type ErrorCodeValue = (typeof ErrorCode)[keyof typeof ErrorCode];

/**
 * Extracts the error_code field from a PluginApiClient error.
 * The React SDK throws: `[PluginApiClient] METHOD URL → STATUS: BODY`
 * where BODY is the JSON from the plugin backend.
 */
export function getPluginErrorCode(err: unknown): ErrorCodeValue | null {
  if (!(err instanceof Error)) return null;
  const arrowIdx = err.message.lastIndexOf("→ ");
  if (arrowIdx === -1) return null;
  const rest = err.message.slice(arrowIdx + 2);
  const colonIdx = rest.indexOf(": ");
  if (colonIdx === -1) return null;
  const maybeJson = rest.slice(colonIdx + 2);
  try {
    const body = JSON.parse(maybeJson) as { error_code?: string };
    const code = body.error_code;
    if (!code) return null;
    const known = Object.values(ErrorCode) as string[];
    return known.includes(code) ? (code as ErrorCodeValue) : null;
  } catch {
    return null;
  }
}

// ── Domain types ───────────────────────────────────────────────────────────────

export interface GitLabIntegration {
  id?: string;
  project_id: string;
  connected: boolean;
  instance_url?: string;
  token_kind?: "personal" | "project" | "group";
  created_at?: string;
  updated_at?: string;
}

export type GitLabTokenKind = "personal" | "project" | "group";

export interface AccessibleRepo {
  full_name: string;
  owner: string;
  repo_name: string;
  default_branch: string;
  private: boolean;
  description: string;
}

export interface LinkedRepository {
  id: string;
  project_id: string;
  owner: string;
  repo_name: string;
  full_name: string;
  default_branch: string;
  clone_url: string;
  webhook_active: boolean;
  created_at: string;
  updated_at: string;
}

export interface PullRequest {
  id: string;
  project_id: string;
  repo_id: string;
  pr_number: number;
  github_pr_id: number;
  title: string;
  state: "open" | "closed" | "merged";
  html_url: string;
  head_branch: string;
  base_branch: string;
  author: string;
  merged_at: string | null;
  created_at: string;
  updated_at: string;
}

export interface TaskBranch {
  id: string;
  task_id: string;
  repo_id: string;
  branch_name: string;
  created_at: string;
}

export interface CreateBranchResult {
  branch_name: string;
}

// ── Query key factories ────────────────────────────────────────────────────────

export const integrationKey = (projectId: string) =>
  [PLUGIN_ID, "integration", projectId] as const;

export const linkedReposKey = (projectId: string) =>
  [PLUGIN_ID, "linked-repos", projectId] as const;

export const accessibleReposKey = (projectId: string) =>
  [PLUGIN_ID, "accessible-repos", projectId] as const;

export const taskPRsKey = (projectId: string, taskId: string) =>
  [PLUGIN_ID, "prs", projectId, taskId] as const;

export const taskBranchesKey = (projectId: string, taskId: string) =>
  [PLUGIN_ID, "branches", projectId, taskId] as const;

// ── API functions ──────────────────────────────────────────────────────────────

export async function getGitLabIntegration(
  api: PluginApiClient,
): Promise<GitLabIntegration> {
  return api.pluginGet<GitLabIntegration>(PLUGIN_ID, `/projects/${api.projectId}/integration`);
}

export async function setGitLabToken(
  api: PluginApiClient,
  token: string,
  opts?: { instanceUrl?: string; tokenKind?: GitLabTokenKind },
): Promise<GitLabIntegration> {
  return api.pluginPost<GitLabIntegration>(PLUGIN_ID, `/projects/${api.projectId}/integration/token`, {
    token,
    instance_url: opts?.instanceUrl || "https://gitlab.com",
    token_kind: opts?.tokenKind || "personal",
  });
}

export async function deleteGitLabToken(api: PluginApiClient): Promise<void> {
  return api.pluginDelete(PLUGIN_ID, `/projects/${api.projectId}/integration/token`);
}

export async function listAccessibleRepos(
  api: PluginApiClient,
): Promise<AccessibleRepo[]> {
  return api.pluginGet<AccessibleRepo[]>(PLUGIN_ID, `/projects/${api.projectId}/integration/accessible-repos`);
}

export async function linkRepository(
  api: PluginApiClient,
  owner: string,
  repoName: string,
): Promise<LinkedRepository> {
  return api.pluginPost<LinkedRepository>(
    PLUGIN_ID,
    `/projects/${api.projectId}/repositories`,
    { owner, repo_name: repoName },
  );
}

export async function listLinkedRepositories(
  api: PluginApiClient,
): Promise<LinkedRepository[]> {
  return api.pluginGet<LinkedRepository[]>(
    PLUGIN_ID,
    `/projects/${api.projectId}/repositories`,
  );
}

export async function unlinkRepository(
  api: PluginApiClient,
  repoId: string,
): Promise<void> {
  return api.pluginDelete(
    PLUGIN_ID,
    `/projects/${api.projectId}/repositories/${repoId}`,
  );
}

export async function listTaskPRs(
  api: PluginApiClient,
  taskId: string,
): Promise<PullRequest[]> {
  return api.pluginGet<PullRequest[]>(
    PLUGIN_ID,
    `/projects/${api.projectId}/tasks/${taskId}/pull-requests`,
  );
}

export async function linkPRToTask(
  api: PluginApiClient,
  taskId: string,
  repoId: string,
  prNumber: number,
): Promise<PullRequest> {
  return api.pluginPost<PullRequest>(
    PLUGIN_ID,
    `/projects/${api.projectId}/tasks/${taskId}/pull-requests/link`,
    { repo_id: repoId, pr_number: prNumber },
  );
}

export async function unlinkPRFromTask(
  api: PluginApiClient,
  taskId: string,
  prId: string,
): Promise<void> {
  return api.pluginDelete(
    PLUGIN_ID,
    `/projects/${api.projectId}/tasks/${taskId}/pull-requests/${prId}`,
  );
}

export async function listTaskBranches(
  api: PluginApiClient,
  taskId: string,
): Promise<TaskBranch[]> {
  return api.pluginGet<TaskBranch[]>(
    PLUGIN_ID,
    `/projects/${api.projectId}/tasks/${taskId}/branches`,
  );
}

export async function createBranch(
  api: PluginApiClient,
  taskId: string,
  repoId: string,
  branchName: string,
  sourceBranch?: string,
): Promise<CreateBranchResult> {
  return api.pluginPost<CreateBranchResult>(
    PLUGIN_ID,
    `/projects/${api.projectId}/tasks/${taskId}/branches`,
    { repo_id: repoId, branch_name: branchName, source_branch: sourceBranch },
  );
}

export async function linkBranchToTask(
  api: PluginApiClient,
  taskId: string,
  repoId: string,
  branchName: string,
): Promise<TaskBranch> {
  return api.pluginPost<TaskBranch>(
    PLUGIN_ID,
    `/projects/${api.projectId}/tasks/${taskId}/branches/link`,
    { repo_id: repoId, branch_name: branchName },
  );
}
