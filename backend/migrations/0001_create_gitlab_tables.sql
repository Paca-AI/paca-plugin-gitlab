-- 0001_create_gitlab_tables.sql
-- Creates the GitLab integration tables in the plugin schema.
-- Run with search_path = plugin_data_com_paca_gitlab, public.

CREATE TABLE IF NOT EXISTS gitlab_integrations (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id       UUID        NOT NULL UNIQUE REFERENCES projects(id) ON DELETE CASCADE,
    access_token_enc TEXT        NOT NULL,
    instance_url     TEXT        NOT NULL DEFAULT 'https://gitlab.com',
    token_kind       TEXT        NOT NULL DEFAULT 'personal', -- personal | project | group
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_gitlab_integrations_project_id
    ON gitlab_integrations (project_id);

CREATE TABLE IF NOT EXISTS gitlab_repositories (
    id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id         UUID        NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    integration_id     UUID        NOT NULL REFERENCES gitlab_integrations(id) ON DELETE CASCADE,
    owner              TEXT        NOT NULL,
    repo_name          TEXT        NOT NULL,
    full_name          TEXT        NOT NULL, -- path_with_namespace
    gitlab_project_id  BIGINT      NOT NULL DEFAULT 0,
    webhook_id         BIGINT      NOT NULL DEFAULT 0,
    webhook_secret_enc TEXT        NOT NULL DEFAULT '', -- signing_token (whsec_...)
    default_branch     TEXT        NOT NULL DEFAULT 'main',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (project_id, full_name)
);

CREATE INDEX IF NOT EXISTS idx_gitlab_repositories_project_id
    ON gitlab_repositories (project_id);
CREATE INDEX IF NOT EXISTS idx_gitlab_repositories_full_name
    ON gitlab_repositories (full_name);

CREATE TABLE IF NOT EXISTS gitlab_merge_requests (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   UUID        NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    repo_id      UUID        NOT NULL REFERENCES gitlab_repositories(id) ON DELETE CASCADE,
    pr_number    INT         NOT NULL, -- GitLab MR iid (project-scoped)
    gitlab_mr_id BIGINT      NOT NULL,
    title        TEXT        NOT NULL DEFAULT '',
    state        TEXT        NOT NULL DEFAULT 'open',
    html_url     TEXT        NOT NULL DEFAULT '',
    head_branch  TEXT        NOT NULL DEFAULT '',
    base_branch  TEXT        NOT NULL DEFAULT '',
    author       TEXT        NOT NULL DEFAULT '',
    merged_at    TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (repo_id, pr_number)
);

CREATE INDEX IF NOT EXISTS idx_gitlab_merge_requests_repo_id
    ON gitlab_merge_requests (repo_id);

CREATE TABLE IF NOT EXISTS gitlab_task_mr_links (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id          UUID        NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    merge_request_id UUID        NOT NULL REFERENCES gitlab_merge_requests(id) ON DELETE CASCADE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (task_id, merge_request_id)
);

CREATE INDEX IF NOT EXISTS idx_gitlab_task_mr_links_task_id
    ON gitlab_task_mr_links (task_id);

CREATE TABLE IF NOT EXISTS gitlab_task_branches (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id     UUID        NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    repo_id     UUID        NOT NULL REFERENCES gitlab_repositories(id) ON DELETE CASCADE,
    branch_name TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (task_id, repo_id, branch_name)
);

CREATE INDEX IF NOT EXISTS idx_gitlab_task_branches_task_id
    ON gitlab_task_branches (task_id);
