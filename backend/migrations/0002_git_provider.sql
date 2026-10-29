-- 0002_git_provider.sql
-- The GitLab provider's own state: the access-token identities Paca connects
-- with (tokens and webhook secrets encrypted) and the setup sessions that tie a
-- token-form round trip together.
-- Run with search_path = plugin_data_com_paca_gitlab, public.

CREATE TABLE IF NOT EXISTS gitlab_identities (
    id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    host               TEXT        NOT NULL,
    user_id            BIGINT      NOT NULL DEFAULT 0,
    username           TEXT        NOT NULL,
    display_name       TEXT        NOT NULL DEFAULT '',
    bot                BOOLEAN     NOT NULL DEFAULT FALSE,
    token_enc          TEXT        NOT NULL,
    read_token_enc     TEXT        NOT NULL DEFAULT '',
    webhook_secret_enc TEXT        NOT NULL DEFAULT '',
    -- The earliest expiry of the stored tokens; NULL when GitLab reports none.
    expires_at         TIMESTAMPTZ,
    created_by         TEXT        NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- One row per identity-setup attempt. state is the single-use nonce core echoes
-- back when the token form is submitted.
CREATE TABLE IF NOT EXISTS gitlab_setup_sessions (
    state        TEXT        PRIMARY KEY,
    -- Whom the identity is for: the shared one, or one agent's own.
    scope        TEXT        NOT NULL DEFAULT 'shared',
    agent_id     TEXT        NOT NULL DEFAULT '',
    host         TEXT        NOT NULL,
    completed_at TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The plugin is a Git provider: repositories, merge requests, task links and the
-- per-project connection now live in Paca core (git_* tables). The previous
-- release's project-bound tables (0001) are kept here until
-- 0004_migrate_community_plugin.sql has carried their rows over to core, and
-- dropped there.
