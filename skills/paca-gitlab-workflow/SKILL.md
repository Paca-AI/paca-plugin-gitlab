---
name: paca-gitlab-workflow
description: Link a GitLab repository to a Paca project, create branches, open and manage merge requests, review PRs, and check CI status — using the com.gorlix.gitlab plugin's tools. Use when asked to open a merge request, create a branch, link a merge request to a task, review or comment on a PR, or check CI/build status for a task.
triggers:
  - /paca-gitlab-workflow
  - create a merge request
  - open a merge request
  - create branch
  - link merge request
  - review this pr
  - check ci status
---

# GitLab Workflow Skill

This plugin (`com.gorlix.gitlab`) connects a Paca project to GitLab via a Personal Access Token (PAT) — not a GitLab App — and links repositories, branches, and merge requests to individual tasks. All GitLab API calls happen server-side; you only ever call this plugin's tools.

## Prerequisites — check before doing anything else

1. `gitlab_get_integration({projectId})`. If `connected` is false, a GitLab PAT must be connected first (scopes `api` (Maintainer/Owner to manage webhooks)) — you cannot mint one yourself. Tell the user to add it under Project Settings → GitLab, or, if they give you a token directly in the conversation, call `gitlab_set_token({projectId, token})` yourself.
2. `gitlab_list_linked_repos({projectId})`. If empty and you already know the exact `owner`/`repo_name`, call `gitlab_link_repository({projectId, owner, repo_name})` — this also creates the required webhook automatically. If you don't know the exact owner/repo name, ask the user; there's no tool to browse "which repos can this token see" (that picker is UI-only).

Every branch/PR tool below 404s (`GITLAB_INTEGRATION_NOT_FOUND` / `GITLAB_REPOSITORY_NOT_FOUND`) until both of these are satisfied.

## Tools

**Integration**
- `gitlab_get_integration(projectId)` — connection status.
- `gitlab_set_token(projectId, token)` — only when the user hands you a token directly.
- `gitlab_delete_token(projectId)` — disconnects and removes webhooks.

**Repositories**
- `gitlab_list_linked_repos(projectId)`
- `gitlab_link_repository(projectId, owner, repo_name)`
- `gitlab_unlink_repository(projectId, repoId)`

**Branches**
- `gitlab_create_branch(projectId, taskId, repoId, branch_name, source_branch?)` — `source_branch` defaults to the repo's default branch if omitted.
- `gitlab_list_task_branches(projectId, taskId)`

**Merge requests**
- `gitlab_list_task_prs(projectId, taskId)`
- `gitlab_create_pull_request(projectId, taskId, repoId, title, head_branch, base_branch, body?)` — creates the PR on GitLab **and** links it to the task in the same call.
- `gitlab_link_pr_to_task(projectId, taskId, repoId, pr_number)` — only for a PR that already existed before you touched it (opened by a human, or via `glab mr create` directly).
- `gitlab_unlink_pr_from_task(projectId, taskId, prId)`
- `gitlab_get_pull_request(projectId, taskId, prId)` — title, state, body, and the full diff.
- `gitlab_get_pull_request_ci_status(projectId, taskId, prId)` — combined GitLab Actions / check-run / legacy status state.
- `gitlab_comment_pull_request(projectId, taskId, prId, body)` — a plain issue-style comment.
- `gitlab_review_pull_request(projectId, taskId, prId, event, body?)` — `event` is `APPROVE`, `REQUEST_CHANGES`, or `COMMENT`; `body` is required by GitLab for the latter two.

`repoId` and `prId` are Paca's own internal ids — get `repoId` from `gitlab_list_linked_repos` and `prId` from `gitlab_list_task_prs`. Neither is a GitLab PR number or `owner/repo` string; don't substitute one for the other.

## Workflow — finishing a task with a PR

1. Confirm the prerequisites above.
2. Name the branch `<type>/<PREFIX>-<number>[-slug]` — e.g. `feat/PROJ-42-add-auth` — matching the task's own reference. This is load-bearing, not cosmetic: both the manual UI and the webhook auto-linker match on exactly this pattern to associate branches and PRs with a task without an explicit link call.
3. `gitlab_create_branch`, or push a branch with a matching name yourself.
4. Commit and push your changes (plain git — outside this plugin's scope).
5. `gitlab_create_pull_request`. This both opens the PR and links it to the task — don't also call `gitlab_link_pr_to_task` afterward; that's only for PRs that already existed independently of you.
6. Before merging or requesting review, call `gitlab_get_pull_request_ci_status`. Treat `pending` **and** `unknown` as not yet safe to merge — `unknown` means no checks have reported at all, not that everything passed.

## Reviewing a PR

1. `gitlab_get_pull_request` first, to read the diff — never review blind.
2. `gitlab_review_pull_request` with `event: APPROVE | REQUEST_CHANGES | COMMENT`.
3. For a lighter note that isn't a formal review verdict, use `gitlab_comment_pull_request` instead.

## Constraints

- `gitlab_comment_pull_request` and `gitlab_review_pull_request` post directly to GitLab — they do **not** add anything to the Paca task's own activity or comments. If the user also wants a note left on the Paca task, add that separately with the core `add_task_comment` tool.
- You cannot mint a GitLab PAT and cannot enumerate which repos a token can see — if you don't already know the exact `owner`/`repo_name`, ask rather than guess.
